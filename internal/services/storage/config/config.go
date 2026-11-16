package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr           string
	PublicURL            string
	DBEngine             string
	MilvusURI            string
	MilvusToken          string
	QdrantURL            string
	QdrantAPIKey         string
	PgSQLDSN             string
	MinerAPIURL          string
	MinerTLSCAFile        string
	InternalAPIKey       string
	ServiceName          string
	RegisterOnStart      bool
	RegistrationInterval time.Duration
	LogLevelName         string
	// Environment mirrors the miner's ENVIRONMENT semantics: only the literal
	// values "dev" and "test" are exempt from the Qdrant-only policy. Unset
	// (empty) is treated as non-dev (armed).
	Environment string
	// AllowUnsupportedDBEngine is an explicit escape hatch permitting Milvus
	// and pgvector in non-dev environments once a backend is certified. It
	// must remain false for mainnet v1.
	AllowUnsupportedDBEngine bool
	// StoragePrefix (env STORAGE_PREFIX) keeps this miner's databases apart from other miners
	// that share the same Qdrant. Each such miner needs a different value; a miner with its own
	// Qdrant can leave it empty. Lowercase letters, digits and hyphens, up to 32 characters.
	StoragePrefix string
}

func FromEnv() Config {
	return Config{
		ListenAddr:               env("LISTEN_ADDR", ":9000"),
		PublicURL:                env("SERVICE_PUBLIC_URL", "http://db-service:9000"),
		DBEngine:                 strings.ToLower(env("DB_ENGINE", "qdrant")),
		MilvusURI:                env("MILVUS_URI", "http://milvus:19530"),
		MilvusToken:              env("MILVUS_TOKEN", ""),
		QdrantURL:                env("QDRANT_URL", "http://qdrant:6333"),
		QdrantAPIKey:             env("QDRANT_API_KEY", ""),
		PgSQLDSN:                 env("PGSQL_DSN", env("DATABASE_URL", "postgresql://recall:recall@pgsql:5432/recall")),
		MinerAPIURL:              strings.TrimRight(env("MINER_API_URL", "http://miner-api:8001"), "/"),
		MinerTLSCAFile:            env("MINER_TLS_CA_FILE", ""),
		InternalAPIKey:           env("INTERNAL_API_KEY", ""),
		ServiceName:              env("SERVICE_NAME", "db-go"),
		StoragePrefix:            strings.TrimSpace(env("STORAGE_PREFIX", "")),
		RegisterOnStart:          envBool("REGISTER_ON_START", true),
		RegistrationInterval:     time.Duration(envInt("REGISTRATION_INTERVAL_SECONDS", 1)) * time.Second,
		LogLevelName:             env("LOG_LEVEL", "info"),
		Environment:              strings.ToLower(env("ENVIRONMENT", "")),
		AllowUnsupportedDBEngine: envBool("ALLOW_UNSUPPORTED_DB_ENGINE", false),
	}
}

// Validate returns an error when cfg.DBEngine cannot be used in the
// configured environment. Mainnet v1 is Qdrant-only: an unknown engine is
// always rejected (never a silent Milvus fallback), and the experimental
// Milvus/pgvector backends are permitted only in dev/test or behind the
// explicit ALLOW_UNSUPPORTED_DB_ENGINE allowlist flag.
var storagePrefixPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){0,31}$`)

func (c Config) Validate() error {
	if c.StoragePrefix != "" && (len(c.StoragePrefix) > 32 || !storagePrefixPattern.MatchString(c.StoragePrefix)) {
		return fmt.Errorf("STORAGE_PREFIX must be lowercase letters, digits and hyphens (1-32 characters, starting with a letter or digit, no \"--\")")
	}
	switch strings.ToLower(c.DBEngine) {
	case "qdrant":
		return nil
	case "milvus", "pgsql", "postgres", "postgresql", "pgvector":
		if c.isDev() || c.AllowUnsupportedDBEngine {
			return nil
		}
		return fmt.Errorf("DB_ENGINE %q is not supported in environment %q: mainnet v1 is Qdrant-only; set ALLOW_UNSUPPORTED_DB_ENGINE=true only for a certified experimental backend", strings.ToLower(c.DBEngine), c.Environment)
	default:
		return fmt.Errorf("unknown DB_ENGINE %q: supported engines are qdrant (and, in dev/test or with ALLOW_UNSUPPORTED_DB_ENGINE, milvus or pgsql)", strings.ToLower(c.DBEngine))
	}
}

func (c Config) isDev() bool {
	env := strings.ToLower(c.Environment)
	return env == "dev" || env == "test"
}

func (c Config) LogLevel() slog.Level {
	switch strings.ToLower(c.LogLevelName) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
