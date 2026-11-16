package api

import (
	"context"
	"errors"
	"net/http"
	"recall-miner/internal/retention"
	"recall-miner/internal/services/storage/vector"
	"time"
)

func (s *Server) retentionProof(w http.ResponseWriter, r *http.Request) {
	db := r.PathValue("db_id")
	request, err := retention.Decode(r.Body)
	if err != nil || !retention.ValidID(db) {
		writeJSON(w, 400, map[string]string{"error": "invalid retention request"})
		return
	}
	reader, ok := s.client.(interface {
		RetentionRecord(context.Context, string, string) (retention.Record, error)
	})
	if !ok {
		writeJSON(w, 501, map[string]string{"error": "retention unsupported by backend"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	record, err := reader.RetentionRecord(ctx, db, request.EmbedID)
	if errors.Is(err, retention.ErrNotFound) || errors.Is(err, vector.ErrCollectionNotFound) {
		writeJSON(w, 404, map[string]string{"error": "record not found"})
		return
	}
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "retention backend failed"})
		return
	}
	proof, err := retention.Proof(request, record)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "invalid stored record"})
		return
	}
	writeJSON(w, 200, map[string]string{"db_id": db, "embed_id": request.EmbedID, "proof": proof})
}
