package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"

	"recall-miner/internal/registry"
)

func testKeypair(t *testing.T) signature.KeyringPair {
	t.Helper()
	kp, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

func registerSidecar(t *testing.T, srv *server, name, url string, caps ...string) {
	t.Helper()
	srv.handlers.Registry().Register(registry.RegisterRequest{
		Name:         name,
		URL:          url,
		Capabilities: caps,
		Models:       []string{srv.cfg.GenerationModel},
		Metadata:     map[string]any{"generation_connector": "chutes"},
	})
}

func TestAvailableCapabilities_NoSidecars_Empty(t *testing.T) {
	srv := testServer(t)
	caps := srv.handlers.AvailableCapabilities()
	if len(caps) != 0 {
		t.Fatalf("expected no capabilities with no sidecars, got %v", caps)
	}
}

func TestAvailableCapabilities_DBSidecarOnly_DB(t *testing.T) {
	srv := testServer(t)
	registerSidecar(t, srv, "db", "http://db:9000", "db")
	caps := srv.handlers.AvailableCapabilities()
	if !containsString(caps, "db") {
		t.Fatalf("expected db capability, got %v", caps)
	}
	for _, absent := range []string{"embedding", "rerank", "generate"} {
		if containsString(caps, absent) {
			t.Fatalf("unexpected capability %q in %v", absent, caps)
		}
	}
}

func TestAvailableCapabilities_CPUSidecar_EmbeddingRerank(t *testing.T) {
	srv := testServer(t)
	registerSidecar(t, srv, "cpu", "http://cpu:9200", "embedding", "rerank")
	caps := srv.handlers.AvailableCapabilities()
	if !containsString(caps, "embedding") || !containsString(caps, "rerank") {
		t.Fatalf("expected embedding+rerank, got %v", caps)
	}
	if containsString(caps, "db") || containsString(caps, "generate") {
		t.Fatalf("unexpected db/generate in %v", caps)
	}
}

func TestAvailableCapabilities_GenerateDisabledWithSidecar_NoGenerate(t *testing.T) {
	srv := testServer(t)
	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")
	caps := srv.handlers.AvailableCapabilities()
	if containsString(caps, "generate") {
		t.Fatal("expected generate excluded when GENERATION_ENABLED=false")
	}
}

func TestAvailableCapabilities_GenerateEnabledWithSidecar_Generate(t *testing.T) {
	srv := testServerWithGenEnabled(t)
	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")
	caps := srv.handlers.AvailableCapabilities()
	if !containsString(caps, "generate") {
		t.Fatal("expected generate included when enabled with healthy sidecar")
	}
}

func TestAvailableCapabilities_UnhealthySidecar_NotAdvertised(t *testing.T) {
	srv := testServer(t)
	registerSidecar(t, srv, "cpu", "http://cpu:9200", "embedding")
	srv.handlers.Registry().Touch("cpu", "", map[string]any{"status": "degraded"})
	caps := srv.handlers.AvailableCapabilities()
	if containsString(caps, "embedding") {
		t.Fatalf("expected degraded sidecar excluded, got %v", caps)
	}
}

func TestHeartbeatPayload_IncludesCapabilities(t *testing.T) {
	srv := testServerWithGenEnabled(t)
	registerSidecar(t, srv, "db", "http://db:9000", "db")
	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")
	caps := srv.handlers.AvailableCapabilities()
	if !containsString(caps, "db") || !containsString(caps, "generate") {
		t.Fatalf("expected db+generate, got %v", caps)
	}
}

func TestHeartbeat_CapabilityChange_OnSidecarLifecycle(t *testing.T) {
	srv := testServerWithGenEnabled(t)

	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")
	caps := srv.handlers.AvailableCapabilities()
	if !containsString(caps, "generate") {
		t.Fatal("expected generate after registration")
	}

	srv.handlers.Registry().Unregister("chutes")
	caps = srv.handlers.AvailableCapabilities()
	if containsString(caps, "generate") {
		t.Fatal("expected generate dropped after unregister")
	}

	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")
	caps = srv.handlers.AvailableCapabilities()
	if !containsString(caps, "generate") {
		t.Fatal("expected generate back after re-registration")
	}
}

func TestRegisterValidatorPayload_IncludesCapabilities(t *testing.T) {
	srv := testServerWithGenEnabled(t)
	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")
	caps := srv.handlers.AvailableCapabilities()
	payload := map[string]any{
		"capabilities": caps,
		"endpoint":     srv.cfg.MinerAPIURL,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if !containsString(decoded.Capabilities, "generate") {
		t.Fatal("register payload missing generate")
	}
}

func TestSignedServicesExcludesGenerateWhenDisabled(t *testing.T) {
	srv := testServer(t)
	keypair := testKeypair(t)
	srv.cfg.MinerHotkey = keypair.Address
	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")
	caps := srv.handlers.AvailableCapabilities()
	if containsString(caps, "generate") {
		t.Fatal("AvailableCapabilities should exclude generate when disabled")
	}
}

func TestRegisterValidator_PostsCapabilities(t *testing.T) {
	srv := testServerWithGenEnabled(t)
	registerSidecar(t, srv, "db", "http://db:9000", "db")
	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")

	var captured json.RawMessage
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	srv.cfg.RecallAPIURL = ts.URL

	keypair := testKeypair(t)
	srv.cfg.MinerHotkey = keypair.Address
	if err := srv.registerValidator(t.Context(), keypair); err != nil {
		t.Fatalf("registerValidator: %v", err)
	}

	var payload struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatal(err)
	}
	if !containsString(payload.Capabilities, "generate") {
		t.Fatalf("registerValidator posted caps=%v, missing generate", payload.Capabilities)
	}
	if !containsString(payload.Capabilities, "db") {
		t.Fatal("registerValidator posted caps missing db")
	}
	if containsString(payload.Capabilities, "embedding") {
		t.Fatalf("registerValidator posted caps=%v, should not advertise embedding without a sidecar", payload.Capabilities)
	}
}

func TestHeartbeatValidator_PostsCapabilitiesAndResourceStats(t *testing.T) {
	srv := testServerWithGenEnabled(t)
	registerSidecar(t, srv, "chutes", "http://gen:9200", "generate")

	var captured json.RawMessage
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	srv.cfg.RecallAPIURL = ts.URL

	keypair := testKeypair(t)
	srv.cfg.MinerHotkey = keypair.Address
	if err := srv.heartbeatValidator(t.Context(), keypair); err != nil {
		t.Fatalf("heartbeatValidator: %v", err)
	}

	var payload struct {
		Capabilities  []string       `json:"capabilities"`
		ResourceStats map[string]any `json:"resource_stats"`
	}
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ResourceStats == nil {
		t.Fatal("heartbeatValidator payload missing resource_stats")
	}
	if !containsString(payload.Capabilities, "generate") {
		t.Fatalf("heartbeatValidator posted caps=%v, missing generate", payload.Capabilities)
	}
}

func TestHeartbeatValidator_WithoutGenSidecar_ExcludesGenerate(t *testing.T) {
	srv := testServerWithGenEnabled(t)

	var captured json.RawMessage
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	srv.cfg.RecallAPIURL = ts.URL

	keypair := testKeypair(t)
	srv.cfg.MinerHotkey = keypair.Address
	if err := srv.heartbeatValidator(t.Context(), keypair); err != nil {
		t.Fatalf("heartbeatValidator: %v", err)
	}

	var payload struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatal(err)
	}
	if containsString(payload.Capabilities, "generate") {
		t.Fatal("heartbeatValidator should exclude generate when no sidecar")
	}
}

