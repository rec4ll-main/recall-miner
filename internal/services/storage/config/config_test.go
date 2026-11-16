package config

import (
	"os"
	"testing"
)

func TestFromEnv_DefaultDBEngineQdrant(t *testing.T) {
	os.Unsetenv("DB_ENGINE")
	if got := FromEnv().DBEngine; got != "qdrant" {
		t.Fatalf("default DBEngine = %q, want qdrant", got)
	}
}

func TestFromEnv_DBEngineLowercased(t *testing.T) {
	os.Setenv("DB_ENGINE", "Qdrant")
	defer os.Unsetenv("DB_ENGINE")
	if got := FromEnv().DBEngine; got != "qdrant" {
		t.Fatalf("DBEngine = %q, want lowercased qdrant", got)
	}
}

func TestValidate_QdrantAlwaysAccepted(t *testing.T) {
	for _, env := range []string{"", "dev", "test", "staging", "mainnet"} {
		cfg := Config{DBEngine: "qdrant", Environment: env}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("qdrant in env %q rejected: %v", env, err)
		}
	}
}

func TestValidate_UnknownEngineRejected(t *testing.T) {
	for _, engine := range []string{"weaviate", "cassandra", "elasticsearch", ""} {
		cfg := Config{DBEngine: engine}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unknown engine %q accepted", engine)
		}
	}
}

func TestValidate_MilvusRejectedInNonDev(t *testing.T) {
	for _, env := range []string{"", "staging", "mainnet", "production", "prod", "custom-label"} {
		cfg := Config{DBEngine: "milvus", Environment: env}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("milvus accepted in non-dev env %q", env)
		}
	}
}

func TestValidate_PgvectorRejectedInNonDev(t *testing.T) {
	for _, engine := range []string{"pgsql", "postgres", "postgresql", "pgvector"} {
		cfg := Config{DBEngine: engine, Environment: "mainnet"}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s accepted in mainnet", engine)
		}
	}
}

func TestValidate_ExperimentalBackendsAllowedInDevTest(t *testing.T) {
	for _, env := range []string{"dev", "test"} {
		for _, engine := range []string{"milvus", "pgsql", "postgres", "postgresql", "pgvector"} {
			cfg := Config{DBEngine: engine, Environment: env}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("%s in %q rejected: %v", engine, env, err)
			}
		}
	}
}

func TestValidate_AllowlistPermitsMilvusInNonDev(t *testing.T) {
	cfg := Config{DBEngine: "milvus", Environment: "staging", AllowUnsupportedDBEngine: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("milvus with allowlist rejected: %v", err)
	}
}

func TestValidate_AllowlistPermitsPgvectorInNonDev(t *testing.T) {
	cfg := Config{DBEngine: "pgvector", Environment: "mainnet", AllowUnsupportedDBEngine: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("pgvector with allowlist rejected: %v", err)
	}
}

func TestStoragePrefixValidation(t *testing.T) {
	for prefix, ok := range map[string]bool{"": true, "m0": true, "node7-m3": true, "M0": false, "-m0": false, "m--0": false, "m0-": false, "a b": false, "abcdefghijklmnopqrstuvwxyz0123456": false} {
		err := Config{DBEngine: "qdrant", StoragePrefix: prefix}.Validate()
		if (err == nil) != ok {
			t.Errorf("STORAGE_PREFIX %q: valid=%v, err=%v", prefix, ok, err)
		}
	}
}
