package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"recall-miner/internal/client"
	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

// vLLM rejects embedding inputs longer than the model's context with HTTP 400
// unless the request asks it to truncate. Single and batch embedding requests
// to an OpenAI-style backend must both ask for truncation to the model limit.
func TestOpenAIEmbeddingRequestsAskBackendToTruncate(t *testing.T) {
	var bodies []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "BAAI/bge-small-en-v1.5",
			"data":  []map[string]any{{"index": 0, "embedding": []float64{0.1, 0.2}}},
		})
	}))
	defer backend.Close()
	reg := registry.New()
	reg.Register(registry.RegisterRequest{Name: "gpu", URL: backend.URL, Capabilities: []string{"embedding"}, Models: []string{"BAAI/bge-small-en-v1.5"}})
	reg.Touch("gpu", "", map[string]any{"status": "ok"})
	h := &Handlers{cfg: config.Config{}, registry: reg, apiClient: client.New(backend.Client(), "k")}

	if _, err := h.embedText(context.Background(), "a long probe text", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.embedBatch(context.Background(), []string{"a", "b"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("backend saw %d requests, want 2", len(bodies))
	}
	for i, body := range bodies {
		if got, ok := body["truncate_prompt_tokens"].(float64); !ok || got != -1 {
			t.Fatalf("request %d truncate_prompt_tokens = %v, want -1 (body %v)", i, body["truncate_prompt_tokens"], body)
		}
	}
}
