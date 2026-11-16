package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/subtrahend-labs/gobt/boilerplate"

	"recall-miner/internal/auth"
	"recall-miner/internal/config"
)

func TestNew_ApplicationKeyOnlySucceeds(t *testing.T) {
	appKey, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("API_SIGNING_HOTKEY_SS58", appKey.Address)
	t.Setenv("VALIDATOR_ALLOWLIST_DEV_EMPTY", "false")
	t.Setenv("HOTKEY_URI", "//Bob")
	t.Setenv("INTERNAL_API_KEY", "test-internal-key")
	t.Setenv("TLS_ENABLED", "false")

	srv, err := New(context.Background())
	if err != nil {
		t.Fatalf("New with application key only: %v", err)
	}
	t.Cleanup(srv.Close)
	if got := srv.cfg.ApplicationIdentity(); got != appKey.Address {
		t.Fatalf("ApplicationIdentity = %q", got)
	}
	if got := srv.validatorSignedFor(*srv.keypair); got != appKey.Address {
		t.Fatalf("heartbeat audience = %q, want application key", got)
	}
}

func TestNew_EmptyApplicationKeyFailsClosed(t *testing.T) {
	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("API_SIGNING_HOTKEY_SS58", "")
	t.Setenv("VALIDATOR_ALLOWLIST_DEV_EMPTY", "true")
	t.Setenv("HOTKEY_URI", "//Bob")
	t.Setenv("INTERNAL_API_KEY", "test-internal-key")
	t.Setenv("TLS_ENABLED", "false")

	if _, err := New(context.Background()); err == nil {
		t.Fatal("expected empty API_SIGNING_HOTKEY_SS58 to fail startup")
	}
}

func TestValidatorAuthAcceptsOnlyApplicationIdentity(t *testing.T) {
	other, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	appKey, err := signature.KeyringPairFromSecret("//Charlie", 42)
	if err != nil {
		t.Fatal(err)
	}
	miner, err := signature.KeyringPairFromSecret("//Bob", 42)
	if err != nil {
		t.Fatal(err)
	}

	srv := newServer(config.Config{
		MinerHotkey:             miner.Address,
		ApplicationProberHotkey: appKey.Address,
		EpistulaEnvelopeRequired: false,
		MaxBodyBytes:            1 << 20,
		ResponseSigningEnabled:  false,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.keypair = &miner
	srv.replays = auth.NewReplayCache(30*time.Second, 100)

	handler := srv.validatorAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	if status := epistulaStatus(t, handler, other, miner.Address, []byte(`{}`)); status != http.StatusForbidden {
		t.Fatalf("non-API signer status = %d, want 403", status)
	}
	if status := epistulaStatus(t, handler, appKey, miner.Address, []byte(`{}`)); status != http.StatusOK {
		t.Fatalf("application signer status = %d, want 200", status)
	}
}

func epistulaStatus(t *testing.T, handler http.Handler, signer signature.KeyringPair, signedFor string, body []byte) int {
	t.Helper()
	headers, err := boilerplate.GetEpistulaHeaders(signer, signedFor, body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/embed_text", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestNew_RemovedValidatorHotkeyFailsStartup(t *testing.T) {
	appKey, err := signature.KeyringPairFromSecret("//Charlie", 42)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENVIRONMENT", "test")
	t.Setenv("VALIDATOR_HOTKEY_SS58", appKey.Address)
	t.Setenv("API_SIGNING_HOTKEY_SS58", appKey.Address)
	t.Setenv("VALIDATOR_ALLOWLIST_DEV_EMPTY", "false")
	t.Setenv("HOTKEY_URI", "//Bob")
	t.Setenv("INTERNAL_API_KEY", "test-internal-key")
	t.Setenv("TLS_ENABLED", "false")

	srv, err := New(context.Background())
	if err == nil {
		srv.Close()
		t.Fatal("expected VALIDATOR_HOTKEY_SS58 to fail startup")
	}
	if !strings.Contains(err.Error(), "VALIDATOR_HOTKEY_SS58 was removed: set API_SIGNING_HOTKEY_SS58") {
		t.Fatalf("unexpected error: %v", err)
	}
}
