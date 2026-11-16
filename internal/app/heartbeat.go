package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/subtrahend-labs/gobt/boilerplate"
)

func (s *server) runValidatorLoop(ctx context.Context, keypair signature.KeyringPair) {
	if s.cfg.RecallAPIURL == "" {
		s.log.Warn("Recall API registration disabled: API base URL is empty")
		return
	}
	for {
		if err := s.registerValidator(ctx, keypair); err != nil {
			s.log.Warn("Recall API registration failed", "error", err)
			if !sleep(ctx, 10*time.Second) {
				return
			}
			continue
		}
		break
	}

	ticker := time.NewTicker(s.cfg.HeartbeatInterval)
	defer ticker.Stop()
	var regTicker *time.Ticker
	var regC <-chan time.Time
	if s.cfg.MinerRegistrationInterval > 0 {
		regTicker = time.NewTicker(s.cfg.MinerRegistrationInterval)
		defer regTicker.Stop()
		regC = regTicker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.heartbeatValidator(ctx, keypair); err != nil {
				s.log.Warn("Recall API heartbeat failed", "error", err)
			}
		case <-regC:
			if err := s.registerValidator(ctx, keypair); err != nil {
				s.log.Warn("Recall API re-registration failed", "error", err)
			}
		}
	}
}

func (s *server) registerValidator(ctx context.Context, keypair signature.KeyringPair) error {
	payload := map[string]any{
		"capabilities":     s.handlers.AvailableCapabilities(),
		"embedding_models": nonNilStrings(s.cfg.EmbeddingModels),
		"endpoint":         s.cfg.MinerAPIURL,
		"capacity": map[string]any{
			"max_dbs":     100,
			"max_vectors": 1000000,
			"storage_gb":  500,
		},
	}
	if pin := s.SPKIPin(); pin != "" {
		payload["tls_spki_sha256"] = pin
	}
	if err := s.signedPost(ctx, keypair, validatorAPIURL(s.cfg.RecallAPIURL, "/api/miner/register"), payload); err != nil {
		return err
	}
	s.log.Info("registered with Recall API", "recall_api_url", s.cfg.RecallAPIURL, "hotkey", keypair.Address)
	return nil
}

func (s *server) heartbeatValidator(ctx context.Context, keypair signature.KeyringPair) error {
	payload := map[string]any{
		"resource_stats":   s.handlers.ResourceStats(),
		"capabilities":     s.handlers.AvailableCapabilities(),
		"embedding_models": nonNilStrings(s.cfg.EmbeddingModels),
	}
	return s.signedPost(ctx, keypair, validatorAPIURL(s.cfg.RecallAPIURL, "/api/miner/heartbeat"), payload)
}

func validatorAPIURL(baseURL, path string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	path = "/" + strings.TrimLeft(path, "/")
	if strings.HasSuffix(baseURL, "/api") && strings.HasPrefix(path, "/api/") {
		path = strings.TrimPrefix(path, "/api")
	}
	return baseURL + path
}

func (s *server) signedPost(ctx context.Context, keypair signature.KeyringPair, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	headers, err := boilerplate.GetEpistulaHeaders(keypair, s.validatorSignedFor(keypair), body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Del("Connection")

	response, err := s.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%s returned %d: %s", url, response.StatusCode, string(body))
	}
	return nil
}

func (s *server) validatorSignedFor(keypair signature.KeyringPair) string {
	if identity := s.cfg.ApplicationIdentity(); identity != "" {
		return identity
	}
	return keypair.Address
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
