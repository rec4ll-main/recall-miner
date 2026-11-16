package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"recall-miner/internal/registry"
	"testing"
)

func TestRerankLoadedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body, model string
		status            int
	}{
		{"legacy unknown", `[{"index":0,"score":1}]`, "", 200},
		{"loaded identity", `[{"index":0,"score":1,"model":"cross-encoder/actual","model_revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`, "cross-encoder/actual", 200},
		{"local path", `[{"index":0,"score":1,"model":"/private/model"}]`, "", 502},
		{"revision without identity", `[{"index":0,"score":1,"model_revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`, "", 502},
		{"invalid revision", `[{"index":0,"score":1,"model":"cross-encoder/actual","model_revision":"main"}]`, "", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHandler(t, false)
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) }))
			defer b.Close()
			h.registry.Register(registry.RegisterRequest{Name: "r", URL: b.URL, Capabilities: []string{"rerank"}, Models: []string{"embedding/wrong"}})
			w := doRerank(t, h, `{"query":"q","texts":["a"]}`)
			if w.Code != tc.status {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			if w.Code == 200 {
				var out map[string]any
				json.Unmarshal(w.Body.Bytes(), &out)
				if out["model"] != tc.model {
					t.Fatal(out)
				}
			}
		})
	}
	t.Run("inconsistent rows", func(t *testing.T) {
		h := testHandler(t, false)
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`[{"index":0,"score":1,"model":"cross-encoder/a"},{"index":1,"score":0,"model":"cross-encoder/b"}]`))
		}))
		defer b.Close()
		h.registry.Register(registry.RegisterRequest{Name: "r", URL: b.URL, Capabilities: []string{"rerank"}})
		w := doRerank(t, h, `{"query":"q","texts":["a","b"]}`)
		if w.Code != 502 {
			t.Fatal(w.Code)
		}
	})
}
