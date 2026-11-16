package auth

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/vedhavyas/go-subkey/v2"
)

const testMaxBodyBytes = int64(1 << 20)

func testKeypair(t *testing.T) signature.KeyringPair {
	t.Helper()
	kp, err := signature.KeyringPairFromSecret("//EpistulaTest", 42)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	return kp
}

func anotherKeypair(t *testing.T) signature.KeyringPair {
	t.Helper()
	kp, err := signature.KeyringPairFromSecret("//OtherSigner", 42)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	return kp
}

// signEnvelopeRequest builds a request signed with both the gobt Epistula
// headers and the recall envelope signature over the canonical request
// attributes. An explicit requestUUID can be passed to craft replay pairs.
func signEnvelopeRequest(t *testing.T, kp signature.KeyringPair, minerHotkey, method, path, query, contentType string, body []byte, requestUUID string) *http.Request {
	t.Helper()
	requestURL := "http://miner.local" + path
	if query != "" {
		requestURL += "?" + query
	}
	req, err := http.NewRequest(method, requestURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	timestamp := time.Now().UnixMilli()
	if requestUUID == "" {
		requestUUID = "test-uuid-" + fmt.Sprintf("%d", timestamp)
	}
	hash := bodyHash(body)
	message := fmt.Sprintf("%s.%s.%d.%s", hash, requestUUID, timestamp, minerHotkey)
	sig, err := signature.Sign([]byte(message), kp.URI)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req.Header.Set("Epistula-Version", "2")
	req.Header.Set("Epistula-Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("Epistula-Uuid", requestUUID)
	req.Header.Set("Epistula-Signed-By", kp.Address)
	req.Header.Set("Epistula-Signed-For", minerHotkey)
	req.Header.Set("Epistula-Request-Signature", "0x"+hex.EncodeToString(sig))

	envelope := EnvelopeMessage(method, req.URL.EscapedPath(), req.URL.RawQuery, contentType, hash, requestUUID, fmt.Sprintf("%d", timestamp), minerHotkey, kp.Address)
	envSig, err := signature.Sign(sha256Digest([]byte(envelope)), kp.URI)
	if err != nil {
		t.Fatalf("envelope sign: %v", err)
	}
	req.Header.Set("Epistula-Envelope-Signature", "0x"+hex.EncodeToString(envSig))
	return req
}

// verifyRequest mirrors the middleware body handling and calls the pure
// verifier.
func verifyRequest(t *testing.T, req *http.Request, minerHotkey string) (VerifiedEpistula, error) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return VerifyEpistula(req, body, minerHotkey, true)
}

func serveVerified(t *testing.T, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func validHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestVerifyEpistulaValidSignedRequestAccepted(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{"text":"hello"}`), "")
	verified, err := verifyRequest(t, req, kp.Address)
	if err != nil {
		t.Fatalf("expected valid request accepted: %v", err)
	}
	if verified.SignedBy != kp.Address {
		t.Fatalf("expected signed_by %q, got %q", kp.Address, verified.SignedBy)
	}
}

func TestVerifyEpistulaPathSubstitutionRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{"text":"hello"}`), "")
	req.URL.Path = "/db/2/store"
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected path substitution rejected")
	}
}

func TestVerifyEpistulaMethodSubstitutionRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{"text":"hello"}`), "")
	req.Method = http.MethodGet
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected method substitution rejected")
	}
}

func TestVerifyEpistulaQueryValueSubstitutionRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/query", "limit=5", "application/json", []byte(`{"q":"x"}`), "")
	req.URL.RawQuery = "limit=50"
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected query value substitution rejected")
	}
}

func TestVerifyEpistulaQueryReorderingAccepted(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/query", "b=2&a=1", "application/json", []byte(`{"q":"x"}`), "")
	req.URL.RawQuery = "a=1&b=2"
	if _, err := verifyRequest(t, req, kp.Address); err != nil {
		t.Fatalf("expected query reordering accepted after canonicalization: %v", err)
	}
}

func TestCanonicalQueryCollisionDistinct(t *testing.T) {
	encoded := canonicalQuery("a=1%26b%3D2")
	separate := canonicalQuery("a=1&b=2")
	if encoded == separate {
		t.Fatalf("canonical forms must differ: %q", encoded)
	}
}

func TestVerifyEpistulaQueryEncodedCollisionRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/query", "a=1%26b%3D2", "application/json", []byte(`{"q":"x"}`), "")
	req.URL.RawQuery = "a=1&b=2"
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected encoded-vs-separate query collision rejected")
	}
}

func TestVerifyEpistulaBodySubstitutionRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{"text":"hello"}`), "")
	if _, err := VerifyEpistula(req, []byte(`{"text":"tampered"}`), kp.Address, true); err == nil {
		t.Fatal("expected body substitution rejected")
	}
}

