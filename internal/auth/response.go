package auth

import (
	"bytes"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/google/uuid"

	"recall-miner/internal/httpx"
)

// Response header names set on validator-authed responses. Epistula-Uuid and
// Epistula-Timestamp reuse the request header names: on a response they carry
// the echoed request uuid and the signing time respectively, and are read
// back by recall-web's verifier.
const (
	ResponseSignatureHeader = "Epistula-Response-Signature"
	ResponseTimestampHeader = "Epistula-Timestamp"
	ResponseUUIDHeader      = "Epistula-Uuid"
	ResponseSignedByHeader  = "Epistula-Signed-By"
)

// ResponseMessage canonicalizes the response attributes bound to the
// response signature. The signer (miner) and verifier (recall-web) must
// produce the identical string, so any change here must be mirrored in the
// recall-web minerclient package. The subnet id is deliberately excluded:
// request-uuid binding alone prevents cross-request replay, and including
// it would create cross-repo config-divergence hazards.
func ResponseMessage(method, path, query, statusCode, requestBodyHash, requestUUID, responseBodyHash, timestampMs, servingHotkey string) string {
	return strings.Join([]string{
		"recall-epistula-response-v1",
		strings.ToUpper(strings.TrimSpace(method)),
		path,
		canonicalQuery(query),
		statusCode,
		requestBodyHash,
		requestUUID,
		responseBodyHash,
		timestampMs,
		servingHotkey,
	}, "\n")
}

// SignResponseMessage signs the sha256 digest of the canonical response
// message with the given keypair and returns the 0x-prefixed hex signature.
// Signing an explicit digest keeps the signer deterministic on both sides
// (the underlying signer pre-hashes inputs over 256 bytes with blake2b,
// which would otherwise disagree with the sha256-based verifier).
func SignResponseMessage(kp signature.KeyringPair, message string) (string, error) {
	sig, err := signature.Sign(sha256Digest([]byte(message)), kp.URI)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(sig), nil
}

// ResponseSigner wraps a handler and signs its buffered response before any
// body bytes are written. It is a no-op when disabled or when no keypair is
// available, so a miner without a hotkey still serves unsigned responses.
type ResponseSigner struct {
	keypair      *signature.KeyringPair
	enabled      bool
	maxBodyBytes int64
	logger       *slog.Logger
}

func NewResponseSigner(keypair *signature.KeyringPair, enabled bool, maxBodyBytes int64, logger *slog.Logger) *ResponseSigner {
	return &ResponseSigner{keypair: keypair, enabled: enabled, maxBodyBytes: maxBodyBytes, logger: logger}
}

// Wrap returns the signer middleware, or next unchanged when signing is
// disabled or no keypair is configured.
func (s *ResponseSigner) Wrap(next http.Handler) http.Handler {
	if s == nil || !s.enabled || s.keypair == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := s.maxBodyBytes
		if limit < minBodyFloor {
			limit = minBodyFloor
		}
		requestBody, tooLarge, err := httpx.ReadBodyLimited(r, limit)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("unable to read request body for response signing", "path", r.URL.Path, "error", err)
			}
			next.ServeHTTP(w, r)
			return
		}
		if tooLarge {
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"detail": "request body too large"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(requestBody))

		buf := &responseBuffer{rw: w}
		next.ServeHTTP(buf, r)
		s.finalize(buf, r, requestBody)
	})
}

// finalize signs the buffered response and writes it out, setting the
// response signature headers before the status line and body.
func (s *ResponseSigner) finalize(buf *responseBuffer, r *http.Request, requestBody []byte) {
	status := buf.status
	if status == 0 {
		status = http.StatusOK
	}
	requestUUID := r.Header.Get("Epistula-Uuid")
	if requestUUID == "" {
		requestUUID = uuid.New().String()
	}
	timestampMs := strconv.FormatInt(time.Now().UnixMilli(), 10)
	message := ResponseMessage(
		r.Method,
		r.URL.EscapedPath(),
		r.URL.RawQuery,
		strconv.Itoa(status),
		bodyHash(requestBody),
		requestUUID,
		bodyHash(buf.body.Bytes()),
		timestampMs,
		s.keypair.Address,
	)
	sig, err := SignResponseMessage(*s.keypair, message)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("failed to sign response", "path", r.URL.Path, "error", err)
		}
	} else {
		header := buf.rw.Header()
		header.Set(ResponseSignatureHeader, sig)
		header.Set(ResponseTimestampHeader, timestampMs)
		header.Set(ResponseUUIDHeader, requestUUID)
		header.Set(ResponseSignedByHeader, s.keypair.Address)
	}
	buf.rw.WriteHeader(status)
	_, _ = buf.rw.Write(buf.body.Bytes())
}

// responseBuffer defers the status line and body writes until the handler
// returns, so the response signature headers (which depend on the body hash)
// can be set before any bytes reach the wire.
type responseBuffer struct {
	rw          http.ResponseWriter
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (b *responseBuffer) Header() http.Header { return b.rw.Header() }

func (b *responseBuffer) WriteHeader(status int) {
	if b.wroteHeader {
		return
	}
	b.wroteHeader = true
	b.status = status
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	return b.body.Write(p)
}
