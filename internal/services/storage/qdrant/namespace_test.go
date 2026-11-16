package qdrant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type pointRec struct {
	ID      string
	Vector  []float64
	Payload map[string]any
}

type nsFake struct {
	mu          sync.Mutex
	registry    map[string]map[string]any
	collections map[string]int
	points      map[string]map[string]pointRec // physical collection -> id -> point
}

func newNSFake() *nsFake {
	return &nsFake{
		registry:    map[string]map[string]any{},
		collections: map[string]int{registryCollection: 1},
		points:      map[string]map[string]pointRec{},
	}
}

func (f *nsFake) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/collections/"):
		name := strings.TrimPrefix(path, "/collections/")
		if dim, ok := f.collections[name]; ok {
			writeQJSON(w, map[string]any{"result": map[string]any{
				"config": map[string]any{"params": map[string]any{"vectors": map[string]any{"size": dim}}},
			}})
			return
		}
		http.Error(w, `{"status":{"error":"not found"}}`, http.StatusNotFound)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/index"):
		writeQJSON(w, map[string]any{"result": true})
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/collections/") && !strings.Contains(path, "/points"):
		name := strings.TrimPrefix(path, "/collections/")
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		vectors, _ := payload["vectors"].(map[string]any)
		size, _ := vectors["size"].(float64)
		f.collections[name] = int(size)
		if f.points[name] == nil {
			f.points[name] = map[string]pointRec{}
		}
		writeQJSON(w, map[string]any{"result": true})
	case r.Method == http.MethodPut && strings.Contains(path, "/points"):
		coll := collectionFromPointsPath(path)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if coll == registryCollection {
			for _, raw := range payload["points"].([]any) {
				point, _ := raw.(map[string]any)
				id, _ := point["id"].(string)
				pl, _ := point["payload"].(map[string]any)
				f.registry[id] = pl
			}
			writeQJSON(w, map[string]any{"result": map[string]any{"status": "ok"}})
			return
		}
		if f.points[coll] == nil {
			f.points[coll] = map[string]pointRec{}
		}
		for _, raw := range payload["points"].([]any) {
			point, _ := raw.(map[string]any)
			id := stringifyID(point["id"])
			pl, _ := point["payload"].(map[string]any)
			vec := toFloatSlice(point["vector"])
			// Simulate Qdrant upsert overwrite by physical id only.
			f.points[coll][id] = pointRec{ID: id, Vector: vec, Payload: cloneMap(pl)}
		}
		writeQJSON(w, map[string]any{"result": map[string]any{"status": "ok"}})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/points") && !strings.Contains(path, "/points/"):
		coll := collectionFromPointsPath(path)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		ids, _ := payload["ids"].([]any)
		result := []map[string]any{}
		if coll == registryCollection {
			for _, raw := range ids {
				id, _ := raw.(string)
				if pl, ok := f.registry[id]; ok {
					result = append(result, map[string]any{"id": id, "payload": pl})
				}
			}
		} else {
			store := f.points[coll]
			for _, raw := range ids {
				id := stringifyID(raw)
				if pt, ok := store[id]; ok {
					result = append(result, map[string]any{"id": pt.ID, "payload": pt.Payload})
				}
			}
		}
		writeQJSON(w, map[string]any{"result": result})
	case r.Method == http.MethodPost && strings.Contains(path, "/points/delete"):
		coll := collectionFromPointsPath(path)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		filter, _ := payload["filter"].(map[string]any)
		store := f.points[coll]
		for id, pt := range store {
			if matchFilter(pt, filter) {
				delete(store, id)
			}
		}
		writeQJSON(w, map[string]any{"result": map[string]any{"status": "ok"}})
	case r.Method == http.MethodPost && strings.Contains(path, "/points/search"):
		coll := collectionFromPointsPath(path)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		filter, _ := payload["filter"].(map[string]any)
		limit, _ := payload["limit"].(float64)
		result := []map[string]any{}
		for _, pt := range f.points[coll] {
			if !matchFilter(pt, filter) {
				continue
			}
			result = append(result, map[string]any{
				"id":      pt.ID,
				"score":   1.0,
				"payload": pt.Payload,
			})
			if limit > 0 && len(result) >= int(limit) {
				break
			}
		}
		writeQJSON(w, map[string]any{"result": result})
	case r.Method == http.MethodPost && strings.Contains(path, "/points/count"):
		coll := collectionFromPointsPath(path)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		filter, _ := payload["filter"].(map[string]any)
		count := 0
		for _, pt := range f.points[coll] {
			if matchFilter(pt, filter) {
				count++
			}
		}
		writeQJSON(w, map[string]any{"result": map[string]any{"count": count}})
	default:
		writeQJSON(w, map[string]any{"result": true})
	}
}

