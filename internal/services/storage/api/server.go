package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"recall-miner/internal/services/storage/config"
	"recall-miner/internal/services/storage/milvus"
	"recall-miner/internal/services/storage/pgsql"
	"recall-miner/internal/services/storage/qdrant"
	"recall-miner/internal/services/storage/vector"
)

type Server struct {
	cfg         config.Config
	log         *slog.Logger
	client      vector.Client
	collections map[string]collectionRecord
	mu          sync.RWMutex
}

type collectionRecord struct {
	Name      string `json:"name"`
	Dimension int    `json:"dimension"`
	Count     int    `json:"count"`
}

type createRequest struct {
	Name      string `json:"name"`
	Dimension int    `json:"dimension"`
}

type restoreRequest struct {
	Name      string          `json:"name"`
	Dimension int             `json:"dimension,omitempty"`
	Reset     bool            `json:"reset,omitempty"`
	Vectors   [][]float64     `json:"vectors,omitempty"`
	Records   []restoreRecord `json:"records,omitempty"`
}

type restoreRecord struct {
	EmbedID  string         `json:"embed_id"`
	Vector   []float64      `json:"vector"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type storeRequest struct {
	Vector   []float64      `json:"vector"`
	Metadata map[string]any `json:"metadata"`
}

type queryRequest struct {
	Vector []float64 `json:"vector"`
	TopK   int       `json:"top_k"`
	// Filter and Namespace are not supported by any backend here; they are
	// decoded only so a request carrying them is rejected, not dropped.
	Filter    json.RawMessage `json:"filter,omitempty"`
	Namespace json.RawMessage `json:"namespace,omitempty"`
}

const (
	maxQueryBodyBytes = 16 << 20
	maxQueryTopK      = 100
)

func NewServer(cfg config.Config, logger *slog.Logger) (http.Handler, error) {
	handler, _, err := NewServerWithClaim(cfg, logger)
	return handler, err
}

// NewServerWithClaim builds the storage API for production: the backend client, kept apart
// from other miners by STORAGE_PREFIX, and the claim that refuses storage another db service
// with the same prefix already uses. The caller runs claim.Keep.
func NewServerWithClaim(cfg config.Config, logger *slog.Logger) (http.Handler, *StorageClaim, error) {
	client, err := newVectorClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	client, err = newPrefixedClient(client, cfg.StoragePrefix)
	if err != nil {
		return nil, nil, err
	}
	claim := &StorageClaim{client: client, owner: cfg.PublicURL, prefix: cfg.StoragePrefix, log: logger, now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := claim.Check(ctx); err != nil {
		return nil, nil, err
	}
	return NewServerWithClient(cfg, logger, client), claim, nil
}

// NewServerWithClient builds the storage API around an existing vector
// client (used by cross-package tests with an in-memory client).
func NewServerWithClient(cfg config.Config, logger *slog.Logger, client vector.Client) http.Handler {
	s := &Server{
		cfg:         cfg,
		log:         logger,
		client:      client,
		collections: map[string]collectionRecord{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("POST /db/create", s.createCollection)
	mux.HandleFunc("POST /db/{db_id}/retention-proof", databaseHandler(s.retentionProof))
	mux.HandleFunc("DELETE /db/{db_id}", databaseHandler(s.deleteCollection))
	mux.HandleFunc("GET /db/{db_id}", databaseHandler(s.collectionInfo))
	mux.HandleFunc("POST /db/{db_id}/restore", databaseHandler(s.restoreCollection))
	mux.HandleFunc("POST /db/{db_id}/store", databaseHandler(s.storeVector))
	mux.HandleFunc("DELETE /db/{db_id}/embed/{embed_id}", databaseHandler(s.deleteVector))
	mux.HandleFunc("POST /db/{db_id}/query", databaseHandler(s.queryCollection))
	return s.auth(mux)
}

// Backend namespaces and the storage ownership claim are not database IDs.
// Check after routing so escaped path segments are validated after decoding.
func databaseHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("db_id")
		if id == storageClaimDB || strings.Contains(id, "--") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid database id"})
			return
		}
		next(w, r)
	}
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		if s.cfg.InternalAPIKey == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "INTERNAL_API_KEY is not configured"})
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Api-Key")), []byte(s.cfg.InternalAPIKey)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "invalid internal API key"})
			return
		}

		next.ServeHTTP(w, r)
	})
}

// health is unauthenticated (Docker health checks), so it reports status only and
// never the backend address.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "recall-db-service",
		"engine":  s.cfg.DBEngine,
	})
}

func newVectorClient(cfg config.Config) (vector.Client, error) {
	switch strings.ToLower(cfg.DBEngine) {
	case "pgsql", "postgres", "postgresql", "pgvector":
		return pgsql.NewClient(cfg), nil
	case "qdrant":
		return qdrant.NewClient(cfg), nil
	case "milvus":
		return milvus.NewClient(cfg), nil
	default:
		return nil, fmt.Errorf("unknown DB_ENGINE %q: supported engines are qdrant, milvus, pgsql", cfg.DBEngine)
	}
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	collections, err := s.client.ListCollections(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	visible := collections[:0]
	for _, collection := range collections {
		// The storage claim, and other miners' prefixed databases on a shared Qdrant, are not
		// this miner's databases.
		if collection == storageClaimDB || strings.Contains(collection, "--") {
			continue
		}
		visible = append(visible, collection)
	}
	collections = visible
	totalVectors := 0
	for _, collection := range collections {
		count, err := s.client.GetCollectionStats(r.Context(), collection)
		record := s.lookup(collection)
		if err != nil {
			totalVectors += record.Count
			continue
		}
		if record.Count > count {
			count = record.Count
		}
		totalVectors += count
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"dbs_count":     len(collections),
		"vectors_count": totalVectors,
	})
}

func (s *Server) createCollection(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Dimension <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "dimension must be positive"})
		return
	}

	dbID := fmt.Sprintf("db_%d", time.Now().UnixNano())
	if err := s.client.CreateCollection(r.Context(), dbID, body.Dimension); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	s.remember(dbID, collectionRecord{Name: body.Name, Dimension: body.Dimension, Count: 0})
	writeJSON(w, http.StatusOK, map[string]any{"db_id": dbID, "name": body.Name, "dimension": body.Dimension})
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	if err := s.client.DropCollection(r.Context(), dbID); err != nil {
		if errors.Is(err, vector.ErrCollectionNotFound) {
			// Already gone (e.g. a retry after a lost success response):
			// the documented not-found answer, never a 5xx (M5).
			s.forget(dbID)
			writeJSON(w, http.StatusNotFound, map[string]any{"db_id": dbID, "error": "db not found"})
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.forget(dbID)
	writeJSON(w, http.StatusOK, map[string]any{"db_id": dbID, "status": "deleted"})
}

func (s *Server) collectionInfo(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	info, err := s.client.GetCollectionInfo(r.Context(), dbID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	count, err := s.client.GetCollectionStats(r.Context(), dbID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	record := s.lookup(dbID)
	if record.Count > count {
		count = record.Count
	}
	name := record.Name
	if name == "" {
		name = info.Name
	}
	dimension := record.Dimension
	if dimension == 0 {
		dimension = info.Dimension
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"db_id":         dbID,
		"name":          name,
		"vectors_count": count,
		"dimension":     dimension,
	})
}

func (s *Server) restoreCollection(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	var body restoreRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	dimension := body.Dimension
	if dimension == 0 && len(body.Records) > 0 {
		dimension = len(body.Records[0].Vector)
	}
	if dimension == 0 && len(body.Vectors) > 0 {
		dimension = len(body.Vectors[0])
	}
	if err := validateRestorePayload(s.client, dimension, body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	points := buildRestorePoints(body)
	if body.Reset {
		if staged, ok := s.client.(interface {
			StagedResetRestore(context.Context, string, int, []vector.RestorePoint) error
		}); ok {
			assignMissingRestoreIDs(points)
			if err := staged.StagedResetRestore(r.Context(), dbID, dimension, points); err != nil {
				writeError(w, http.StatusBadGateway, err)
				return
			}
			s.remember(dbID, collectionRecord{Name: body.Name, Dimension: dimension, Count: len(points)})
			writeJSON(w, http.StatusOK, map[string]any{"db_id": dbID, "name": body.Name, "vectors_restored": len(points)})
			return
		}
		resetter, ok := s.client.(interface {
			ResetCollection(context.Context, string, int) error
		})
		if !ok {
			writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "identity-preserving reset restore is not supported by this backend"})
			return
		}
		if err := resetter.ResetCollection(r.Context(), dbID, dimension); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		s.forget(dbID)
	} else if err := s.client.CreateCollection(r.Context(), dbID, dimension); err != nil {
		if errors.Is(err, vector.ErrDimensionImmutable) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}

	for _, point := range points {
		if point.EmbedID == "" {
			embedID, err := s.client.InsertVector(r.Context(), dbID, point.Vector, point.Metadata)
			if err != nil {
				writeError(w, http.StatusBadGateway, err)
				return
			}
			_ = embedID
			continue
		}
		if err := s.client.RestoreVector(r.Context(), dbID, point.EmbedID, point.Vector, point.Metadata); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
	}

	restored := len(points)
	s.remember(dbID, collectionRecord{Name: body.Name, Dimension: dimension, Count: restored})
	writeJSON(w, http.StatusOK, map[string]any{"db_id": dbID, "name": body.Name, "vectors_restored": restored})
}


func assignMissingRestoreIDs(points []vector.RestorePoint) {
	for i := range points {
		if points[i].EmbedID == "" {
			points[i].EmbedID = uuid.NewString()
		}
	}
}

// buildRestorePoints flattens vectors (anonymous) and records into one list.
// Anonymous vectors receive empty EmbedID and are inserted with generated IDs
// on non-staged paths; staged reset assigns IDs before staging.
func buildRestorePoints(body restoreRequest) []vector.RestorePoint {
	points := make([]vector.RestorePoint, 0, len(body.Vectors)+len(body.Records))
	for _, value := range body.Vectors {
		points = append(points, vector.RestorePoint{Vector: value})
	}
	for _, record := range body.Records {
		points = append(points, vector.RestorePoint{
			EmbedID:  record.EmbedID,
			Vector:   record.Vector,
			Metadata: record.Metadata,
		})
	}
	return points
}

// validateRestorePayload checks the full replacement set before any reset or create.
// A validation failure must leave the existing collection unchanged.
func validateRestorePayload(client vector.Client, dimension int, body restoreRequest) error {
	if dimension <= 0 {
		return fmt.Errorf("dimension or records are required")
	}
	for _, vector := range body.Vectors {
		if len(vector) != dimension {
			return fmt.Errorf("restore vector has invalid dimension")
		}
	}
	seen := make(map[string]struct{}, len(body.Records))
	validator, hasValidator := client.(interface {
		ValidateRestorePointID(string) error
	})
	for _, record := range body.Records {
		if record.EmbedID == "" || len(record.Vector) != dimension {
			return fmt.Errorf("restore record has invalid embed_id or dimension")
		}
		if _, dup := seen[record.EmbedID]; dup {
			return fmt.Errorf("restore record has duplicate embed_id")
		}
		seen[record.EmbedID] = struct{}{}
		if hasValidator {
			if err := validator.ValidateRestorePointID(record.EmbedID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) storeVector(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	var body storeRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Vector) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "vector is required"})
		return
	}

	embedID, err := s.client.InsertVector(r.Context(), dbID, body.Vector, body.Metadata)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.increment(dbID)
	writeJSON(w, http.StatusOK, map[string]any{"db_id": dbID, "embed_id": embedID})
}

func (s *Server) deleteVector(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	embedID := r.PathValue("embed_id")
	if err := s.client.DeleteVector(r.Context(), dbID, embedID); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.decrement(dbID)
	writeJSON(w, http.StatusOK, map[string]any{"db_id": dbID, "embed_id": embedID, "status": "deleted"})
}

func (s *Server) queryCollection(w http.ResponseWriter, r *http.Request) {
	dbID := r.PathValue("db_id")
	var body queryRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxQueryBodyBytes)
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Filter != nil || body.Namespace != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "filter and namespace are not supported"})
		return
	}
	// An omitted top_k keeps the default of 10.
	if body.TopK == 0 {
		body.TopK = 10
	}
	if body.TopK < 1 || body.TopK > maxQueryTopK {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("top_k must be an integer from 1 to %d", maxQueryTopK)})
		return
	}

	hits, err := s.client.Search(r.Context(), dbID, body.Vector, body.TopK)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	metadata := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		meta := hit.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		meta["id"] = hit.ID
		meta["distance"] = hit.Distance
		metadata = append(metadata, meta)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"db_id":    dbID,
		"metadata": metadata,
		"count":    len(hits),
	})
}

func (s *Server) remember(dbID string, record collectionRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collections[dbID] = record
}

func (s *Server) lookup(dbID string) collectionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collections[dbID]
}

func (s *Server) forget(dbID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.collections, dbID)
}

func (s *Server) increment(dbID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.collections[dbID]
	record.Count++
	s.collections[dbID] = record
}

func (s *Server) decrement(dbID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.collections[dbID]
	if record.Count > 0 {
		record.Count--
	}
	s.collections[dbID] = record
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil && !strings.Contains(err.Error(), "broken pipe") {
		slog.Error("failed to encode response", "error", err)
	}
}
