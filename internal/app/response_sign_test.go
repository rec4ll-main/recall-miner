package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/subtrahend-labs/gobt/boilerplate"

	"recall-miner/internal/auth"
)

// TestValidatorAuthedResponseIsSigned exercises the full route chain: a
// validator-signed request to a validator-authed endpoint must produce a
// signed response whose envelope verifies against the serving hotkey.
func TestValidatorAuthedResponseIsSigned(t *testing.T) {
	srv := testServer(t)
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	srv.keypair = &kp
	srv.cfg.MinerHotkey = kp.Address
	srv.cfg.ApplicationProberHotkey = kp.Address

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	setEpistulaHeaders(t, req, kp, kp.Address, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	sig := rec.Header().Get(auth.ResponseSignatureHeader)
	if sig == "" {
		t.Fatal("response signature header missing")
	}
	timestamp := rec.Header().Get(auth.ResponseTimestampHeader)
	if timestamp == "" {
		t.Fatal("response timestamp header missing")
	}
	requestUUID := rec.Header().Get(auth.ResponseUUIDHeader)
	if requestUUID == "" {
		t.Fatal("response uuid header missing")
	}
	if got := rec.Header().Get(auth.ResponseSignedByHeader); got != kp.Address {
		t.Fatalf("signed-by = %q, want %q", got, kp.Address)
	}

	respHash := sha256.Sum256(rec.Body.Bytes())
	message := auth.ResponseMessage(http.MethodGet, "/services", "", "200", auth.EnvelopeBodyHash(nil), requestUUID, hex.EncodeToString(respHash[:]), timestamp, kp.Address)
	raw, err := hex.DecodeString(strings.TrimPrefix(sig, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	if !boilerplate.VerfySignature(kp.Address, sha256Bytes([]byte(message)), raw) {
		t.Fatal("response signature did not verify against reconstructed message")
	}
}

func TestResponseSigningDisabledOmitsHeaders(t *testing.T) {
	srv := testServer(t)
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	srv.keypair = &kp
	srv.cfg.MinerHotkey = kp.Address
	srv.cfg.ApplicationProberHotkey = kp.Address
	srv.cfg.ResponseSigningEnabled = false

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	setEpistulaHeaders(t, req, kp, kp.Address, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(auth.ResponseSignatureHeader) != "" {
		t.Fatal("response signature header must be absent when signing is disabled")
	}
}

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
