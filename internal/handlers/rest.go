package handlers

import (
	"context"
	"fmt"
	"net/http"

	"recall-miner/internal/auth"
	"recall-miner/internal/httpx"
	"recall-miner/internal/registry"
)

func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "recall-miner"})
}

func (h *Handlers) Services(w http.ResponseWriter, r *http.Request) {
	registered := h.registry.List()
	// Use the same capability gate as registration and heartbeats. Listing a
	// registered sidecar must not imply readiness or enable generation.
	caps := map[string]any{
		"db":        map[string]any{"available": false, "endpoint": "", "max_dbs": 100, "max_vectors": 1000000, "similarity": "cosine", "backend": "qdrant"},
		"embedding": map[string]any{"available": false, "endpoint": "", "models": []string{}, "default_model": "", "max_batch_size": 64},
		"rerank":    map[string]any{"available": false, "endpoint": "", "backend": "transformers"},
		"generate":  map[string]any{"available": false, "endpoint": "", "backend": "inference-service"},
	}
	for _, capability := range h.AvailableCapabilities() {
		backends := h.registry.ByCapabilityHealthy(capability)
		if capability == "generate" {
			backends = nil
			if svc, ok := h.chutesGenerationReady(); ok {
				backends = []registry.ServiceInfo{svc}
			}
		}
		if len(backends) == 0 {
			continue // A sidecar may expire between the two snapshots.
		}
		svc := backends[0]
		caps[capability] = map[string]any{"available": true, "endpoint": svc.URL, "models": svc.Models, "backend": svc.Name}
	}

	resources := map[string]any{"gpu_available": false, "gpu_util": 0.0, "dbs_count": 0, "vectors_count": 0, "storage_used_gb": 0}
	for _, svc := range registered {
		if gpu, _ := svc.Metadata["gpu"].(bool); gpu {
			resources["gpu_available"] = true
		}
	}
	if dbSvc, err := h.PickDBService("", ""); err == nil {
		var stats map[string]any
		if err := h.apiClient.RequestJSON(r.Context(), http.MethodGet, dbSvc.URL+"/stats", nil, &stats); err == nil {
			for key, value := range stats {
				resources[key] = value
			}
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"hotkey": r.Header.Get("X-Verified-Hotkey"), "capabilities": caps, "resources": resources})
}

