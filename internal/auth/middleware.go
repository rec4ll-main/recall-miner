package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/subtrahend-labs/gobt/boilerplate"
	"github.com/vedhavyas/go-subkey/v2"

	"recall-miner/internal/httpx"
)

const maxEpistulaAge = 15 * time.Second

// minBodyFloor is the smallest inner body bound the auth path will accept
// (defense-in-depth if a caller passes a zero or negative configured cap).
const minBodyFloor = 1 << 20

// ReplayCache rejects duplicate (sender, uuid) pairs. Entries are retained
// until the signed timestamp plus the acceptance window, so an entry always
// outlives its own acceptance window even for future-skewed timestamps.
// Expired entries are pruned lazily; when at capacity after pruning the
// cache fails closed (rejects).
type ReplayCache struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	age     time.Duration
	maxSize int
	clock   func() time.Time
}

func NewReplayCache(age time.Duration, maxSize int) *ReplayCache {
	return &ReplayCache{seen: make(map[string]time.Time), age: age, maxSize: maxSize, clock: time.Now}
}

// checkAndRecord reports whether a (sender, uuid) pair is fresh and records
// it. Expiry is keyed to the signed timestamp, not receipt time.
func (r *ReplayCache) checkAndRecord(sender, requestUUID string, signedAt time.Time) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	for key, expiresAt := range r.seen {
		if now.After(expiresAt) {
			delete(r.seen, key)
		}
	}
	key := sender + ":" + requestUUID
	if _, ok := r.seen[key]; ok {
		return false
	}
	if r.maxSize > 0 && len(r.seen) >= r.maxSize {
		return false
	}
	r.seen[key] = signedAt.Add(r.age)
	return true
}

// VerifiedEpistula carries the result of a successful Epistula verification
// so the caller can authorize and record the replay pair in the right order.
type VerifiedEpistula struct {
	SignedBy    string
	RequestUUID string
	Timestamp   string
	SignedAt    time.Time
}

// EnvelopeMessage canonicalizes the request attributes that must be bound
// to the Epistula signature. The verifier and signer must produce the
// identical string for a request, so any change to this function must be
// mirrored in the signing client.
func EnvelopeMessage(method, path, query, contentType, bodyHash, requestUUID, timestamp, signedFor, signedBy string) string {
	return strings.Join([]string{
		"recall-epistula-v1",
		strings.ToUpper(strings.TrimSpace(method)),
		path,
		canonicalQuery(query),
		strings.ToLower(strings.TrimSpace(contentType)),
		strings.Join([]string{bodyHash, requestUUID, timestamp, signedFor, signedBy}, "."),
	}, "\n")
}