func TestVerifyEpistulaContentTypeSubstitutionRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{"text":"hello"}`), "")
	req.Header.Set("Content-Type", "text/plain")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected content-type substitution rejected")
	}
}

func TestVerifyEpistulaStaleTimestampRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequestAt(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), time.Now().Add(-30*time.Second).UnixMilli(), "")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected stale timestamp rejected")
	}
}

func TestVerifyEpistulaFutureTimestampRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequestAt(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), time.Now().Add(30*time.Second).UnixMilli(), "")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected future timestamp rejected")
	}
}

func TestVerifyEpistulaMissingSignedForRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Del("Epistula-Signed-For")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected missing signed-for rejected")
	}
}

func TestVerifyEpistulaMissingSignedByRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Del("Epistula-Signed-By")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected missing signed-by rejected")
	}
}

func TestVerifyEpistulaMissingRequestSignatureRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Del("Epistula-Request-Signature")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected missing request signature rejected")
	}
}

func TestVerifyEpistulaMissingTimestampRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Del("Epistula-Timestamp")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected missing timestamp rejected")
	}
}

func TestVerifyEpistulaMissingUUIDRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Del("Epistula-Uuid")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected missing uuid rejected")
	}
}

func TestVerifyEpistulaMissingEnvelopeSignatureRejected(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Del("Epistula-Envelope-Signature")
	if _, err := verifyRequest(t, req, kp.Address); err == nil {
		t.Fatal("expected missing envelope signature rejected")
	}
}

func TestVerifyEpistulaMalformedShortBaseSignatureNoPanic(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Set("Epistula-Request-Signature", "0xdeadbeef")
	_, err := verifyRequest(t, req, kp.Address)
	if err == nil {
		t.Fatal("expected malformed short base signature rejected")
	}
}

func TestVerifyEpistulaMalformedShortEnvelopeSignatureNoPanic(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Set("Epistula-Envelope-Signature", "0xdeadbeef")
	_, err := verifyRequest(t, req, kp.Address)
	if err == nil {
		t.Fatal("expected malformed short envelope signature rejected")
	}
}

// shortPayloadSS58 returns a valid-checksum SS58 address whose decoded
// payload is shorter than 32 bytes, which panics gobt's VerfySignature.
func shortPayloadSS58(t *testing.T) string {
	t.Helper()
	return subkey.SS58Encode([]byte{0x01}, 42)
}

func TestVerifyEpistulaShortPayloadSignedByNoPanic(t *testing.T) {
	kp := testKeypair(t)
	bad := shortPayloadSS58(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	// The request carries a genuine 64-byte signature, but the signed-by
	// header claims a short-payload address. gobt would panic decoding it;
	// the guard must reject it cleanly.
	req.Header.Set("Epistula-Signed-By", bad)
	_, err := verifyRequest(t, req, kp.Address)
	if err == nil {
		t.Fatal("expected short-payload signed_by rejected")
	}
}

func TestValidatorShortPayloadSignedByReturnsClean401(t *testing.T) {
	kp := testKeypair(t)
	bad := shortPayloadSS58(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Set("Epistula-Signed-By", bad)
	rec := serveVerified(t, Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, validHandler(t)), req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (panic would crash the connection)", rec.Code)
	}
	health := httptest.NewRecorder()
	ValidHealthHandler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("expected health 200 after malformed signed_by, got %d", health.Code)
	}
}

func TestValidatorDecoderPanicVectorsReturnClean401(t *testing.T) {
	for _, malformed := range []string{"NZoQ", "4jS"} {
		kp := testKeypair(t)
		req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
		req.Header.Set("Epistula-Signed-By", malformed)
		rec := serveVerified(t, Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, validHandler(t)), req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("signed_by=%q: expected 401, got %d (panic would crash the connection)", malformed, rec.Code)
		}
		health := httptest.NewRecorder()
		ValidHealthHandler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
		if health.Code != http.StatusOK {
			t.Fatalf("signed_by=%q: expected health 200 after, got %d", malformed, health.Code)
		}
	}
}

func TestVerifyEpistulaDecoderPanicVectorsReturnError(t *testing.T) {
	for _, malformed := range []string{"NZoQ", "4jS"} {
		kp := testKeypair(t)
		req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
		req.Header.Set("Epistula-Signed-By", malformed)
		if _, err := verifyRequest(t, req, kp.Address); err == nil {
			t.Fatalf("signed_by=%q: expected error, got none", malformed)
		}
	}
}

func TestHealthStillWorksAfterMalformedSignature(t *testing.T) {
	kp := testKeypair(t)
	srv := &mockHealthServer{}
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	req.Header.Set("Epistula-Request-Signature", "0xdeadbeef")
	rec := serveVerified(t, Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, srv), req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	health := httptest.NewRecorder()
	ValidHealthHandler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("expected health 200 after malformed signature, got %d", health.Code)
	}
}

func TestValidatorMiddlewareRejectsUnauthorizedSigner(t *testing.T) {
	kp := testKeypair(t)
	other := anotherKeypair(t)
	req := signEnvelopeRequest(t, other, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "")
	recorder := httptest.NewRecorder()
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run for unauthorized signer")
	}))
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", recorder.Code)
	}
}

func TestValidatorMiddlewareAcceptsAuthorizedSigner(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodGet, "/services", "", "", []byte(nil), "")
	called := false
	recorder := httptest.NewRecorder()
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := r.Header.Get("X-Verified-Hotkey"); got != kp.Address {
			t.Fatalf("expected verified hotkey %q, got %q", kp.Address, got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(recorder, req)
	if !called {
		t.Fatal("expected handler to run")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
}

func TestValidatorReplayRejectedAfterAuthorization(t *testing.T) {
	kp := testKeypair(t)
	replays := NewReplayCache(15*time.Second, 100)
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, replays, nil, validHandler(t))
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "fixed-uuid")

	first := serveVerified(t, handler, req)
	if first.Code != http.StatusOK {
		t.Fatalf("expected first request 200, got %d", first.Code)
	}
	second := serveVerified(t, handler, req)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("expected replayed request 401, got %d", second.Code)
	}
}

func TestValidatorReplayAcrossRoutesRejected(t *testing.T) {
	kp := testKeypair(t)
	replays := NewReplayCache(15*time.Second, 100)
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, replays, nil, validHandler(t))
	reqA := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "shared-uuid")
	reqB := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/2/store", "", "application/json", []byte(`{}`), "shared-uuid")

	first := serveVerified(t, handler, reqA)
	if first.Code != http.StatusOK {
		t.Fatalf("expected first request 200, got %d", first.Code)
	}
	second := serveVerified(t, handler, reqB)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("expected replay across routes 401, got %d", second.Code)
	}
}

func TestValidatorInvalidEnvelopeDoesNotConsumeUUID(t *testing.T) {
	kp := testKeypair(t)
	replays := NewReplayCache(15*time.Second, 100)
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, replays, nil, validHandler(t))
	bad := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "precious-uuid")
	bad.Header.Del("Epistula-Envelope-Signature")
	if code := serveVerified(t, handler, bad).Code; code != http.StatusUnauthorized {
		t.Fatalf("expected invalid envelope 401, got %d", code)
	}
	good := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "precious-uuid")
	if code := serveVerified(t, handler, good).Code; code != http.StatusOK {
		t.Fatalf("expected valid request with same uuid accepted after rejection: %d", code)
	}
}

func TestValidatorUnauthorizedHotkeyDoesNotPopulateCache(t *testing.T) {
	kp := testKeypair(t)
	other := anotherKeypair(t)
	// Capacity 1: if the unauthorized request consumed a slot, the
	// authorized request below would be rejected. The authorized request
	// uses a fresh uuid, so only a poisoned cache can fail it.
	replays := NewReplayCache(15*time.Second, 1)
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, replays, nil, validHandler(t))
	unauthorized := signEnvelopeRequest(t, other, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "any-uuid")
	if code := serveVerified(t, handler, unauthorized).Code; code != http.StatusForbidden {
		t.Fatalf("expected unauthorized 403, got %d", code)
	}
	authorized := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), "fresh-uuid")
	if code := serveVerified(t, handler, authorized).Code; code != http.StatusOK {
		t.Fatalf("expected authorized request accepted (unauthorized must not consume cache capacity): %d", code)
	}
}

func TestReplayCacheFutureSkewEntryOutlivesReceipt(t *testing.T) {
	kp := testKeypair(t)
	replays := NewReplayCache(15*time.Second, 100)
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, replays, nil, validHandler(t))
	futureTimestamp := time.Now().Add(10 * time.Second).UnixMilli()
	req := signEnvelopeRequestAt(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{}`), futureTimestamp, "skewed-uuid")

	first := serveVerified(t, handler, req)
	if first.Code != http.StatusOK {
		t.Fatalf("expected future-skewed request 200, got %d", first.Code)
	}
	// Simulate a replay 16s after receipt: still inside the signed window
	// (timestamp is only 6s old), but the receipt-based TTL would have
	// expired. The timestamp-keyed expiry must still reject it.
	replays.mu.Lock()
	replays.clock = func() time.Time { return time.Now().Add(16 * time.Second) }
	replays.mu.Unlock()
	second := serveVerified(t, handler, req)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("expected future-skew replay rejected 401, got %d", second.Code)
	}
}

