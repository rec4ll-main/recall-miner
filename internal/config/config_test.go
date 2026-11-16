package config

import (
	"os"
	"testing"
	"time"
)

func TestEnvBool_Unset_False(t *testing.T) {
	os.Unsetenv("GENERATION_ENABLED")
	cfg := FromEnv()
	if cfg.GenerationEnabled {
		t.Fatal("expected GenerationEnabled=false when unset")
	}
}

func TestEnvBool_TrueValue_True(t *testing.T) {
	os.Setenv("GENERATION_ENABLED", "true")
	defer os.Unsetenv("GENERATION_ENABLED")
	cfg := FromEnv()
	if !cfg.GenerationEnabled {
		t.Fatal("expected GenerationEnabled=true")
	}
}

func TestEnvBool_OneValue_True(t *testing.T) {
	os.Setenv("GENERATION_ENABLED", "1")
	defer os.Unsetenv("GENERATION_ENABLED")
	cfg := FromEnv()
	if !cfg.GenerationEnabled {
		t.Fatal("expected GenerationEnabled=true for value '1'")
	}
}

func TestEnvBool_Garbage_Fallback(t *testing.T) {
	os.Setenv("GENERATION_ENABLED", "garbage")
	defer os.Unsetenv("GENERATION_ENABLED")
	cfg := FromEnv()
	if cfg.GenerationEnabled {
		t.Fatal("expected GenerationEnabled=false when value is garbage")
	}
}

func TestGenerationBackendTimeout_Unset_Default(t *testing.T) {
	os.Unsetenv("GENERATION_BACKEND_TIMEOUT_SECONDS")
	cfg := FromEnv()
	if cfg.GenerationBackendTimeout != 85*time.Second {
		t.Fatalf("expected 85s default, got %v", cfg.GenerationBackendTimeout)
	}
}

func TestGenerationBackendTimeout_Set_Value(t *testing.T) {
	os.Setenv("GENERATION_BACKEND_TIMEOUT_SECONDS", "60")
	defer os.Unsetenv("GENERATION_BACKEND_TIMEOUT_SECONDS")
	cfg := FromEnv()
	if cfg.GenerationBackendTimeout != 60*time.Second {
		t.Fatalf("expected 60s, got %v", cfg.GenerationBackendTimeout)
	}
}

func TestReplayCacheMaxEntries_ClampedWhenNonPositive(t *testing.T) {
	os.Setenv("REPLAY_CACHE_MAX_ENTRIES", "0")
	defer os.Unsetenv("REPLAY_CACHE_MAX_ENTRIES")
	if got := FromEnv().ReplayCacheMaxEntries; got != 10000 {
		t.Fatalf("expected clamped default 10000, got %d", got)
	}
	os.Setenv("REPLAY_CACHE_MAX_ENTRIES", "-5")
	if got := FromEnv().ReplayCacheMaxEntries; got != 10000 {
		t.Fatalf("expected clamped default for negative, got %d", got)
	}
	os.Setenv("REPLAY_CACHE_MAX_ENTRIES", "250")
	if got := FromEnv().ReplayCacheMaxEntries; got != 250 {
		t.Fatalf("expected 250, got %d", got)
	}
}

func TestServerTimeouts_Defaults(t *testing.T) {
	os.Unsetenv("SERVER_READ_TIMEOUT_SECONDS")
	os.Unsetenv("SERVER_WRITE_TIMEOUT_SECONDS")
	os.Unsetenv("SERVER_IDLE_TIMEOUT_SECONDS")
	cfg := FromEnv()
	if cfg.ServerReadTimeoutSeconds != 30 {
		t.Fatalf("expected read timeout 30, got %d", cfg.ServerReadTimeoutSeconds)
	}
	if cfg.ServerWriteTimeoutSeconds != 120 {
		t.Fatalf("expected write timeout 120, got %d", cfg.ServerWriteTimeoutSeconds)
	}
	if cfg.ServerIdleTimeoutSeconds != 120 {
		t.Fatalf("expected idle timeout 120, got %d", cfg.ServerIdleTimeoutSeconds)
	}
}

func TestMaxBodyBytes_Default(t *testing.T) {
	os.Unsetenv("MAX_BODY_BYTES")
	if got := FromEnv().MaxBodyBytes; got != 1048576 {
		t.Fatalf("expected default 1048576, got %d", got)
	}
	os.Setenv("MAX_BODY_BYTES", "4096")
	defer os.Unsetenv("MAX_BODY_BYTES")
	if got := FromEnv().MaxBodyBytes; got != 4096 {
		t.Fatalf("expected 4096, got %d", got)
	}
}

func TestTLSSettings_Defaults(t *testing.T) {
	os.Unsetenv("TLS_ENABLED")
	os.Unsetenv("TLS_CERT_FILE")
	os.Unsetenv("TLS_KEY_FILE")
	os.Unsetenv("TLS_AUTO_GENERATE")
	os.Unsetenv("MINER_REGISTRATION_INTERVAL_SECONDS")
	cfg := FromEnv()
	if cfg.TLSEnabled {
		t.Fatal("expected TLSEnabled=false by default")
	}
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		t.Fatalf("expected empty cert/key files by default, got %q/%q", cfg.TLSCertFile, cfg.TLSKeyFile)
	}
	if !cfg.TLSAutoGenerate {
		t.Fatal("expected TLSAutoGenerate=true by default")
	}
	if cfg.MinerRegistrationInterval != 0 {
		t.Fatalf("expected MinerRegistrationInterval=0 by default, got %v", cfg.MinerRegistrationInterval)
	}
}

