package handlers

import (
	"context"
	"encoding/hex"
	"net/http"
	"recall-miner/internal/httpx"
	"recall-miner/internal/retention"
	"time"
)

func (h *Handlers) DBRetentionProof(w http.ResponseWriter, r *http.Request) {
	db := r.PathValue("db_id")
	body, err := retention.Decode(r.Body)
	if err != nil || !retention.ValidID(db) {
		httpx.WriteJSON(w, 400, map[string]string{"detail": "invalid retention request"})
		return
	}
	// Unlike discovery/query, proofs must never fall back to an arbitrary backend.
	owner := h.registry.DBOwner(db)
	if owner == "" {
		httpx.WriteJSON(w, 404, map[string]string{"detail": "database owner unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result struct {
		DBID    string `json:"db_id"`
		EmbedID string `json:"embed_id"`
		Proof   string `json:"proof"`
	}
	service, err := h.PickDBService(db, owner)
	if err == nil {
		err = h.apiClient.RequestJSONLimited(ctx, http.MethodPost, service.URL+"/db/"+db+"/retention-proof", body, &result, 1024)
	}
	if err != nil {
		httpx.WriteJSON(w, httpx.ErrorStatus(err, 502), map[string]string{"detail": "retention backend failed"})
		return
	}
	proof, decodeErr := hex.DecodeString(result.Proof)
	if h.registry.DBOwner(db) != owner || result.DBID != db || result.EmbedID != body.EmbedID || decodeErr != nil || len(proof) != 32 {
		httpx.WriteJSON(w, 502, map[string]string{"detail": "invalid retention response"})
		return
	}
	httpx.WriteJSON(w, 200, result)
}
