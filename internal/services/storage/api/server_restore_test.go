package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recall-miner/internal/services/storage/vector"
)

type memClient struct {
	calls    []string
	points   map[string][]float64
	meta     map[string]string
	created  map[string]int
	resetHit int
}

func newMemClient() *memClient {
	return &memClient{
		points:  map[string][]float64{},
		meta:    map[string]string{},
		created: map[string]int{},
	}
}

func (c *memClient) CreateCollection(_ context.Context, collectionName string, dimension int) error {
	c.calls = append(c.calls, "create")
	if existing, ok := c.created[collectionName]; ok && existing != dimension {
		return vector.DimensionConflict(collectionName, existing, dimension)
	}
	c.created[collectionName] = dimension
	return nil
}

func (c *memClient) DropCollection(context.Context, string) error { return nil }

func (c *memClient) GetCollectionInfo(context.Context, string) (vector.CollectionInfo, error) {
	return vector.CollectionInfo{}, nil
}

func (c *memClient) GetCollectionStats(context.Context, string) (int, error) { return 0, nil }

func (c *memClient) ListCollections(context.Context) ([]string, error) { return nil, nil }

func (c *memClient) InsertVector(_ context.Context, collectionName string, value []float64, _ map[string]any) (string, error) {
	c.calls = append(c.calls, "insert")
	id := collectionName + ":insert"
	c.points[id] = append([]float64(nil), value...)
	return id, nil
}

func (c *memClient) RestoreVector(_ context.Context, collectionName, embedID string, value []float64, _ map[string]any) error {
	c.calls = append(c.calls, "restore:"+embedID)
	c.points[collectionName+"/"+embedID] = append([]float64(nil), value...)
	c.meta[collectionName+"/"+embedID] = embedID
	return nil
}

func (c *memClient) DeleteVector(context.Context, string, string) error { return nil }

func (c *memClient) Search(context.Context, string, []float64, int) ([]vector.SearchHit, error) {
	return nil, nil
}

type resettingClient struct {
	*memClient
}

func (c resettingClient) ResetCollection(_ context.Context, collectionName string, dimension int) error {
	c.calls = append(c.calls, "reset")
	c.resetHit++
	prefix := collectionName + "/"
	for key := range c.points {
		if strings.HasPrefix(key, prefix) {
			delete(c.points, key)
			delete(c.meta, key)
		}
	}
	c.created[collectionName] = dimension
	return nil
}

func restoreServer(client vector.Client) *Server {
	s := &Server{
		client:      client,
		collections: map[string]collectionRecord{},
	}
	s.remember("db1", collectionRecord{Name: "kept", Dimension: 2, Count: 4})
	return s
}

func doRestore(s *Server, payload string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/db/db1/restore", strings.NewReader(payload))
	req.SetPathValue("db_id", "db1")
	rr := httptest.NewRecorder()
	s.restoreCollection(rr, req)
	return rr
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return body
}

