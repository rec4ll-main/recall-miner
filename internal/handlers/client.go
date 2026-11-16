package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"recall-miner/internal/config"
	"recall-miner/internal/httpx"
	"recall-miner/internal/registry"
)

func (h *Handlers) pickEmbedBackend(model string) (registry.ServiceInfo, error) {
	registered := h.registry.ByCapability("embedding")
	candidates := h.registry.ByCapabilityHealthy("embedding")
	if len(registered) > 0 && len(candidates) == 0 {
		return registry.ServiceInfo{}, httpx.StatusError(http.StatusServiceUnavailable, "no healthy embedding backend")
	}
	if len(candidates) == 0 {
		if model != "" {
			// No registered embedding backends can satisfy an explicit model (miner#2).
			return registry.ServiceInfo{}, httpx.StatusError(http.StatusUnprocessableEntity, "unsupported embedding model: no embedding backend registered")
		}
		return registry.ServiceInfo{Name: "gpu", URL: config.TrimRight(config.Env("VLLM_URL", "http://vllm:8000")), Models: []string{config.Env("VLLM_MODEL", "")}}, nil
	}
	if model != "" {
		for _, candidate := range candidates {
			if registry.Contains(candidate.Models, model) {
				return candidate, nil
			}
		}
		// Catalog ids are not the Hugging Face repo ids sidecars advertise.
		// Accept a catalog id only when this miner declared it and a backend
		// advertises that catalog id's upstream model.
		if upstream, ok := catalogEmbeddingUpstream(model); ok && declaredEmbeddingModel(h.cfg.EmbeddingModels, model) {
			for _, candidate := range candidates {
				if registry.Contains(candidate.Models, upstream) {
					return candidate, nil
				}
			}
		}
		// Explicit model was requested but no backend advertises it - do not
		// silently fall through to TEI/default (miner#2 / GitHub #2).
		return registry.ServiceInfo{}, httpx.StatusError(http.StatusUnprocessableEntity, "unsupported embedding model")
	}
	for _, candidate := range candidates {
		if registry.IsTEIStyle(candidate.Name) {
			return candidate, nil
		}
	}
	return candidates[0], nil
}

func (h *Handlers) embedText(ctx context.Context, text, model string) (embedResult, error) {
	backend, err := h.pickEmbedBackend(model)
	if err != nil {
		return embedResult{}, err
	}
	if registry.IsTEIStyle(backend.Name) {
		var raw any
		if err := h.apiClient.RequestJSON(ctx, http.MethodPost, backend.URL+"/embed", map[string]any{"inputs": text}, &raw); err != nil {
			return embedResult{}, inferenceDependencyError(err)
		}
		vector := parseTEIVector(raw)
		return embedResult{Vector: vector, Model: fallback(model, "tei"), Dimension: len(vector)}, nil
	}

	var response struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Model string `json:"model"`
	}
	sentModel := outboundEmbedModel(model, backend.Models)
	if err := h.apiClient.RequestJSON(ctx, http.MethodPost, backend.URL+"/v1/embeddings", openAIEmbedRequest(text, sentModel), &response); err != nil {
		return embedResult{}, inferenceDependencyError(err)
	}
	if len(response.Data) == 0 {
		return embedResult{}, httpx.StatusError(http.StatusBadGateway, "embedding backend returned empty data")
	}
	return embedResult{Vector: response.Data[0].Embedding, Model: fallback(response.Model, sentModel), Dimension: len(response.Data[0].Embedding)}, nil
}

func (h *Handlers) embedBatch(ctx context.Context, texts []string, model string) (map[string]any, error) {
	backend, err := h.pickEmbedBackend(model)
	if err != nil {
		return nil, err
	}
	if registry.IsTEIStyle(backend.Name) {
		var vectors [][]float64
		if err := h.apiClient.RequestJSON(ctx, http.MethodPost, backend.URL+"/embed", map[string]any{"inputs": texts}, &vectors); err != nil {
			return nil, inferenceDependencyError(err)
		}
		dimension := 0
		if len(vectors) > 0 {
			dimension = len(vectors[0])
		}
		return map[string]any{"vectors": vectors, "model": fallback(model, "tei"), "dimension": dimension}, nil
	}

	var response struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Model string `json:"model"`
	}
	sentModel := outboundEmbedModel(model, backend.Models)
	if err := h.apiClient.RequestJSON(ctx, http.MethodPost, backend.URL+"/v1/embeddings", openAIEmbedRequest(texts, sentModel), &response); err != nil {
		return nil, inferenceDependencyError(err)
	}
	sort.Slice(response.Data, func(i, j int) bool { return response.Data[i].Index < response.Data[j].Index })
	vectors := make([][]float64, 0, len(response.Data))
	for _, item := range response.Data {
		vectors = append(vectors, item.Embedding)
	}
	dimension := 0
	if len(vectors) > 0 {
		dimension = len(vectors[0])
	}
	return map[string]any{"vectors": vectors, "model": fallback(response.Model, sentModel), "dimension": dimension}, nil
}

// openAIEmbedRequest builds an OpenAI-style /v1/embeddings request body.
// truncate_prompt_tokens=-1 asks vLLM to cut each input to the model's context
// length, as the CPU inference service (sentence-transformers) already does.
// Without it vLLM rejects inputs over the limit (512 tokens for
// bge-small-en-v1.5) with HTTP 400, and the miner answers the probe with 502.
func openAIEmbedRequest(input any, model string) map[string]any {
	return map[string]any{"input": input, "model": model, "truncate_prompt_tokens": -1}
}

func parseTEIVector(raw any) []float64 {
	encoded, _ := json.Marshal(raw)
	var batch [][]float64
	if err := json.Unmarshal(encoded, &batch); err == nil && len(batch) > 0 {
		return batch[0]
	}
	var vector []float64
	_ = json.Unmarshal(encoded, &vector)
	return vector
}

func fallback(value, fallbackValue string) string {
	if value != "" {
		return value
	}
	return fallbackValue
}
