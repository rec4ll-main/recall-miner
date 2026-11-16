package api

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"recall-miner/internal/retention"
	"recall-miner/internal/services/storage/vector"
)

// sharedStore is one storage (a Qdrant) that several db services can point at.
type sharedStore struct {
	mu  sync.Mutex
	dbs map[string]map[string]map[string]any
}

func newSharedStore() *sharedStore { return &sharedStore{dbs: map[string]map[string]map[string]any{}} }

func (s *sharedStore) CreateCollection(_ context.Context, name string, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbs[name] == nil {
		s.dbs[name] = map[string]map[string]any{}
	}
	return nil
}
func (s *sharedStore) DropCollection(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbs[name] == nil {
		return vector.CollectionNotFound(name)
	}
	delete(s.dbs, name)
	return nil
}
func (s *sharedStore) GetCollectionInfo(_ context.Context, name string) (vector.CollectionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbs[name] == nil {
		return vector.CollectionInfo{}, vector.CollectionNotFound(name)
	}
	return vector.CollectionInfo{Name: name, Dimension: 4}, nil
}
func (s *sharedStore) GetCollectionStats(_ context.Context, name string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbs[name] == nil {
		return 0, vector.CollectionNotFound(name)
	}
	return len(s.dbs[name]), nil
}
func (s *sharedStore) ListCollections(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for n := range s.dbs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}
func (s *sharedStore) InsertVector(ctx context.Context, name string, v []float64, md map[string]any) (string, error) {
	return "x", s.RestoreVector(ctx, name, "x", v, md)
}
func (s *sharedStore) RestoreVector(_ context.Context, name, id string, _ []float64, md map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbs[name] == nil {
		return vector.CollectionNotFound(name)
	}
	s.dbs[name][id] = md
	return nil
}
func (s *sharedStore) DeleteVector(_ context.Context, name, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dbs[name], id)
	return nil
}
func (s *sharedStore) Search(_ context.Context, name string, _ []float64, _ int) ([]vector.SearchHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbs[name] == nil {
		return nil, vector.CollectionNotFound(name)
	}
	hits := []vector.SearchHit{}
	for id, md := range s.dbs[name] {
		hits = append(hits, vector.SearchHit{ID: id, Distance: 1, Metadata: md})
	}
	return hits, nil
}
func (s *sharedStore) StagedResetRestore(ctx context.Context, name string, dim int, points []vector.RestorePoint) error {
	s.mu.Lock()
	s.dbs[name] = map[string]map[string]any{}
	s.mu.Unlock()
	for _, p := range points {
		if err := s.RestoreVector(ctx, name, p.EmbedID, p.Vector, p.Metadata); err != nil {
			return err
		}
	}
	return nil
}
func (s *sharedStore) ResetCollection(ctx context.Context, name string, dim int) error {
	return s.StagedResetRestore(ctx, name, dim, nil)
}
func (s *sharedStore) RetentionRecord(context.Context, string, string) (retention.Record, error) {
	return retention.Record{}, retention.ErrNotFound
}
func (s *sharedStore) ValidateRestorePointID(string) error { return nil }

func prefixed(t *testing.T, store vector.Client, prefix string) vector.Client {
	t.Helper()
	c, err := newPrefixedClient(store, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPrefixesKeepMinersOnOneStorageApart(t *testing.T) {
	ctx := context.Background()
	store := newSharedStore()
	a, b := prefixed(t, store, "m1"), prefixed(t, store, "m2")
	for _, c := range []vector.Client{a, b} {
		if err := c.CreateCollection(ctx, "db_1", 4); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.RestoreVector(ctx, "db_1", "r1", []float64{1, 0, 0, 0}, map[string]any{"text": "kept"}); err != nil {
		t.Fatal(err)
	}
	// The API deletes miner b's copy of db_1 (a replaced copy's clean-up).
	if err := b.DropCollection(ctx, "db_1"); err != nil {
		t.Fatal(err)
	}
	if n, err := a.GetCollectionStats(ctx, "db_1"); err != nil || n != 1 {
		t.Fatalf("miner a's copy must survive a delete through miner b: %d %v", n, err)
	}
	if _, err := b.GetCollectionInfo(ctx, "db_1"); !errors.Is(err, vector.ErrCollectionNotFound) {
		t.Fatalf("miner b's copy must be gone, got %v", err)
	}
	// A restore through b never touches a's records.
	if err := b.(interface {
		StagedResetRestore(context.Context, string, int, []vector.RestorePoint) error
	}).StagedResetRestore(ctx, "db_1", 4, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.GetCollectionStats(ctx, "db_1"); n != 1 {
		t.Fatalf("a restore through miner b must not reset miner a's copy, a has %d", n)
	}
	la, _ := a.ListCollections(ctx)
	if len(la) != 1 || la[0] != "db_1" {
		t.Fatalf("each miner lists only its own databases, without the prefix: %v", la)
	}
	info, _ := a.GetCollectionInfo(ctx, "db_1")
	if info.Name != "db_1" {
		t.Fatalf("names come back without the prefix, got %q", info.Name)
	}
}

func TestPrefixNeedsTheQdrantClient(t *testing.T) {
	if _, err := newPrefixedClient(newMemClient(), "m1"); err == nil {
		t.Fatal("a backend without the Qdrant features must refuse STORAGE_PREFIX")
	}
	c, err := newPrefixedClient(newMemClient(), "")
	if err != nil || c == nil {
		t.Fatalf("no prefix keeps the backend as it is: %v", err)
	}
}

func claimFor(store vector.Client, owner, prefix string, now *time.Time) *StorageClaim {
	c, _ := newPrefixedClient(store, prefix)
	return &StorageClaim{client: c, owner: owner, prefix: prefix, now: func() time.Time { return *now }}
}

func TestStorageClaimRefusesASecondServiceWithTheSamePrefix(t *testing.T) {
	ctx := context.Background()
	store := newSharedStore()
	now := time.Unix(1_800_000_000, 0)
	a := claimFor(store, "http://192.0.2.10:9100", "", &now)
	b := claimFor(store, "http://192.0.2.10:9101", "", &now)
	if err := a.Check(ctx); err != nil {
		t.Fatalf("first service: %v", err)
	}
	if err := a.Check(ctx); err != nil {
		t.Fatalf("a restart of the same service keeps its claim: %v", err)
	}
	err := b.Check(ctx)
	var shared *SharedStorageError
	if !errors.As(err, &shared) {
		t.Fatalf("a second service on the same storage without a prefix must be refused, got %v", err)
	}
	// With different prefixes both may share the storage.
	pa := claimFor(store, "http://192.0.2.10:9100", "m0", &now)
	pb := claimFor(store, "http://192.0.2.10:9101", "m1", &now)
	if err := pa.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pb.Check(ctx); err != nil {
		t.Fatalf("different prefixes must share one storage: %v", err)
	}
	// A claim nobody refreshed for longer than the window is taken over (the old service is gone).
	now = now.Add(storageClaimFresh + time.Second)
	if err := b.Check(ctx); err != nil {
		t.Fatalf("a stale claim must not block: %v", err)
	}
}