func TestReplayCacheCapacityFailsClosed(t *testing.T) {
	replays := NewReplayCache(15*time.Second, 3)
	if !replays.checkAndRecord("s", "u1", time.Now()) {
		t.Fatal("expected u1 accepted")
	}
	if !replays.checkAndRecord("s", "u2", time.Now()) {
		t.Fatal("expected u2 accepted")
	}
	if !replays.checkAndRecord("s", "u3", time.Now()) {
		t.Fatal("expected u3 accepted")
	}
	if replays.checkAndRecord("s", "u4", time.Now()) {
		t.Fatal("expected u4 rejected at capacity")
	}
}

func TestReplayCacheAllowsDistinctUUIDs(t *testing.T) {
	replays := NewReplayCache(15*time.Second, 100)
	if !replays.checkAndRecord("sender-a", "uuid-1", time.Now()) {
		t.Fatal("expected first uuid accepted")
	}
	if !replays.checkAndRecord("sender-a", "uuid-2", time.Now()) {
		t.Fatal("expected distinct uuid accepted")
	}
	if !replays.checkAndRecord("sender-b", "uuid-1", time.Now()) {
		t.Fatal("expected distinct sender with same uuid accepted")
	}
}

func TestEnvelopeMessageCanonicalization(t *testing.T) {
	msg := EnvelopeMessage("POST", "/db/1/store", "b=2&a=1", " Application/JSON ", "hash", "uuid", "123", "signedFor", "signedBy")
	if !strings.HasPrefix(msg, "recall-epistula-v1\nPOST\n/db/1/store\na=1&b=2\napplication/json\n") {
		t.Fatalf("unexpected canonical message: %q", msg)
	}
	if !strings.HasSuffix(msg, "hash.uuid.123.signedFor.signedBy") {
		t.Fatalf("unexpected canonical tail: %q", msg)
	}
}