func TestTLSEnv_TrueValues(t *testing.T) {
	os.Setenv("TLS_ENABLED", "true")
	os.Setenv("TLS_CERT_FILE", "/certs/cert.pem")
	os.Setenv("TLS_KEY_FILE", "/certs/key.pem")
	os.Setenv("TLS_AUTO_GENERATE", "false")
	os.Setenv("MINER_REGISTRATION_INTERVAL_SECONDS", "600")
	defer func() {
		os.Unsetenv("TLS_ENABLED")
		os.Unsetenv("TLS_CERT_FILE")
		os.Unsetenv("TLS_KEY_FILE")
		os.Unsetenv("TLS_AUTO_GENERATE")
		os.Unsetenv("MINER_REGISTRATION_INTERVAL_SECONDS")
	}()
	cfg := FromEnv()
	if !cfg.TLSEnabled {
		t.Fatal("expected TLSEnabled=true")
	}
	if cfg.TLSCertFile != "/certs/cert.pem" || cfg.TLSKeyFile != "/certs/key.pem" {
		t.Fatalf("unexpected cert/key: %q/%q", cfg.TLSCertFile, cfg.TLSKeyFile)
	}
	if cfg.TLSAutoGenerate {
		t.Fatal("expected TLSAutoGenerate=false")
	}
	if cfg.MinerRegistrationInterval != 600*time.Second {
		t.Fatalf("expected 600s, got %v", cfg.MinerRegistrationInterval)
	}
}

func TestFromEnv_ApplicationProberAbsentHasNoIdentity(t *testing.T) {
	t.Cleanup(func() { os.Unsetenv("API_SIGNING_HOTKEY_SS58"); os.Unsetenv("VALIDATOR_HOTKEY_SS58") })
	os.Unsetenv("API_SIGNING_HOTKEY_SS58")
	os.Unsetenv("VALIDATOR_HOTKEY_SS58")

	cfg := FromEnv()
	if cfg.applicationProberHotkeySet {
		t.Fatal("absent API_SIGNING_HOTKEY_SS58 should not be marked set")
	}
	if err := ValidateApplicationProberHotkey(cfg); err != nil {
		t.Fatalf("absent application key passes field validation (the app's allowlist check rejects it): %v", err)
	}
	if got := cfg.ApplicationIdentity(); got != "" {
		t.Fatalf("identity = %q, want empty (no fallback)", got)
	}
}

func TestFromEnv_RemovedValidatorHotkeyFailsStartup(t *testing.T) {
	for _, value := range []string{"", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"} {
		t.Setenv("VALIDATOR_HOTKEY_SS58", value)
		t.Setenv("API_SIGNING_HOTKEY_SS58", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
		err := ValidateApplicationProberHotkey(FromEnv())
		want := "VALIDATOR_HOTKEY_SS58 was removed: set API_SIGNING_HOTKEY_SS58 (the Recall API signing key) and delete VALIDATOR_HOTKEY_SS58"
		if err == nil || err.Error() != want {
			t.Fatalf("VALIDATOR_HOTKEY_SS58=%q: err = %v, want %q", value, err, want)
		}
	}
}

func TestFromEnv_ApplicationProberEmptyFailsClosed(t *testing.T) {
	t.Setenv("API_SIGNING_HOTKEY_SS58", "")
	t.Cleanup(func() { os.Unsetenv("API_SIGNING_HOTKEY_SS58") })

	cfg := FromEnv()
	if !cfg.applicationProberHotkeySet {
		t.Fatal("empty API_SIGNING_HOTKEY_SS58 should be marked set")
	}
	if err := ValidateApplicationProberHotkey(cfg); err == nil {
		t.Fatal("expected present-but-empty application key to fail validation")
	}
}

func TestFromEnv_ApplicationProberWhitespaceFailsClosed(t *testing.T) {
	t.Setenv("API_SIGNING_HOTKEY_SS58", "   ")
	t.Cleanup(func() { os.Unsetenv("API_SIGNING_HOTKEY_SS58") })

	cfg := FromEnv()
	if err := ValidateApplicationProberHotkey(cfg); err == nil {
		t.Fatal("expected whitespace application key to fail validation")
	}
}

func TestSS58Format_DefaultAndBounds(t *testing.T) {
	cases := []struct {
		value string
		want  uint16
		ok    bool
	}{
		{"", 42, true},
		{"0", 0, true},
		{"42", 42, true},
		{"16383", 16383, true},
		{"16384", 0, false},
		{"65578", 0, false}, // would wrap to 42 in uint16
		{"-1", 0, false},
		{"forty-two", 0, false},
	}
	for _, tc := range cases {
		t.Setenv("SS58_FORMAT", tc.value)
		cfg := FromEnv()
		err := ValidateSS58Format(cfg)
		if tc.ok {
			if err != nil || cfg.SS58Format != tc.want {
				t.Fatalf("SS58_FORMAT=%q: got %d, %v; want %d, nil", tc.value, cfg.SS58Format, err, tc.want)
			}
		} else if err == nil {
			t.Fatalf("SS58_FORMAT=%q: expected a startup error", tc.value)
		}
	}
}
