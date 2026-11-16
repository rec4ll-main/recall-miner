package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

func TestServicesMatchesAdvertisedReadiness(t *testing.T) {
	for _, tc := range []struct {
		name       string
		health     string
		generation bool
		wantEmbed  bool
		wantGen    bool
	}{
		{"healthy generation disabled", "ok", false, true, false},
		{"healthy generation enabled", "ok", true, true, true},
		{"degraded generation enabled", "degraded", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := registry.New()
			reg.Register(registry.RegisterRequest{
				Name: "cpu", URL: "http://unused.invalid",
				Capabilities: []string{"embedding", "generate"},
				Models:       []string{"synthetic-model"}, Metadata: map[string]any{"generation_connector": "chutes"},
			})
			reg.Touch("cpu", "", map[string]any{"status": tc.health})
			h := &Handlers{cfg: config.Config{GenerationEnabled: tc.generation, GenerationBackendName: "cpu", GenerationModel: "synthetic-model"}, registry: reg}
			recorder := httptest.NewRecorder()
			h.Services(recorder, httptest.NewRequest(http.MethodGet, "/services", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			var body struct {
				Capabilities map[string]struct {
					Available bool `json:"available"`
				} `json:"capabilities"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			for capability, want := range map[string]bool{"embedding": tc.wantEmbed, "generate": tc.wantGen, "db": false, "rerank": false} {
				got, exists := body.Capabilities[capability]
				if !exists || got.Available != want {
					t.Fatalf("%s available = %v (present %v), want %v", capability, got.Available, exists, want)
				}
			}
		})
	}
}

func TestServicesDoesNotAdvertiseDegradedDuplicate(t *testing.T) {
	reg := registry.New()
	for name, health := range map[string]string{"cpu": "degraded", "gpu": "ok"} {
		reg.Register(registry.RegisterRequest{Name: name, URL: "http://" + name + ".invalid", Capabilities: []string{"embedding"}})
		reg.Touch(name, "", map[string]any{"status": health})
	}
	h := &Handlers{registry: reg}
	recorder := httptest.NewRecorder()
	h.Services(recorder, httptest.NewRequest(http.MethodGet, "/services", nil))
	var body struct {
		Capabilities map[string]struct {
			Available bool   `json:"available"`
			Backend   string `json:"backend"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := body.Capabilities["embedding"]; !got.Available || got.Backend != "gpu" {
		t.Fatalf("embedding = %#v; want healthy gpu", got)
	}
}