func (h *Handlers) DBCreate(w http.ResponseWriter, r *http.Request) {
	var body dbCreateRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.Dimension <= 0 {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"detail": "dimension must be positive"})
		return
	}
	var result map[string]any
	service, err := h.dbRequest(r.Context(), http.MethodPost, "", body.Backend, "/db/create", map[string]any{"name": body.Name, "dimension": body.Dimension}, &result)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	if dbID, _ := result["db_id"].(string); dbID != "" {
		h.registry.PinDB(dbID, service.Name)
		result["backend"] = service.Name
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

// DBDelete erases a DB idempotently (M5): a DB that is already gone
// answers 404 (the documented not-found response the API treats as erased),
// never a 5xx, so a retry after a lost success response is safe. A DB with
// no pin (already deleted, or pins lost on restart) is deleted on every
// registered DB backend, and 404 is returned only when every backend
// confirms it is absent; any backend error is returned as an error so the
// caller retries instead of treating the DB as erased.
func (h *Handlers) DBDelete(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	if h.registry.DBOwner(dbID) == "" && h.registry.StaleDBOwner(dbID) == "" {
		h.dbDeleteUnpinned(w, r, dbID)
		return
	}
	var result map[string]any
	_, err := h.dbRequest(r.Context(), http.MethodDelete, dbID, "", "/db/"+dbID, nil, &result)
	if err != nil {
		status := httpx.ErrorStatus(err, http.StatusBadGateway)
		if status == http.StatusNotFound {
			h.registry.UnpinDB(dbID)
			httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"db_id": dbID, "error": "db not found"})
			return
		}
		httpx.WriteError(w, status, err)
		return
	}
	h.registry.UnpinDB(dbID)
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) dbDeleteUnpinned(w http.ResponseWriter, r *http.Request, dbID string) {
	candidates := []registry.ServiceInfo{}
	for _, candidate := range h.registry.ByCapability("db") {
		if registry.IsDBHTTPService(candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.StatusError(http.StatusServiceUnavailable, "No db-service backend registered"))
		return
	}
	var (
		deleted  map[string]any
		firstErr error
	)
	for _, service := range candidates {
		var result map[string]any
		err := h.apiClient.RequestJSON(r.Context(), http.MethodDelete, service.URL+"/db/"+dbID, nil, &result)
		switch {
		case err == nil:
			if result == nil {
				result = map[string]any{"db_id": dbID, "status": "deleted"}
			}
			deleted = result
		case httpx.ErrorStatus(err, http.StatusBadGateway) == http.StatusNotFound:
			// absent on this backend
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		httpx.WriteError(w, httpx.ErrorStatus(firstErr, http.StatusBadGateway), firstErr)
		return
	}
	if deleted == nil {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"db_id": dbID, "error": "db not found"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, deleted)
}

func (h *Handlers) DBInfo(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	var result map[string]any
	_, err := h.dbRequest(r.Context(), http.MethodGet, dbID, "", "/db/"+dbID, nil, &result)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusNotFound), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) DBRestore(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	var body dbRestoreRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	var result map[string]any
	// Stale ownership after sidecar purge must still be restorable: pick an
	// explicit live backend (bypass pin), restore, then reassign (miner#3).
	backend := ""
	if stale := h.registry.StaleDBOwner(dbID); stale != "" {
		candidates := []string{}
		for _, candidate := range h.registry.ByCapability("db") {
			if registry.IsDBHTTPService(candidate) {
				candidates = append(candidates, candidate.Name)
			}
		}
		if len(candidates) == 0 {
			httpx.WriteError(w, http.StatusServiceUnavailable, fmt.Errorf("no db-service backend registered for restore of stale DB %s (last owner %s)", dbID, stale))
			return
		}
		backend = candidates[0]
	}
	service, err := h.dbRequest(r.Context(), http.MethodPost, dbID, backend, "/db/"+dbID+"/restore", body, &result)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	h.registry.ReassignDB(dbID, service.Name)
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) DBStore(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	var body storeEmbedRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	var result map[string]any
	_, err := h.dbRequest(r.Context(), http.MethodPost, dbID, "", "/db/"+dbID+"/store", body, &result)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) DBDeleteEmbed(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	embedID := r.PathValue("embed_id")
	var result map[string]any
	_, err := h.dbRequest(r.Context(), http.MethodDelete, dbID, "", "/db/"+dbID+"/embed/"+embedID, nil, &result)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

const (
	maxQueryBodyBytes = 16 << 20
	maxQueryTopK      = 100
)

func (h *Handlers) DBQuery(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	var body dbQueryRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.Filter != nil || body.Namespace != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"detail": "filter and namespace are not supported"})
		return
	}
	// An omitted top_k keeps the default of 10; recall-web relies on it.
	if body.TopK == 0 {
		body.TopK = 10
	}
	if body.TopK < 1 || body.TopK > maxQueryTopK {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"detail": fmt.Sprintf("top_k must be an integer from 1 to %d", maxQueryTopK)})
		return
	}
	vector := body.Vector
	if len(vector) == 0 {
		if body.Text == "" {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"detail": "text or vector is required"})
			return
		}
		embedded, err := h.embedText(r.Context(), body.Text, body.Model)
		if err != nil {
			httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
			return
		}
		vector = embedded.Vector
	}
	var result map[string]any
	_, err := h.dbRequest(r.Context(), http.MethodPost, dbID, "", "/db/"+dbID+"/query", map[string]any{"vector": vector, "top_k": body.TopK}, &result)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) EmbedText(w http.ResponseWriter, r *http.Request) {
	var body embedRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	result, err := h.embedText(r.Context(), body.Text, body.Model)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) EmbedBatch(w http.ResponseWriter, r *http.Request) {
	var body embedBatchRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	result, err := h.embedBatch(r.Context(), body.Texts, body.Model)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) Rerank(w http.ResponseWriter, r *http.Request) {
	var body rerankRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	result, err := h.rerank(r.Context(), body)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) Generate(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.GenerationEnabled {
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.StatusError(http.StatusServiceUnavailable, "generation is not enabled on this miner"))
		return
	}
	var body generateRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if body.Query == "" {
		httpx.WriteError(w, http.StatusBadRequest, httpx.StatusError(http.StatusBadRequest, "query is required"))
		return
	}
	if body.RequestKey != "" {
		sender, verified := auth.VerifiedSender(r.Context())
		scoped, err := scopeGenerationRequestKey(sender, body.RequestKey)
		if !verified || err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.StatusError(http.StatusBadRequest, "invalid authenticated generation request key"))
			return
		}
		body.scopedRequestKey = scoped
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.GenerationBackendTimeout)
	defer cancel()
	result, err := h.generate(ctx, body)
	if err != nil {
		httpx.WriteError(w, httpx.ErrorStatus(err, http.StatusBadGateway), err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handlers) RegisterService(w http.ResponseWriter, r *http.Request) {
	var body registry.RegisterRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	info := h.registry.Register(body)
	h.log.Info("registered sidecar", "name", info.Name, "url", info.URL, "capabilities", info.Capabilities)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "registered", "service": info})
}

func (h *Handlers) ServiceHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body registry.HeartbeatRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	info, ok := h.registry.Touch(body.Name, body.MachineID, body.Health)
	if !ok {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Service '" + body.Name + "' not registered"})
		return
	}
	h.heartbeats.Record(r.Context(), body.Name, body.MachineID, body.Health)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": info})
}

func (h *Handlers) ListServiceHeartbeats(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"heartbeats": h.heartbeats.List(r.URL.Query().Get("service"), httpx.QueryInt(r, "limit", 100))})
}

func (h *Handlers) UnregisterService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.registry.Unregister(name) {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Service '" + name + "' not found"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "unregistered", "name": name})
}

func (h *Handlers) ListRegisteredServices(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"services": h.registry.List()})
}

func (h *Handlers) GetSidecarConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h.configMu.RLock()
	defer h.configMu.RUnlock()
	cfg, ok := h.sidecarConfig[name]
	if !ok {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"detail": "No config for sidecar '" + name + "'"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": name, "model": cfg.Model, "download": cfg.Download, "config_version": h.configVersion})
}

func (h *Handlers) SetSidecarModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body setModelRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	h.configMu.Lock()
	defer h.configMu.Unlock()
	cfg, ok := h.sidecarConfig[name]
	if !ok {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"detail": "No config for sidecar '" + name + "'"})
		return
	}
	cfg.Model = body.Model
	if cfg.Download == nil {
		cfg.Download = map[string]any{}
	}
	if body.DownloadURL != "" {
		cfg.Download["url"] = body.DownloadURL
	}
	if body.DownloadPath != "" {
		cfg.Download["path"] = body.DownloadPath
	}
	if body.DownloadSource != "" {
		cfg.Download["source"] = body.DownloadSource
	} else if body.DownloadURL != "" {
		if _, ok := cfg.Download["source"]; !ok {
			cfg.Download["source"] = "url"
		}
	}
	h.sidecarConfig[name] = cfg
	h.configVersion++
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": name, "model": cfg.Model, "download": cfg.Download, "config_version": h.configVersion})
}
