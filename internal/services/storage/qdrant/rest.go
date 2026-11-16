package qdrant

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"recall-miner/internal/services/storage/config"
	"recall-miner/internal/services/storage/vector"
)

const (
	registryCollection = "recall_vec_registry"
	tenantPayloadKey   = "tenant_id"
	embedPayloadKey    = "embed_id"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client

	stagingMu    sync.Mutex
	stagingOwn   map[string]struct{}      // collections with an in-process staged restore writer
	promoteLocks map[string]*sync.Mutex   // per-collection restore/recovery promotion lock
	selectLocks  map[string]*sync.RWMutex // per-collection selection fence: reads hold R, generation switches hold W

	// Optional test hooks (nil in production).
	testBeforePromote      func()
	testAfterRecoverScroll func()
	testAfterReadResolve   func()
	testBeforeSelect       func(activeTenant string)
}

type collectionList struct {
	Result struct {
		Collections []struct {
			Name string `json:"name"`
		} `json:"collections"`
	} `json:"result"`
	Status string `json:"status"`
}

type collectionInfo struct {
	Result struct {
		PointsCount  int `json:"points_count"`
		VectorsCount int `json:"vectors_count"`
		Config       struct {
			Params struct {
				Vectors any `json:"vectors"`
			} `json:"params"`
		} `json:"config"`
	} `json:"result"`
	Status string `json:"status"`
}

type genericResponse struct {
	Status string `json:"status"`
}

type searchResponse struct {
	Result []struct {
		ID      any            `json:"id"`
		Score   float64        `json:"score"`
		Payload map[string]any `json:"payload"`
	} `json:"result"`
	Status string `json:"status"`
}

type pointsResponse struct {
	Result []struct {
		ID      any            `json:"id"`
		Payload map[string]any `json:"payload"`
	} `json:"result"`
	Status string `json:"status"`
}

type scrollResponse struct {
	Result struct {
		Points []struct {
			ID      any            `json:"id"`
			Payload map[string]any `json:"payload"`
			Vector  []float64      `json:"vector"`
		} `json:"points"`
		NextPageOffset any `json:"next_page_offset"`
	} `json:"result"`
	Status string `json:"status"`
}

type countResponse struct {
	Result struct {
		Count int `json:"count"`
	} `json:"result"`
	Status string `json:"status"`
}

type registryEntry struct {
	CollectionName     string
	PhysicalCollection string
	Dimension          int
	ActiveTenant       string // empty means CollectionName (legacy registry rows)
}

func (e registryEntry) liveTenant() string {
	if e.ActiveTenant != "" {
		return e.ActiveTenant
	}
	return e.CollectionName
}

type errorResponse struct {
	Status string `json:"status"`
	Result any    `json:"result"`
}

func NewClient(cfg config.Config) *Client {
	return &Client{
		baseURL: strings.TrimRight(cfg.QdrantURL, "/"),
		apiKey:  cfg.QdrantAPIKey,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) CreateCollection(ctx context.Context, collectionName string, dimension int) error {
	if dimension <= 0 {
		return fmt.Errorf("dimension must be positive")
	}
	if err := c.ensureRegistryCollection(ctx); err != nil {
		return err
	}
	if entry, err := c.registryEntry(ctx, collectionName); err == nil {
		if entry.Dimension != dimension {
			return vector.DimensionConflict(collectionName, entry.Dimension, dimension)
		}
		// Same dimension: idempotent. Ensure the physical collection still exists.
		return c.ensureVectorCollection(ctx, entry.PhysicalCollection, dimension)
	} else if !strings.Contains(err.Error(), "not found") {
		return err
	}
	physical := physicalCollectionName(dimension)
	if err := c.ensureVectorCollection(ctx, physical, dimension); err != nil {
		return err
	}
	if err := c.do(ctx, http.MethodPut, "/collections/"+registryCollection+"/points?wait=true", map[string]any{
		"points": []map[string]any{{
			"id":     deterministicUUID(collectionName),
			"vector": []float64{0},
			"payload": map[string]any{
				"collection_name":     collectionName,
				"physical_collection": physical,
				"dimension":           dimension,
				activeTenantKey:       collectionName,
			},
		}},
	}, &genericResponse{}); err != nil {
		return err
	}
	// Re-read so a concurrent first-time create with a different dimension cannot
	// silently win past the existence check above.
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return err
	}
	if entry.Dimension != dimension {
		return vector.DimensionConflict(collectionName, entry.Dimension, dimension)
	}
	return nil
}

