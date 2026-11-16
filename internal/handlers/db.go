package handlers

import (
	"context"
	"fmt"
	"net/http"

	"recall-miner/internal/httpx"
	"recall-miner/internal/registry"
)

func (h *Handlers) PickDBService(dbID, backend string) (registry.ServiceInfo, error) {
	candidates := []registry.ServiceInfo{}
	for _, candidate := range h.registry.ByCapability("db") {
		if registry.IsDBHTTPService(candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if backend != "" {
		for _, candidate := range candidates {
			if candidate.Name == backend {
				return candidate, nil
			}
		}
		return registry.ServiceInfo{}, httpx.StatusError(http.StatusNotFound, fmt.Sprintf("DB backend %q is not registered", backend))
	}
	if len(candidates) == 0 {
		return registry.ServiceInfo{}, httpx.StatusError(http.StatusServiceUnavailable, "No db-service backend registered")
	}
	if dbID != "" {
		owner := h.registry.DBOwner(dbID)
		if owner != "" {
			for _, candidate := range candidates {
				if candidate.Name == owner {
					return candidate, nil
				}
			}
			return registry.ServiceInfo{}, httpx.StatusError(http.StatusServiceUnavailable, fmt.Sprintf("Pinned DB backend %q is unavailable", owner))
		}
		if stale := h.registry.StaleDBOwner(dbID); stale != "" {
			// Fail closed until explicit restore reassigns (miner#3).
			return registry.ServiceInfo{}, httpx.StatusError(http.StatusConflict, fmt.Sprintf("DB %q ownership is stale after sidecar purge; restore required (last owner %q)", dbID, stale))
		}
	}
	return candidates[0], nil
}

func (h *Handlers) dbRequest(ctx context.Context, method, dbID, backend, path string, payload any, out any) (registry.ServiceInfo, error) {
	service, err := h.PickDBService(dbID, backend)
	if err != nil {
		return registry.ServiceInfo{}, err
	}
	return service, h.apiClient.RequestJSON(ctx, method, service.URL+path, payload, out)
}
