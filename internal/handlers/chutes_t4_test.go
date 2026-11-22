package handlers

// Chutes generation: deadline ordering, exact registration and advertisement.
// Synthetic registry entries and transports only; no listener or provider call.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"recall-miner/internal/client"
	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

const productionModel = "Qwen/Qwen3.8-27B-TEE"

func TestSidecarBudgetOrdering(t *testing.T) {
	for _, tc := range []struct {
		remaining, margin, want time.Duration
		ok                      bool
	}{
		{30 * time.Second, 2 * time.Second, 28 * time.Second, true},
		{60 * time.Second, 2 * time.Second, 58 * time.Second, true},
		{85 * time.Second, 2 * time.Second, 83 * time.Second, true},
		{time.Second, 2 * time.Second, 750 * time.Millisecond, true},
		{0, 2 * time.Second, 0, false},
		{-time.Second, 2 * time.Second, 0, false},
		{30 * time.Second, 0, 0, false},
		{30 * time.Second, -time.Second, 0, false},
	} {
		got, ok := sidecarBudget(tc.remaining, tc.margin)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("sidecarBudget(%v,%v)=%v,%v want %v,%v", tc.remaining, tc.margin, got, ok, tc.want, tc.ok)
		}
		if ok && got >= tc.remaining {
			t.Fatalf("budget %v not strictly inside Go deadline %v", got, tc.remaining)
		}
	}
}

// productionRegistration is exactly what cmd/model-proxy posts for the env in
// RESULT.md: SERVICE_NAME=chutes CAPABILITIES=generate MODELS=<model>
// METADATA={"generation_connector":"chutes"} (model-proxy adds "host").
func productionRegistration(url string) []byte {
	body, _ := json.Marshal(map[string]any{
		"name": "chutes", "url": url, "capabilities": []string{"generate"}, "models": []string{productionModel},
		"metadata":   map[string]any{"generation_connector": "chutes", "host": map[string]any{"goos": "linux"}},
		"machine_id": "synthetic-machine",
	})
	return body
}

func t4Handlers(t *testing.T, cfg config.Config, transport fakeHostedTransport) *Handlers {
	t.Helper()
	hc := &http.Client{Transport: transport}
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), hc, client.New(hc, "synthetic-internal-key"), registry.New(), nil)
}

