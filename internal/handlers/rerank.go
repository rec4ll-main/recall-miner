package handlers

import (
	"context"
	"net/http"
	"regexp"

	"recall-miner/internal/httpx"
	rank "recall-miner/internal/rerank"
)

func (h *Handlers) rerank(ctx context.Context, req rerankRequest) (map[string]any, error) {
	if len(req.Texts) > 0 {
		return h.rerankTexts(ctx, req)
	}
	if len(req.Candidates) > 0 {
		embedded, err := h.embedText(ctx, req.Query, "")
		if err != nil {
			return nil, err
		}
		return rank.CosineResult(embedded.Vector, req.Candidates, req.CandidateIDs), nil
	}
	return map[string]any{"scores": []float64{}, "ranked_ids": req.CandidateIDs, "model": ""}, nil
}

func (h *Handlers) rerankTexts(ctx context.Context, req rerankRequest) (map[string]any, error) {
	backends := h.registry.ByCapabilityHealthy("rerank")
	if len(backends) == 0 {
		// Fail closed: a miner without a healthy rerank
		// sidecar must not answer 200 with zero scores, because the validator would
		// otherwise record a served rerank for work that was never done. Never
		// contact a degraded sidecar either.
		return nil, httpx.StatusError(http.StatusServiceUnavailable, "no healthy rerank backend")
	}
	backend := backends[0]
	var response []struct {
		Index    *int     `json:"index"`
		Score    *float64 `json:"score"`
		Model    string   `json:"model"`
		Revision string   `json:"model_revision"`
	}
	if err := h.apiClient.RequestJSON(ctx, http.MethodPost, backend.URL+"/rerank", map[string]any{"query": req.Query, "texts": req.Texts}, &response); err != nil {
		return nil, inferenceDependencyError(err)
	}
	if len(response) != len(req.Texts) {
		return nil, httpx.StatusError(http.StatusBadGateway, "rerank backend returned incomplete ranking")
	}
	scores := make([]float64, len(req.Texts))
	seen := make([]bool, len(scores))
	model, revision := response[0].Model, response[0].Revision
	if (model != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(model)) || (revision != "" && (model == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(revision))) {
		return nil, httpx.StatusError(http.StatusBadGateway, "rerank backend returned invalid identity")
	}
	for _, item := range response {
		if item.Model != model || item.Revision != revision {
			return nil, httpx.StatusError(http.StatusBadGateway, "rerank backend returned inconsistent identity")
		}
		if item.Index == nil || item.Score == nil || *item.Index < 0 || *item.Index >= len(scores) || seen[*item.Index] {
			return nil, httpx.StatusError(http.StatusBadGateway, "rerank backend returned invalid ranking")
		}
		seen[*item.Index] = true
		scores[*item.Index] = *item.Score
	}

	return map[string]any{"scores": scores, "ranked_ids": rank.RankedIDs(scores, req.CandidateIDs), "model": model, "model_revision": revision}, nil
}