func TestGoldenEnvelopeVector(t *testing.T) {
	got := EnvelopeMessage("POST", "/db/1/store", "b=2&a=1", "Application/JSON", "cbbbdcd27692344de5dbab3abcaba413fb0f45307267de7081401576df1cb176", "545376da-4230-4b28-a991-20fa1784910e", "1787153905211", "5H6oTiDQrhNzH9kwMiN6BdfSCCdNAXhAhcgvFUCcsyfuXTFX", "5H6oTiDQrhNzH9kwMiN6BdfSCCdNAXhAhcgvFUCcsyfuXTFX")
	want := "recall-epistula-v1\nPOST\n/db/1/store\na=1&b=2\napplication/json\ncbbbdcd27692344de5dbab3abcaba413fb0f45307267de7081401576df1cb176.545376da-4230-4b28-a991-20fa1784910e.1787153905211.5H6oTiDQrhNzH9kwMiN6BdfSCCdNAXhAhcgvFUCcsyfuXTFX.5H6oTiDQrhNzH9kwMiN6BdfSCCdNAXhAhcgvFUCcsyfuXTFX"
	if got != want {
		t.Fatalf("golden vector mismatch:\n got: %q\nwant: %q", got, want)
	}
}

type mockHealthServer struct{}

func (m *mockHealthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func TestValidatorRejectsOversizedBody413(t *testing.T) {
	kp := testKeypair(t)
	body := bytes.Repeat([]byte("x"), int(testMaxBodyBytes)+1)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", body, "")
	rec := serveVerified(t, Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, validHandler(t)), req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", rec.Code)
	}
}

