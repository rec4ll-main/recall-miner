package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"

	"recall-miner/internal/auth"
	"recall-miner/internal/config"
	"recall-miner/internal/heartbeat"
	"recall-miner/internal/registry"
	rank "recall-miner/internal/rerank"
)

func testServer(t *testing.T) *server {
	t.Helper()
	cfg := config.FromEnv()
	cfg.InternalAPIKey = "test-internal-key"
	cfg.HotkeySecret = ""
	cfg.MinerHotkey = ""
	cfg.RecallAPIURL = ""
	cfg.HTTPTimeout = time.Second
	return newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testServerWithGenEnabled(t *testing.T) *server {
	t.Helper()
	cfg := config.FromEnv()
	cfg.InternalAPIKey = "test-internal-key"
	cfg.HotkeySecret = ""
	cfg.MinerHotkey = ""
	cfg.RecallAPIURL = ""
	cfg.HTTPTimeout = time.Second
	cfg.GenerationEnabled = true
	return newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestValidatorAPIURLHandlesBaseWithAPIPath(t *testing.T) {
	tests := []struct {
		base string
		path string
		want string
	}{
		{base: "https://rec4ll.com/api", path: "/api/miner/register", want: "https://rec4ll.com/api/miner/register"},
		{base: "https://rec4ll.com/api/", path: "/api/miner/heartbeat", want: "https://rec4ll.com/api/miner/heartbeat"},
		{base: "http://api:8000", path: "/api/miner/register", want: "http://api:8000/api/miner/register"},
	}

	for _, test := range tests {
		if got := validatorAPIURL(test.base, test.path); got != test.want {
			t.Fatalf("validatorAPIURL(%q, %q) = %q, want %q", test.base, test.path, got, test.want)
		}
	}
}

func TestInternalServiceRegistrationAndServices(t *testing.T) {
	srv := testServer(t)
	handler := srv.routes()

	payload := registry.RegisterRequest{
		Name:         "cpu",
		URL:          "http://tei:80/",
		Capabilities: []string{"embedding", "rerank"},
		Models:       []string{"BAAI/bge-small-en-v1.5"},
		Metadata:     map[string]any{"gpu": false},
		MachineID:    "machine-a",
	}
	requestJSON := mustJSON(t, payload)
	req := httptest.NewRequest(http.MethodPost, "/internal/services/register", bytes.NewReader(requestJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Api-Key", srv.cfg.InternalAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/internal/services", nil)
	req.Header.Set("X-Internal-Api-Key", srv.cfg.InternalAPIKey)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var listed struct {
		Services map[string]registry.ServiceInfo `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	service := listed.Services["cpu"]
	if service.URL != "http://tei:80" {
		t.Fatalf("service URL = %q", service.URL)
	}
	if !registry.Contains(service.Capabilities, "embedding") || !registry.Contains(service.Capabilities, "rerank") {
		t.Fatalf("capabilities = %#v", service.Capabilities)
	}
}

func TestServiceHeartbeatRecorded(t *testing.T) {
	srv := testServer(t)
	handler := srv.routes()

	registerBody := mustJSON(t, registry.RegisterRequest{Name: "cpu", URL: "http://tei:80", Capabilities: []string{"embedding"}})
	req := httptest.NewRequest(http.MethodPost, "/internal/services/register", bytes.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Api-Key", srv.cfg.InternalAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, body = %s", rec.Code, rec.Body.String())
	}

	heartbeatBody := mustJSON(t, registry.HeartbeatRequest{
		Name:      "cpu",
		MachineID: "machine-a",
		Health: map[string]any{
			"status":               "ok",
			"upstream_status_code": 200,
			"metrics": map[string]any{
				"cpu_percent": 12.5,
				"memory":      map[string]any{"total_bytes": 1000, "used_bytes": 400, "used_percent": 40.0},
				"disk":        map[string]any{"total_bytes": 2000, "used_bytes": 500, "used_percent": 25.0},
			},
		},
	})
	req = httptest.NewRequest(http.MethodPost, "/internal/services/heartbeat", bytes.NewReader(heartbeatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Api-Key", srv.cfg.InternalAPIKey)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/internal/services/heartbeats?service=cpu&limit=1", nil)
	req.Header.Set("X-Internal-Api-Key", srv.cfg.InternalAPIKey)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeats status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var listed struct {
		Heartbeats []heartbeat.Record `json:"heartbeats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Heartbeats) != 1 {
		t.Fatalf("heartbeats count = %d", len(listed.Heartbeats))
	}
	got := listed.Heartbeats[0]
	if got.ServiceName != "cpu" || got.MachineID != "machine-a" || got.Status != "ok" {
		t.Fatalf("heartbeat = %#v", got)
	}
	if got.CPUPercent == nil || *got.CPUPercent != 12.5 {
		t.Fatalf("cpu_percent = %#v", got.CPUPercent)
	}
}

func TestSignedServicesEndpoint(t *testing.T) {
	srv := testServer(t)
	keypair, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.MinerHotkey = keypair.Address

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	setEpistulaHeaders(t, req, keypair, keypair.Address, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("services status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var response struct {
		Hotkey       string                 `json:"hotkey"`
		Capabilities map[string]interface{} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Hotkey != keypair.Address {
		t.Fatalf("hotkey = %q, want %q", response.Hotkey, keypair.Address)
	}
	if _, ok := response.Capabilities["embedding"]; !ok {
		t.Fatalf("missing embedding capability: %#v", response.Capabilities)
	}
}

func TestSignedServicesEndpointRejectsWrongSignedFor(t *testing.T) {
	srv := testServer(t)
	keypair, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.MinerHotkey = keypair.Address

	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	setEpistulaHeaders(t, req, keypair, "5GrwvaEF5zXb26Fz9rcQpDWS2X3kaJjUFkKToVvQWgY93gE6", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("services status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestValidatorSignedForUsesAPISigningHotkey(t *testing.T) {
	srv := testServer(t)
	keypair, err := signature.KeyringPairFromSecret("//Bob", 42)
	if err != nil {
		t.Fatal(err)
	}
	appKey, err := signature.KeyringPairFromSecret("//Charlie", 42)
	if err != nil {
		t.Fatal(err)
	}

	if got := srv.validatorSignedFor(keypair); got != keypair.Address {
		t.Fatalf("signed_for fallback = %q, want miner %q", got, keypair.Address)
	}
	srv.cfg.ApplicationProberHotkey = appKey.Address
	if got := srv.validatorSignedFor(keypair); got != appKey.Address {
		t.Fatalf("signed_for with application key = %q, want %q", got, appKey.Address)
	}
}

func TestDBBackendPinning(t *testing.T) {
	srv := testServer(t)
	srv.handlers.Registry().Register(registry.RegisterRequest{
		Name:         "db-a",
		URL:          "http://db-a:9000",
		Capabilities: []string{"db"},
		Metadata:     map[string]any{"api": "recall-db-v1"},
	})
	srv.handlers.Registry().Register(registry.RegisterRequest{
		Name:         "db-b",
		URL:          "http://db-b:9000",
		Capabilities: []string{"db"},
		Metadata:     map[string]any{"api": "recall-db-v1"},
	})
	srv.handlers.Registry().PinDB("db_123", "db-b")

	service, err := srv.handlers.PickDBService("db_123", "")
	if err != nil {
		t.Fatal(err)
	}
	if service.Name != "db-b" {
		t.Fatalf("picked %q, want db-b", service.Name)
	}

	service, err = srv.handlers.PickDBService("", "db-a")
	if err != nil {
		t.Fatal(err)
	}
	if service.Name != "db-a" {
		t.Fatalf("picked %q, want db-a", service.Name)
	}
}

func TestRerankCosineRanksCandidateIDs(t *testing.T) {
	result := rank.CosineResult(
		[]float64{1, 0},
		[][]float64{{0, 1}, {1, 0}, {0.5, 0.5}},
		[]string{"orthogonal", "same", "mixed"},
	)
	ranked, ok := result["ranked_ids"].([]string)
	if !ok {
		t.Fatalf("ranked_ids type = %T", result["ranked_ids"])
	}
	if got, want := ranked[0], "same"; got != want {
		t.Fatalf("top ranked id = %q, want %q; all=%v", got, want, ranked)
	}
	scores := result["scores"].([]float64)
	if scores[1] != 1 {
		t.Fatalf("same-vector score = %f, want 1", scores[1])
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func setEpistulaHeaders(t *testing.T, req *http.Request, keypair signature.KeyringPair, signedFor string, body []byte) {
	t.Helper()
	setEpistulaHeadersWithUUID(t, req, keypair, signedFor, body, "")
}

// setEpistulaHeadersWithUUID signs the request with an explicit request
// uuid so tests can craft replay pairs.
func setEpistulaHeadersWithUUID(t *testing.T, req *http.Request, keypair signature.KeyringPair, signedFor string, body []byte, requestUUID string) {
	t.Helper()
	timestamp := time.Now().UnixMilli()
	if requestUUID == "" {
		requestUUID = "test-uuid-" + fmt.Sprintf("%d", timestamp)
	}
	hash := auth.EnvelopeBodyHash(body)
	message := fmt.Sprintf("%s.%s.%d.%s", hash, requestUUID, timestamp, signedFor)
	sig, err := signature.Sign([]byte(message), keypair.URI)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Epistula-Version", "2")
	req.Header.Set("Epistula-Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("Epistula-Uuid", requestUUID)
	req.Header.Set("Epistula-Signed-By", keypair.Address)
	req.Header.Set("Epistula-Signed-For", signedFor)
	req.Header.Set("Epistula-Request-Signature", "0x"+hex.EncodeToString(sig))
	envelope := auth.EnvelopeMessage(req.Method, req.URL.EscapedPath(), req.URL.RawQuery, req.Header.Get("Content-Type"), hash, requestUUID, fmt.Sprintf("%d", timestamp), signedFor, keypair.Address)
	envSig, err := signature.Sign(auth.EnvelopeDigest(envelope), keypair.URI)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Epistula-Envelope-Signature", "0x"+hex.EncodeToString(envSig))
}

func TestCrossRouteReplayRejectedThroughServerRoutes(t *testing.T) {
	srv := testServer(t)
	keypair, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.MinerHotkey = keypair.Address
	srv.cfg.ApplicationProberHotkey = keypair.Address

	// Two distinct protected routes signed with the SAME sender and uuid.
	// With the server-scoped replay cache, the second request must be
	// rejected even though it hits a different route. Under per-route
	// caches this would pass.
	reqA := httptest.NewRequest(http.MethodPost, "/db/create", bytes.NewReader([]byte(`{}`)))
	setEpistulaHeadersWithUUID(t, reqA, keypair, keypair.Address, []byte(`{}`), "cross-route-uuid")
	recA := httptest.NewRecorder()
	srv.routes().ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK && recA.Code != http.StatusBadRequest {
		t.Fatalf("expected first route to pass auth (200/400), got %d", recA.Code)
	}

	reqB := httptest.NewRequest(http.MethodPost, "/embed_text", bytes.NewReader([]byte(`{}`)))
	setEpistulaHeadersWithUUID(t, reqB, keypair, keypair.Address, []byte(`{}`), "cross-route-uuid")
	recB := httptest.NewRecorder()
	srv.routes().ServeHTTP(recB, reqB)
	if recB.Code != http.StatusUnauthorized {
		t.Fatalf("expected cross-route replay rejected with 401, got %d", recB.Code)
	}
}

func TestAppStartupFailsWithEmptyValidatorAllowlist(t *testing.T) {
	os.Setenv("ENVIRONMENT", "test")
	defer os.Unsetenv("ENVIRONMENT")
	os.Unsetenv("VALIDATOR_HOTKEY_SS58")
	os.Unsetenv("API_SIGNING_HOTKEY_SS58")
	os.Unsetenv("VALIDATOR_ALLOWLIST_DEV_EMPTY")
	app, err := New(context.Background())
	if err == nil {
		if app != nil {
			app.Close()
		}
		t.Fatal("expected startup to fail with empty validator allowlist in production mode")
	}
	if !strings.Contains(err.Error(), "validator allowlist is empty") || !strings.Contains(err.Error(), "set API_SIGNING_HOTKEY_SS58 (the Recall API signing key)") || strings.Contains(err.Error(), "VALIDATOR_HOTKEY_SS58") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAppStartupAllowsDevEmptyAllowlistOnlyWhenEnabled(t *testing.T) {
	os.Setenv("ENVIRONMENT", "test")
	defer os.Unsetenv("ENVIRONMENT")
	os.Unsetenv("VALIDATOR_HOTKEY_SS58")
	os.Unsetenv("API_SIGNING_HOTKEY_SS58")
	os.Setenv("VALIDATOR_ALLOWLIST_DEV_EMPTY", "true")
	defer os.Unsetenv("VALIDATOR_ALLOWLIST_DEV_EMPTY")
	app, err := New(context.Background())
	if err != nil {
		t.Fatalf("expected startup to succeed with dev empty allowlist: %v", err)
	}
	if app != nil {
		app.Close()
	}
}

func TestProtectedRoutesRequireAuth(t *testing.T) {
	srv := testServer(t)
	protected := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/services"},
		{http.MethodPost, "/db/create"},
		{http.MethodDelete, "/db/db-1"},
		{http.MethodGet, "/db/db-1"},
		{http.MethodPost, "/db/db-1/restore"},
		{http.MethodPost, "/db/db-1/store"},
		{http.MethodDelete, "/db/db-1/embed/e-1"},
		{http.MethodPost, "/db/db-1/query"},
		{http.MethodPost, "/embed_text"},
		{http.MethodPost, "/embed_text/batch"},
		{http.MethodPost, "/rerank"},
		{http.MethodPost, "/generate"},
	}
	for _, tc := range protected {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth: expected 401, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestValidateDeploymentSecrets_UnsetEnvironmentIsArmed(t *testing.T) {
	cfg := config.Config{Environment: "", HotkeySecret: "//RecallMiner//local"}
	if err := validateDeploymentSecrets(cfg); err == nil {
		t.Fatal("expected unset ENVIRONMENT to be armed")
	}
}

func TestValidateDeploymentSecrets_DevExempt(t *testing.T) {
	cfg := config.Config{Environment: "dev", HotkeySecret: "//RecallMiner//local"}
	if err := validateDeploymentSecrets(cfg); err != nil {
		t.Fatalf("expected dev exempt: %v", err)
	}
	cfg.Environment = "test"
	if err := validateDeploymentSecrets(cfg); err != nil {
		t.Fatalf("expected test exempt: %v", err)
	}
}

func TestValidateDeploymentSecrets_NonDevRejectsPlaceholder(t *testing.T) {
	cfg := config.Config{Environment: "staging", HotkeySecret: "//RecallMiner//local"}
	if err := validateDeploymentSecrets(cfg); err == nil {
		t.Fatal("expected placeholder hotkey rejected in staging")
	}
	cfg.HotkeySecret = ""
	if err := validateDeploymentSecrets(cfg); err == nil {
		t.Fatal("expected empty hotkey rejected in staging")
	}
	cfg.HotkeySecret = "8f8d5b2e9c1a4f6e3d7c5b9a2e8f1d4c6b0a9e8f7d6c5b4a3e2f1d0c9b8a7f"
	if err := validateDeploymentSecrets(cfg); err != nil {
		t.Fatalf("expected real hotkey accepted: %v", err)
	}
}

func TestValidateInternalAPIKey_DevExempt(t *testing.T) {
	for _, env := range []string{"dev", "test"} {
		cfg := config.Config{Environment: env, InternalAPIKey: "local-miner-internal-key"}
		if err := validateInternalAPIKey(cfg); err != nil {
			t.Fatalf("expected %s exempt from internal key guard: %v", env, err)
		}
	}
}

func TestValidateInternalAPIKey_NonDevRejectsEmptyOrPlaceholder(t *testing.T) {
	for _, env := range []string{"", "staging", "mainnet", "production", "prod", "custom-label"} {
		for _, key := range []string{"", "local-miner-internal-key", "change-me", "changeme", "placeholder", "local-dev", "local-api", "my-local-api-key-123"} {
			cfg := config.Config{Environment: env, InternalAPIKey: key}
			if err := validateInternalAPIKey(cfg); err == nil {
				t.Fatalf("expected internal key %q rejected in environment %q", key, env)
			}
		}
	}
}

func TestValidateInternalAPIKey_NonDevAcceptsRealKey(t *testing.T) {
	for _, env := range []string{"staging", "mainnet"} {
		cfg := config.Config{Environment: env, InternalAPIKey: "8f8d5b2e9c1a4f6e3d7c5b9a2e8f1d4c6b0a9e8f7d6c5b4a3e2f1d0c9b8a7f"}
		if err := validateInternalAPIKey(cfg); err != nil {
			t.Fatalf("expected real internal key accepted for %q: %v", env, err)
		}
	}
}

func TestRoutesRejectOversizedBodyBeforeAuth(t *testing.T) {
	srv := testServer(t)
	srv.cfg.MaxBodyBytes = 16
	body := bytes.Repeat([]byte("x"), 32)
	req := httptest.NewRequest(http.MethodPost, "/db/1/store", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 before auth for oversized body, got %d", rec.Code)
	}
}

func TestRoutesServeNormalSizedAuthRequest(t *testing.T) {
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
		t.Fatalf("expected 200 for normal signed request, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRoutesAcceptOversizedRestorePageWhenCapRaised(t *testing.T) {
	srv := testServer(t)
	srv.cfg.MaxBodyBytes = 4 << 20
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	srv.keypair = &kp
	srv.cfg.MinerHotkey = kp.Address
	srv.cfg.ApplicationProberHotkey = kp.Address
	payload := []byte(`{"vector":[` + strings.Repeat("0.5,", 2<<17) + `0.5]}`)
	req := httptest.NewRequest(http.MethodPost, "/db/1/store", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	setEpistulaHeaders(t, req, kp, kp.Address, payload)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("body >1MiB must pass when MAX_BODY_BYTES is raised (4MiB), got 413")
	}
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("expected the oversized body to pass the cap and reach the handler; got 400 body=%s", rec.Body.String())
	}
}
