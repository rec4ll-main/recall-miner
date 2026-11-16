package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"recall-miner/internal/auth"
	"recall-miner/internal/config"
	"recall-miner/internal/ratelimit"
)

func New(_ context.Context) (*server, error) {
	cfg := config.FromEnv()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := validateDeploymentSecrets(cfg); err != nil {
		return nil, err
	}
	if err := validateInternalAPIKey(cfg); err != nil {
		return nil, err
	}
	if err := config.ValidateApplicationProberHotkey(cfg); err != nil {
		return nil, err
	}
	if err := config.ValidateSS58Format(cfg); err != nil {
		return nil, err
	}
	if err := validateOverloadConfig(cfg); err != nil {
		return nil, err
	}

	identity := cfg.ApplicationIdentity()
	if identity == "" && !cfg.ValidatorAllowlistDevEmpty {
		return nil, fmt.Errorf("validator allowlist is empty in production mode; set API_SIGNING_HOTKEY_SS58 (the Recall API signing key), or explicitly set VALIDATOR_ALLOWLIST_DEV_EMPTY=true for development only")
	}
	if identity == "" && cfg.ValidatorAllowlistDevEmpty {
		logger.Warn("VALIDATOR_ALLOWLIST_DEV_EMPTY=true: accepting ANY Epistula signer as validator - development mode only, do not run this in production")
	}

	keypair, err := auth.KeyringFromSecret(cfg.HotkeySecret, cfg.SS58Format)
	app := newServer(cfg, logger)
	if err == nil {
		if app.cfg.MinerHotkey == "" {
			app.cfg.MinerHotkey = keypair.Address
		}
		app.keypair = &keypair
		auth.LogHotkey(logger, keypair)
	} else {
		logger.Warn("validator registration disabled", "error", err)
	}

	if err := app.initTLS(app.cfg.MinerHotkey); err != nil {
		return nil, err
	}

	return app, nil
}

// validateDeploymentSecrets fails fast outside dev/test when the hotkey
// secret is empty or a known placeholder. The secret
// value is never logged.
func validateDeploymentSecrets(cfg config.Config) error {
	if cfg.Environment == "dev" || cfg.Environment == "test" {
		return nil
	}
	if cfg.HotkeySecret == "" || isPlaceholder(cfg.HotkeySecret) {
		return fmt.Errorf("HOTKEY_URI/HOTKEY_PHRASE must be a real secret in environment %q (placeholder or empty detected)", cfg.Environment)
	}
	return nil
}

func isPlaceholder(value string) bool {
	if value == "" {
		return true
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"change-me", "changeme", "local-miner", "local-dev", "placeholder", "//alice", "//bob", "//recallminer"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// validateInternalAPIKey fails fast outside dev/test when the internal
// sidecar API key is empty or a known placeholder. The internal key
// authenticates sidecar control-plane routes (service registration and model
// control) on the same public listener, so a carried-over development
// placeholder must never reach a public deployment. The key value is never
// logged.
func validateInternalAPIKey(cfg config.Config) error {
	if cfg.Environment == "dev" || cfg.Environment == "test" {
		return nil
	}
	if isInternalKeyPlaceholder(cfg.InternalAPIKey) {
		return fmt.Errorf("INTERNAL_API_KEY must be a real secret in environment %q (placeholder or empty detected)", cfg.Environment)
	}
	return nil
}

func isInternalKeyPlaceholder(value string) bool {
	if value == "" {
		return true
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"local-miner-internal-key", "change-me", "changeme", "placeholder", "local-dev", "local-api"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (s *server) Routes() http.Handler { return s.routes() }

func (s *server) ListenAddr() string { return s.cfg.ListenAddr }

func (s *server) ServerReadTimeoutSeconds() int  { return s.cfg.ServerReadTimeoutSeconds }
func (s *server) ServerWriteTimeoutSeconds() int { return s.cfg.ServerWriteTimeoutSeconds }
func (s *server) ServerIdleTimeoutSeconds() int  { return s.cfg.ServerIdleTimeoutSeconds }

func (s *server) Log() *slog.Logger { return s.log }

func (s *server) Close() { s.heartbeats.Close() }

func (s *server) RunValidatorLoop(ctx context.Context) {
	if s.keypair == nil {
		return
	}
	s.log.Info("validator loop enabled", "hotkey", s.keypair.Address)
	s.runValidatorLoop(ctx, *s.keypair)
}

// validateOverloadConfig rejects an unparseable trusted-proxy list at startup, so a
// typo can neither trust an unintended peer nor silently disable proxy handling.
func validateOverloadConfig(cfg config.Config) error {
	if _, err := ratelimit.ParseTrustedProxies(cfg.RateLimitTrustedProxies); err != nil {
		return fmt.Errorf("RATE_LIMIT_TRUSTED_PROXIES: %w", err)
	}
	return nil
}
