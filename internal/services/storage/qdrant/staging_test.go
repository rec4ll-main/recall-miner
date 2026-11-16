package qdrant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"recall-miner/internal/services/storage/vector"
)

type stagingFake struct {
	mu               sync.Mutex
	registry         map[string]map[string]any
	collections      map[string]int
	points           map[string]map[string]pointRec
	failAfterPut     int
	puts             int
	deleteLiveHit    int
	failCanonicalPut int  // fail on Nth upsert whose tenant is non-staging (compact)
	failAllCanonical bool // when set, every non-staging tenant upsert fails
	canonicalPuts    int
	gateAfterStaging int // after N staging-tenant point upserts, block until gate is closed
	stagingPointPuts int
	gate             chan struct{}
}

func newStagingFake() *stagingFake {
	return &stagingFake{
		registry:    map[string]map[string]any{},
		collections: map[string]int{registryCollection: 1},
		points:      map[string]map[string]pointRec{},
	}
}

func (f *stagingFake) handler(w http.ResponseWriter, r *http.Request) {
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
		f.puts++
		if f.failAfterPut > 0 && f.puts == f.failAfterPut {
			http.Error(w, `{"status":{"error":"injected put failure"}}`, http.StatusInternalServerError)
			return
		}
		coll := collectionFromPointsPath(path)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if coll == registryCollection {
			for _, raw := range payload["points"].([]any) {
				point, _ := raw.(map[string]any)
				id, _ := point["id"].(string)
				pl, _ := point["payload"].(map[string]any)
				f.registry[id] = cloneMap(pl)
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
			tenant, _ := pl[tenantPayloadKey].(string)
			if tenant != "" && !strings.HasSuffix(tenant, stagingTenantSuffix) {
				f.canonicalPuts++
				if f.failAllCanonical || (f.failCanonicalPut > 0 && f.canonicalPuts == f.failCanonicalPut) {
					http.Error(w, `{"status":{"error":"injected canonical put failure"}}`, http.StatusInternalServerError)
					return
				}
			}
			vec := toFloatSlice(point["vector"])
			f.points[coll][id] = pointRec{ID: id, Vector: vec, Payload: cloneMap(pl)}
			if tenant != "" && strings.HasSuffix(tenant, stagingTenantSuffix) {
				f.stagingPointPuts++
				if f.gateAfterStaging > 0 && f.stagingPointPuts == f.gateAfterStaging && f.gate != nil {
					gate := f.gate
					f.mu.Unlock()
					<-gate
					f.mu.Lock()
				}
			}
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
				tenant, _ := pt.Payload[tenantPayloadKey].(string)
				if tenant != "" && !strings.HasSuffix(tenant, stagingTenantSuffix) {
					f.deleteLiveHit++
				}
				delete(store, id)
			}
		}
		if coll == registryCollection {
			if rawPoints, ok := payload["points"].([]any); ok {
				for _, raw := range rawPoints {
					id := stringifyID(raw)
					delete(f.registry, id)
				}
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
			result = append(result, map[string]any{"id": pt.ID, "score": 1.0, "payload": pt.Payload})
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
	case r.Method == http.MethodPost && strings.Contains(path, "/points/scroll"):
		coll := collectionFromPointsPath(path)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		filter, _ := payload["filter"].(map[string]any)
		points := []map[string]any{}
		for _, pt := range f.points[coll] {
			if !matchFilter(pt, filter) {
				continue
			}
			points = append(points, map[string]any{
				"id":      pt.ID,
				"payload": pt.Payload,
				"vector":  pt.Vector,
			})
		}
		writeQJSON(w, map[string]any{"result": map[string]any{"points": points, "next_page_offset": nil}})
	default:
		writeQJSON(w, map[string]any{"result": true})
	}
}

func TestStagedResetRestoreKeepsLiveOnMidWriteFailure(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, map[string]any{"k": "live"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Fail during staging upserts: create + seed puts already happened; set failAfter to next puts.
	fake.mu.Lock()
	fake.failAfterPut = fake.puts + 3 // mark write + first staging point + fail on second staging point-ish
	// Be precise: next StagedResetRestore does recover (reads), mark writing (1 put), then staging points.
	// Count puts before call:
	before := fake.puts
	fake.failAfterPut = before + 2 // writing mark + first record, fail on second record put
	fake.mu.Unlock()

	err := client.StagedResetRestore(context.Background(), "db_a", 2, []vector.RestorePoint{
		{EmbedID: "550e8400-e29b-41d4-a716-446655440001", Vector: []float64{1, 0}},
		{EmbedID: "550e8400-e29b-41d4-a716-446655440002", Vector: []float64{0, 1}},
	})
	if err == nil {
		t.Fatal("expected staging failure")
	}

	hits, err := client.Search(context.Background(), "db_a", []float64{9, 9}, 10)
	if err != nil {
		t.Fatalf("search after failure: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != liveEmbed {
		t.Fatalf("live generation lost after mid-write failure: %#v", hits)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deleteLiveHit != 0 {
		t.Fatalf("live tenant deletes during failed staging: %d", fake.deleteLiveHit)
	}
}

func TestStagedResetRestorePromotesOnlyAfterFullSet(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	oldEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", oldEmbed, []float64{9, 9}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	newA := "550e8400-e29b-41d4-a716-446655440001"
	newB := "550e8400-e29b-41d4-a716-446655440002"
	if err := client.StagedResetRestore(context.Background(), "db_a", 2, []vector.RestorePoint{
		{EmbedID: newA, Vector: []float64{1, 0}, Metadata: map[string]any{"n": "a"}},
		{EmbedID: newB, Vector: []float64{0, 1}, Metadata: map[string]any{"n": "b"}},
	}); err != nil {
		t.Fatalf("staged reset: %v", err)
	}

	hits, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %#v", hits)
	}
	ids := map[string]bool{}
	for _, hit := range hits {
		ids[hit.ID] = true
	}
	if ids[oldEmbed] || !ids[newA] || !ids[newB] {
		t.Fatalf("ids after promote = %#v", ids)
	}
	count, err := client.GetCollectionStats(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d", count)
	}
}

func TestRecoverStagingFinishesReadyPromotionAfterCrash(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	oldEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", oldEmbed, []float64{9, 9}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Manually install a ready staging set and mark as if promote crashed after live delete.
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	stg := stagingTenant("db_a")
	newA := "550e8400-e29b-41d4-a716-446655440001"
	newB := "550e8400-e29b-41d4-a716-446655440002"
	if err := client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, newA, []float64{1, 0}, map[string]any{"n": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, newB, []float64{0, 1}, map[string]any{"n": "b"}); err != nil {
		t.Fatal(err)
	}
	if err := client.upsertStagingMark(context.Background(), "db_a", entry.PhysicalCollection, 2, 2, stagingStatusPromoting); err != nil {
		t.Fatal(err)
	}
	// Simulate crash after live tenant was deleted during promote.
	if err := client.deleteByTenant(context.Background(), entry.PhysicalCollection, "db_a"); err != nil {
		t.Fatal(err)
	}

	// Restart path: Search triggers RecoverStaging and should restore the staged generation.
	hits, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatalf("search/recover: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("recovered hits = %#v", hits)
	}
	ids := map[string]bool{}
	for _, hit := range hits {
		ids[hit.ID] = true
	}
	if !ids[newA] || !ids[newB] || ids[oldEmbed] {
		t.Fatalf("recovered ids = %#v", ids)
	}

	// Staging mark and staging tenant should be cleared.
	if _, ok, err := client.loadStagingMark(context.Background(), "db_a"); err != nil || ok {
		t.Fatalf("staging mark still present ok=%v err=%v", ok, err)
	}
	stgCount, err := client.countTenant(context.Background(), entry.PhysicalCollection, stg)
	if err != nil {
		t.Fatal(err)
	}
	if stgCount != 0 {
		t.Fatalf("staging tenant not cleaned: %d", stgCount)
	}
}

func TestRecoverStagingDiscardsIncompleteWritingMark(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	stg := stagingTenant("db_a")
	_ = client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, "550e8400-e29b-41d4-a716-446655440001", []float64{1, 0}, nil)
	if err := client.upsertStagingMark(context.Background(), "db_a", entry.PhysicalCollection, 2, 2, stagingStatusWriting); err != nil {
		t.Fatal(err)
	}

	if err := client.RecoverStaging(context.Background(), "db_a"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	hits, err := client.Search(context.Background(), "db_a", []float64{9, 9}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != liveEmbed {
		t.Fatalf("live changed after discarding writing staging: %#v", hits)
	}
	stgCount, _ := client.countTenant(context.Background(), entry.PhysicalCollection, stg)
	if stgCount != 0 {
		t.Fatalf("writing staging not discarded: %d", stgCount)
	}
}

func TestActiveWritingStageSurvivesSearchAndStats(t *testing.T) {
	fake := newStagingFake()
	gate := make(chan struct{})
	fake.gate = gate
	fake.gateAfterStaging = 1
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, map[string]any{"k": "live"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	newA := "550e8400-e29b-41d4-a716-446655440001"
	newB := "550e8400-e29b-41d4-a716-446655440002"
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.StagedResetRestore(context.Background(), "db_a", 2, []vector.RestorePoint{
			{EmbedID: newA, Vector: []float64{1, 0}, Metadata: map[string]any{"n": "a"}},
			{EmbedID: newB, Vector: []float64{0, 1}, Metadata: map[string]any{"n": "b"}},
		})
	}()

	// Wait until the restore has written its first staging point and blocked.
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		n := fake.stagingPointPuts
		fake.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for first staging write")
		}
		time.Sleep(5 * time.Millisecond)
	}

	hits, err := client.Search(context.Background(), "db_a", []float64{9, 9}, 10)
	if err != nil {
		t.Fatalf("search during active restore: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != liveEmbed {
		t.Fatalf("live generation not retained during active restore: %#v", hits)
	}
	count, err := client.GetCollectionStats(context.Background(), "db_a")
	if err != nil {
		t.Fatalf("stats during active restore: %v", err)
	}
	if count != 1 {
		t.Fatalf("stats during active restore = %d", count)
	}

	mark, ok, err := client.loadStagingMark(context.Background(), "db_a")
	if err != nil || !ok || mark.Status != stagingStatusWriting {
		t.Fatalf("writing stage discarded during reads: ok=%v mark=%#v err=%v", ok, mark, err)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	stgCount, err := client.countTenant(context.Background(), entry.PhysicalCollection, stagingTenant("db_a"))
	if err != nil {
		t.Fatal(err)
	}
	if stgCount < 1 {
		t.Fatalf("staging points discarded during reads: %d", stgCount)
	}

	close(gate)
	if err := <-errCh; err != nil {
		t.Fatalf("restore after resume: %v", err)
	}
	hits, err = client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("after resume hits = %#v", hits)
	}
}

func TestAbandonedWritingStageIsReclaimed(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	stg := stagingTenant("db_a")
	_ = client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, "550e8400-e29b-41d4-a716-446655440001", []float64{1, 0}, nil)
	if err := client.upsertStagingMark(context.Background(), "db_a", entry.PhysicalCollection, 2, 2, stagingStatusWriting); err != nil {
		t.Fatal(err)
	}

	hits, err := client.Search(context.Background(), "db_a", []float64{9, 9}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != liveEmbed {
		t.Fatalf("live changed after reclaiming abandoned writing: %#v", hits)
	}
	if _, ok, err := client.loadStagingMark(context.Background(), "db_a"); err != nil || ok {
		t.Fatalf("abandoned writing mark not cleared ok=%v err=%v", ok, err)
	}
	stgCount, _ := client.countTenant(context.Background(), entry.PhysicalCollection, stg)
	if stgCount != 0 {
		t.Fatalf("abandoned writing staging not discarded: %d", stgCount)
	}
}

func TestSameDimensionPromoteKeepsGenerationOnCompactFailure(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, map[string]any{"k": "live"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Arm sticky compact failure only after seeding so RestoreVector can succeed.
	fake.mu.Lock()
	fake.failAllCanonical = true
	fake.mu.Unlock()

	newA := "550e8400-e29b-41d4-a716-446655440001"
	newB := "550e8400-e29b-41d4-a716-446655440002"
	err := client.StagedResetRestore(context.Background(), "db_a", 2, []vector.RestorePoint{
		{EmbedID: newA, Vector: []float64{1, 0}, Metadata: map[string]any{"n": "a"}},
		{EmbedID: newB, Vector: []float64{0, 1}, Metadata: map[string]any{"n": "b"}},
	})
	if err == nil {
		t.Fatal("expected compact failure")
	}

	// Keep compact failure active: public reads must still serve the selected generation.
	hits, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatalf("search must succeed against selected generation while compact fails: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected complete replacement generation after switch, got %#v", hits)
	}
	ids := map[string]bool{}
	for _, hit := range hits {
		ids[hit.ID] = true
	}
	if !ids[newA] || !ids[newB] {
		t.Fatalf("replacement generation incomplete: %#v", ids)
	}
	count, err := client.GetCollectionStats(context.Background(), "db_a")
	if err != nil {
		t.Fatalf("stats must succeed against selected generation while compact fails: %v", err)
	}
	if count != 2 {
		t.Fatalf("stats while compact fails = %d", count)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	if entry.liveTenant() != stagingTenant("db_a") {
		t.Fatalf("after compact failure active tenant = %q", entry.liveTenant())
	}
}

func TestPromoteFailureBeforeSwitchKeepsPreviousGeneration(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	stg := stagingTenant("db_a")
	newA := "550e8400-e29b-41d4-a716-446655440001"
	if err := client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, newA, []float64{1, 0}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.upsertStagingMark(context.Background(), "db_a", entry.PhysicalCollection, 2, 1, stagingStatusPromoting); err != nil {
		t.Fatal(err)
	}

	// Fail the generation-switch registry upsert; keep it failing across recovery.
	fake.mu.Lock()
	fake.failAfterPut = fake.puts + 1
	fake.mu.Unlock()

	if err := client.RecoverStaging(context.Background(), "db_a"); err == nil {
		t.Fatal("expected switch failure")
	}
	entry, err = client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	if entry.liveTenant() != "db_a" {
		t.Fatalf("switch failure moved active tenant to %q", entry.liveTenant())
	}
	liveCount, err := client.countTenant(context.Background(), entry.PhysicalCollection, "db_a")
	if err != nil || liveCount != 1 {
		t.Fatalf("previous generation lost count=%d err=%v", liveCount, err)
	}

	// Re-arm so recovery's switch keeps failing; public reads must still serve the previous generation.
	fake.mu.Lock()
	fake.failAfterPut = fake.puts + 1
	fake.mu.Unlock()
	hits, err := client.Search(context.Background(), "db_a", []float64{9, 9}, 10)
	if err != nil {
		t.Fatalf("search must serve previous generation while switch fails: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != liveEmbed {
		t.Fatalf("previous generation not served during switch failure: %#v", hits)
	}
	entry, err = client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	if entry.liveTenant() != "db_a" {
		t.Fatalf("active tenant changed during failed recovery to %q", entry.liveTenant())
	}
	liveCount, err = client.countTenant(context.Background(), entry.PhysicalCollection, "db_a")
	if err != nil || liveCount != 1 {
		t.Fatalf("previous generation lost after failed read recovery count=%d err=%v", liveCount, err)
	}

	fake.mu.Lock()
	fake.failAfterPut = 0
	fake.mu.Unlock()
	hits, err = client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatalf("recover after clearing failure: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != newA {
		t.Fatalf("recovered hits = %#v", hits)
	}
}

func TestRecoverStagingIdempotentAroundGenerationSwitch(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	oldEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", oldEmbed, []float64{9, 9}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	stg := stagingTenant("db_a")
	newA := "550e8400-e29b-41d4-a716-446655440001"
	newB := "550e8400-e29b-41d4-a716-446655440002"
	if err := client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, newA, []float64{1, 0}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, newB, []float64{0, 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.upsertStagingMark(context.Background(), "db_a", entry.PhysicalCollection, 2, 2, stagingStatusPromoting); err != nil {
		t.Fatal(err)
	}

	// Interrupt after generation switch, before compact completes: flip active to staging only.
	if err := client.ensureLiveRegistry(context.Background(), "db_a", entry.PhysicalCollection, 2, stg); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := client.RecoverStaging(context.Background(), "db_a"); err != nil {
			t.Fatalf("recover #%d: %v", i, err)
		}
	}
	hits, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %#v", hits)
	}
	entry, err = client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	if entry.liveTenant() != "db_a" {
		t.Fatalf("active tenant after recover = %q", entry.liveTenant())
	}
	if _, ok, err := client.loadStagingMark(context.Background(), "db_a"); err != nil || ok {
		t.Fatalf("staging mark remain ok=%v err=%v", ok, err)
	}
}

func TestStaleRecoveryCannotUndoCompletedPromotion(t *testing.T) {
	run := func(t *testing.T, viaStats bool) {
		t.Helper()
		fake := newStagingFake()
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		t.Cleanup(srv.Close)
		client := newQClient(t, srv)

		if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
			t.Fatalf("create: %v", err)
		}
		liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
		if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, map[string]any{"k": "live"}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		newA := "550e8400-e29b-41d4-a716-446655440001"
		newB := "550e8400-e29b-41d4-a716-446655440002"
		aAtPromote := make(chan struct{})
		bScrolled := make(chan struct{})
		aDone := make(chan struct{})
		var promoteSeen int32

		client.testBeforePromote = func() {
			if atomic.CompareAndSwapInt32(&promoteSeen, 0, 1) {
				close(aAtPromote)
				<-bScrolled
			}
		}
		client.testAfterRecoverScroll = func() {
			close(bScrolled)
			<-aDone
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- client.StagedResetRestore(context.Background(), "db_a", 2, []vector.RestorePoint{
				{EmbedID: newA, Vector: []float64{1, 0}, Metadata: map[string]any{"n": "a"}},
				{EmbedID: newB, Vector: []float64{0, 1}, Metadata: map[string]any{"n": "b"}},
			})
		}()

		select {
		case <-aAtPromote:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for restore promote")
		}

		readErr := make(chan error, 1)
		go func() {
			if viaStats {
				_, err := client.GetCollectionStats(context.Background(), "db_a")
				readErr <- err
				return
			}
			_, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
			readErr <- err
		}()

		select {
		case <-bScrolled:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for recovery scroll")
		}

		if err := <-errCh; err != nil {
			t.Fatalf("restore A: %v", err)
		}
		close(aDone)

		if err := <-readErr; err != nil {
			t.Fatalf("stale recovery read: %v", err)
		}

		hits, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
		if err != nil {
			t.Fatalf("final search: %v", err)
		}
		if len(hits) != 2 {
			t.Fatalf("completed generation lost: %#v", hits)
		}
		ids := map[string]bool{}
		for _, hit := range hits {
			ids[hit.ID] = true
		}
		if !ids[newA] || !ids[newB] || ids[liveEmbed] {
			t.Fatalf("unexpected generation after stale recovery: %#v", ids)
		}
		entry, err := client.registryEntry(context.Background(), "db_a")
		if err != nil {
			t.Fatal(err)
		}
		if entry.liveTenant() != "db_a" {
			t.Fatalf("active tenant = %q", entry.liveTenant())
		}
		if _, ok, err := client.loadStagingMark(context.Background(), "db_a"); err != nil || ok {
			t.Fatalf("staging mark remain ok=%v err=%v", ok, err)
		}
		stgCount, err := client.countTenant(context.Background(), entry.PhysicalCollection, stagingTenant("db_a"))
		if err != nil || stgCount != 0 {
			t.Fatalf("staging tenant linger count=%d err=%v", stgCount, err)
		}
	}

	t.Run("Search", func(t *testing.T) { run(t, false) })
	t.Run("GetCollectionStats", func(t *testing.T) { run(t, true) })
}

func TestStaleRecoveryCanonicalCopyFailureLeavesCompletedGeneration(t *testing.T) {
	for _, failNth := range []int{1, 2} {
		failNth := failNth
		t.Run("failCanonicalPut="+itoa(failNth), func(t *testing.T) {
			fake := newStagingFake()
			srv := httptest.NewServer(http.HandlerFunc(fake.handler))
			t.Cleanup(srv.Close)
			client := newQClient(t, srv)

			if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
				t.Fatalf("create: %v", err)
			}
			liveEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
			if err := client.RestoreVector(context.Background(), "db_a", liveEmbed, []float64{9, 9}, nil); err != nil {
				t.Fatalf("seed: %v", err)
			}

			newA := "550e8400-e29b-41d4-a716-446655440001"
			newB := "550e8400-e29b-41d4-a716-446655440002"
			aAtPromote := make(chan struct{})
			bScrolled := make(chan struct{})
			aDone := make(chan struct{})
			var promoteSeen int32

			client.testBeforePromote = func() {
				if atomic.CompareAndSwapInt32(&promoteSeen, 0, 1) {
					close(aAtPromote)
					<-bScrolled
				}
			}
			client.testAfterRecoverScroll = func() {
				close(bScrolled)
				<-aDone
				// After A finishes, arm sticky failure for B's compact attempts.
				fake.mu.Lock()
				if failNth == 1 {
					fake.failAllCanonical = true
				} else {
					fake.failCanonicalPut = fake.canonicalPuts + failNth
				}
				fake.mu.Unlock()
			}

			errCh := make(chan error, 1)
			go func() {
				errCh <- client.StagedResetRestore(context.Background(), "db_a", 2, []vector.RestorePoint{
					{EmbedID: newA, Vector: []float64{1, 0}},
					{EmbedID: newB, Vector: []float64{0, 1}},
				})
			}()

			select {
			case <-aAtPromote:
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for restore promote")
			}

			readDone := make(chan error, 1)
			go func() {
				_, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
				readDone <- err
			}()

			select {
			case <-bScrolled:
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for recovery scroll")
			}

			if err := <-errCh; err != nil {
				t.Fatalf("restore A: %v", err)
			}
			close(aDone)

			if err := <-readDone; err != nil {
				t.Fatalf("search after stale recovery: %v", err)
			}

			hits, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
			if err != nil {
				t.Fatalf("final search: %v", err)
			}
			if len(hits) != 2 {
				t.Fatalf("A's completed generation not queryable: %#v", hits)
			}
			entry, err := client.registryEntry(context.Background(), "db_a")
			if err != nil {
				t.Fatal(err)
			}
			if entry.liveTenant() == stagingTenant("db_a") {
				t.Fatalf("stale recovery reselected removed staging")
			}
			liveCount, err := client.countTenant(context.Background(), entry.PhysicalCollection, "db_a")
			if err != nil || liveCount != 2 {
				t.Fatalf("canonical generation damaged count=%d err=%v", liveCount, err)
			}
		})
	}
}

func TestConcurrentRecoverStagingAroundPromotionBoundaries(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	oldEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
	if err := client.RestoreVector(context.Background(), "db_a", oldEmbed, []float64{9, 9}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	stg := stagingTenant("db_a")
	newA := "550e8400-e29b-41d4-a716-446655440001"
	newB := "550e8400-e29b-41d4-a716-446655440002"
	if err := client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, newA, []float64{1, 0}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.upsertTenantPoint(context.Background(), entry.PhysicalCollection, stg, newB, []float64{0, 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.upsertStagingMark(context.Background(), "db_a", entry.PhysicalCollection, 2, 2, stagingStatusPromoting); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2; j++ {
				if err := client.RecoverStaging(context.Background(), "db_a"); err != nil {
					errs <- err
					return
				}
				if _, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10); err != nil {
					errs <- err
					return
				}
				if _, err := client.GetCollectionStats(context.Background(), "db_a"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent recover/read: %v", err)
	}

	hits, err := client.Search(context.Background(), "db_a", []float64{1, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %#v", hits)
	}
	entry, err = client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatal(err)
	}
	if entry.liveTenant() != "db_a" {
		t.Fatalf("active tenant = %q", entry.liveTenant())
	}
	if _, ok, err := client.loadStagingMark(context.Background(), "db_a"); err != nil || ok {
		t.Fatalf("staging mark remain ok=%v err=%v", ok, err)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// A delayed recovery caller that observed restore 1's pending stage must not
// promote that stale view over restore 2's stage once it gets the lock.
func TestStaleRecoveryCannotPromoteOldStageOverNewerRestore(t *testing.T) {
	run := func(t *testing.T, viaStats bool) {
		fake := newStagingFake()
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		t.Cleanup(srv.Close)
		client := newQClient(t, srv)
		ctx := context.Background()
		if err := client.CreateCollection(ctx, "db_a", 2); err != nil {
			t.Fatalf("create: %v", err)
		}

		var promoteCalls int32
		a1At, a1Go := make(chan struct{}), make(chan struct{})
		a2At, a2Go := make(chan struct{}), make(chan struct{})
		bAt, bGo := make(chan struct{}), make(chan struct{})
		client.testBeforePromote = func() {
			switch atomic.AddInt32(&promoteCalls, 1) {
			case 1:
				close(a1At)
				<-a1Go
			case 2:
				close(a2At)
				<-a2Go
			}
		}
		var bSeen int32
		client.testAfterRecoverScroll = func() {
			if atomic.CompareAndSwapInt32(&bSeen, 0, 1) {
				close(bAt)
				<-bGo
			}
		}

		gen1 := []vector.RestorePoint{{EmbedID: "550e8400-e29b-41d4-a716-446655440001", Vector: []float64{1, 0}}}
		gen2 := []vector.RestorePoint{
			{EmbedID: "550e8400-e29b-41d4-a716-446655440002", Vector: []float64{0, 1}},
			{EmbedID: "550e8400-e29b-41d4-a716-446655440003", Vector: []float64{1, 1}},
		}

		// Restore 1 reaches promoting; reader B observes that pending stage.
		a1 := make(chan error, 1)
		go func() { a1 <- client.StagedResetRestore(ctx, "db_a", 2, gen1) }()
		waitSignal(t, a1At, "restore 1 promoting")
		b := make(chan error, 1)
		go func() {
			if viaStats {
				_, err := client.GetCollectionStats(ctx, "db_a")
				b <- err
				return
			}
			_, err := client.Search(ctx, "db_a", []float64{1, 0}, 10)
			b <- err
		}()
		waitSignal(t, bAt, "reader observing pending stage")

		// Restore 1 completes through marker deletion, then restore 2 reaches
		// promoting before B resumes.
		close(a1Go)
		if err := waitErr(t, a1, "restore 1"); err != nil {
			t.Fatalf("restore 1: %v", err)
		}
		a2 := make(chan error, 1)
		go func() { a2 <- client.StagedResetRestore(ctx, "db_a", 2, gen2) }()
		waitSignal(t, a2At, "restore 2 promoting")

		close(bGo)
		if err := waitErr(t, b, "delayed reader"); err != nil {
			t.Fatalf("delayed reader: %v", err)
		}
		close(a2Go)
		if err := waitErr(t, a2, "restore 2"); err != nil {
			t.Fatalf("restore 2: %v", err)
		}

		hits, err := client.Search(ctx, "db_a", []float64{1, 0}, 10)
		if err != nil {
			t.Fatalf("final search: %v", err)
		}
		ids := map[string]bool{}
		for _, hit := range hits {
			ids[hit.ID] = true
		}
		if len(hits) != 2 || !ids[gen2[0].EmbedID] || !ids[gen2[1].EmbedID] {
			t.Fatalf("restore 2 succeeded but live generation is %#v", ids)
		}
		entry, err := client.registryEntry(ctx, "db_a")
		if err != nil {
			t.Fatal(err)
		}
		if entry.liveTenant() != "db_a" {
			t.Fatalf("active tenant = %q", entry.liveTenant())
		}
		if _, ok, err := client.loadStagingMark(ctx, "db_a"); err != nil || ok {
			t.Fatalf("staging mark remain ok=%v err=%v", ok, err)
		}
		if n, err := client.countTenant(ctx, entry.PhysicalCollection, stagingTenant("db_a")); err != nil || n != 0 {
			t.Fatalf("staging tenant linger count=%d err=%v", n, err)
		}
	}
	t.Run("Search", func(t *testing.T) { run(t, false) })
	t.Run("GetCollectionStats", func(t *testing.T) { run(t, true) })
}

// A read that resolved the selected staging generation keeps it until its
// query completes: a concurrent promotion cannot switch away from and reclaim
// it underneath the read.
func TestReadHoldsSelectedGenerationUntilQueryCompletes(t *testing.T) {
	run := func(t *testing.T, viaStats bool) {
		fake := newStagingFake()
		srv := httptest.NewServer(http.HandlerFunc(fake.handler))
		t.Cleanup(srv.Close)
		client := newQClient(t, srv)
		ctx := context.Background()
		if err := client.CreateCollection(ctx, "db_a", 2); err != nil {
			t.Fatalf("create: %v", err)
		}
		oldEmbed := "550e8400-e29b-41d4-a716-4466554400aa"
		if err := client.RestoreVector(ctx, "db_a", oldEmbed, []float64{9, 9}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
		entry, err := client.registryEntry(ctx, "db_a")
		if err != nil {
			t.Fatal(err)
		}
		stg := stagingTenant("db_a")
		newA := "550e8400-e29b-41d4-a716-446655440001"
		newB := "550e8400-e29b-41d4-a716-446655440002"
		for _, p := range []vector.RestorePoint{{EmbedID: newA, Vector: []float64{1, 0}}, {EmbedID: newB, Vector: []float64{0, 1}}} {
			if err := client.upsertTenantPoint(ctx, entry.PhysicalCollection, stg, p.EmbedID, p.Vector, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := client.upsertStagingMark(ctx, "db_a", entry.PhysicalCollection, 2, 2, stagingStatusPromoting); err != nil {
			t.Fatal(err)
		}
		// Interrupted after the generation switch: staging is selected.
		if err := client.ensureLiveRegistry(ctx, "db_a", entry.PhysicalCollection, 2, stg); err != nil {
			t.Fatal(err)
		}

		// Reader's own recovery hits a sticky compact failure, so it serves
		// the selected staging generation and pauses before querying.
		fake.mu.Lock()
		fake.failAllCanonical = true
		fake.mu.Unlock()
		readerAt, readerGo := make(chan struct{}), make(chan struct{})
		var readerSeen int32
		client.testAfterReadResolve = func() {
			if atomic.CompareAndSwapInt32(&readerSeen, 0, 1) {
				close(readerAt)
				<-readerGo
			}
		}
		type readResult struct {
			n   int
			ids map[string]bool
			err error
		}
		reader := make(chan readResult, 1)
		go func() {
			if viaStats {
				n, err := client.GetCollectionStats(ctx, "db_a")
				reader <- readResult{n: n, err: err}
				return
			}
			hits, err := client.Search(ctx, "db_a", []float64{1, 0}, 10)
			ids := map[string]bool{}
			for _, hit := range hits {
				ids[hit.ID] = true
			}
			reader <- readResult{n: len(hits), ids: ids, err: err}
		}()
		waitSignal(t, readerAt, "reader resolving selection")

		// Recovery can now compact; it must wait for the reader before
		// selecting canonical and reclaiming staging.
		fake.mu.Lock()
		fake.failAllCanonical = false
		fake.mu.Unlock()
		promoterAt := make(chan struct{})
		var promoterSeen int32
		client.testBeforeSelect = func(tenant string) {
			if tenant == "db_a" && atomic.CompareAndSwapInt32(&promoterSeen, 0, 1) {
				close(promoterAt)
			}
		}
		promoter := make(chan error, 1)
		go func() { promoter <- client.RecoverStaging(ctx, "db_a") }()
		waitSignal(t, promoterAt, "promoter reaching canonical selection")

		cur, err := client.registryEntry(ctx, "db_a")
		if err != nil {
			t.Fatal(err)
		}
		if cur.liveTenant() != stg {
			t.Fatalf("selection switched while a read held it: %q", cur.liveTenant())
		}
		if n, err := client.countTenant(ctx, entry.PhysicalCollection, stg); err != nil || n != 2 {
			t.Fatalf("selected staging reclaimed during read count=%d err=%v", n, err)
		}

		close(readerGo)
		res := <-reader
		if res.err != nil {
			t.Fatalf("reader: %v", res.err)
		}
		if res.n != 2 || (!viaStats && (!res.ids[newA] || !res.ids[newB])) {
			t.Fatalf("reader saw incomplete generation n=%d ids=%#v", res.n, res.ids)
		}
		if err := waitErr(t, promoter, "promoter"); err != nil {
			t.Fatalf("promoter: %v", err)
		}
		cur, err = client.registryEntry(ctx, "db_a")
		if err != nil {
			t.Fatal(err)
		}
		if cur.liveTenant() != "db_a" {
			t.Fatalf("active tenant after promotion = %q", cur.liveTenant())
		}
		if n, err := client.countTenant(ctx, entry.PhysicalCollection, "db_a"); err != nil || n != 2 {
			t.Fatalf("canonical count=%d err=%v", n, err)
		}
		if _, ok, err := client.loadStagingMark(ctx, "db_a"); err != nil || ok {
			t.Fatalf("staging mark remain ok=%v err=%v", ok, err)
		}
	}
	t.Run("Search", func(t *testing.T) { run(t, false) })
	t.Run("GetCollectionStats", func(t *testing.T) { run(t, true) })
}

// Restoring an empty database onto a miner that does not have it must create it (the validator
// rebuilds empty databases this way); restoring it empty over an existing one must empty it.
func TestStagedResetRestoreWithNoRecordsCreatesOrEmptiesTheDatabase(t *testing.T) {
	fake := newStagingFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)
	ctx := context.Background()

	if err := client.StagedResetRestore(ctx, "db_new", 2, nil); err != nil {
		t.Fatalf("empty restore of a missing database: %v", err)
	}
	info, err := client.GetCollectionInfo(ctx, "db_new")
	if err != nil || info.Dimension != 2 {
		t.Fatalf("the empty database must exist after the restore: %+v %v", info, err)
	}
	if n, err := client.GetCollectionStats(ctx, "db_new"); err != nil || n != 0 {
		t.Fatalf("the restored database must be empty: %d %v", n, err)
	}

	if err := client.CreateCollection(ctx, "db_old", 2); err != nil {
		t.Fatal(err)
	}
	if err := client.RestoreVector(ctx, "db_old", "550e8400-e29b-41d4-a716-4466554400bb", []float64{1, 0}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.StagedResetRestore(ctx, "db_old", 2, []vector.RestorePoint{}); err != nil {
		t.Fatalf("empty restore over an existing database: %v", err)
	}
	if n, err := client.GetCollectionStats(ctx, "db_old"); err != nil || n != 0 {
		t.Fatalf("an empty restore must leave the database empty: %d %v", n, err)
	}
}
