package handlers

import (
	"testing"

	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

func TestPickEmbedBackendPrefersHealthyOverDegraded(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.RegisterRequest{
		Name:         "cpu",
		URL:          "http://cpu-embed",
		Capabilities: []string{"embedding"},
		Models:       []string{"BAAI/bge-small-en-v1.5"},
	})
	reg.Register(registry.RegisterRequest{
		Name:         "gpu",
		URL:          "http://gpu-embed",
		Capabilities: []string{"embedding"},
		Models:       []string{"BAAI/bge-small-en-v1.5"},
	})
	reg.Touch("cpu", "", map[string]any{"status": "degraded"})
	reg.Touch("gpu", "", map[string]any{"status": "ok"})

	h := &Handlers{cfg: config.Config{}, registry: reg}
	backend, err := h.pickEmbedBackend("")
	if err != nil {
		t.Fatal(err)
	}
	if backend.Name != "gpu" {
		t.Fatalf("omitted-model pick = %s, want gpu", backend.Name)
	}

	backend, err = h.pickEmbedBackend("BAAI/bge-small-en-v1.5")
	if err != nil {
		t.Fatal(err)
	}
	if backend.Name != "gpu" {
		t.Fatalf("explicit-model pick = %s, want gpu", backend.Name)
	}

	h.cfg.EmbeddingModels = []string{"bge-small-en-v1.5"}
	backend, err = h.pickEmbedBackend("bge-small-en-v1.5")
	if err != nil {
		t.Fatal(err)
	}
	if backend.Name != "gpu" {
		t.Fatalf("catalog-model pick = %s, want gpu", backend.Name)
	}
}

func TestPickEmbedBackendAllDegradedUnavailable(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.RegisterRequest{
		Name:         "cpu",
		URL:          "http://cpu-embed",
		Capabilities: []string{"embedding"},
		Models:       []string{"BAAI/bge-small-en-v1.5"},
	})
	reg.Touch("cpu", "", map[string]any{"status": "degraded"})
	h := &Handlers{cfg: config.Config{}, registry: reg}
	if _, err := h.pickEmbedBackend(""); err == nil {
		t.Fatal("expected unavailable when all embedding backends are degraded")
	}
}

func TestAvailableCapabilitiesMatchesHealthySelection(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.RegisterRequest{
		Name:         "cpu",
		URL:          "http://cpu-embed",
		Capabilities: []string{"embedding"},
		Models:       []string{"BAAI/bge-small-en-v1.5"},
	})
	reg.Touch("cpu", "", map[string]any{"status": "degraded"})
	h := &Handlers{cfg: config.Config{}, registry: reg}
	caps := h.AvailableCapabilities()
	for _, cap := range caps {
		if cap == "embedding" {
			t.Fatalf("advertised embedding while only degraded backends exist: %v", caps)
		}
	}
	if _, err := h.pickEmbedBackend(""); err == nil {
		t.Fatal("selection must agree with advertising and refuse degraded backends")
	}
}
