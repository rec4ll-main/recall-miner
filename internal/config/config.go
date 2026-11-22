package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vedhavyas/go-subkey/v2"
)

type Config struct {
	ListenAddr                 string
	RecallAPIURL               string // Recall API base (RECALL_API_URL)
	MinerAPIURL                string
	InternalAPIKey             string
	MetricsStorage             string
	MetricsDuckDBPath          string
	ClickHouseURL              string
	ClickHouseDB               string
	MinerHotkey                string
	HotkeySecret               string
	SS58Format                 uint16
	ss58FormatErr              error
	ApplicationProberHotkey    string // env API_SIGNING_HOTKEY_SS58 (renamed from APPLICATION_PROBER_HOTKEY_SS58): the Recall API signing key, the only trusted prober
	applicationProberHotkeySet bool
	legacyProberHotkeySet      bool // the removed name APPLICATION_PROBER_HOTKEY_SS58 is still in the environment
	legacyValidatorHotkeySet   bool // the removed setting VALIDATOR_HOTKEY_SS58 is still in the environment
	HeartbeatInterval          time.Duration
	HTTPTimeout                time.Duration
	GenerationEnabled          bool   // env GENERATION_ENABLED, default false
	GenerationBackendName      string // explicit Chutes sidecar; no first-healthy fallback
	GenerationModel            string
	GenerationBackendTimeout   time.Duration // env GENERATION_BACKEND_TIMEOUT_SECONDS, default 58s (fits the API's 60 s scoring window)
	GenerationSidecarMargin    time.Duration // env GENERATION_SIDECAR_DEADLINE_MARGIN_MS, default 2000ms; sidecar budget = remaining - margin
	ValidatorAllowlistDevEmpty bool          // env VALIDATOR_ALLOWLIST_DEV_EMPTY, default false
	EpistulaEnvelopeRequired   bool          // env EPISTULA_ENVELOPE_REQUIRED, default true
	ReplayCacheMaxEntries      int           // env REPLAY_CACHE_MAX_ENTRIES, default 10000
	ResponseSigningEnabled     bool          // env RESPONSE_SIGNING_ENABLED, default true
	Environment                string        // env ENVIRONMENT; unset is treated as non-dev (armed)
	MaxBodyBytes               int64         // env MAX_BODY_BYTES, default 1048576 (1 MiB)
	ServerReadTimeoutSeconds   int           // env SERVER_READ_TIMEOUT_SECONDS, default 30
	ServerWriteTimeoutSeconds  int           // env SERVER_WRITE_TIMEOUT_SECONDS, default 120
	ServerIdleTimeoutSeconds   int           // env SERVER_IDLE_TIMEOUT_SECONDS, default 120
	TLSEnabled                 bool          // env TLS_ENABLED, default false
	TLSCertFile                string        // env TLS_CERT_FILE
	TLSKeyFile                 string        // env TLS_KEY_FILE
	TLSAutoGenerate            bool          // env TLS_AUTO_GENERATE, default true
	TLSExtraSANHosts           []string      // env TLS_SAN_HOSTS (comma-separated DNS/IP SANs)
	MinerRegistrationInterval  time.Duration // env MINER_REGISTRATION_INTERVAL_SECONDS, default 0
	// Advertise / external IP
	ExternalIP                string // env MINER_EXTERNAL_IP
	ExternalHost              string // env MINER_EXTERNAL_HOST
	ExternalPort              int    // env MINER_EXTERNAL_PORT
	InternalPort              int    // env MINER_INTERNAL_PORT or MINER_HOST_PORT
	AdvertiseMode             string // env MINER_ADVERTISE_MODE: off|auto (default off)
	AdvertiseSource           string // env MINER_ADVERTISE_SOURCE: E1|E2|S1
	AdvertiseRequirePublic    bool
	AdvertiseRequirePublicSet bool // true if MINER_ADVERTISE_REQUIRE_PUBLIC was set in env
	AllowPrivateAdvertise     bool // env MINER_ALLOW_PRIVATE_ADVERTISE
	SidecarConfig             map[string]SidecarConfig
	// EmbeddingModels is MINER_EMBEDDING_MODELS: catalog ids this miner serves.
	EmbeddingModels []string

	// Overload controls (SECURITY_BACKLOG "Rate Limiting"); all in-process, 0 disables.
	RateLimitAuthFailuresPerMin int    // env RATE_LIMIT_AUTH_FAILURES_PER_MIN, default 60: 401/403 per client address before pre-auth refusal
	RateLimitFailureTableSize   int    // env RATE_LIMIT_AUTH_FAILURE_TABLE_SIZE, default 10000: client addresses tracked by the failure guard
	RateLimitFailureOverflow    int    // env RATE_LIMIT_AUTH_FAILURE_OVERFLOW_PER_MIN, default 600: shared pending/failure budget for untracked addresses while the table is full
	RateLimitTrustedProxies     string // env RATE_LIMIT_TRUSTED_PROXIES: CSV of CIDRs/IPs allowed to supply X-Forwarded-For; empty = trust no peer
	RateLimitValidatorPerMin    int    // env RATE_LIMIT_VALIDATOR_PER_MIN, default 3000: authenticated requests per signer hotkey
	RateLimitDBCreatePerMin     int    // env RATE_LIMIT_DB_CREATE_PER_MIN, default 20: POST /db/create per signer hotkey
	EmbedMaxInflight            int    // env EMBED_MAX_INFLIGHT, default 64: /embed_text and /embed_text/batch
	RerankMaxInflight           int    // env RERANK_MAX_INFLIGHT, default 32
	QueryMaxInflight            int    // env QUERY_MAX_INFLIGHT, default 128: /db/{id}/query
	StoreMaxInflight            int    // env STORE_MAX_INFLIGHT, default 128: /db/{id}/store and /db/{id}/restore
	GenerateMaxInflight         int    // env GENERATE_MAX_INFLIGHT, default 8
}