func (c *Client) ensureVectorCollection(ctx context.Context, collectionName string, dimension int) error {
	if exists, err := c.existingVectorCollection(ctx, collectionName, dimension, false); err != nil {
		return err
	} else if exists {
		return c.ensureTenantPayloadIndex(ctx, collectionName)
	}

	payload := map[string]any{
		"vectors": map[string]any{
			"size":     dimension,
			"distance": "Cosine",
			"on_disk":  true,
		},
		"hnsw_config": map[string]any{
			"m":         0,
			"payload_m": 16,
		},
		"quantization_config": map[string]any{
			"binary": map[string]any{"always_ram": true},
		},
	}
	if err := c.do(ctx, http.MethodPut, "/collections/"+collectionName, payload, &genericResponse{}); err != nil {
		created := false
		if !isAlreadyExists(err) {
			delete(payload, "quantization_config")
			fallbackErr := c.do(ctx, http.MethodPut, "/collections/"+collectionName, payload, &genericResponse{})
			created = fallbackErr == nil
			if !created && !isAlreadyExists(fallbackErr) {
				return err
			}
		}
		if !created {
			// A concurrent create won the race (first databases on a fresh
			// node). Already existing is success only if it matches.
			if exists, verr := c.existingVectorCollection(ctx, collectionName, dimension, true); verr != nil {
				return verr
			} else if !exists {
				return err
			}
		}
	}
	return c.ensureTenantPayloadIndex(ctx, collectionName)
}

// collectionSettleTimeout bounds retry scheduling for a collection GET that
// answers 5xx: no new retry is started once it has elapsed. It does not bound
// each HTTP call and is not a hard wall-clock timeout; an in-flight GET is
// bounded by the caller's context (which governs cancellation) and the HTTP
// client. Qdrant (1.12) answers 500 "0 of 0 read operations failed"
// for a collection whose creation by a concurrent request is still in
// progress; that clears once the create finishes (well under a second).
var collectionSettleTimeout = 5 * time.Second

// existingVectorCollection reports whether the collection exists, and fails
// if it exists with a different vector size. A 5xx is retried with backoff
// while a concurrent create settles; retries stop being scheduled after
// collectionSettleTimeout (see above: a scheduling bound, not a hard timeout).
//
// strict (the conflict-recovery path) additionally requires the vector size
// to be known: a collection created by someone else is accepted only if it
// provably matches.
func (c *Client) existingVectorCollection(ctx context.Context, collectionName string, dimension int, strict bool) (bool, error) {
	var existing collectionInfo
	deadline := time.Now().Add(collectionSettleTimeout)
	backoff := 25 * time.Millisecond
	for {
		err := c.do(ctx, http.MethodGet, "/collections/"+collectionName, nil, &existing)
		if err == nil {
			break
		}
		if isHTTPStatus(err, http.StatusNotFound) {
			return false, nil
		}
		if !isServerError(err) || time.Now().Add(backoff).After(deadline) {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, err
		case <-time.After(backoff):
		}
		if backoff < 400*time.Millisecond {
			backoff *= 2
		}
	}
	size := qdrantVectorSize(existing.Result.Config.Params.Vectors)
	if size > 0 && size != dimension {
		return true, fmt.Errorf("qdrant collection %q has dimension %d, expected %d", collectionName, size, dimension)
	}
	if strict && size <= 0 {
		return true, fmt.Errorf("qdrant collection %q exists but its vector size is unknown; expected %d", collectionName, dimension)
	}
	return true, nil
}

func isServerError(err error) bool {
	for _, st := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if isHTTPStatus(err, st) {
			return true
		}
	}
	return false
}

// isAlreadyExists: Qdrant (pinned v1.12.6, verified) answers a create for an
// existing collection with 409 Conflict. Other versions' responses are not
// relied on.
func isAlreadyExists(err error) bool {
	return isHTTPStatus(err, http.StatusConflict)
}

