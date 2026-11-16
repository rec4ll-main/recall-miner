package auth

import (
	"net/http"
	"testing"
)

func TestLogicalKeyVerifiedIdentityNotHeader(t *testing.T) {
	kp := testKeypair(t)
	req := signEnvelopeRequest(t, kp, kp.Address, "POST", "/generate", "", "application/json", []byte(`{"request_key":"o1:fixed"}`), "logical-envelope-1")
	req.Header.Set("X-Verified-Hotkey", "forged")
	if _, ok := VerifiedSender(req.Context()); ok {
		t.Fatal("header created verified identity")
	}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		v, ok := VerifiedSender(r.Context())
		if !ok || v != kp.Address {
			t.Fatal("wrong scoped principal")
		}
		w.WriteHeader(200)
	})
	rec := serveVerified(t, Validator(kp.Address, kp.Address, true, 1<<20, nil, nil, next), req)
	if rec.Code != 200 || !called {
		t.Fatalf("status %d", rec.Code)
	}
	bad := signEnvelopeRequest(t, kp, kp.Address, "POST", "/generate", "", "application/json", []byte(`{}`), "logical-envelope-2")
	bad.Header.Set("Epistula-Request-Signature", "0x00")
	called = false
	rec = serveVerified(t, Validator(kp.Address, kp.Address, true, 1<<20, nil, nil, next), bad)
	if rec.Code == 200 || called {
		t.Fatal("forgery accepted")
	}
}
