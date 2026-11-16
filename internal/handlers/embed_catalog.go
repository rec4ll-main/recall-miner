package handlers

// catalogEmbeddingUpstream maps public catalog ids to the upstream repo ids
// sidecars advertise. Keep this list aligned with the platform catalog.
func catalogEmbeddingUpstream(id string) (string, bool) {
	switch id {
	case "bge-small-en-v1.5":
		return "BAAI/bge-small-en-v1.5", true
	case "bge-m3":
		return "BAAI/bge-m3", true
	case "qwen3-embedding-0.6b":
		return "Qwen/Qwen3-Embedding-0.6B", true
	case "qwen3-embedding-4b":
		return "Qwen/Qwen3-Embedding-4B", true
	case "qwen3-embedding-8b":
		return "Qwen/Qwen3-Embedding-8B", true
	default:
		return "", false
	}
}

func declaredEmbeddingModel(models []string, id string) bool {
	for _, model := range models {
		if model == id {
			return true
		}
	}
	return false
}

// outboundEmbedModel is the model name sent to an OpenAI-compatible backend.
// An empty request stays "default", matching the previous fallback. A catalog
// id is rewritten to the upstream repo id when that is what the backend loaded.
func outboundEmbedModel(requestModel string, advertised []string) string {
	if requestModel == "" {
		return "default"
	}
	for _, model := range advertised {
		if model == requestModel {
			return requestModel
		}
	}
	if upstream, ok := catalogEmbeddingUpstream(requestModel); ok {
		for _, model := range advertised {
			if model == upstream {
				return upstream
			}
		}
	}
	return requestModel
}
