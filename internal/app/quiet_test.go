package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"

	"recall-miner/internal/config"
	"recall-miner/internal/ratelimit"
)

const gateSigner = "5GateApiSigningKeyForTestsOnly"

func gateReq(method, path, from string, header map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = from
	for k, v := range header {
		r.Header.Set(k, v)
	}
	return r
}

// stub answers like the miner would: 401 for a failed signature, 403 for a wrong
// internal key, 200 otherwise. It also sets a header that must not leak on a refusal.
func gateStub(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("X-Detail", "recall-miner")
		switch r.Header.Get("X-Stub") {
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"Invalid Epistula signature"}`))
		case "403":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"Invalid internal API key"}`))
		default:
			_, _ = w.Write([]byte("ok"))
		}
	})
}

func plainNotFound(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusNotFound || rec.Body.String() != notFoundBody || rec.Header().Get("X-Detail") != "" {
		t.Fatalf("%s: want the plain 404, got %d %q headers %v", what, rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestQuietGate(t *testing.T) {
	calls := 0
	h := newQuietGate(gateSigner, ratelimit.ProxyPolicy{}, nil).Middleware(gateStub(&calls))
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	// A scan from a public address: every path, every method, the same plain 404, and
	// nothing behind the gate runs.
	for _, p := range []string{"/health", "/embed_text", "/db/x/query", "/internal/services", "/nope"} {
		plainNotFound(t, serve(gateReq(http.MethodGet, p, "198.51.100.7:4000", nil)), "unsigned "+p)
	}
	plainNotFound(t, serve(gateReq(http.MethodPost, "/generate", "198.51.100.7:4000",
		map[string]string{"Epistula-Signed-By": "5SomeOtherKey"})), "signed by another key")
	if calls != 0 {
		t.Fatalf("refused requests must not reach the miner, ran %d", calls)
	}

	// Claims to be the API: verified as before; success passes, a failure is the same 404.
	if rec := serve(gateReq(http.MethodPost, "/embed_text", "198.51.100.7:4000",
		map[string]string{"Epistula-Signed-By": gateSigner})); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("a request signed by the API is served, got %d", rec.Code)
	}
	plainNotFound(t, serve(gateReq(http.MethodPost, "/embed_text", "198.51.100.7:4000",
		map[string]string{"Epistula-Signed-By": gateSigner, "X-Stub": "401"})), "forged API claim")

	// Sidecars on another machine: a keyed /internal/ request passes; a wrong key is 404.
	if rec := serve(gateReq(http.MethodPost, "/internal/services/heartbeat", "203.0.113.9:1",
		map[string]string{"X-Internal-Api-Key": "k"})); rec.Code != 200 {
		t.Fatalf("a keyed internal request is served, got %d", rec.Code)
	}
	plainNotFound(t, serve(gateReq(http.MethodPost, "/internal/services/heartbeat", "203.0.113.9:1",
		map[string]string{"X-Internal-Api-Key": "wrong", "X-Stub": "403"})), "wrong internal key")
	plainNotFound(t, serve(gateReq(http.MethodGet, "/health", "203.0.113.9:1",
		map[string]string{"X-Internal-Api-Key": "k"})), "internal key outside /internal/")

	// The miner's own machine and private network are served exactly as before.
	for _, from := range []string{"127.0.0.1:5", "[::1]:5", "10.1.2.3:5", "172.20.0.4:5", "192.168.1.9:5", "[fd00::2]:5"} {
		if rec := serve(gateReq(http.MethodGet, "/health", from, nil)); rec.Code != 200 {
			t.Fatalf("local %s: want 200, got %d", from, rec.Code)
		}
		if rec := serve(gateReq(http.MethodGet, "/x", from, map[string]string{"X-Stub": "401"})); rec.Code != http.StatusUnauthorized {
			t.Fatalf("local %s keeps its 401, got %d", from, rec.Code)
		}
	}
}

func TestQuietGate_UsesTrustedProxyPolicy(t *testing.T) {
	p, err := ratelimit.ParseTrustedProxies("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := newQuietGate(gateSigner, p, nil).Middleware(gateStub(&calls))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gateReq(http.MethodGet, "/health", "127.0.0.1:9", map[string]string{"X-Forwarded-For": "198.51.100.7"}))
	plainNotFound(t, rec, "public client behind the operator's own proxy")
}

func TestQuietGate_OffWithoutSigningKey(t *testing.T) {
	calls := 0
	h := newQuietGate("", ratelimit.ProxyPolicy{}, nil).Middleware(gateStub(&calls))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gateReq(http.MethodGet, "/x", "198.51.100.7:1", map[string]string{"X-Stub": "401"}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("development mode hides nothing, got %d", rec.Code)
	}
}

// Through the real routes: /health from a public address needs the API's signature.
func TestRoutes_HealthFromPublicNeedsAPISignature(t *testing.T) {
	api, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	minerKey, err := signature.KeyringPairFromSecret("//Bob", 42)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.FromEnv()
	cfg.InternalAPIKey = "test-internal-key"
	cfg.HotkeySecret = ""
	cfg.MinerHotkey = minerKey.Address
	cfg.ApplicationProberHotkey = api.Address
	cfg.RecallAPIURL = ""
	cfg.HTTPTimeout = time.Second
	srv := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	routes := srv.routes()

	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, gateReq(http.MethodGet, "/health", "198.51.100.7:1", nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "recall") {
		t.Fatalf("unsigned public /health: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, gateReq(http.MethodGet, "/health", "127.0.0.1:1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "recall-miner") {
		t.Fatalf("local /health (Docker health check): %d %q", rec.Code, rec.Body.String())
	}

	signed := httptest.NewRequest(http.MethodGet, "/health", nil)
	signed.RemoteAddr = "198.51.100.7:1"
	setEpistulaHeaders(t, signed, api, minerKey.Address, nil)
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, signed)
	if rec.Code != http.StatusOK {
		t.Fatalf("API-signed public /health: %d %q", rec.Code, rec.Body.String())
	}

	forged := httptest.NewRequest(http.MethodGet, "/health", nil)
	forged.RemoteAddr = "198.51.100.7:1"
	setEpistulaHeaders(t, forged, minerKey, minerKey.Address, nil)
	forged.Header.Set("Epistula-Signed-By", api.Address) // claims the API, signed by another key
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, forged)
	if rec.Code != http.StatusNotFound || rec.Body.String() != notFoundBody {
		t.Fatalf("forged API claim on /health: %d %q", rec.Code, rec.Body.String())
	}
}
