package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"recall-miner/internal/client"
	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

func testHandler(t *testing.T, genEnabled bool) *Handlers {
	t.Helper()
	cfg := config.FromEnv()
	cfg.GenerationEnabled = genEnabled
	cfg.GenerationBackendTimeout = time.Second
	cfg.InternalAPIKey = "test-key"
	httpClient := &http.Client{}
	apiClient := client.New(httpClient, cfg.InternalAPIKey)
	reg := registry.New()
	h := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), httpClient, apiClient, reg, nil)
	return h
}

// registerGenerationFixture makes the ordinary fixture eligible through the same
// explicit backend/model/connector contract as production. No selection bypass.
func registerGenerationFixture(h *Handlers, url string) {
	h.registry.Register(registry.RegisterRequest{Name: h.cfg.GenerationBackendName, URL: url,
		Capabilities: []string{"generate"}, Models: []string{"unselected-model", h.cfg.GenerationModel},
		Metadata: map[string]any{"generation_connector": "chutes"}})
}

func testFakeGenBackend(t *testing.T, statusCode int, response map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if response != nil {
			json.NewEncoder(w).Encode(response)
		}
	}))
}

func TestGenerate_Disabled_Returns503(t *testing.T) {
	h := testHandler(t, false)
	backend := testFakeGenBackend(t, 200, map[string]any{"answer": "hi"})
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)
	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGenerate_RequiresQuery(t *testing.T) {
	h := testHandler(t, true)
	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":""}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGenerate_NoBackend_Returns503(t *testing.T) {
	h := testHandler(t, true)
	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGenerate_ReturnsStructuredResponse(t *testing.T) {
	h := testHandler(t, true)
	fakeResp := map[string]any{
		"answer":    "The API key rotation process...",
		"citations": []map[string]any{{"context_id": "doc-1", "score": 0.95}},
		"model":     h.cfg.GenerationModel,
		"usage":     map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
	}
	backend := testFakeGenBackend(t, 200, fakeResp)
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)

	var capturedPayload map[string]any
	captureBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedPayload)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(fakeResp)
	}))
	defer captureBackend.Close()
	h.registry.Unregister(h.cfg.GenerationBackendName)
	registerGenerationFixture(h, captureBackend.URL)

	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{
		"query": "How do I rotate an API key?",
		"contexts": [{"id":"doc-1","text":"Rotate an API key by..."}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["answer"] != fakeResp["answer"] {
		t.Fatalf("answer not passed through: %v", out)
	}
	if out["model"] == nil {
		t.Fatal("model missing from response")
	}
	if out["usage"] == nil {
		t.Fatal("usage missing from response")
	}
	if capturedPayload == nil {
		t.Fatal("backend received no payload")
	}
	if capturedPayload["query"] != "How do I rotate an API key?" {
		t.Fatalf("backend received wrong query: %v", capturedPayload)
	}
	ctxs, _ := capturedPayload["contexts"].([]interface{})
	if len(ctxs) != 1 {
		t.Fatalf("backend received %d contexts, want 1", len(ctxs))
	}
	if capturedPayload["model"] != h.cfg.GenerationModel {
		t.Fatalf("backend received model=%v, want explicitly configured model, not Models[0]", capturedPayload["model"])
	}
}

func TestGenerate_MaxTokensForwarded(t *testing.T) {
	h := testHandler(t, true)
	var captured map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"answer": "ok", "model": h.cfg.GenerationModel})
	}))
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)

	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"test","max_tokens":256}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if v, _ := captured["max_tokens"].(float64); v != 256 {
		t.Fatalf("max_tokens not forwarded: %v", captured["max_tokens"])
	}
}

func TestGenerate_MaxTokensAbsentWhenZero(t *testing.T) {
	h := testHandler(t, true)
	var captured map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"answer": "ok", "model": h.cfg.GenerationModel})
	}))
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)

	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if _, ok := captured["max_tokens"]; ok {
		t.Fatal("max_tokens should be absent when 0")
	}
}

func TestGenerate_TemperatureForwarded(t *testing.T) {
	h := testHandler(t, true)
	var captured map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"answer": "ok", "model": h.cfg.GenerationModel})
	}))
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)

	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"test","temperature":0.7}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if v, _ := captured["temperature"].(float64); v != 0.7 {
		t.Fatalf("temperature not forwarded: %v", captured["temperature"])
	}
}

func TestGenerate_TemperatureAbsentWhenNil(t *testing.T) {
	h := testHandler(t, true)
	var captured map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"answer": "ok", "model": h.cfg.GenerationModel})
	}))
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)

	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if _, ok := captured["temperature"]; ok {
		t.Fatal("temperature should be absent when nil")
	}
}

func TestGenerate_ModelExplicitMatchesConfiguredModel(t *testing.T) {
	h := testHandler(t, true)
	var captured map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"answer": "ok", "model": h.cfg.GenerationModel})
	}))
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)

	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"test","model":"`+h.cfg.GenerationModel+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if captured["model"] != h.cfg.GenerationModel {
		t.Fatalf("explicit model not used: %v", captured["model"])
	}
}

func TestGenerate_BackendError_Returns502(t *testing.T) {
	h := testHandler(t, true)
	backend := testFakeGenBackend(t, 500, nil)
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)

	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 5xx, got %d", rec.Code)
	}
}

func jsonBody(s string) *io.PipeReader {
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(s))
		pw.Close()
	}()
	return pr
}

func TestGenerate_BackendTimeout_Returns502(t *testing.T) {
	h := testHandler(t, true)
	h.cfg.GenerationBackendTimeout = 50 * time.Millisecond
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"answer": "late"})
	}))
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)
	req := httptest.NewRequest(http.MethodPost, "/generate", jsonBody(`{"query":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Generate(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 5xx on backend timeout, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A caller may select the configured model, never an arbitrary advertised model.
func TestGenerate_ExplicitOtherModelRefusedWithoutDispatch(t *testing.T) {
	h := testHandler(t, true)
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer backend.Close()
	registerGenerationFixture(h, backend.URL)
	rec := httptest.NewRecorder()
	h.Generate(rec, httptest.NewRequest("POST", "/generate", jsonBody(`{"query":"synthetic","model":"unselected-model"}`)))
	if rec.Code != 400 || calls != 0 {
		t.Fatalf("status=%d calls=%d", rec.Code, calls)
	}
}