func (c *Client) ensureRegistryCollection(ctx context.Context) error {
	if err := c.ensureVectorCollection(ctx, registryCollection, 1); err != nil {
		return err
	}
	if err := c.ensurePayloadIndex(ctx, registryCollection, "collection_name", false); err != nil {
		return err
	}
	return nil

}

func (c *Client) DropCollection(ctx context.Context, collectionName string) error {
	_ = c.RecoverStaging(ctx, collectionName)
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return err
	}
	live := entry.liveTenant()
	if err := c.do(ctx, http.MethodPost, "/collections/"+entry.PhysicalCollection+"/points/delete?wait=true", map[string]any{
		"filter": tenantFilter(live),
	}, &genericResponse{}); err != nil {
		return err
	}
	if live != collectionName {
		_ = c.deleteByTenant(ctx, entry.PhysicalCollection, collectionName)
	}
	_ = c.deleteByTenant(ctx, entry.PhysicalCollection, stagingTenant(collectionName))
	_ = c.clearStagingMark(ctx, collectionName)
	return c.do(ctx, http.MethodPost, "/collections/"+registryCollection+"/points/delete?wait=true", map[string]any{
		"points": []string{deterministicUUID(collectionName)},
	}, &genericResponse{})
}

func (c *Client) ResetCollection(ctx context.Context, collectionName string, dimension int) error {
	_ = c.RecoverStaging(ctx, collectionName)
	entry, err := c.registryEntry(ctx, collectionName)
	if err == nil {
		live := entry.liveTenant()
		if err := c.do(ctx, http.MethodPost, "/collections/"+entry.PhysicalCollection+"/points/delete?wait=true", map[string]any{
			"filter": tenantFilter(live),
		}, &genericResponse{}); err != nil {
			return err
		}
		if live != collectionName {
			_ = c.deleteByTenant(ctx, entry.PhysicalCollection, collectionName)
		}
		_ = c.deleteByTenant(ctx, entry.PhysicalCollection, stagingTenant(collectionName))
		_ = c.clearStagingMark(ctx, collectionName)
		if err := c.do(ctx, http.MethodPost, "/collections/"+registryCollection+"/points/delete?wait=true", map[string]any{
			"points": []string{deterministicUUID(collectionName)},
		}, &genericResponse{}); err != nil {
			return err
		}
	} else if !strings.Contains(err.Error(), "not found") {
		return err
	}
	return c.CreateCollection(ctx, collectionName, dimension)
}

func (c *Client) GetCollectionInfo(ctx context.Context, collectionName string) (vector.CollectionInfo, error) {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return vector.CollectionInfo{}, err
	}
	return vector.CollectionInfo{Name: collectionName, Dimension: entry.Dimension}, nil
}

func (c *Client) GetCollectionStats(ctx context.Context, collectionName string) (int, error) {
	entry, release, err := c.resolveForRead(ctx, collectionName)
	if err != nil {
		return 0, err
	}
	defer release()
	var response countResponse
	if err := c.do(ctx, http.MethodPost, "/collections/"+entry.PhysicalCollection+"/points/count", map[string]any{
		"exact":  true,
		"filter": tenantFilter(entry.liveTenant()),
	}, &response); err != nil {
		return 0, err
	}
	return response.Result.Count, nil
}

func (c *Client) ListCollections(ctx context.Context) ([]string, error) {
	if err := c.ensureRegistryCollection(ctx); err != nil {
		return nil, err
	}
	collections := []string{}
	var offset any
	for {
		payload := map[string]any{"limit": 256, "with_payload": true, "with_vector": false}
		if offset != nil {
			payload["offset"] = offset
		}
		var response scrollResponse
		if err := c.do(ctx, http.MethodPost, "/collections/"+registryCollection+"/points/scroll", payload, &response); err != nil {
			return nil, err
		}
		for _, point := range response.Result.Points {
			if name, ok := point.Payload["collection_name"].(string); ok && name != "" {
				collections = append(collections, name)
			}
		}
		if response.Result.NextPageOffset == nil {
			break
		}
		offset = response.Result.NextPageOffset
	}
	return collections, nil
}