type SidecarConfig struct {
	Model    string         `json:"model"`
	Download map[string]any `json:"download,omitempty"`
}

func FromEnv() Config {
	// API_SIGNING_HOTKEY_SS58 is the same name and value the Recall API uses for its signing
	// key. It was renamed from APPLICATION_PROBER_HOTKEY_SS58, which is no longer read.
	appProberHotkey, appProberHotkeySet := os.LookupEnv("API_SIGNING_HOTKEY_SS58")
	_, legacyProberHotkeySet := os.LookupEnv("APPLICATION_PROBER_HOTKEY_SS58")
	// VALIDATOR_HOTKEY_SS58 was removed: only the Recall API talks to miners.
	_, legacyValidatorHotkeySet := os.LookupEnv("VALIDATOR_HOTKEY_SS58")
	minerAPIURL, minerAPIURLSet := os.LookupEnv("MINER_API_URL")
	minerAPI := ""
	if minerAPIURLSet {
		minerAPI = TrimRight(minerAPIURL)
	}
	requirePublic, requirePublicSet := lookupEnvBool("MINER_ADVERTISE_REQUIRE_PUBLIC", false)
	internalPort := EnvInt("MINER_INTERNAL_PORT", 0)
	if internalPort == 0 {
		internalPort = EnvInt("MINER_HOST_PORT", 0)
	}
	ss58Format, ss58FormatErr := ss58FormatFromEnv()
	return Config{
		ListenAddr:                 Env("LISTEN_ADDR", ":8001"),
		RecallAPIURL:               TrimRight(Env("RECALL_API_URL", "https://rec4ll.com/api")),
		MinerAPIURL:                minerAPI,
		InternalAPIKey:             Env("INTERNAL_API_KEY", ""),
		MetricsStorage:             strings.ToLower(Env("METRICS_STORAGE", "duckdb")),
		MetricsDuckDBPath:          Env("METRICS_DUCKDB_PATH", "/var/lib/recall-miner/heartbeats.duckdb"),
		ClickHouseURL:              TrimRight(Env("CLICKHOUSE_URL", "")),
		ClickHouseDB:               Env("CLICKHOUSE_DB", "recall_ts"),
		MinerHotkey:                FirstEnv("MINER_HOTKEY", "HOTKEY_SS58"),
		HotkeySecret:               FirstEnv("HOTKEY_URI", "HOTKEY_PHRASE", "MINER_HOTKEY_URI", "MINER_HOTKEY_PHRASE"),
		SS58Format:                 ss58Format,
		ss58FormatErr:              ss58FormatErr,
		ApplicationProberHotkey:    appProberHotkey,
		applicationProberHotkeySet: appProberHotkeySet,
		legacyProberHotkeySet:      legacyProberHotkeySet,
		legacyValidatorHotkeySet:   legacyValidatorHotkeySet,
		HeartbeatInterval:          time.Duration(EnvInt("HEARTBEAT_INTERVAL_SECONDS", 30)) * time.Second,
		HTTPTimeout:                time.Duration(EnvInt("HTTP_TIMEOUT_SECONDS", 120)) * time.Second,
		GenerationEnabled:          EnvBool("GENERATION_ENABLED", false),
		GenerationBackendName:      Env("CHUTES_BACKEND_NAME", "chutes"),
		GenerationModel:            Env("CHUTES_MODEL", "Qwen/Qwen3.8-27B-TEE"),
		GenerationBackendTimeout:   time.Duration(EnvInt("GENERATION_BACKEND_TIMEOUT_SECONDS", 58)) * time.Second,
		GenerationSidecarMargin:    time.Duration(EnvInt("GENERATION_SIDECAR_DEADLINE_MARGIN_MS", 2000)) * time.Millisecond,
		ValidatorAllowlistDevEmpty: EnvBool("VALIDATOR_ALLOWLIST_DEV_EMPTY", false),
		EpistulaEnvelopeRequired:   EnvBool("EPISTULA_ENVELOPE_REQUIRED", true),
		ReplayCacheMaxEntries:      clampPositive(EnvInt("REPLAY_CACHE_MAX_ENTRIES", 10000), 10000),
		ResponseSigningEnabled:     EnvBool("RESPONSE_SIGNING_ENABLED", true),
		Environment:                strings.ToLower(Env("ENVIRONMENT", "")),
		MaxBodyBytes:               int64(EnvInt("MAX_BODY_BYTES", 1048576)),
		ServerReadTimeoutSeconds:   EnvInt("SERVER_READ_TIMEOUT_SECONDS", 30),
		ServerWriteTimeoutSeconds:  EnvInt("SERVER_WRITE_TIMEOUT_SECONDS", 120),
		ServerIdleTimeoutSeconds:   EnvInt("SERVER_IDLE_TIMEOUT_SECONDS", 120),
		TLSEnabled:                 EnvBool("TLS_ENABLED", false),
		TLSCertFile:                Env("TLS_CERT_FILE", ""),
		TLSKeyFile:                 Env("TLS_KEY_FILE", ""),
		TLSAutoGenerate:            EnvBool("TLS_AUTO_GENERATE", true),
		TLSExtraSANHosts:           ParseEmbeddingModels(Env("TLS_SAN_HOSTS", "")),
		MinerRegistrationInterval:  time.Duration(EnvInt("MINER_REGISTRATION_INTERVAL_SECONDS", 0)) * time.Second,
		ExternalIP:                 Env("MINER_EXTERNAL_IP", ""),
		ExternalHost:               Env("MINER_EXTERNAL_HOST", ""),
		ExternalPort:               EnvInt("MINER_EXTERNAL_PORT", 0),
		InternalPort:               internalPort,
		AdvertiseMode:              strings.ToLower(Env("MINER_ADVERTISE_MODE", "off")),
		AdvertiseSource:            strings.ToUpper(Env("MINER_ADVERTISE_SOURCE", "E1")),
		AdvertiseRequirePublic:     requirePublic,
		AdvertiseRequirePublicSet:  requirePublicSet,
		AllowPrivateAdvertise:      EnvBool("MINER_ALLOW_PRIVATE_ADVERTISE", false),
		EmbeddingModels:            ParseEmbeddingModels(os.Getenv("MINER_EMBEDDING_MODELS")),
		SidecarConfig: map[string]SidecarConfig{
			"db": {Model: ""},
			"gpu": {
				Model: Env("VLLM_MODEL", "BAAI/bge-small-en-v1.5"),
				Download: map[string]any{
					"source": "huggingface",
					"repo":   Env("VLLM_MODEL", "BAAI/bge-small-en-v1.5"),
				},
			},
			"cpu": {
				Model: Env("HF_MODEL_ID", "BAAI/bge-small-en-v1.5"),
				Download: map[string]any{
					"source": "huggingface",
					"repo":   Env("HF_MODEL_ID", "BAAI/bge-small-en-v1.5"),
				},
			},
		},

		RateLimitAuthFailuresPerMin: EnvInt("RATE_LIMIT_AUTH_FAILURES_PER_MIN", 60),
		RateLimitFailureTableSize:   EnvInt("RATE_LIMIT_AUTH_FAILURE_TABLE_SIZE", 10000),
		RateLimitFailureOverflow:    EnvInt("RATE_LIMIT_AUTH_FAILURE_OVERFLOW_PER_MIN", 600),
		RateLimitTrustedProxies:     strings.TrimSpace(Env("RATE_LIMIT_TRUSTED_PROXIES", "")),
		RateLimitValidatorPerMin:    EnvInt("RATE_LIMIT_VALIDATOR_PER_MIN", 3000),
		RateLimitDBCreatePerMin:     EnvInt("RATE_LIMIT_DB_CREATE_PER_MIN", 20),
		EmbedMaxInflight:            EnvInt("EMBED_MAX_INFLIGHT", 64),
		RerankMaxInflight:           EnvInt("RERANK_MAX_INFLIGHT", 32),
		QueryMaxInflight:            EnvInt("QUERY_MAX_INFLIGHT", 128),
		StoreMaxInflight:            EnvInt("STORE_MAX_INFLIGHT", 128),
		GenerateMaxInflight:         EnvInt("GENERATE_MAX_INFLIGHT", 8),
	}
}