func TestRestoreResetRejectsBadRecordBeforeWipe(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{9, 9}
	mem.meta["db1/existing"] = "existing"
	s := restoreServer(resettingClient{mem})

	payload := `{
		"name": "kept",
		"reset": true,
		"dimension": 2,
		"records": [
			{"embed_id": "ok", "vector": [1, 2]},
			{"embed_id": "", "vector": [3, 4]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["error"] != "restore record has invalid embed_id or dimension" {
		t.Fatalf("error %v", body["error"])
	}
	if mem.resetHit != 0 || len(mem.calls) != 0 {
		t.Fatalf("backend calls after invalid restore: resetHit=%d calls=%v", mem.resetHit, mem.calls)
	}
	if got := mem.points["db1/existing"]; len(got) != 2 || got[0] != 9 || got[1] != 9 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
	if _, wiped := mem.points["db1/ok"]; wiped {
		t.Fatalf("prefix record was written: %#v", mem.points)
	}
	kept := s.lookup("db1")
	if kept.Name != "kept" || kept.Dimension != 2 || kept.Count != 4 {
		t.Fatalf("in-memory collection changed: %+v", kept)
	}
}

func TestRestoreResetRejectsMismatchedRecordDimensionBeforeWipe(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{1, 1}
	s := restoreServer(resettingClient{mem})

	payload := `{
		"reset": true,
		"records": [
			{"embed_id": "a", "vector": [1, 2]},
			{"embed_id": "b", "vector": [3]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if mem.resetHit != 0 || len(mem.calls) != 0 {
		t.Fatalf("backend calls after dimension mismatch: resetHit=%d calls=%v", mem.resetHit, mem.calls)
	}
	if got := mem.points["db1/existing"]; len(got) != 2 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
}

func TestRestoreResetRejectsBadVectorBeforeWipe(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{4, 5}
	s := restoreServer(resettingClient{mem})

	payload := `{"reset": true, "dimension": 2, "vectors": [[1, 2], [3]]}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["error"] != "restore vector has invalid dimension" {
		t.Fatalf("error %v", body["error"])
	}
	if mem.resetHit != 0 || len(mem.calls) != 0 {
		t.Fatalf("backend calls after bad vector: resetHit=%d calls=%v", mem.resetHit, mem.calls)
	}
	if len(mem.points["db1/existing"]) != 2 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
}

func TestRestoreCreateRejectsBadRecordBeforeCreate(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{7, 8}
	s := restoreServer(mem)

	payload := `{"dimension": 2, "records": [{"embed_id": "", "vector": [1, 2]}]}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if len(mem.calls) != 0 {
		t.Fatalf("backend calls after invalid create restore: %v", mem.calls)
	}
	if _, created := mem.created["db1"]; created {
		t.Fatalf("collection was created: %#v", mem.created)
	}
	if len(mem.points["db1/existing"]) != 2 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
	kept := s.lookup("db1")
	if kept.Count != 4 || kept.Name != "kept" {
		t.Fatalf("in-memory collection changed: %+v", kept)
	}
}

func TestRestoreResetReplacesOnlyAfterFullSetValid(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{9, 9}
	s := restoreServer(resettingClient{mem})

	payload := `{
		"name": "replaced",
		"reset": true,
		"dimension": 2,
		"records": [
			{"embed_id": "a", "vector": [1, 2]},
			{"embed_id": "b", "vector": [3, 4]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if mem.resetHit != 1 {
		t.Fatalf("resetHit %d calls %v", mem.resetHit, mem.calls)
	}
	if mem.calls[0] != "reset" {
		t.Fatalf("reset was not first: %v", mem.calls)
	}
	if _, still := mem.points["db1/existing"]; still {
		t.Fatalf("old point survived a valid reset: %#v", mem.points)
	}
	if len(mem.points["db1/a"]) != 2 || len(mem.points["db1/b"]) != 2 {
		t.Fatalf("replacement records missing: %#v", mem.points)
	}
	kept := s.lookup("db1")
	if kept.Name != "replaced" || kept.Dimension != 2 || kept.Count != 2 {
		t.Fatalf("collection record %+v", kept)
	}
}

func TestRestoreMissingDimensionDoesNotTouchBackend(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{1}
	s := restoreServer(resettingClient{mem})
	rr := doRestore(s, `{"reset": true, "records": [{"embed_id": "a", "vector": []}]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["error"] != "dimension or records are required" {
		t.Fatalf("error %v", body["error"])
	}
	if mem.resetHit != 0 || len(mem.calls) != 0 {
		t.Fatalf("backend calls: resetHit=%d calls=%v", mem.resetHit, mem.calls)
	}
	if len(mem.points["db1/existing"]) != 1 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
}

func TestRestoreBodyIsConsumed(t *testing.T) {
	mem := newMemClient()
	s := restoreServer(resettingClient{mem})
	req := httptest.NewRequest(http.MethodPost, "/db/db1/restore", strings.NewReader(`{"dimension": 1, "records": [{"embed_id": "a", "vector": [1]}]}`))
	req.SetPathValue("db_id", "db1")
	rr := httptest.NewRecorder()
	s.restoreCollection(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	_, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestoreNonResetRejectsDimensionChange(t *testing.T) {
	mem := newMemClient()
	mem.created["db1"] = 2
	mem.points["db1/existing"] = []float64{1, 2}
	s := restoreServer(mem)

	payload := `{
		"reset": false,
		"dimension": 3,
		"records": [
			{"embed_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "vector": [1, 2, 3]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "dimension") {
		t.Fatalf("error %v", body["error"])
	}
	if mem.created["db1"] != 2 {
		t.Fatalf("dimension remapped: %#v", mem.created)
	}
	if got := mem.points["db1/existing"]; len(got) != 2 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
	if _, wrote := mem.points["db1/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"]; wrote {
		t.Fatalf("conflicting record was written: %#v", mem.points)
	}
	kept := s.lookup("db1")
	if kept.Dimension != 2 || kept.Count != 4 {
		t.Fatalf("in-memory collection changed: %+v", kept)
	}
}

func TestRestoreNonResetRejectsDimensionChangeWithEmptyRecords(t *testing.T) {
	mem := newMemClient()
	mem.created["db1"] = 2
	mem.points["db1/existing"] = []float64{9, 9}
	s := restoreServer(mem)

	rr := doRestore(s, `{"reset": false, "dimension": 4, "records": []}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if mem.created["db1"] != 2 {
		t.Fatalf("dimension remapped: %#v", mem.created)
	}
	if len(mem.points["db1/existing"]) != 2 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
}

func TestRestoreNonResetSameDimensionPreservesAndAdds(t *testing.T) {
	mem := newMemClient()
	mem.created["db1"] = 2
	mem.points["db1/existing"] = []float64{1, 1}
	s := restoreServer(mem)

	payload := `{
		"name": "kept",
		"reset": false,
		"dimension": 2,
		"records": [
			{"embed_id": "new-one", "vector": [3, 4]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if mem.created["db1"] != 2 {
		t.Fatalf("dimension changed: %#v", mem.created)
	}
	if len(mem.points["db1/existing"]) != 2 {
		t.Fatalf("existing point lost: %#v", mem.points)
	}
	if len(mem.points["db1/new-one"]) != 2 {
		t.Fatalf("new point missing: %#v", mem.points)
	}
}

func TestRestoreResetMayChangeDimension(t *testing.T) {
	mem := newMemClient()
	mem.created["db1"] = 2
	mem.points["db1/existing"] = []float64{1, 2}
	s := restoreServer(resettingClient{mem})

	payload := `{
		"reset": true,
		"dimension": 3,
		"records": [
			{"embed_id": "a", "vector": [1, 2, 3]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if mem.created["db1"] != 3 {
		t.Fatalf("reset did not adopt new dimension: %#v", mem.created)
	}
	if _, still := mem.points["db1/existing"]; still {
		t.Fatalf("old point survived reset: %#v", mem.points)
	}
}


type qdrantValidatingClient struct {
	vector.Client
}

func (c qdrantValidatingClient) ValidateRestorePointID(embedID string) error {
	return qdrantValidateRestorePointID(embedID)
}

func (c qdrantValidatingClient) ResetCollection(ctx context.Context, collectionName string, dimension int) error {
	resetter, ok := c.Client.(interface {
		ResetCollection(context.Context, string, int) error
	})
	if !ok {
		return errors.New("reset not supported")
	}
	return resetter.ResetCollection(ctx, collectionName, dimension)
}

// Local copy of the Qdrant point-id rule so api tests do not import the adapter.
func qdrantValidateRestorePointID(embedID string) error {
	if embedID == "" {
		return errInvalidQdrantPointID
	}
	if looksLikeUUID(embedID) || looksLikeUint64(embedID) {
		return nil
	}
	return errInvalidQdrantPointID
}

var errInvalidQdrantPointID = errors.New("embed_id is not a valid Qdrant point id")

func looksLikeUUID(embedID string) bool {
	if len(embedID) != 36 {
		return false
	}
	for i, r := range embedID {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}

func looksLikeUint64(embedID string) bool {
	if embedID == "" {
		return false
	}
	var n uint64
	for _, r := range embedID {
		if r < '0' || r > '9' {
			return false
		}
		dig := uint64(r - '0')
		if n > (^uint64(0)-dig)/10 {
			return false
		}
		n = n*10 + dig
	}
	return true
}

func TestRestoreResetRejectsInvalidQdrantIDBeforeWipe(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{1, 2}
	s := restoreServer(qdrantValidatingClient{Client: resettingClient{mem}})

	payload := `{
		"reset": true,
		"dimension": 2,
		"records": [
			{"embed_id": "not-a-uuid", "vector": [1, 2]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["error"] != "embed_id is not a valid Qdrant point id" {
		t.Fatalf("error %v", body["error"])
	}
	if mem.resetHit != 0 || len(mem.calls) != 0 {
		t.Fatalf("backend calls after invalid id: resetHit=%d calls=%v", mem.resetHit, mem.calls)
	}
	if got := mem.points["db1/existing"]; len(got) != 2 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
}

func TestRestoreResetRejectsDuplicateEmbedIDsBeforeWipe(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{1, 2}
	s := restoreServer(qdrantValidatingClient{Client: resettingClient{mem}})

	payload := `{
		"reset": true,
		"dimension": 2,
		"records": [
			{"embed_id": "550e8400-e29b-41d4-a716-446655440000", "vector": [1, 2]},
			{"embed_id": "550e8400-e29b-41d4-a716-446655440000", "vector": [3, 4]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["error"] != "restore record has duplicate embed_id" {
		t.Fatalf("error %v", body["error"])
	}
	if mem.resetHit != 0 || len(mem.calls) != 0 {
		t.Fatalf("backend calls after duplicate id: resetHit=%d calls=%v", mem.resetHit, mem.calls)
	}
	if got := mem.points["db1/existing"]; len(got) != 2 {
		t.Fatalf("existing point changed: %#v", mem.points)
	}
}

func TestRestoreResetAcceptsValidQdrantIDs(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{9, 9}
	s := restoreServer(qdrantValidatingClient{Client: resettingClient{mem}})

	payload := `{
		"reset": true,
		"dimension": 2,
		"records": [
			{"embed_id": "550e8400-e29b-41d4-a716-446655440000", "vector": [1, 2]},
			{"embed_id": "42", "vector": [3, 4]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if mem.resetHit != 1 {
		t.Fatalf("resetHit %d", mem.resetHit)
	}
	if len(mem.points["db1/550e8400-e29b-41d4-a716-446655440000"]) != 2 {
		t.Fatalf("replacement missing: %#v", mem.points)
	}
}

type stagedMemClient struct {
	*memClient
	failAfter int // fail on Nth staged upsert (1-based); 0 disables
	staged    int
}

func (c *stagedMemClient) StagedResetRestore(_ context.Context, collectionName string, dimension int, records []vector.RestorePoint) error {
	c.calls = append(c.calls, "staged-reset")
	staging := map[string][]float64{}
	stagingMeta := map[string]string{}
	for i, record := range records {
		c.staged++
		if c.failAfter > 0 && c.staged == c.failAfter {
			return errInjectedStageFailure
		}
		key := "stg/" + collectionName + "/" + record.EmbedID
		staging[key] = append([]float64(nil), record.Vector...)
		stagingMeta[key] = record.EmbedID
		_ = i
	}
	// Promote only after the full staging set is present.
	prefix := collectionName + "/"
	for key := range c.points {
		if strings.HasPrefix(key, prefix) {
			delete(c.points, key)
			delete(c.meta, key)
		}
	}
	for key, value := range staging {
		embedID := stagingMeta[key]
		liveKey := collectionName + "/" + embedID
		c.points[liveKey] = value
		c.meta[liveKey] = embedID
		c.calls = append(c.calls, "promote:"+embedID)
	}
	c.created[collectionName] = dimension
	return nil
}

var errInjectedStageFailure = errors.New("injected staging write failure")

func TestRestoreResetStagedKeepsLiveOnMidWriteFailure(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{9, 9}
	mem.meta["db1/existing"] = "existing"
	mem.created["db1"] = 2
	client := &stagedMemClient{memClient: mem, failAfter: 2}
	s := restoreServer(client)

	payload := `{
		"reset": true,
		"dimension": 2,
		"records": [
			{"embed_id": "550e8400-e29b-41d4-a716-446655440000", "vector": [1, 2]},
			{"embed_id": "550e8400-e29b-41d4-a716-446655440001", "vector": [3, 4]},
			{"embed_id": "550e8400-e29b-41d4-a716-446655440002", "vector": [5, 6]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if got := mem.points["db1/existing"]; len(got) != 2 || got[0] != 9 {
		t.Fatalf("live data changed after mid-write failure: %#v", mem.points)
	}
	for _, id := range []string{"550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440001", "550e8400-e29b-41d4-a716-446655440002"} {
		if _, ok := mem.points["db1/"+id]; ok {
			t.Fatalf("partial promote leaked into live: %#v", mem.points)
		}
	}
	kept := s.lookup("db1")
	if kept.Count != 4 || kept.Dimension != 2 {
		t.Fatalf("in-memory record changed: %+v", kept)
	}
}

func TestRestoreResetStagedReplacesAfterFullWrite(t *testing.T) {
	mem := newMemClient()
	mem.points["db1/existing"] = []float64{9, 9}
	client := &stagedMemClient{memClient: mem}
	s := restoreServer(client)

	payload := `{
		"name": "replaced",
		"reset": true,
		"dimension": 2,
		"records": [
			{"embed_id": "550e8400-e29b-41d4-a716-446655440000", "vector": [1, 2]},
			{"embed_id": "550e8400-e29b-41d4-a716-446655440001", "vector": [3, 4]}
		]
	}`
	rr := doRestore(s, payload)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	if _, still := mem.points["db1/existing"]; still {
		t.Fatalf("old point survived staged reset: %#v", mem.points)
	}
	if len(mem.points["db1/550e8400-e29b-41d4-a716-446655440000"]) != 2 {
		t.Fatalf("replacement missing: %#v", mem.points)
	}
	if mem.calls[0] != "staged-reset" {
		t.Fatalf("expected staged-reset first, got %v", mem.calls)
	}
	kept := s.lookup("db1")
	if kept.Name != "replaced" || kept.Count != 2 || kept.Dimension != 2 {
		t.Fatalf("collection record %+v", kept)
	}
}