func (c *Client) InsertVector(ctx context.Context, collectionName string, value []float64, metadata map[string]any) (string, error) {
	embedID := newUUID()
	if err := c.RestoreVector(ctx, collectionName, embedID, value, metadata); err != nil {
		return "", err
	}
	return embedID, nil
}

func (c *Client) RestoreVector(ctx context.Context, collectionName, embedID string, value []float64, metadata map[string]any) error {
	recoverErr := c.RecoverStaging(ctx, collectionName)
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		if recoverErr != nil {
			return recoverErr
		}
		return err
	}
	if recoverErr != nil && !c.selectedGenerationReadable(ctx, collectionName, stagingTenant(collectionName)) {
		return recoverErr
	}
	live := entry.liveTenant()
	physicalID := pointIDForTenant(live, embedID)
	ids := []string{physicalID}
	if physicalID != embedID {
		ids = append(ids, embedID)
	}
	existing, err := c.getPoints(ctx, entry.PhysicalCollection, ids)
	if err != nil {
		return err
	}
	for _, point := range existing {
		tenant, _ := point.Payload[tenantPayloadKey].(string)
		if tenant != "" && tenant != live {
			return fmt.Errorf("point id conflict: embed_id %q is owned by another database", embedID)
		}
	}

	payload := map[string]any{
		tenantPayloadKey: live,
		embedPayloadKey:  embedID,
	}
	if metadata != nil {
		payload["metadata"] = metadata
	}
	if err := c.do(ctx, http.MethodPut, "/collections/"+entry.PhysicalCollection+"/points?wait=true", map[string]any{
		"points": []map[string]any{{
			"id":      physicalID,
			"vector":  value,
			"payload": payload,
		}},
	}, &genericResponse{}); err != nil {
		return err
	}

	// Migrate-on-write: drop the legacy unnamespaced point for this tenant when
	// it still uses the external embed ID as the physical point ID.
	if physicalID != embedID {
		for _, point := range existing {
			if fmt.Sprint(point.ID) != embedID {
				continue
			}
			tenant, _ := point.Payload[tenantPayloadKey].(string)
			if tenant != live {
				continue
			}
			if err := c.do(ctx, http.MethodPost, "/collections/"+entry.PhysicalCollection+"/points/delete?wait=true", map[string]any{
				"filter": map[string]any{
					"must": []map[string]any{
						{"key": tenantPayloadKey, "match": map[string]any{"value": live}},
						{"has_id": []any{pointIDWireValue(embedID)}},
					},
				},
			}, &genericResponse{}); err != nil {
				// The replacement upsert is idempotent. Surface cleanup failure so
				// a retry can finish migration instead of accepting duplicate results.
				return fmt.Errorf("remove legacy point after restore: %w", err)
			}
			break
		}
	}
	return nil
}

func (c *Client) DeleteVector(ctx context.Context, collectionName, embedID string) error {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return err
	}
	live := entry.liveTenant()
	physicalID := pointIDForTenant(live, embedID)
	should := []map[string]any{
		{"has_id": []string{physicalID}},
		{"key": embedPayloadKey, "match": map[string]any{"value": embedID}},
	}
	if physicalID != embedID {
		should = append(should, map[string]any{"has_id": []any{pointIDWireValue(embedID)}})
	}
	return c.do(ctx, http.MethodPost, "/collections/"+entry.PhysicalCollection+"/points/delete?wait=true", map[string]any{
		"filter": map[string]any{
			"must": []any{
				map[string]any{"key": tenantPayloadKey, "match": map[string]any{"value": live}},
				map[string]any{"should": should},
			},
		},
	}, &genericResponse{})
}

