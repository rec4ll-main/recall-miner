package config

import "testing"

func TestParseEmbeddingModels(t *testing.T) {
	got := ParseEmbeddingModels("")
	if len(got) != 0 {
		t.Fatalf("empty = %#v", got)
	}
	got = ParseEmbeddingModels(" bge-small-en-v1.5, bge-m3,bge-small-en-v1.5,,qwen3-embedding-8b ")
	want := []string{"bge-small-en-v1.5", "bge-m3", "qwen3-embedding-8b"}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %#v", got)
		}
	}
}

func TestFromEnvEmbeddingModels(t *testing.T) {
	t.Setenv("MINER_EMBEDDING_MODELS", "bge-m3,bge-small-en-v1.5")
	cfg := FromEnv()
	if len(cfg.EmbeddingModels) != 2 || cfg.EmbeddingModels[0] != "bge-m3" || cfg.EmbeddingModels[1] != "bge-small-en-v1.5" {
		t.Fatalf("models = %#v", cfg.EmbeddingModels)
	}
}
