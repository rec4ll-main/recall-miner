package auth

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/subtrahend-labs/gobt/boilerplate"
)

// Golden vector inputs shared with recall-web's minerclient verifier. The
// canonical string and its sha256 digest are deterministic and byte-identical
// across the two repos. sr25519 signatures themselves are randomized (the
// signer derives a fresh nonce per call), so signature correctness is pinned
// by a sign->verify round-trip rather than a fixed hex literal.
const (
	goldenMethod           = "POST"
	goldenPath             = "/db/abc123/query"
	goldenQuery            = "b=2&a=1"
	goldenStatusCode       = "200"
	goldenRequestBodyHash  = "fe97fd255439a6bc99506f20ce2d26abc6109e68e0327249d3f0a77cbc4d3a74"
	goldenRequestUUID      = "545376da-4230-4b28-a991-20fa1784910e"
	goldenResponseBodyHash = "aad3fb714a0028cd940bdc2564ba9a2951bdd48c8644fdca02b03231f577a26a"
	goldenTimestampMs      = "1787153905211"
	goldenHotkey           = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
)

const goldenCanonical = "recall-epistula-response-v1\nPOST\n/db/abc123/query\na=1&b=2\n200\n" +
	"fe97fd255439a6bc99506f20ce2d26abc6109e68e0327249d3f0a77cbc4d3a74\n" +
	"545376da-4230-4b28-a991-20fa1784910e\n" +
	"aad3fb714a0028cd940bdc2564ba9a2951bdd48c8644fdca02b03231f577a26a\n" +
	"1787153905211\n5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"

const goldenDigestHex = "851052ff7b5b640b82d88373caeb908697f84fb52883e8d30df1d793a4198a1e"

func TestGoldenResponseMessageVector(t *testing.T) {
	got := ResponseMessage(goldenMethod, goldenPath, goldenQuery, goldenStatusCode, goldenRequestBodyHash, goldenRequestUUID, goldenResponseBodyHash, goldenTimestampMs, goldenHotkey)
	if got != goldenCanonical {
		t.Fatalf("golden vector mismatch:\n got: %q\nwant: %q", got, goldenCanonical)
	}
	if digest := hex.EncodeToString(sha256Digest([]byte(got))); digest != goldenDigestHex {
		t.Fatalf("golden digest mismatch: got %q, want %q", digest, goldenDigestHex)
	}
}

// TestGoldenResponseMessageTenFields locks the reworked 10-field form: the
// legacy subnet-id field was removed from the canonical string on both
// sides, so no field line may sit between timestampMs and servingHotkey.
func TestGoldenResponseMessageTenFields(t *testing.T) {
	lines := strings.Split(goldenCanonical, "\n")
	if len(lines) != 10 {
		t.Fatalf("canonical string must be exactly 10 fields, got %d: %q", len(lines), goldenCanonical)
	}
	// The legacy pre-rework form inserted a subnet id ("31") between the
	// timestamp and the hotkey.
	if lines[8] == "31" || strings.Contains(goldenCanonical, "1787153905211\n31\n") {
		t.Fatalf("canonical string must not contain a subnet-id line: %q", goldenCanonical)
	}
}

func TestResponseMessageMethodNormalized(t *testing.T) {
	got := ResponseMessage(" post ", "/x", "", "200", "a", "b", "c", "1", "h")
	if !strings.HasPrefix(got, "recall-epistula-response-v1\nPOST\n/x\n") {
		t.Fatalf("method not normalized: %q", got)
	}
}

func TestResponseMessageQueryCanonicalized(t *testing.T) {
	got := ResponseMessage("POST", "/x", "b=2&a=1", "200", "a", "b", "c", "1", "h")
	if !strings.Contains(got, "\na=1&b=2\n") {
		t.Fatalf("query not canonicalized: %q", got)
	}
}

func TestSignResponseMessageRoundTrip(t *testing.T) {
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := SignResponseMessage(kp, goldenCanonical)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decodeSignature(sig)
	if err != nil {
		t.Fatal(err)
	}
	if !boilerplate.VerfySignature(kp.Address, sha256Digest([]byte(goldenCanonical)), raw) {
		t.Fatal("signed response message did not verify")
	}
}

