package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"recall-miner/internal/registry"
	"strings"
	"testing"
)

func TestRetentionSignedRouteAuthorizationReplayAndOwnership(t *testing.T) {
	f := newM5Fixture(t, "db-a")
	f.srv.handlers.Registry().PinDB("db_test", "db-a")
	body := []byte(`{"embed_id":"record","nonce":"` + strings.Repeat("01", 32) + `"}`)
	unsigned := httptest.NewRequest(http.MethodPost, "/db/db_test/retention-proof", bytes.NewReader(body))
	w := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(w, unsigned)
	if w.Code != 401 {
		t.Fatalf("unsigned %d", w.Code)
	}
	bad := httptest.NewRequest(http.MethodPost, "/db/db_test/retention-proof", bytes.NewReader(body))
	setEpistulaHeaders(t, bad, f.bob, f.alice.Address, body)
	w = httptest.NewRecorder()
	f.srv.routes().ServeHTTP(w, bad)
	if w.Code != 403 {
		t.Fatalf("unauthorized signer %d", w.Code)
	}
	good := httptest.NewRequest(http.MethodPost, "/db/db_test/retention-proof", bytes.NewReader(body))
	setEpistulaHeaders(t, good, f.alice, f.alice.Address, body)
	headers := good.Header.Clone()
	w = httptest.NewRecorder()
	f.srv.routes().ServeHTTP(w, good)
	// This fake backend does not implement proofs; reaching its501 proves routing.
	if w.Code != 501 {
		t.Fatalf("authorized route %d: %s", w.Code, w.Body.String())
	}
	replay := httptest.NewRequest(http.MethodPost, "/db/db_test/retention-proof", bytes.NewReader(body))
	replay.Header = headers
	w = httptest.NewRecorder()
	f.srv.routes().ServeHTTP(w, replay)
	if w.Code != 401 {
		t.Fatalf("replay %d", w.Code)
	}
	missing := httptest.NewRequest(http.MethodPost, "/db/db_unknown/retention-proof", bytes.NewReader(body))
	setEpistulaHeaders(t, missing, f.alice, f.alice.Address, body)
	w = httptest.NewRecorder()
	f.srv.routes().ServeHTTP(w, missing)
	if w.Code != 404 {
		t.Fatalf("unknown owner must not fallback: %d", w.Code)
	}
}

func TestRetentionRouteRejectsOversizedSidecarSuccess(t *testing.T) {
	for _, field := range []string{"ignored", "proof"} {
		t.Run(field, func(t *testing.T) {
			f := newM5Fixture(t, "unused")
			side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := map[string]string{"db_id": "db_test", "embed_id": "record", "proof": strings.Repeat("01", 32)}
				body[field] = strings.Repeat("x", 4096)
				json.NewEncoder(w).Encode(body)
			}))
			defer side.Close()
			f.srv.handlers.Registry().Register(registry.RegisterRequest{Name: "proof-side", URL: side.URL, Capabilities: []string{"db"}, Metadata: map[string]any{"api": "recall-db-v1"}})
			f.srv.handlers.Registry().PinDB("db_test", "proof-side")
			body := []byte(`{"embed_id":"record","nonce":"` + strings.Repeat("01", 32) + `"}`)
			req := httptest.NewRequest(http.MethodPost, "/db/db_test/retention-proof", bytes.NewReader(body))
			setEpistulaHeaders(t, req, f.alice, f.alice.Address, body)
			w := httptest.NewRecorder()
			f.srv.routes().ServeHTTP(w, req)
			if w.Code != 502 {
				t.Fatalf("oversized %s response status %d", field, w.Code)
			}
		})
	}
}