// canonicalQuery is injective: every key and value is re-escaped before
// joining, so percent-encoded separators cannot collide with real ones
// (e.g. "a=1%26b%3D2" != "a=1&b=2"). Pairs are sorted so reordering the
// raw query is still the same canonical request.
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	var pairs []string
	for key, vals := range values {
		for _, v := range vals {
			pairs = append(pairs, url.QueryEscape(key)+"="+url.QueryEscape(v))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func sha256Digest(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// EnvelopeBodyHash is the hex sha256 of the request body, used inside the
// canonical envelope message.
func EnvelopeBodyHash(body []byte) string {
	return bodyHash(body)
}

// EnvelopeDigest is the sha256 digest of the canonical envelope message
// that the envelope signature covers.
func EnvelopeDigest(message string) []byte {
	return sha256Digest([]byte(message))
}

func verifyEnvelopeSignature(signedBy, message, envelopeSig string) error {
	if err := validateSignedBy(signedBy); err != nil {
		return err
	}
	raw, err := decodeSignature(envelopeSig)
	if err != nil {
		return fmt.Errorf("failed decoding envelope signature: %w", err)
	}
	if len(raw) != sr25519SignatureLength {
		return fmt.Errorf("invalid envelope signature length")
	}
	// The envelope contract signs the sha256 digest of the canonical
	// message. The underlying signer pre-hashes inputs over 256 bytes with
	// blake2b, which would silently disagree with this verifier for long
	// messages; signing an explicit digest keeps both sides deterministic.
	if !boilerplate.VerfySignature(signedBy, sha256Digest([]byte(message)), raw) {
		return fmt.Errorf("envelope signature mismatch")
	}
	return nil
}

// invalidSignatureLog lets one "invalid epistula request" warning through per second and
// counts the rest. Requests that claim the Recall API signing key are not throttled before
// verification (ratelimit.FailureGuard), so forged ones must not be able to flood the log.
type invalidSignatureLog struct {
	last       atomic.Int64 // unix second of the last warning written
	suppressed atomic.Int64
}

func (l *invalidSignatureLog) warn(logger *slog.Logger, path string, err error) {
	if logger == nil {
		return
	}
	now := time.Now().Unix()
	prev := l.last.Load()
	if now == prev || !l.last.CompareAndSwap(prev, now) {
		l.suppressed.Add(1)
		return
	}
	logger.Warn("invalid epistula request", "path", path, "error", err, "suppressed_since_last", l.suppressed.Swap(0))
}

func Validator(minerHotkey, validatorHotkey string, envelopeRequired bool, maxBodyBytes int64, replays *ReplayCache, logger *slog.Logger, next http.Handler) http.Handler {
	invalidLog := &invalidSignatureLog{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := maxBodyBytes
		if limit < minBodyFloor {
			limit = minBodyFloor
		}
		body, tooLarge, err := httpx.ReadBodyLimited(r, limit)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, err)
			return
		}
		if tooLarge {
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"detail": "request body too large"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		verified, err := VerifyEpistula(r, body, minerHotkey, envelopeRequired)
		if err != nil {
			invalidLog.warn(logger, r.URL.Path, err)
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Invalid Epistula signature"})
			return
		}
		if validatorHotkey != "" && verified.SignedBy != validatorHotkey {
			if logger != nil {
				logger.Warn("unauthorized validator hotkey", "path", r.URL.Path, "signed_by", verified.SignedBy)
			}
			httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"detail": "Unauthorized validator hotkey"})
			return
		}
		// Replay detection runs only after signature and authorization
		// pass, so a rejected request never consumes a legitimate
		// (sender, uuid) pair.
		if replays != nil && !replays.checkAndRecord(verified.SignedBy, verified.RequestUUID, verified.SignedAt) {
			if logger != nil {
				logger.Warn("replayed epistula request", "path", r.URL.Path, "signed_by", verified.SignedBy, "uuid", verified.RequestUUID)
			}
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Replayed Epistula request"})
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), verifiedSenderKey{}, verified.SignedBy))
		r.Header.Set("X-Verified-Hotkey", verified.SignedBy)
		next.ServeHTTP(w, r)
	})
}