func TestResponseSignerSetsHeadersAndSigns(t *testing.T) {
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	signer := NewResponseSigner(&kp, true, testMaxBodyBytes, nil)

	requestBody := []byte(`{"vector":[0.1,0.2],"top_k":10}`)
	responseBody := []byte(`{"matches":[{"id":"doc1","score":0.95}]}`)

	handler := signer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))

	req := httptest.NewRequest(http.MethodPost, "/db/abc123/query?b=2&a=1", strings.NewReader(string(requestBody)))
	req.Header.Set("Epistula-Uuid", goldenRequestUUID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if string(rec.Body.Bytes()) != string(responseBody) {
		t.Fatalf("body = %q, want %q", rec.Body.String(), responseBody)
	}
	if rec.Header().Get(ResponseUUIDHeader) != goldenRequestUUID {
		t.Fatalf("uuid = %q, want echoed %q", rec.Header().Get(ResponseUUIDHeader), goldenRequestUUID)
	}
	if rec.Header().Get(ResponseSignedByHeader) != kp.Address {
		t.Fatalf("signed-by = %q, want %q", rec.Header().Get(ResponseSignedByHeader), kp.Address)
	}
	timestamp := rec.Header().Get(ResponseTimestampHeader)
	if timestamp == "" {
		t.Fatal("timestamp header missing")
	}
	sig := rec.Header().Get(ResponseSignatureHeader)
	if sig == "" {
		t.Fatal("signature header missing")
	}

	message := ResponseMessage(http.MethodPost, "/db/abc123/query", "b=2&a=1", "200", bodyHash(requestBody), goldenRequestUUID, bodyHash(responseBody), timestamp, kp.Address)
	raw, err := decodeSignature(sig)
	if err != nil {
		t.Fatal(err)
	}
	if !boilerplate.VerfySignature(kp.Address, sha256Digest([]byte(message)), raw) {
		t.Fatal("response signature did not verify against reconstructed message")
	}
}

func TestResponseSignerGeneratesFreshUUIDWhenAbsent(t *testing.T) {
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	signer := NewResponseSigner(&kp, true, testMaxBodyBytes, nil)
	handler := signer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get(ResponseUUIDHeader)
	if got == "" {
		t.Fatal("expected fresh uuid header when request uuid absent")
	}
	if got == goldenRequestUUID {
		t.Fatal("fresh uuid must not equal the request uuid")
	}
}

func TestResponseSignerDisabledWritesBodyUnsigned(t *testing.T) {
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	signer := NewResponseSigner(&kp, false, testMaxBodyBytes, nil)
	handler := signer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("disabled signer altered response: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(ResponseSignatureHeader) != "" {
		t.Fatal("disabled signer must not set signature header")
	}
}

func TestResponseSignerNilKeypairWritesBodyUnsigned(t *testing.T) {
	signer := NewResponseSigner(nil, true, testMaxBodyBytes, nil)
	handler := signer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("nil-keypair signer altered response: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(ResponseSignatureHeader) != "" {
		t.Fatal("nil keypair must not set signature header")
	}
}

func TestResponseSignerImplicitStatus200(t *testing.T) {
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	signer := NewResponseSigner(&kp, true, testMaxBodyBytes, nil)
	handler := signer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write without an explicit WriteHeader: status must default to 200.
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	req.Header.Set("Epistula-Uuid", goldenRequestUUID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("implicit status = %d, want 200", rec.Code)
	}
	sig := rec.Header().Get(ResponseSignatureHeader)
	if sig == "" {
		t.Fatal("expected signature for implicit-200 response")
	}
	message := ResponseMessage(http.MethodGet, "/services", "", "200", bodyHash(nil), goldenRequestUUID, bodyHash([]byte(`{"ok":true}`)), rec.Header().Get(ResponseTimestampHeader), kp.Address)
	raw, err := decodeSignature(sig)
	if err != nil {
		t.Fatal(err)
	}
	if !boilerplate.VerfySignature(kp.Address, sha256Digest([]byte(message)), raw) {
		t.Fatal("implicit-200 response signature did not verify")
	}
}