func TestValidatorAcceptsNormalSignedBody(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{"text":"hello"}`), "")
	rec := serveVerified(t, Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, validHandler(t)), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for normal signed request, got %d", rec.Code)
	}
}

func TestValidatorAuthEnforcedBeforeHandler(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, http.MethodPost, "/db/1/store", "", "application/json", []byte(`{"text":"hello"}`), "")
	req.Header.Del("Epistula-Request-Signature")
	ran := false
	handler := Validator(kp.Address, kp.Address, true, testMaxBodyBytes, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := serveVerified(t, handler, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing signature, got %d", rec.Code)
	}
	if ran {
		t.Fatal("handler must not run when auth fails")
	}
}

func ValidHealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestConstantTimeKeyEqual(t *testing.T) {
	if !ConstantTimeKeyEqual("secret-key", "secret-key") {
		t.Fatal("expected equal keys to match")
	}
	if ConstantTimeKeyEqual("secret-key", "secret-key2") {
		t.Fatal("expected differing keys to not match")
	}
	if ConstantTimeKeyEqual("", "secret-key") {
		t.Fatal("expected empty vs non-empty to not match")
	}
	if ConstantTimeKeyEqual("secret-key", "") {
		t.Fatal("expected non-empty vs empty to not match")
	}
	if !ConstantTimeKeyEqual("", "") {
		t.Fatal("expected empty keys to match")
	}
}

func TestInternalMiddlewareAcceptsValidKey(t *testing.T) {
	called := false
	handler := Internal("secret-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/internal/services/register", nil)
	req.Header.Set("X-Internal-Api-Key", "secret-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("expected handler to run with valid key, called=%v code=%d", called, rec.Code)
	}
}

func TestInternalMiddlewareRejectsInvalidKey(t *testing.T) {
	handler := Internal("secret-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run for invalid key")
	}))
	req := httptest.NewRequest(http.MethodPost, "/internal/services/register", nil)
	req.Header.Set("X-Internal-Api-Key", "wrong-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for invalid key, got %d", rec.Code)
	}
}

func TestInternalMiddlewareEmptyConfiguredKeyFailsClosed(t *testing.T) {
	handler := Internal("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run when internal key is unset")
	}))
	req := httptest.NewRequest(http.MethodPost, "/internal/services/register", nil)
	req.Header.Set("X-Internal-Api-Key", "anything")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when internal key unset, got %d", rec.Code)
	}
}

// signEnvelopeRequestAt signs a request with explicit timestamp components
// (used for stale/future timestamp and skew tests).
func signEnvelopeRequestAt(t *testing.T, kp signature.KeyringPair, minerHotkey, method, path, query, contentType string, body []byte, timestampMillis int64, requestUUID string) *http.Request {
	t.Helper()
	requestURL := "http://miner.local" + path
	if query != "" {
		requestURL += "?" + query
	}
	req, err := http.NewRequest(method, requestURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if requestUUID == "" {
		requestUUID = "test-uuid-fixed"
	}
	hash := bodyHash(body)
	message := fmt.Sprintf("%s.%s.%d.%s", hash, requestUUID, timestampMillis, minerHotkey)
	sig, err := signature.Sign([]byte(message), kp.URI)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req.Header.Set("Epistula-Version", "2")
	req.Header.Set("Epistula-Timestamp", fmt.Sprintf("%d", timestampMillis))
	req.Header.Set("Epistula-Uuid", requestUUID)
	req.Header.Set("Epistula-Signed-By", kp.Address)
	req.Header.Set("Epistula-Signed-For", minerHotkey)
	req.Header.Set("Epistula-Request-Signature", "0x"+hex.EncodeToString(sig))

	envelope := EnvelopeMessage(method, req.URL.EscapedPath(), req.URL.RawQuery, contentType, hash, requestUUID, fmt.Sprintf("%d", timestampMillis), minerHotkey, kp.Address)
	envSig, err := signature.Sign(sha256Digest([]byte(envelope)), kp.URI)
	if err != nil {
		t.Fatalf("envelope sign: %v", err)
	}
	req.Header.Set("Epistula-Envelope-Signature", "0x"+hex.EncodeToString(envSig))
	return req
}
