package milvus

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"recall-miner/internal/services/storage/config"
	"recall-miner/internal/services/storage/vector"
)

const (
	registryCollection = "recall_vec_registry"
	tenantField        = "tenant_id"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type collectionList struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Data    []string `json:"data"`
}

type collectionStats struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RowCount json.Number `json:"rowCount"`
	} `json:"data"`
}

type collectionDescribe struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		CollectionName string `json:"collectionName"`
		Description    string `json:"description"`
		Fields         []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Params []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"params"`
		} `json:"fields"`
	} `json:"data"`
}

type insertResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		InsertCount int   `json:"insertCount"`
		InsertIDs   []any `json:"insertIds"`
	} `json:"data"`
}

type searchResponse struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    []map[string]any `json:"data"`
}

type queryResponse struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    []map[string]any `json:"data"`
}

type registryEntry struct {
	CollectionName     string
	PhysicalCollection string
	Dimension          int
}

type genericResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func NewClient(cfg config.Config) *Client {
	return &Client{
		baseURL: strings.TrimRight(cfg.MilvusURI, "/"),
		token:   cfg.MilvusToken,
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
	physical := physicalCollectionName(dimension)
	if err := c.ensureVectorCollection(ctx, physical, dimension); err != nil {
		return err
	}
	_ = c.deleteByFilter(ctx, registryCollection, fmt.Sprintf("id == %q", deterministicUUID(collectionName)))
	return c.insertRegistryEntry(ctx, registryEntry{CollectionName: collectionName, PhysicalCollection: physical, Dimension: dimension})
}

func (c *Client) ensureVectorCollection(ctx context.Context, collectionName string, dimension int) error {
	if info, err := c.describeCollection(ctx, collectionName); err == nil {
		if info.Dimension > 0 && info.Dimension != dimension {
			return fmt.Errorf("milvus collection %q has dimension %d, expected %d", collectionName, info.Dimension, dimension)
		}
		return c.loadCollection(ctx, collectionName)
	} else if !isNotFoundOrAlreadyExists(err) {
		return err
	}

	payload := map[string]any{
		"collectionName": collectionName,
		"schema": map[string]any{
			"autoID":             true,
			"enableDynamicField": true,
			"fields": []map[string]any{
				{"fieldName": "id", "dataType": "Int64", "isPrimary": true},
				{"fieldName": tenantField, "dataType": "VarChar", "isPartitionKey": true, "elementTypeParams": map[string]any{"max_length": "128"}},
				{"fieldName": "metadata", "dataType": "JSON"},
				{"fieldName": "vector", "dataType": "FloatVector", "elementTypeParams": map[string]any{"dim": strconv.Itoa(dimension)}},
			},
		},
		"indexParams": []map[string]any{{
			"fieldName":  "vector",
			"indexName":  "vector_idx",
			"metricType": "COSINE",
			"params": map[string]any{
				"index_type": "IVF_RABITQ",
				"nlist":      1024,
			},
		}},
		"params": map[string]any{
			"partitionsNum":         128,
			"partitionKeyIsolation": true,
		},
	}
	var response genericResponse
	if err := c.post(ctx, "/v2/vectordb/collections/create", payload, &response); err != nil {
		if !isNotFoundOrAlreadyExists(err) {
			return err
		}
	}
	return c.loadCollection(ctx, collectionName)
}

func (c *Client) ensureRegistryCollection(ctx context.Context) error {
	if _, err := c.describeCollection(ctx, registryCollection); err == nil {
		return c.loadCollection(ctx, registryCollection)
	} else if !isNotFoundOrAlreadyExists(err) {
		return err
	}
	payload := map[string]any{
		"collectionName": registryCollection,
		"schema": map[string]any{
			"autoID":             false,
			"enableDynamicField": false,
			"fields": []map[string]any{
				{"fieldName": "id", "dataType": "VarChar", "isPrimary": true, "elementTypeParams": map[string]any{"max_length": "64"}},
				{"fieldName": "collection_name", "dataType": "VarChar", "elementTypeParams": map[string]any{"max_length": "128"}},
				{"fieldName": "physical_collection", "dataType": "VarChar", "elementTypeParams": map[string]any{"max_length": "128"}},
				{"fieldName": "dimension", "dataType": "Int64"},
				{"fieldName": "vector", "dataType": "FloatVector", "elementTypeParams": map[string]any{"dim": "1"}},
			},
		},
	}
	var response genericResponse
	if err := c.post(ctx, "/v2/vectordb/collections/create", payload, &response); err != nil {
		if !isNotFoundOrAlreadyExists(err) {
			return err
		}
	}
	return c.loadCollection(ctx, registryCollection)
}

func (c *Client) DropCollection(ctx context.Context, collectionName string) error {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return err
	}
	if err := c.deleteByFilter(ctx, entry.PhysicalCollection, tenantFilter(collectionName)); err != nil {
		return err
	}
	return c.deleteByFilter(ctx, registryCollection, fmt.Sprintf("id == %q", deterministicUUID(collectionName)))
}

func (c *Client) FlushCollection(ctx context.Context, collectionName string) error {
	var response genericResponse
	return c.post(ctx, "/v2/vectordb/collections/flush", map[string]any{"collectionName": collectionName}, &response)
}

func (c *Client) GetCollectionInfo(ctx context.Context, collectionName string) (vector.CollectionInfo, error) {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return vector.CollectionInfo{}, err
	}
	return vector.CollectionInfo{Name: collectionName, Dimension: entry.Dimension}, nil
}

func (c *Client) describeCollection(ctx context.Context, collectionName string) (vector.CollectionInfo, error) {
	var response collectionDescribe
	if err := c.post(ctx, "/v2/vectordb/collections/describe", map[string]any{"collectionName": collectionName}, &response); err != nil {
		return vector.CollectionInfo{}, err
	}

	info := vector.CollectionInfo{Name: response.Data.Description, Dimension: 0}
	if info.Name == "" {
		info.Name = response.Data.CollectionName
	}

	for _, field := range response.Data.Fields {
		if field.Name != "vector" {
			continue
		}
		for _, param := range field.Params {
			if param.Key != "dim" {
				continue
			}
			dim, _ := strconv.Atoi(param.Value)
			info.Dimension = dim
		}
	}

	return info, nil
}

func (c *Client) GetCollectionStats(ctx context.Context, collectionName string) (int, error) {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return 0, err
	}
	count, err := c.countByFilter(ctx, entry.PhysicalCollection, tenantFilter(collectionName))
	if err == nil {
		return count, nil
	}
	return 0, err
}

func (c *Client) ListCollections(ctx context.Context) ([]string, error) {
	if err := c.ensureRegistryCollection(ctx); err != nil {
		return nil, err
	}
	var response queryResponse
	if err := c.post(ctx, "/v2/vectordb/entities/query", map[string]any{
		"collectionName": registryCollection,
		"filter":         `collection_name != ""`,
		"outputFields":   []string{"collection_name"},
		"limit":          16384,
	}, &response); err != nil {
		return nil, err
	}
	collections := make([]string, 0, len(response.Data))
	for _, item := range response.Data {
		if name, ok := item["collection_name"].(string); ok && name != "" {
			collections = append(collections, name)
		}
	}
	return collections, nil
}

func (c *Client) InsertVector(ctx context.Context, collectionName string, vector []float64, metadata map[string]any) (string, error) {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return "", err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	entity := map[string]any{tenantField: collectionName, "vector": vector, "metadata": metadata}

	var response insertResponse
	if err := c.post(ctx, "/v2/vectordb/entities/insert", map[string]any{
		"collectionName": entry.PhysicalCollection,
		"data":           []map[string]any{entity},
	}, &response); err != nil {
		return "", err
	}

	if len(response.Data.InsertIDs) == 0 {
		return "", fmt.Errorf("milvus insert returned no ids")
	}
	return fmt.Sprint(response.Data.InsertIDs[0]), nil
}

func (c *Client) RestoreVector(ctx context.Context, collectionName, embedID string, value []float64, metadata map[string]any) error {
	return fmt.Errorf("identity-preserving restore is not supported by the Milvus auto-ID backend")
}

func (c *Client) DeleteVector(ctx context.Context, collectionName, embedID string) error {
	id, err := parseInt64EmbedID(embedID)
	if err != nil {
		return err
	}
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return err
	}
	return c.deleteByFilter(ctx, entry.PhysicalCollection, fmt.Sprintf("%s && id == %d", tenantFilter(collectionName), id))
}

// parseInt64EmbedID accepts only a strict base-10 Int64 primary key.
// Raw path input must never reach Milvus filter syntax.
func parseInt64EmbedID(embedID string) (int64, error) {
	if embedID == "" {
		return 0, fmt.Errorf("embed_id must be a valid int64")
	}
	id, err := strconv.ParseInt(embedID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("embed_id must be a valid int64")
	}
	return id, nil
}

func (c *Client) Search(ctx context.Context, collectionName string, queryVector []float64, topK int) ([]vector.SearchHit, error) {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return nil, err
	}
	var response searchResponse
	if err := c.post(ctx, "/v2/vectordb/entities/search", map[string]any{
		"collectionName": entry.PhysicalCollection,
		"data":           [][]float64{queryVector},
		"annsField":      "vector",
		"filter":         tenantFilter(collectionName),
		"limit":          topK,
		"outputFields":   []string{"metadata"},
		"searchParams": map[string]any{
			"metricType": "COSINE",
		},
	}, &response); err != nil {
		return nil, err
	}

	hits := make([]vector.SearchHit, 0, len(response.Data))
	for _, item := range response.Data {
		hit := vector.SearchHit{ID: fmt.Sprint(item["id"]), Metadata: map[string]any{}}
		if distance, ok := numberToFloat(item["distance"]); ok {
			hit.Distance = distance
		}
		if metadata, ok := item["metadata"].(map[string]any); ok {
			hit.Metadata = metadata
		}
		hits = append(hits, hit)
	}

	return hits, nil
}

func (c *Client) insertRegistryEntry(ctx context.Context, entry registryEntry) error {
	var response insertResponse
	return c.post(ctx, "/v2/vectordb/entities/insert", map[string]any{
		"collectionName": registryCollection,
		"data": []map[string]any{{
			"id":                  deterministicUUID(entry.CollectionName),
			"collection_name":     entry.CollectionName,
			"physical_collection": entry.PhysicalCollection,
			"dimension":           entry.Dimension,
			"vector":              []float64{0},
		}},
	}, &response)
}

func (c *Client) registryEntry(ctx context.Context, collectionName string) (registryEntry, error) {
	if err := c.ensureRegistryCollection(ctx); err != nil {
		return registryEntry{}, err
	}
	var response queryResponse
	if err := c.post(ctx, "/v2/vectordb/entities/query", map[string]any{
		"collectionName": registryCollection,
		"filter":         fmt.Sprintf("id == %q", deterministicUUID(collectionName)),
		"outputFields":   []string{"collection_name", "physical_collection", "dimension"},
		"limit":          1,
	}, &response); err != nil {
		return registryEntry{}, err
	}
	if len(response.Data) == 0 {
		return registryEntry{}, vector.CollectionNotFound(collectionName)
	}
	item := response.Data[0]
	entry := registryEntry{CollectionName: collectionName}
	entry.PhysicalCollection, _ = item["physical_collection"].(string)
	if dimension, ok := numberToInt(item["dimension"]); ok {
		entry.Dimension = dimension
	}
	if entry.PhysicalCollection == "" || entry.Dimension <= 0 {
		return registryEntry{}, fmt.Errorf("collection %q registry entry is invalid", collectionName)
	}
	return entry, nil
}

func (c *Client) loadCollection(ctx context.Context, collectionName string) error {
	var response genericResponse
	if err := c.post(ctx, "/v2/vectordb/collections/load", map[string]any{"collectionName": collectionName}, &response); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "loaded") {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) deleteByFilter(ctx context.Context, collectionName, filter string) error {
	var response genericResponse
	return c.post(ctx, "/v2/vectordb/entities/delete", map[string]any{"collectionName": collectionName, "filter": filter}, &response)
}

func (c *Client) countByFilter(ctx context.Context, collectionName, filter string) (int, error) {
	var response queryResponse
	if err := c.post(ctx, "/v2/vectordb/entities/query", map[string]any{
		"collectionName": collectionName,
		"filter":         filter,
		"outputFields":   []string{"count(*)"},
	}, &response); err != nil {
		return 0, err
	}
	if len(response.Data) == 0 {
		return 0, nil
	}
	for _, key := range []string{"count(*)", "count"} {
		if count, ok := numberToInt(response.Data[0][key]); ok {
			return count, nil
		}
	}
	return 0, nil
}

func tenantFilter(collectionName string) string {
	return fmt.Sprintf("%s == %q", tenantField, collectionName)
}

func physicalCollectionName(dimension int) string {
	if dimension <= 0 {
		dimension = 1
	}
	return "recall_vec_dim_" + strconv.Itoa(dimension)
}

func isNotFoundOrAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "not exist") || strings.Contains(message, "already exist")
}

func (c *Client) post(ctx context.Context, path string, payload any, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Request-Timeout", "30")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}

	if response.StatusCode >= 300 {
		return fmt.Errorf("milvus %s returned HTTP %d", path, response.StatusCode)
	}
	if responseCode(target) != 0 {
		return fmt.Errorf("milvus %s returned code %d: %s", path, responseCode(target), responseMessage(target))
	}
	return nil
}

func responseCode(target any) int {
	switch value := target.(type) {
	case *genericResponse:
		return value.Code
	case *collectionDescribe:
		return value.Code
	case *collectionStats:
		return value.Code
	case *collectionList:
		return value.Code
	case *insertResponse:
		return value.Code
	case *searchResponse:
		return value.Code
	case *queryResponse:
		return value.Code
	default:
		return 0
	}
}

func responseMessage(target any) string {
	switch value := target.(type) {
	case *genericResponse:
		return value.Message
	case *collectionDescribe:
		return value.Message
	case *collectionStats:
		return value.Message
	case *collectionList:
		return value.Message
	case *insertResponse:
		return value.Message
	case *searchResponse:
		return value.Message
	case *queryResponse:
		return value.Message
	default:
		return ""
	}
}

func listToFloat64(values []any) []float64 {
	result := make([]float64, 0, len(values))
	for _, value := range values {
		if number, ok := numberToFloat(value); ok {
			result = append(result, number)
		}
	}
	return result
}

func numberToFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
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

func deterministicUUID(value string) string {
	sum := sha1.Sum([]byte(value))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