func (c *Client) Search(ctx context.Context, collectionName string, value []float64, topK int) ([]vector.SearchHit, error) {
	entry, release, err := c.resolveForRead(ctx, collectionName)
	if err != nil {
		return nil, err
	}
	defer release()
	var response searchResponse
	if err := c.do(ctx, http.MethodPost, "/collections/"+entry.PhysicalCollection+"/points/search", map[string]any{
		"vector":       value,
		"filter":       tenantFilter(entry.liveTenant()),
		"limit":        topK,
		"with_vector":  false,
		"with_payload": true,
	}, &response); err != nil {
		return nil, err
	}

	hits := make([]vector.SearchHit, 0, len(response.Result))
	for _, item := range response.Result {
		metadata, _ := item.Payload["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		hits = append(hits, vector.SearchHit{
			ID:       externalEmbedID(item.ID, item.Payload),
			Distance: item.Score,
			Metadata: metadata,
		})
	}
	return hits, nil
}

// resolveForRead runs staging recovery, then resolves the selected generation
// under the per-collection selection read lock. The returned release must be
// called after the query, so a concurrent promotion cannot switch away from
// and reclaim the generation this read selected while it is still in flight.
// A recovery error (for example a failing compaction or cleanup step) only
// fails the read when the selected generation cannot be validated.
func (c *Client) resolveForRead(ctx context.Context, collectionName string) (registryEntry, func(), error) {
	recoverErr := c.RecoverStaging(ctx, collectionName)
	mu := c.selectLockFor(collectionName)
	mu.RLock()
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		mu.RUnlock()
		if recoverErr != nil {
			return registryEntry{}, nil, recoverErr
		}
		return registryEntry{}, nil, err
	}
	if recoverErr != nil && !c.selectedGenerationReadable(ctx, collectionName, stagingTenant(collectionName)) {
		mu.RUnlock()
		return registryEntry{}, nil, recoverErr
	}
	if c.testAfterReadResolve != nil {
		c.testAfterReadResolve()
	}
	return entry, mu.RUnlock, nil
}

func (c *Client) registryEntry(ctx context.Context, collectionName string) (registryEntry, error) {
	if err := c.ensureRegistryCollection(ctx); err != nil {
		return registryEntry{}, err
	}
	var response pointsResponse
	if err := c.do(ctx, http.MethodPost, "/collections/"+registryCollection+"/points", map[string]any{
		"ids":          []string{deterministicUUID(collectionName)},
		"with_payload": true,
		"with_vector":  false,
	}, &response); err != nil {
		return registryEntry{}, err
	}
	if len(response.Result) == 0 {
		return registryEntry{}, vector.CollectionNotFound(collectionName)
	}
	payload := response.Result[0].Payload
	entry := registryEntry{CollectionName: collectionName}
	entry.PhysicalCollection, _ = payload["physical_collection"].(string)
	entry.ActiveTenant, _ = payload[activeTenantKey].(string)
	if dimension, ok := numberToInt(payload["dimension"]); ok {
		entry.Dimension = dimension
	}
	if entry.PhysicalCollection == "" || entry.Dimension <= 0 {
		return registryEntry{}, fmt.Errorf("collection %q registry entry is invalid", collectionName)
	}
	return entry, nil
}

func (c *Client) ensureTenantPayloadIndex(ctx context.Context, collectionName string) error {
	if err := c.ensurePayloadIndex(ctx, collectionName, tenantPayloadKey, true); err != nil {
		return err
	}
	return c.ensurePayloadIndex(ctx, collectionName, embedPayloadKey, false)
}

func (c *Client) ensurePayloadIndex(ctx context.Context, collectionName, fieldName string, tenant bool) error {
	schema := any("keyword")
	if tenant {
		schema = map[string]any{"type": "keyword", "is_tenant": true}
	}
	if err := c.do(ctx, http.MethodPut, "/collections/"+collectionName+"/index", map[string]any{
		"field_name":   fieldName,
		"field_schema": schema,
	}, &genericResponse{}); err != nil {
		if isHTTPStatus(err, http.StatusConflict) {
			return nil
		}
		if tenant {
			if fallbackErr := c.do(ctx, http.MethodPut, "/collections/"+collectionName+"/index", map[string]any{
				"field_name":   fieldName,
				"field_schema": "keyword",
			}, &genericResponse{}); fallbackErr == nil || isHTTPStatus(fallbackErr, http.StatusConflict) {
				return nil
			}
		}
		return err
	}
	return nil
}

