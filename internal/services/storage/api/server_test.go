package api

import (
	"testing"

	"recall-miner/internal/services/storage/config"
)

func TestNewVectorClient_UnknownEngineErrors(t *testing.T) {
	for _, engine := range []string{"weaviate", "cassandra", "", "  "} {
		client, err := newVectorClient(config.Config{DBEngine: engine})
		if err == nil {
			t.Fatalf("expected error for engine %q, got client %v", engine, client)
		}
		if client != nil {
			t.Fatalf("expected nil client for unknown engine %q", engine)
		}
	}
}

func TestNewVectorClient_KnownEnginesSucceed(t *testing.T) {
	for _, engine := range []string{"qdrant", "milvus", "pgsql", "postgres", "postgresql", "pgvector"} {
		client, err := newVectorClient(config.Config{DBEngine: engine})
		if err != nil {
			t.Fatalf("unexpected error for engine %q: %v", engine, err)
		}
		if client == nil {
			t.Fatalf("expected client for engine %q", engine)
		}
	}
}