func TestRegisterValidator_WithoutGenSidecar_ExcludesGenerate(t *testing.T) {
	srv := testServerWithGenEnabled(t)

	var captured json.RawMessage
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	srv.cfg.RecallAPIURL = ts.URL

	keypair := testKeypair(t)
	srv.cfg.MinerHotkey = keypair.Address
	if err := srv.registerValidator(t.Context(), keypair); err != nil {
		t.Fatalf("registerValidator: %v", err)
	}

	var payload struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatal(err)
	}
	if containsString(payload.Capabilities, "generate") {
		t.Fatal("registerValidator should exclude generate when no sidecar")
	}
	if containsString(payload.Capabilities, "db") {
		t.Fatal("registerValidator should exclude db when no db sidecar")
	}
}

func containsString(slice []string, needle string) bool {
	for _, s := range slice {
		if s == needle {
			return true
		}
	}
	return false
}

func TestRegisterValidator_PostsEmbeddingModels(t *testing.T) {
	srv := testServer(t)
	srv.cfg.EmbeddingModels = []string{"bge-small-en-v1.5", "bge-m3"}

	var captured json.RawMessage
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	srv.cfg.RecallAPIURL = ts.URL

	keypair := testKeypair(t)
	srv.cfg.MinerHotkey = keypair.Address
	if err := srv.registerValidator(t.Context(), keypair); err != nil {
		t.Fatalf("registerValidator: %v", err)
	}
	var payload struct {
		EmbeddingModels []string `json:"embedding_models"`
	}
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.EmbeddingModels) != 2 || payload.EmbeddingModels[0] != "bge-small-en-v1.5" || payload.EmbeddingModels[1] != "bge-m3" {
		t.Fatalf("embedding_models = %#v", payload.EmbeddingModels)
	}

	if err := srv.heartbeatValidator(t.Context(), keypair); err != nil {
		t.Fatalf("heartbeatValidator: %v", err)
	}
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.EmbeddingModels) != 2 || payload.EmbeddingModels[1] != "bge-m3" {
		t.Fatalf("heartbeat embedding_models = %#v", payload.EmbeddingModels)
	}
}
