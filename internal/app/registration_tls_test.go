package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"

	"recall-miner/internal/config"
)

func tlsTestServer(t *testing.T, mutate func(*config.Config)) *server {
	t.Helper()
	cfg := config.FromEnv()
	cfg.InternalAPIKey = "test-internal-key"
	cfg.HotkeySecret = ""
	cfg.MinerHotkey = ""
	cfg.RecallAPIURL = ""
	cfg.HTTPTimeout = time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	return newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func tlsTestKeypair(t *testing.T) signature.KeyringPair {
	t.Helper()
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

func TestInitTLS_SetsTLSConfigAndPin(t *testing.T) {
	dir := t.TempDir()
	srv := tlsTestServer(t, func(cfg *config.Config) {
		cfg.TLSEnabled = true
		cfg.TLSCertFile = filepath.Join(dir, "cert.pem")
		cfg.TLSKeyFile = filepath.Join(dir, "key.pem")
		cfg.TLSAutoGenerate = true
		cfg.MinerAPIURL = "http://192.0.2.43:8001"
	})
	kp := tlsTestKeypair(t)
	if err := srv.initTLS(kp.Address); err != nil {
		t.Fatalf("initTLS: %v", err)
	}
	if srv.TLSConfig() == nil {
		t.Fatal("expected TLSConfig non-nil when TLS enabled")
	}
	if srv.SPKIPin() == "" {
		t.Fatal("expected SPKI pin non-empty when TLS enabled")
	}
	// Loading the same files a second time must produce the same pin.
	srv2 := tlsTestServer(t, func(cfg *config.Config) {
		cfg.TLSEnabled = true
		cfg.TLSCertFile = filepath.Join(dir, "cert.pem")
		cfg.TLSKeyFile = filepath.Join(dir, "key.pem")
		cfg.TLSAutoGenerate = true
	})
	if err := srv2.initTLS(kp.Address); err != nil {
		t.Fatalf("initTLS second: %v", err)
	}
	if srv2.SPKIPin() != srv.SPKIPin() {
		t.Fatalf("SPKI pin not stable across restarts: %q vs %q", srv.SPKIPin(), srv2.SPKIPin())
	}
}

func TestInitTLS_TLSDisabledNoOp(t *testing.T) {
	srv := tlsTestServer(t, nil)
	kp := tlsTestKeypair(t)
	if err := srv.initTLS(kp.Address); err != nil {
		t.Fatalf("initTLS when disabled should be a no-op, got %v", err)
	}
	if srv.TLSConfig() != nil {
		t.Fatal("expected TLSConfig nil when TLS disabled")
	}
	if srv.SPKIPin() != "" {
		t.Fatal("expected empty SPKI pin when TLS disabled")
	}
}

func TestInitTLS_AutoGenerateFalseMissingFilesError(t *testing.T) {
	dir := t.TempDir()
	srv := tlsTestServer(t, func(cfg *config.Config) {
		cfg.TLSEnabled = true
		cfg.TLSCertFile = filepath.Join(dir, "missing-cert.pem")
		cfg.TLSKeyFile = filepath.Join(dir, "missing-key.pem")
		cfg.TLSAutoGenerate = false
	})
	kp := tlsTestKeypair(t)
	if err := srv.initTLS(kp.Address); err == nil {
		t.Fatal("expected startup error when TLS_AUTO_GENERATE=false and files missing")
	}
}

func TestRegisterValidatorPayload_IncludesSPKIPin(t *testing.T) {
	dir := t.TempDir()
	var captured []byte
	var sigHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		sigHeader = r.Header.Get("Epistula-Request-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	srv := tlsTestServer(t, func(cfg *config.Config) {
		cfg.TLSEnabled = true
		cfg.TLSCertFile = filepath.Join(dir, "cert.pem")
		cfg.TLSKeyFile = filepath.Join(dir, "key.pem")
		cfg.TLSAutoGenerate = true
		cfg.RecallAPIURL = ts.URL
		cfg.MinerAPIURL = "http://miner:8001"
	})
	kp := tlsTestKeypair(t)
	srv.cfg.MinerHotkey = kp.Address
	if err := srv.initTLS(kp.Address); err != nil {
		t.Fatalf("initTLS: %v", err)
	}
	pin := srv.SPKIPin()
	if pin == "" {
		t.Fatal("expected a pin to register")
	}
	if err := srv.registerValidator(t.Context(), kp); err != nil {
		t.Fatalf("registerValidator: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("no registration body captured")
	}
	var payload map[string]any
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got, _ := payload["tls_spki_sha256"].(string); got != pin {
		t.Fatalf("payload tls_spki_sha256 = %q, want %q", got, pin)
	}
	if sigHeader == "" {
		t.Fatal("expected Epistula signature header on registration (pin must be inside the signed body)")
	}
}

func TestTLSServing_ServesTLSAndRefusesPlaintext(t *testing.T) {
	dir := t.TempDir()
	srv := tlsTestServer(t, func(cfg *config.Config) {
		cfg.TLSEnabled = true
		cfg.TLSCertFile = filepath.Join(dir, "cert.pem")
		cfg.TLSKeyFile = filepath.Join(dir, "key.pem")
		cfg.TLSAutoGenerate = true
		cfg.ListenAddr = "127.0.0.1:0"
		cfg.MinerAPIURL = "http://127.0.0.1:0"
	})
	kp := tlsTestKeypair(t)
	if err := srv.initTLS(kp.Address); err != nil {
		t.Fatalf("initTLS: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, srv.TLSConfig())
	httpSrv := &http.Server{Handler: srv.Routes()}
	go func() { _ = httpSrv.Serve(tlsLn) }()
	defer httpSrv.Close()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	addr := ln.Addr().String()
	resp, err := client.Get("https://" + addr + "/health")
	if err != nil {
		t.Fatalf("TLS GET /health failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS /health status = %d, want 200", resp.StatusCode)
	}

	// Plain HTTP on the same port must NOT serve the endpoint: Go's TLS
	// server answers a plaintext request with a 400, so require either a
	// transport error or a non-200 response (never a served 200).
	plainResp, plainErr := http.Get("http://" + addr + "/health")
	if plainErr == nil && plainResp != nil && plainResp.StatusCode == http.StatusOK {
		t.Fatal("expected plaintext HTTP to fail on a TLS-only listener")
	}
	if plainResp != nil {
		_ = plainResp.Body.Close()
	}
}

func TestRunValidatorLoop_PeriodicReRegisterFires(t *testing.T) {
	dir := t.TempDir()
	var regCalls atomic.Int32
	var lastBody atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			regCalls.Add(1)
			b, _ := io.ReadAll(r.Body)
			lastBody.Store(string(b))
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	srv := tlsTestServer(t, func(cfg *config.Config) {
		cfg.TLSEnabled = true
		cfg.TLSCertFile = filepath.Join(dir, "cert.pem")
		cfg.TLSKeyFile = filepath.Join(dir, "key.pem")
		cfg.TLSAutoGenerate = true
		cfg.RecallAPIURL = ts.URL
		cfg.MinerAPIURL = "http://miner:8001"
		cfg.HeartbeatInterval = 10 * time.Second // keep heartbeats out of the way
		cfg.MinerRegistrationInterval = 50 * time.Millisecond
	})
	kp := tlsTestKeypair(t)
	srv.cfg.MinerHotkey = kp.Address
	if err := srv.initTLS(kp.Address); err != nil {
		t.Fatalf("initTLS: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		srv.runValidatorLoop(ctx, kp)
		close(done)
	}()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	if regCalls.Load() < 2 {
		t.Fatalf("expected periodic re-registration to fire >=2 times, got %d", regCalls.Load())
	}
	body := lastBody.Load().(string)
	if !strings.Contains(body, srv.SPKIPin()) {
		t.Fatal("re-registration body did not carry the current SPKI pin")
	}
}
