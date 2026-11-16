package handlers

import (
	"testing"

	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

func TestOutboundEmbedModel(t *testing.T) {
	advertised := []string{"BAAI/bge-m3"}
	if got := outboundEmbedModel("", advertised); got != "default" {
		t.Fatalf("empty request = %s", got)
	}
	if got := outboundEmbedModel("BAAI/bge-m3", advertised); got != "BAAI/bge-m3" {
		t.Fatalf("upstream passthrough = %s", got)
	}
	if got := outboundEmbedModel("bge-m3", advertised); got != "BAAI/bge-m3" {
		t.Fatalf("catalog rewrite = %s", got)
	}
	if got := outboundEmbedModel("qwen3-embedding-8b", advertised); got != "qwen3-embedding-8b" {
		t.Fatalf("unmatched catalog id = %s", got)
	}
}

func TestPickEmbedBackend_CatalogIDRequiresDeclaration(t *testing.T) {
	reg := registry.New()
	reg.Register(registry.RegisterRequest{
		Name:         "gpu",
		URL:          "http://vllm:8000",
		Capabilities: []string{"embedding"},
		Models:       []string{"BAAI/bge-m3"},
	})
	undeclared := &Handlers{cfg: config.Config{}, registry: reg}
	if _, err := undeclared.pickEmbedBackend("bge-m3"); err == nil {
		t.Fatal("catalog id must be rejected when the miner did not declare it")
	}
	declared := &Handlers{cfg: config.Config{EmbeddingModels: []string{"bge-m3"}}, registry: reg}
	backend, err := declared.pickEmbedBackend("bge-m3")
	if err != nil {
		t.Fatal(err)
	}
	if backend.Name != "gpu" {
		t.Fatalf("backend = %s", backend.Name)
	}
	if _, err := declared.pickEmbedBackend("qwen3-embedding-8b"); err == nil {
		t.Fatal("declared id with no matching backend must be rejected")
	}
}

func TestCatalogEmbeddingUpstreamIDs(t *testing.T) {
	want := map[string]string{
		"bge-small-en-v1.5":    "BAAI/bge-small-en-v1.5",
		"bge-m3":               "BAAI/bge-m3",
		"qwen3-embedding-0.6b": "Qwen/Qwen3-Embedding-0.6B",
		"qwen3-embedding-4b":   "Qwen/Qwen3-Embedding-4B",
		"qwen3-embedding-8b":   "Qwen/Qwen3-Embedding-8B",
	}
	for id, upstream := range want {
		got, ok := catalogEmbeddingUpstream(id)
		if !ok || got != upstream {
			t.Fatalf("%s -> %s ok=%v", id, got, ok)
		}
	}
	if _, ok := catalogEmbeddingUpstream("bge-large-en-v1.5"); ok {
		t.Fatal("bge-large is not a catalog id")
	}
	if _, ok := catalogEmbeddingUpstream("recall-embed-v1"); ok {
		t.Fatal("recall-embed-v1 is not a catalog id")
	}
}
