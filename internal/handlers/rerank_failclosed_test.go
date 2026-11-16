package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"recall-miner/internal/registry"
)

// A miner with no healthy rerank sidecar must fail closed
// with 503 instead of answering 200 with all-zero scores and model "none", which the
// validator would otherwise record as a served rerank.
func TestRerank_NoHealthyBackend_Returns503(t *testing.T) {
	h := testHandler(t, false)
	req := httptest.NewRequest(http.MethodPost, "/rerank", jsonBody(`{"query":"q","texts":["a","b"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Rerank(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body == "" || !json.Valid([]byte(body)) {
		t.Fatalf("expected a JSON error body, got %q", body)
	}
}

func TestRerank_HealthyBackend_StillRanks(t *testing.T) {
	h := testHandler(t, false)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"index": 0, "score": 0.1, "model": "cross-encoder/test-reranker"}, {"index": 1, "score": 0.9, "model": "cross-encoder/test-reranker"}})
	}))
	defer backend.Close()
	h.registry.Register(registry.RegisterRequest{Name: "rerank", URL: backend.URL, Capabilities: []string{"rerank"}, Models: []string{"embedding/wrong-label"}})

	req := httptest.NewRequest(http.MethodPost, "/rerank", jsonBody(`{"query":"q","texts":["a","b"],"candidate_ids":["x","y"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Rerank(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Scores   []float64 `json:"scores"`
		RankedID []string  `json:"ranked_ids"`
		Model    string    `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.RankedID) != 2 || out.RankedID[0] != "y" || out.Model != "cross-encoder/test-reranker" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

// A registered sidecar whose heartbeat reports a non-ok status is not used: the
// miner answers 503 instead of contacting it or inventing scores.
func TestRerank_DegradedBackend_Returns503WithoutContactingIt(t *testing.T) {
	h := testHandler(t, false)
	contacted := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	h.registry.Register(registry.RegisterRequest{Name: "rerank", URL: backend.URL, Capabilities: []string{"rerank"}})
	if _, ok := h.registry.Touch("rerank", "", map[string]any{"status": "degraded"}); !ok {
		t.Fatal("heartbeat for the registered sidecar must be accepted")
	}
	rec := doRerank(t, h, `{"query":"q","texts":["a","b"]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded sidecar: expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if contacted {
		t.Fatal("a degraded sidecar must not be contacted")
	}
}

// Genuine upstream failures surface as 5xx, never as a 200 ranking and never as a
// 4xx (which the Recall API would record as a content failure).
func TestRerank_UpstreamFailures_Return5xx(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"sidecar 500": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"model crashed"}`))
		},
		"malformed body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"not":"a list"`))
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			h := testHandler(t, false)
			backend := httptest.NewServer(fn)
			defer backend.Close()
			h.registry.Register(registry.RegisterRequest{Name: "rerank", URL: backend.URL, Capabilities: []string{"rerank"}})
			rec := doRerank(t, h, `{"query":"q","texts":["a","b"]}`)
			if rec.Code < 500 || rec.Code > 599 {
				t.Fatalf("expected a 5xx, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("unreachable sidecar", func(t *testing.T) {
		h := testHandler(t, false)
		backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := backend.URL
		backend.Close() // registered but nothing is listening any more
		h.registry.Register(registry.RegisterRequest{Name: "rerank", URL: url, Capabilities: []string{"rerank"}})
		rec := doRerank(t, h, `{"query":"q","texts":["a","b"]}`)
		if rec.Code < 500 || rec.Code > 599 {
			t.Fatalf("expected a 5xx, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func doRerank(t *testing.T, h *Handlers, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/rerank", jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Rerank(rec, req)
	return rec
}

func TestRerank_InvalidSidecarRankingRejected(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `[{"index":0,"score":1}]`, `[{"index":0,"score":1},{"index":0,"score":2}]`, `[{"index":0,"score":1},{"index":2,"score":2}]`, `[{"score":1},{"index":1,"score":2}]`, `[{"index":0},{"index":1,"score":2}]`} {
		t.Run(body, func(t *testing.T) {
			h := testHandler(t, false)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer backend.Close()
			h.registry.Register(registry.RegisterRequest{Name: "rerank", URL: backend.URL, Capabilities: []string{"rerank"}})
			rec := doRerank(t, h, `{"query":"q","texts":["a","b"]}`)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("invalid ranking accepted: status=%d", rec.Code)
			}
		})
	}
}