func t4Register(t *testing.T, h *Handlers, body []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RegisterService(rec, httptest.NewRequest(http.MethodPost, "/internal/services/register", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("register status %d", rec.Code)
	}
}

func TestProductionDefaultsAndExactRegistrationRoute(t *testing.T) {
	t.Setenv("GENERATION_ENABLED", "")
	t.Setenv("CHUTES_BACKEND_NAME", "")
	t.Setenv("CHUTES_MODEL", "")
	t.Setenv("GENERATION_BACKEND_TIMEOUT_SECONDS", "")
	t.Setenv("GENERATION_SIDECAR_DEADLINE_MARGIN_MS", "")
	cfg := config.FromEnv()
	if cfg.GenerationBackendName != "chutes" || cfg.GenerationModel != productionModel ||
		cfg.GenerationBackendTimeout != 58*time.Second || cfg.GenerationSidecarMargin != 2*time.Second || cfg.GenerationEnabled {
		t.Fatalf("unexpected defaults %q %q %v %v %v", cfg.GenerationBackendName, cfg.GenerationModel,
			cfg.GenerationBackendTimeout, cfg.GenerationSidecarMargin, cfg.GenerationEnabled)
	}
	cfg.GenerationEnabled = true
	var sent map[string]any
	h := t4Handlers(t, cfg, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "http://chutes.invalid:9100/generate" {
			t.Fatalf("unexpected sidecar URL %s", r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"answer":"synthetic","model":"` + productionModel + `"}`))}, nil
	})
	t4Register(t, h, productionRegistration("http://chutes.invalid:9100"))
	rec := httptest.NewRecorder()
	h.Generate(rec, httptest.NewRequest(http.MethodPost, "/generate", strings.NewReader(`{"query":"synthetic"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if sent["model"] != productionModel {
		t.Fatalf("sidecar model %v", sent["model"])
	}
	// Go's 58s deadline minus the 2s margin, measured a few ms after the context began.
	ms, _ := sent["deadline_ms"].(float64)
	if ms <= 55000 || ms > 56000 {
		t.Fatalf("deadline_ms %v not inside (55000, 56000]", sent["deadline_ms"])
	}
}

func TestMisregistrationNeverRoutesOrAdvertises(t *testing.T) {
	cfg := config.FromEnv()
	cfg.GenerationEnabled = true
	cfg.GenerationModel = productionModel
	cfg.GenerationBackendName = "chutes"
	for name, mutate := range map[string]func(map[string]any){
		"name case":         func(p map[string]any) { p["name"] = "Chutes" },
		"connector missing": func(p map[string]any) { p["metadata"] = map[string]any{} },
		"connector case":    func(p map[string]any) { p["metadata"] = map[string]any{"generation_connector": "Chutes"} },
		"model without TEE": func(p map[string]any) { p["models"] = []string{"Qwen/Qwen3.8-27B"} },
		"no capability":     func(p map[string]any) { p["capabilities"] = []string{"embedding"} },
	} {
		t.Run(name, func(t *testing.T) {
			var p map[string]any
			_ = json.Unmarshal(productionRegistration("http://chutes.invalid:9100"), &p)
			mutate(p)
			body, _ := json.Marshal(p)
			calls := 0
			h := t4Handlers(t, cfg, func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			t4Register(t, h, body)
			for _, c := range h.AvailableCapabilities() {
				if c == "generate" {
					t.Fatal("misregistered backend advertised generate")
				}
			}
			rec := httptest.NewRecorder()
			h.Generate(rec, httptest.NewRequest(http.MethodPost, "/generate", strings.NewReader(`{"query":"synthetic"}`)))
			if rec.Code == http.StatusOK || calls != 0 {
				t.Fatalf("status %d calls %d", rec.Code, calls)
			}
		})
	}
}

func TestAdvertisementMatchesRoutingNotFirstHealthy(t *testing.T) {
	cfg := config.FromEnv()
	cfg.GenerationEnabled = true
	cfg.GenerationModel = productionModel
	cfg.GenerationBackendName = "chutes"
	h := t4Handlers(t, cfg, func(r *http.Request) (*http.Response, error) {
		t.Fatal("no sidecar dispatch expected")
		return nil, nil
	})
	// "aaa" sorts first; the pre-T4 gate advertised generate for any healthy backend.
	h.registry.Register(registry.RegisterRequest{Name: "aaa-local", URL: "http://aaa.invalid", Capabilities: []string{"generate"},
		Models: []string{productionModel}, Metadata: map[string]any{"generation_connector": "vllm"}})
	for _, c := range h.AvailableCapabilities() {
		if c == "generate" {
			t.Fatal("non-Chutes backend advertised")
		}
	}
	t4Register(t, h, productionRegistration("http://chutes.invalid:9100"))
	found := false
	for _, c := range h.AvailableCapabilities() {
		found = found || c == "generate"
	}
	if !found {
		t.Fatal("exact Chutes registration not advertised")
	}
	rec := httptest.NewRecorder()
	h.Services(rec, httptest.NewRequest(http.MethodGet, "/services", nil))
	var listed struct {
		Capabilities map[string]map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	gen := listed.Capabilities["generate"]
	if gen["available"] != true || gen["backend"] != "chutes" || gen["endpoint"] != "http://chutes.invalid:9100" {
		t.Fatalf("services lists %v", gen)
	}
	// Disabled generation is never advertised even with the exact registration.
	h.cfg.GenerationEnabled = false
	for _, c := range h.AvailableCapabilities() {
		if c == "generate" {
			t.Fatal("advertised while GENERATION_ENABLED=false")
		}
	}
}

func TestDegradedChutesHeartbeatWithdrawsGenerate(t *testing.T) {
	cfg := config.FromEnv()
	cfg.GenerationEnabled = true
	cfg.GenerationModel = productionModel
	cfg.GenerationBackendName = "chutes"
	h := t4Handlers(t, cfg, func(r *http.Request) (*http.Response, error) {
		t.Fatal("no sidecar dispatch expected")
		return nil, nil
	})
	t4Register(t, h, productionRegistration("http://chutes.invalid:9100"))
	// model-proxy reports "degraded" when UPSTREAM_HEALTH_PATH=/health/generation
	// answers 503 (e.g. an unresolved durable hold).
	if _, ok := h.registry.Touch("chutes", "synthetic-machine", map[string]any{"status": "degraded"}); !ok {
		t.Fatal("heartbeat not recorded")
	}
	for _, c := range h.AvailableCapabilities() {
		if c == "generate" {
			t.Fatal("degraded backend advertised")
		}
	}
	rec := httptest.NewRecorder()
	h.Generate(rec, httptest.NewRequest(http.MethodPost, "/generate", strings.NewReader(`{"query":"synthetic"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestMarginMisconfigurationRefusedBeforeDispatch(t *testing.T) {
	cfg := config.FromEnv()
	cfg.GenerationEnabled = true
	cfg.GenerationModel = productionModel
	cfg.GenerationBackendName = "chutes"
	cfg.GenerationSidecarMargin = 0
	calls := 0
	h := t4Handlers(t, cfg, func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, io.EOF
	})
	t4Register(t, h, productionRegistration("http://chutes.invalid:9100"))
	rec := httptest.NewRecorder()
	h.Generate(rec, httptest.NewRequest(http.MethodPost, "/generate", strings.NewReader(`{"query":"synthetic"}`)))
	if rec.Code != http.StatusServiceUnavailable || calls != 0 {
		t.Fatalf("status %d calls %d", rec.Code, calls)
	}
}
