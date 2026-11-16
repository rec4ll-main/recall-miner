package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"recall-miner/internal/config"
)

// SECURITY_BACKLOG "Rate Limiting": a client that keeps
// failing Epistula authentication is cut off before signature work, and the cut-off
// never touches other clients or the health route's normal behaviour.
func TestRoutes_AuthFailureGuardCutsOffUnsignedFlood(t *testing.T) {
	cfg := config.FromEnv()
	cfg.InternalAPIKey = "test-internal-key"
	cfg.HotkeySecret = ""
	cfg.MinerHotkey = ""
	cfg.RecallAPIURL = ""
	cfg.HTTPTimeout = time.Second
	cfg.RateLimitAuthFailuresPerMin = 3
	srv := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := srv.routes()

	do := func(addr string) int {
		req := httptest.NewRequest(http.MethodPost, "/rerank", strings.NewReader(`{"query":"q","texts":["a"]}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 3; i++ {
		if code := do("203.0.113.9:1000"); code != http.StatusUnauthorized {
			t.Fatalf("unsigned request %d: expected 401, got %d", i, code)
		}
	}
	if code := do("203.0.113.9:1000"); code != http.StatusTooManyRequests {
		t.Fatalf("4th unsigned request from the same IP: expected 429, got %d", code)
	}
	if code := do("198.51.100.5:1000"); code != http.StatusUnauthorized {
		t.Fatalf("another IP must still reach authentication: expected 401, got %d", code)
	}
	// Health is unauthenticated and never contributes failures.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("the cut-off IP must not pass the guard even for health")
	}
}

func TestRoutes_AuthFailureGuardDisabledWithZero(t *testing.T) {
	cfg := config.FromEnv()
	cfg.InternalAPIKey = "test-internal-key"
	cfg.HotkeySecret = ""
	cfg.MinerHotkey = ""
	cfg.RecallAPIURL = ""
	cfg.HTTPTimeout = time.Second
	cfg.RateLimitAuthFailuresPerMin = 0
	srv := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := srv.routes()
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/rerank", strings.NewReader(`{}`))
		req.RemoteAddr = "203.0.113.9:1000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("guard disabled: expected 401 every time, got %d on attempt %d", rec.Code, i)
		}
	}
}

func TestValidateOverloadConfig_RejectsInvalidTrustedProxies(t *testing.T) {
	cfg := config.FromEnv()
	for _, bad := range []string{"10.0.0.0/33", "proxy.internal", "10.0.0.0/8,,garbage"} {
		cfg.RateLimitTrustedProxies = bad
		if err := validateOverloadConfig(cfg); err == nil {
			t.Errorf("%q must fail startup validation", bad)
		}
	}
	for _, ok := range []string{"", "10.0.0.0/8", "192.0.2.10, 2001:db8::/32"} {
		cfg.RateLimitTrustedProxies = ok
		if err := validateOverloadConfig(cfg); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
}