func collectionFromPointsPath(path string) string {
	// /collections/{name}/points...
	parts := strings.Split(path, "/")
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

func stringifyID(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		b, _ := json.Marshal(t); return string(b)
	default:
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}
}

func toFloatSlice(v any) []float64 {
	arr, _ := v.([]any)
	out := make([]float64, 0, len(arr))
	for _, x := range arr {
		if n, ok := x.(float64); ok {
			out = append(out, n)
		}
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func matchFilter(pt pointRec, filter map[string]any) bool {
	if filter == nil {
		return true
	}
	must, _ := filter["must"].([]any)
	for _, raw := range must {
		cond, _ := raw.(map[string]any)
		if should, ok := cond["should"].([]any); ok {
			okAny := false
			for _, sraw := range should {
				scond, _ := sraw.(map[string]any)
				if matchCondition(pt, scond) {
					okAny = true
					break
				}
			}
			if !okAny {
				return false
			}
			continue
		}
		if !matchCondition(pt, cond) {
			return false
		}
	}
	return true
}

func matchCondition(pt pointRec, cond map[string]any) bool {
	if ids, ok := cond["has_id"].([]any); ok {
		for _, raw := range ids {
			if stringifyID(raw) == pt.ID {
				return true
			}
		}
		// also string slice decoded differently
		return false
	}
	if ids, ok := cond["has_id"].([]string); ok {
		for _, id := range ids {
			if id == pt.ID {
				return true
			}
		}
		return false
	}
	key, _ := cond["key"].(string)
	match, _ := cond["match"].(map[string]any)
	want := match["value"]
	got := pt.Payload[key]
	return got == want
}

func TestPointIDForTenantDiffersAcrossDatabases(t *testing.T) {
	a := pointIDForTenant("db_a", "550e8400-e29b-41d4-a716-446655440000")
	b := pointIDForTenant("db_b", "550e8400-e29b-41d4-a716-446655440000")
	if a == b {
		t.Fatalf("expected different physical ids, both %s", a)
	}
	if a == "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatal("physical id should not equal external embed id")
	}
}

func TestRestoreVectorIsolatesTenantsWithSameEmbedID(t *testing.T) {
	fake := newNSFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := client.CreateCollection(context.Background(), "db_b", 2); err != nil {
		t.Fatalf("create b: %v", err)
	}

	embed := "550e8400-e29b-41d4-a716-446655440000"
	if err := client.RestoreVector(context.Background(), "db_a", embed, []float64{1, 0}, map[string]any{"from": "a"}); err != nil {
		t.Fatalf("restore a: %v", err)
	}
	if err := client.RestoreVector(context.Background(), "db_b", embed, []float64{0, 1}, map[string]any{"from": "b"}); err != nil {
		t.Fatalf("restore b: %v", err)
	}

	hitsA, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatalf("search a: %v", err)
	}
	hitsB, err := client.Search(context.Background(), "db_b", []float64{0, 1}, 10)
	if err != nil {
		t.Fatalf("search b: %v", err)
	}
	if len(hitsA) != 1 || hitsA[0].ID != embed {
		t.Fatalf("hitsA = %#v", hitsA)
	}
	if len(hitsB) != 1 || hitsB[0].ID != embed {
		t.Fatalf("hitsB = %#v", hitsB)
	}
	if hitsA[0].Metadata["from"] != "a" || hitsB[0].Metadata["from"] != "b" {
		t.Fatalf("metadata crossed tenants: a=%v b=%v", hitsA[0].Metadata, hitsB[0].Metadata)
	}

	if err := client.DeleteVector(context.Background(), "db_b", embed); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	hitsA, err = client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatalf("search a after delete b: %v", err)
	}
	if len(hitsA) != 1 || hitsA[0].ID != embed {
		t.Fatalf("db_a lost data after deleting db_b: %#v", hitsA)
	}
	hitsB, err = client.Search(context.Background(), "db_b", []float64{0, 1}, 10)
	if err != nil {
		t.Fatalf("search b after delete: %v", err)
	}
	if len(hitsB) != 0 {
		t.Fatalf("db_b still has points: %#v", hitsB)
	}
}