func tenantFilter(collectionName string) map[string]any {
	return map[string]any{
		"must": []map[string]any{{
			"key":   tenantPayloadKey,
			"match": map[string]any{"value": collectionName},
		}},
	}
}

func physicalCollectionName(dimension int) string {
	if dimension <= 0 {
		dimension = 1
	}
	return "recall_vec_dim_" + strconv.Itoa(dimension)
}

func isHTTPStatus(err error, status int) bool {
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status))
}

func (c *Client) do(ctx context.Context, method, path string, payload any, target any) error {
	body := bytes.NewReader(nil)
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		request.Header.Set("api-key", c.apiKey)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if response.StatusCode >= 300 {
		var errorBody errorResponse
		_ = decoder.Decode(&errorBody)
		return fmt.Errorf("qdrant %s %s returned HTTP %d: %s", method, path, response.StatusCode, errorBody.Status)
	}
	if target == nil {
		return nil
	}
	return decoder.Decode(target)
}

func qdrantVectorSize(raw any) int {
	switch value := raw.(type) {
	case map[string]any:
		if size, ok := numberToInt(value["size"]); ok {
			return size
		}
		for _, nested := range value {
			if nestedMap, ok := nested.(map[string]any); ok {
				if size, ok := numberToInt(nestedMap["size"]); ok {
					return size
				}
			}
		}
	}
	return 0
}

func numberToInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

// pointIDForTenant derives a collision-resistant physical Qdrant point ID from
// the logical database identity and the external embed ID (UUIDv5 / SHA1).
func pointIDForTenant(collectionName, embedID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(collectionName+"\x00"+embedID)).String()
}

// externalEmbedID returns the API-facing embed ID. Namespaced points store it
// in payload; legacy points used the physical point ID as the external ID.
func externalEmbedID(pointID any, payload map[string]any) string {
	if payload != nil {
		if id, ok := payload[embedPayloadKey].(string); ok && id != "" {
			return id
		}
	}
	return fmt.Sprint(pointID)
}

func (c *Client) getPoints(ctx context.Context, physicalCollection string, ids []string) ([]struct {
	ID      any
	Payload map[string]any
}, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Qdrant distinguishes UUID strings from uint64 JSON numbers. Preserve the
	// logical ID; encode only the legacy lookup ID using its supported wire type.
	wireIDs := make([]any, len(ids))
	for i, id := range ids {
		wireIDs[i] = pointIDWireValue(id)
	}
	var response pointsResponse
	if err := c.do(ctx, http.MethodPost, "/collections/"+physicalCollection+"/points", map[string]any{
		"ids":          wireIDs,
		"with_payload": true,
		"with_vector":  false,
	}, &response); err != nil {
		return nil, err
	}
	out := make([]struct {
		ID      any
		Payload map[string]any
	}, 0, len(response.Result))
	for _, point := range response.Result {
		out = append(out, struct {
			ID      any
			Payload map[string]any
		}{ID: point.ID, Payload: point.Payload})
	}
	return out, nil
}

// ValidateRestorePointID reports whether embedID is a Qdrant-supported point id
// (UUID or unsigned 64-bit integer). Call before any destructive restore.
func ValidateRestorePointID(embedID string) error {
	if embedID == "" {
		return fmt.Errorf("embed_id is not a valid Qdrant point id")
	}
	if _, err := uuid.Parse(embedID); err == nil {
		return nil
	}
	if _, err := strconv.ParseUint(embedID, 10, 64); err == nil {
		return nil
	}
	return fmt.Errorf("embed_id is not a valid Qdrant point id")
}

func (c *Client) ValidateRestorePointID(embedID string) error {
	return ValidateRestorePointID(embedID)
}

func deterministicUUID(value string) string {
	sum := sha1.Sum([]byte(value))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		b[6] = (b[6] & 0x0f) | 0x40
		b[8] = (b[8] & 0x3f) | 0x80
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	now := time.Now().UnixNano()
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", uint32(now>>32), uint16(now>>16), uint16(now), uint16(now>>48), uint64(now))
}

// pointIDWireValue changes only the backend wire type, never logical identity.
func pointIDWireValue(id string) any {
	if n, err := strconv.ParseUint(id, 10, 64); err == nil {
		return n
	}
	return id
}