func Internal(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Internal registration not configured"})
			return
		}
		if !ConstantTimeKeyEqual(r.Header.Get("X-Internal-Api-Key"), apiKey) {
			httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"detail": "Invalid internal API key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ConstantTimeKeyEqual reports whether provided and expected are equal using
// a constant-time comparison so key verification does not leak byte position
// through timing. Unequal lengths short-circuit (the standard subtle package
// behavior); that is acceptable because internal keys are high-entropy and
// their length is not secret.
func ConstantTimeKeyEqual(provided, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// VerifyEpistula verifies the base Epistula signature and the envelope
// binding without touching the replay cache; the caller records the pair
// only after its own authorization passes.
func VerifyEpistula(r *http.Request, body []byte, minerHotkey string, envelopeRequired bool) (VerifiedEpistula, error) {
	sig := r.Header.Get("Epistula-Request-Signature")
	timestamp := r.Header.Get("Epistula-Timestamp")
	requestUUID := r.Header.Get("Epistula-Uuid")
	signedFor := r.Header.Get("Epistula-Signed-For")
	signedBy := r.Header.Get("Epistula-Signed-By")
	if sig == "" || timestamp == "" || requestUUID == "" || signedFor == "" || signedBy == "" {
		return VerifiedEpistula{}, fmt.Errorf("missing epistula headers")
	}
	if err := validateSignedBy(signedBy); err != nil {
		return VerifiedEpistula{}, err
	}
	if minerHotkey != "" && signedFor != minerHotkey {
		return VerifiedEpistula{}, fmt.Errorf("signed_for mismatch")
	}
	expectedSignedFor := minerHotkey
	if expectedSignedFor == "" {
		expectedSignedFor = signedFor
	}

	millis, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return VerifiedEpistula{}, err
	}
	requestTime := time.UnixMilli(millis)
	if age := time.Since(requestTime); age > maxEpistulaAge || age < -maxEpistulaAge {
		return VerifiedEpistula{}, fmt.Errorf("request timestamp outside allowed age")
	}

	if err := verifyBaseSignature(sig, signedBy, body, timestamp, requestUUID, expectedSignedFor, signedFor); err != nil {
		return VerifiedEpistula{}, err
	}

	if envelopeRequired {
		envSig := r.Header.Get("Epistula-Envelope-Signature")
		if envSig == "" {
			return VerifiedEpistula{}, fmt.Errorf("missing envelope signature")
		}
		message := EnvelopeMessage(r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Content-Type"), bodyHash(body), requestUUID, timestamp, signedFor, signedBy)
		if err := verifyEnvelopeSignature(signedBy, message, envSig); err != nil {
			return VerifiedEpistula{}, err
		}
	}

	return VerifiedEpistula{SignedBy: signedBy, RequestUUID: requestUUID, Timestamp: timestamp, SignedAt: requestTime}, nil
}

// verifyBaseSignature decodes and length-guards the Epistula request
// signature before delegating to gobt, which panics on short signatures.
func verifyBaseSignature(sig, signedBy string, body []byte, timestamp, requestUUID, expectedSignedFor, signedFor string) error {
	raw, err := decodeSignature(sig)
	if err != nil {
		return err
	}
	if len(raw) != sr25519SignatureLength {
		return fmt.Errorf("invalid signature length")
	}
	return boilerplate.VerifyEpistulaHeaders(expectedSignedFor, sig, body, timestamp, requestUUID, signedFor, signedBy)
}

func decodeSignature(value string) ([]byte, error) {
	hexValue := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	return hex.DecodeString(hexValue)
}

const sr25519SignatureLength = 64
const sr25519PublicKeyLength = 32

// validateSignedBy rejects signer addresses whose SS58-decoded payload is
// not exactly 32 bytes. gobt's VerfySignature converts the decoded payload
// with [32]byte(pubk), which panics for valid-checksum short payloads, and
// subkey.SS58Decode itself panics on some malformed inputs ("NZoQ", "4jS").
// The recover wrapper converts any decoder panic into a normal auth error,
// so no malformed signedBy can reach gobt or crash the handler.
func validateSignedBy(signedBy string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("signed_by address decode panicked: %v", r)
		}
	}()
	_, pubKey, decodeErr := subkey.SS58Decode(signedBy)
	if decodeErr != nil {
		return fmt.Errorf("decode signed_by address: %w", decodeErr)
	}
	if len(pubKey) != sr25519PublicKeyLength {
		return fmt.Errorf("signed_by address payload is not %d bytes", sr25519PublicKeyLength)
	}
	return nil
}

func KeyringFromSecret(secret string, ss58Format uint16) (signature.KeyringPair, error) {
	if secret == "" {
		return signature.KeyringPair{}, fmt.Errorf("HOTKEY_URI or HOTKEY_PHRASE is required")
	}
	return signature.KeyringPairFromSecret(secret, ss58Format)
}

func LogHotkey(logger *slog.Logger, keypair signature.KeyringPair) {
	logger.Info("miner hotkey loaded", "hotkey", keypair.Address)
}

// VerifiedSender returns only the identity installed after envelope verification.
type verifiedSenderKey struct{}

func VerifiedSender(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(verifiedSenderKey{}).(string)
	return v, ok && v != ""
}