// ParseEmbeddingModels splits MINER_EMBEDDING_MODELS on commas.
// An empty value is an empty list, which declares no catalog models.
func ParseEmbeddingModels(value string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func FirstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func EnvInt(key string, fallback int) int {
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

// MaxSS58Format is the largest SS58 address-type prefix (14 bits).
const MaxSS58Format = 16383

// ss58FormatFromEnv parses SS58_FORMAT (default 42). A value that is not an integer
// in 0..MaxSS58Format is an error, reported at startup by ValidateSS58Format,
// instead of being truncated into uint16 or silently replaced by the default.
func ss58FormatFromEnv() (uint16, error) {
	value := os.Getenv("SS58_FORMAT")
	if value == "" {
		return 42, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 || parsed > MaxSS58Format {
		return 42, fmt.Errorf("SS58_FORMAT must be an integer in 0..%d", MaxSS58Format)
	}
	return uint16(parsed), nil
}

// ValidateSS58Format reports an SS58_FORMAT value outside 0..MaxSS58Format.
func ValidateSS58Format(c Config) error {
	return c.ss58FormatErr
}

func TrimRight(value string) string {
	return strings.TrimRight(value, "/")
}

// clampPositive returns the value when it is > 0, otherwise the fallback,
// so a capacity bound can never be silently disabled with a zero or
// negative env value.
func clampPositive(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func EnvBool(key string, fallback bool) bool {
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

// ApplicationIdentity returns the SS58 identity authorized to sign prober
// requests against this miner and to act as the heartbeat audience: the
// Recall API signing key (API_SIGNING_HOTKEY_SS58). Empty when unset; startup
// then fails unless VALIDATOR_ALLOWLIST_DEV_EMPTY=true.
func (c Config) ApplicationIdentity() string {
	return strings.TrimSpace(c.ApplicationProberHotkey)
}

// ValidateApplicationProberHotkey validates API_SIGNING_HOTKEY_SS58 when
// present. The removed names APPLICATION_PROBER_HOTKEY_SS58 and
// VALIDATOR_HOTKEY_SS58 fail startup whenever they are in the environment, so
// an old configuration is never silently reinterpreted. A present-but-empty
// value fails closed even under dev/test bypass. A present value must decode
// as a valid SS58 address. Absence is reported by the app's empty-allowlist
// check, which VALIDATOR_ALLOWLIST_DEV_EMPTY=true relaxes for development.
func ValidateApplicationProberHotkey(c Config) error {
	if c.legacyValidatorHotkeySet {
		return fmt.Errorf("VALIDATOR_HOTKEY_SS58 was removed: set API_SIGNING_HOTKEY_SS58 (the Recall API signing key) and delete VALIDATOR_HOTKEY_SS58")
	}
	if c.legacyProberHotkeySet {
		return fmt.Errorf("APPLICATION_PROBER_HOTKEY_SS58 was renamed to API_SIGNING_HOTKEY_SS58: rename the setting")
	}
	if !c.applicationProberHotkeySet {
		return nil
	}
	if strings.TrimSpace(c.ApplicationProberHotkey) == "" {
		return fmt.Errorf("API_SIGNING_HOTKEY_SS58 is set but empty: application-key probing requires a valid SS58 address")
	}
	if !validSS58(c.ApplicationProberHotkey) {
		return fmt.Errorf("API_SIGNING_HOTKEY_SS58 is not a valid SS58 address")
	}
	return nil
}

func validSS58(address string) (valid bool) {
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	_, pubKey, err := subkey.SS58Decode(address)
	if err != nil {
		return false
	}
	return len(pubKey) == 32
}

func lookupEnvBool(key string, fallback bool) (value bool, set bool) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback, false
	}
	if raw == "" {
		return fallback, true
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback, true
	}
	return parsed, true
}