func TestRestoreVectorMigratesLegacyPoint(t *testing.T) {
	fake := newNSFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	embed := "550e8400-e29b-41d4-a716-446655440001"
	// Seed a legacy unnamespaced point (physical id == external embed id).
	fake.mu.Lock()
	if fake.points[entry.PhysicalCollection] == nil {
		fake.points[entry.PhysicalCollection] = map[string]pointRec{}
	}
	fake.points[entry.PhysicalCollection][embed] = pointRec{
		ID:     embed,
		Vector: []float64{9, 9},
		Payload: map[string]any{
			tenantPayloadKey: "db_a",
			"metadata":       map[string]any{"legacy": true},
		},
	}
	fake.mu.Unlock()

	// Search still returns the legacy external id.
	hits, err := client.Search(context.Background(), "db_a", []float64{9, 9}, 10)
	if err != nil {
		t.Fatalf("search legacy: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != embed {
		t.Fatalf("legacy search hits = %#v", hits)
	}

	if err := client.RestoreVector(context.Background(), "db_a", embed, []float64{1, 2}, map[string]any{"legacy": false}); err != nil {
		t.Fatalf("restore migrate: %v", err)
	}

	physical := pointIDForTenant("db_a", embed)
	fake.mu.Lock()
	store := fake.points[entry.PhysicalCollection]
	if _, ok := store[embed]; ok {
		fake.mu.Unlock()
		t.Fatalf("legacy physical id still present: %#v", store)
	}
	pt, ok := store[physical]
	fake.mu.Unlock()
	if !ok {
		t.Fatalf("namespaced point missing: %#v", store)
	}
	if pt.Payload[embedPayloadKey] != embed {
		t.Fatalf("embed payload = %#v", pt.Payload)
	}

	hits, err = client.Search(context.Background(), "db_a", []float64{1, 2}, 10)
	if err != nil {
		t.Fatalf("search after migrate: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != embed {
		t.Fatalf("post-migrate hits = %#v", hits)
	}
}

func TestRestoreVectorRejectsForeignTenantLegacyID(t *testing.T) {
	fake := newNSFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := client.CreateCollection(context.Background(), "db_b", 2); err != nil {
		t.Fatalf("create b: %v", err)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	embed := "550e8400-e29b-41d4-a716-446655440002"
	fake.mu.Lock()
	if fake.points[entry.PhysicalCollection] == nil {
		fake.points[entry.PhysicalCollection] = map[string]pointRec{}
	}
	fake.points[entry.PhysicalCollection][embed] = pointRec{
		ID:      embed,
		Vector:  []float64{1, 1},
		Payload: map[string]any{tenantPayloadKey: "db_a"},
	}
	fake.mu.Unlock()

	err = client.RestoreVector(context.Background(), "db_b", embed, []float64{2, 2}, nil)
	if err == nil || !strings.Contains(err.Error(), "owned by another database") {
		t.Fatalf("expected ownership conflict, got %v", err)
	}
}
