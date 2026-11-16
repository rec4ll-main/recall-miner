package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"recall-miner/internal/registry"
	"strings"
	"testing"
)

func TestLogicalKeyScope(t *testing.T) {
	a, e := scopeGenerationRequestKey("sender-a", "o1:logical")
	if e != nil || len(a) != 67 {
		t.Fatal(a, e)
	}
	b, _ := scopeGenerationRequestKey("sender-b", "o1:logical")
	c, _ := scopeGenerationRequestKey("sender-a", "o1:logical")
	if a == b || a != c {
		t.Fatal("scope or stability")
	}
	for _, k := range []string{"", strings.Repeat("x", 129), "bad/key", " space", "雪"} {
		if _, e := scopeGenerationRequestKey("a", k); e == nil {
			t.Fatal(k)
		}
	}
	if _, e := scopeGenerationRequestKey("a", strings.Repeat("x", 128)); e != nil {
		t.Fatal(e)
	}
}
func TestLogicalKeyRejectsForgedHeaderBeforeBackend(t *testing.T) {
	h := testHandler(t, true)
	r := httptest.NewRequest("POST", "/generate", strings.NewReader(`{"query":"synthetic","request_key":"logical"}`))
	r.Header.Set("X-Verified-Hotkey", "forged")
	w := httptest.NewRecorder()
	h.Generate(w, r)
	if w.Code != 400 {
		t.Fatalf("status=%d", w.Code)
	}
}
func TestLogicalKeyForwardedWithoutRetry(t *testing.T) {
	h := testHandler(t, true)
	h.cfg.GenerationBackendName = "chutes"
	h.cfg.GenerationModel = "synthetic-model"
	calls := 0
	key, _ := scopeGenerationRequestKey("verified-synthetic", "o1:logical")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if p["request_key"] != key {
			t.Errorf("key=%v", p["request_key"])
		}
		w.WriteHeader(504)
	}))
	defer backend.Close()
	h.registry.Register(registry.RegisterRequest{Name: "chutes", URL: backend.URL, Capabilities: []string{"generate"}, Models: []string{"synthetic-model"}, Metadata: map[string]any{"generation_connector": "chutes"}})
	_, err := h.generate(context.Background(), generateRequest{Query: "public synthetic", RequestKey: "caller-must-not-be-forwarded", scopedRequestKey: key})
	if err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
