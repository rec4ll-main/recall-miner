package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"recall-miner/internal/httpx"
	"recall-miner/internal/registry"
	"strings"
	"testing"
	"time"
)

func TestInferenceDependencyStatusMapping(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413, 422, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := testHandler(t, false)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				w.Write([]byte(`{"detail":"synthetic dependency error"}`))
			}))
			defer server.Close()
			h.registry.Register(registry.RegisterRequest{Name: "rerank", URL: server.URL, Capabilities: []string{"rerank"}})
			out := doRerank(t, h, `{"query":"valid query","texts":["a","b"]}`)
			t.Logf("valid local request: sidecar=%d caller=%d calls=%d", status, out.Code, calls)
			want := 502
			if status == 429 {
				want = 503
			}
			if out.Code != want || calls != 1 {
				t.Fatalf("unexpected mapping %d %d", out.Code, calls)
			}
			out = doRerank(t, h, `{"query":`)
			if out.Code != 400 || calls != 1 {
				t.Fatal("malformed caller JSON should remain 400 before dispatch")
			}
		})
	}
}
func TestGenerationDependencyStatusMapping(t *testing.T) {
	for _, status := range []int{401, 429} {
		h := testHandler(t, true)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"detail":"synthetic dependency error"}`))
		}))
		registerGenerationFixture(h, server.URL)
		out := httptest.NewRecorder()
		h.Generate(out, httptest.NewRequest("POST", "/generate", jsonBody(`{"query":"valid query"}`)))
		server.Close()
		t.Logf("generation sidecar=%d caller=%d", status, out.Code)
		want := 502
		if status == 429 {
			want = 503
		}
		if out.Code != want {
			t.Fatal(out.Code)
		}
	}
}

func TestInferenceBoundariesHideProviderDetailsAndPreserveLocalValidation(t *testing.T) {
	for _, route := range []string{"rerank", "generate", "embedding", "embedding-batch", "tei", "tei-batch"} {
		for _, status := range []int{400, 401, 403, 404, 413, 422, 429, 500, 503, 200} {
			t.Run(fmt.Sprintf("%s/%d", route, status), func(t *testing.T) {
				h := testHandler(t, true)
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(status)
					w.Write([]byte("synthetic-provider-private-detail"))
				}))
				defer server.Close()
				capability, name := route, route
				if strings.HasPrefix(route, "embedding") || strings.HasPrefix(route, "tei") {
					capability = "embedding"
					name = "gpu"
					if strings.HasPrefix(route, "tei") {
						name = "tei"
					}
				}
				if route == "generate" {
					registerGenerationFixture(h, server.URL)
				} else {
					h.registry.Register(registry.RegisterRequest{Name: name, URL: server.URL, Capabilities: []string{capability}, Models: []string{"available"}})
				}
				handler, payload := h.Rerank, `{"query":"q","texts":["a"]}`
				switch route {
				case "generate":
					handler = h.Generate
					payload = `{"query":"q"}`
				case "embedding", "tei":
					handler = h.EmbedText
					payload = `{"text":"q"}`
				case "embedding-batch", "tei-batch":
					handler = h.EmbedBatch
					payload = `{"texts":["q"]}`
				}
				out := httptest.NewRecorder()
				handler(out, httptest.NewRequest("POST", "/", strings.NewReader(payload)))
				want := 502
				if status == 429 || status == 503 {
					want = 503
				}
				if out.Code != want || calls != 1 {
					t.Fatalf("status%d calls%d", out.Code, calls)
				}
				if strings.Contains(out.Body.String(), "synthetic-provider-private-detail") || strings.Contains(out.Body.String(), server.URL) {
					t.Fatal("provider detail leaked")
				}
				out = httptest.NewRecorder()
				handler(out, httptest.NewRequest("POST", "/", strings.NewReader(`{`)))
				if out.Code != 400 || calls != 1 {
					t.Fatal("local malformed request dispatched")
				}
				if capability == "embedding" {
					out = httptest.NewRecorder()
					h.EmbedText(out, httptest.NewRequest("POST", "/", strings.NewReader(`{"text":"q","model":"unsupported"}`)))
					if out.Code != 422 || calls != 1 {
						t.Fatal("local model validation changed")
					}
				}
				if route == "generate" {
					out = httptest.NewRecorder()
					handler(out, httptest.NewRequest("POST", "/", strings.NewReader(`{"query":""}`)))
					if out.Code != 400 || calls != 1 {
						t.Fatal("missing query dispatched")
					}
				}
			})
		}
	}
}

func TestInferenceTimeoutIsGenericAndGenericClientKeepsStatus(t *testing.T) {
	h := testHandler(t, true)
	// Leave enough budget to dispatch; sub-millisecond admission refusal is tested separately.
	h.cfg.GenerationBackendTimeout = 10 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(50 * time.Millisecond):
		}
	}))
	defer server.Close()
	registerGenerationFixture(h, server.URL)
	out := httptest.NewRecorder()
	h.Generate(out, httptest.NewRequest("POST", "/generate", strings.NewReader(`{"query":"q"}`)))
	if out.Code != 502 || strings.Contains(out.Body.String(), server.URL) {
		t.Fatal("timeout incorrectly mapped or leaked")
	}
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer missing.Close()
	err := h.apiClient.RequestJSON(context.Background(), "GET", missing.URL, nil, nil)
	if httpx.ErrorStatus(err, 0) != 404 {
		t.Fatal("generic client status changed")
	}
}
