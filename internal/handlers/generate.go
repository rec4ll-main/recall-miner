package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"recall-miner/internal/httpx"
	"recall-miner/internal/registry"
)

// chutesBackend is the single selection rule for generation: the configured
// backend name with generation_connector=chutes. No first-healthy fallback.
func (h *Handlers) chutesBackend() (registry.ServiceInfo, bool) {
	for _, b := range h.registry.ByCapabilityHealthy("generate") {
		if b.Name == h.cfg.GenerationBackendName && b.Metadata["generation_connector"] == "chutes" {
			return b, true
		}
	}
	return registry.ServiceInfo{}, false
}

// chutesGenerationReady reports whether generate can be advertised: the selected
// backend exists and advertises exactly the configured model.
func (h *Handlers) chutesGenerationReady() (registry.ServiceInfo, bool) {
	backend, ok := h.chutesBackend()
	if !ok || h.cfg.GenerationModel == "" || !registry.Contains(backend.Models, h.cfg.GenerationModel) {
		return registry.ServiceInfo{}, false
	}
	return backend, true
}

// sidecarBudget is remaining - min(margin, remaining/4): the configured margin
// (default 2s, so 30s -> 28s) shrinks only for deadlines too short to hold it.
// It refuses, before any send, a non-positive margin or a budget under 1ms.
func sidecarBudget(remaining, margin time.Duration) (time.Duration, bool) {
	if margin <= 0 {
		return 0, false
	}
	if margin > remaining/4 {
		margin = remaining / 4
	}
	budget := remaining - margin
	return budget, budget >= time.Millisecond
}

func (h *Handlers) generate(ctx context.Context, req generateRequest) (map[string]any, error) {
	backend, ok := h.chutesBackend()
	if !ok {
		return nil, httpx.StatusError(http.StatusServiceUnavailable, "configured Chutes generation backend unavailable")
	}
	model := req.Model
	if model == "" {
		model = h.cfg.GenerationModel
	}
	if model == "" || model != h.cfg.GenerationModel || strings.ContainsAny(model, ",:?") {
		return nil, httpx.StatusError(http.StatusBadRequest, "unsupported generation model")
	}
	matched := false
	for _, advertised := range backend.Models {
		if advertised == model {
			matched = true
		}
	}
	if !matched {
		return nil, httpx.StatusError(http.StatusServiceUnavailable, "configured Chutes model unavailable")
	}
	payload := map[string]any{
		"query":    req.Query,
		"contexts": req.Contexts,
		"model":    model,
	}
	if req.scopedRequestKey != "" {
		payload["request_key"] = req.scopedRequestKey
	}
	// Deadline ordering: the sidecar's provider budget is this request's remaining
	// deadline minus a margin for the response hop and signing, so the sidecar
	// reaches its own terminal (and closes the provider connection) before Go
	// cancels. The sidecar only ever shortens its own GEN_OPENAI_TIMEOUT_SECONDS.
	if deadline, ok := ctx.Deadline(); ok {
		budget, ok := sidecarBudget(time.Until(deadline), h.cfg.GenerationSidecarMargin)
		if !ok {
			return nil, httpx.StatusError(http.StatusServiceUnavailable, "generation deadline budget insufficient")
		}
		payload["deadline_ms"] = budget.Milliseconds()
	}
	if req.ProbeContract != "" {
		if req.ProbeContract != "computed-probe-v1" {
			return nil, httpx.StatusError(http.StatusBadRequest, "unsupported probe contract")
		}
		payload["probe_contract"] = req.ProbeContract
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	var out map[string]any
	if err := h.apiClient.RequestJSON(ctx, http.MethodPost, backend.URL+"/generate", payload, &out); err != nil {
		return nil, inferenceDependencyError(err)
	}
	if out["model"] != model {
		return nil, httpx.StatusError(http.StatusBadGateway, "generation response model mismatch")
	}
	return out, nil
}

var generationRequestKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func scopeGenerationRequestKey(sender, key string) (string, error) {
	if sender == "" || !generationRequestKeyPattern.MatchString(key) {
		return "", fmt.Errorf("invalid request key")
	}
	// Domain-separated tuple prevents delimiter ambiguity and avoids SS58-length assumptions.
	body, _ := json.Marshal([]string{"recall-generation-request-v1", sender, key})
	sum := sha256.Sum256(body)
	return "g1:" + hex.EncodeToString(sum[:]), nil
}
