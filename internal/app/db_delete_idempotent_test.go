package app

// M5: miner DELETE /db/{id} is idempotent through the signed server routes
// and the real storage API (in-memory vector client): an already-absent DB
// returns the documented 404, a lost success response can be retried, an
// unpinned DB is deleted on whichever backend holds it, backend errors are
// never reported as not-found, and unauthorized signers stay denied.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"

	"recall-miner/internal/registry"
	storageapi "recall-miner/internal/services/storage/api"
	storagecfg "recall-miner/internal/services/storage/config"
	"recall-miner/internal/services/storage/vector"
)

type m5Store struct {
	mu          sync.Mutex
	collections map[string]int
	drops       atomic.Int64
	failDrops   atomic.Bool
}

func (s *m5Store) CreateCollection(_ context.Context, name string, dim int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collections[name] = dim
	return nil
}

func (s *m5Store) DropCollection(_ context.Context, name string) error {
	s.drops.Add(1)
	if s.failDrops.Load() {
		return io.ErrUnexpectedEOF
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.collections[name]; !ok {
		return vector.CollectionNotFound(name)
	}
	delete(s.collections, name)
	return nil
}

func (s *m5Store) has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.collections[name]
	return ok
}

func (s *m5Store) GetCollectionInfo(_ context.Context, name string) (vector.CollectionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dim, ok := s.collections[name]
	if !ok {
		return vector.CollectionInfo{}, vector.CollectionNotFound(name)
	}
	return vector.CollectionInfo{Name: name, Dimension: dim}, nil
}
func (s *m5Store) GetCollectionStats(context.Context, string) (int, error) { return 0, nil }
func (s *m5Store) ListCollections(context.Context) ([]string, error)       { return nil, nil }
func (s *m5Store) InsertVector(context.Context, string, []float64, map[string]any) (string, error) {
	return "e", nil
}
func (s *m5Store) RestoreVector(context.Context, string, string, []float64, map[string]any) error {
	return nil
}
func (s *m5Store) DeleteVector(context.Context, string, string) error { return nil }
func (s *m5Store) Search(context.Context, string, []float64, int) ([]vector.SearchHit, error) {
	return nil, nil
}

type m5Fixture struct {
	srv   *server
	alice signature.KeyringPair
	bob   signature.KeyringPair
	backs map[string]*m5Store
}

// newM5Fixture registers one storage backend per name, in order.
func newM5Fixture(t *testing.T, names ...string) *m5Fixture {
	t.Helper()
	srv := testServer(t)
	alice, err := signature.KeyringPairFromSecret("//Alice", 42)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := signature.KeyringPairFromSecret("//Bob", 42)
	if err != nil {
		t.Fatal(err)
	}
	srv.cfg.MinerHotkey = alice.Address
	srv.cfg.ApplicationProberHotkey = alice.Address
	f := &m5Fixture{srv: srv, alice: alice, bob: bob, backs: map[string]*m5Store{}}
	for _, name := range names {
		store := &m5Store{collections: map[string]int{}}
		h := storageapi.NewServerWithClient(storagecfg.Config{InternalAPIKey: srv.cfg.InternalAPIKey}, slog.New(slog.NewTextHandler(io.Discard, nil)), store)
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		srv.handlers.Registry().Register(registry.RegisterRequest{
			Name:         name,
			URL:          ts.URL,
			Capabilities: []string{"db"},
			Metadata:     map[string]any{"api": "recall-db-v1"},
		})
		f.backs[name] = store
	}
	return f
}

func (f *m5Fixture) del(t *testing.T, signer signature.KeyringPair, dbID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/db/"+dbID, nil)
	setEpistulaHeaders(t, req, signer, f.alice.Address, nil)
	rec := httptest.NewRecorder()
	f.srv.routes().ServeHTTP(rec, req)
	return rec
}

// Delete, then the same delete again (the success response was lost): the
// retry gets 404, not a 5xx, and nothing is recreated.
func TestM5DeleteAlreadyGoneReturns404(t *testing.T) {
	f := newM5Fixture(t, "db-a")
	_ = f.backs["db-a"].CreateCollection(context.Background(), "db_m5_1", 4)
	f.srv.handlers.Registry().PinDB("db_m5_1", "db-a")

	if rec := f.del(t, f.alice, "db_m5_1"); rec.Code != http.StatusOK {
		t.Fatalf("first delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := f.del(t, f.alice, "db_m5_1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("retry after lost response: status=%d body=%s (want 404)", rec.Code, rec.Body.String())
	}
	if f.backs["db-a"].has("db_m5_1") {
		t.Fatal("db must stay deleted")
	}
}

// The pin survives (e.g. delete succeeded on the backend but the miner's
// reply was lost before it unpinned): the pinned retry also maps to 404.
func TestM5PinnedDeleteOfAbsentDBReturns404(t *testing.T) {
	f := newM5Fixture(t, "db-a")
	f.srv.handlers.Registry().PinDB("db_m5_2", "db-a")
	rec := f.del(t, f.alice, "db_m5_2")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s (want 404)", rec.Code, rec.Body.String())
	}
	if owner := f.srv.handlers.Registry().DBOwner("db_m5_2"); owner != "" {
		t.Fatalf("pin of an absent DB must be dropped, owner=%q", owner)
	}
}

// No pin (pins are in memory and lost on restart) and two backends: the DB
// living on the second backend is deleted there, never answered 404 by the
// first backend alone.
func TestM5UnpinnedDeleteReachesTheBackendThatHoldsTheDB(t *testing.T) {
	f := newM5Fixture(t, "db-a", "db-b")
	_ = f.backs["db-a"].CreateCollection(context.Background(), "db_m5_other", 4)
	_ = f.backs["db-b"].CreateCollection(context.Background(), "db_m5_3", 4)
	_ = f.backs["db-a"].CreateCollection(context.Background(), "db_m5_3x", 4)

	rec := f.del(t, f.alice, "db_m5_3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.backs["db-b"].has("db_m5_3") {
		t.Fatal("DB on the second backend must be erased")
	}
	if !f.backs["db-a"].has("db_m5_other") || !f.backs["db-a"].has("db_m5_3x") {
		t.Fatal("other DBs must be untouched")
	}
	if rec := f.del(t, f.alice, "db_m5_3"); rec.Code != http.StatusNotFound {
		t.Fatalf("repeat status=%d (want 404)", rec.Code)
	}
}

// A backend failure is an error, never "not found": the caller must retry
// instead of treating the DB as erased.
func TestM5BackendErrorIsNotReportedAsNotFound(t *testing.T) {
	f := newM5Fixture(t, "db-a", "db-b")
	_ = f.backs["db-b"].CreateCollection(context.Background(), "db_m5_4", 4)
	f.backs["db-b"].failDrops.Store(true)
	rec := f.del(t, f.alice, "db_m5_4")
	if rec.Code < 500 {
		t.Fatalf("backend error answered %d (must be 5xx)", rec.Code)
	}
	f.backs["db-b"].failDrops.Store(false)
	if rec := f.del(t, f.alice, "db_m5_4"); rec.Code != http.StatusOK {
		t.Fatalf("retry status=%d", rec.Code)
	}
}

// A signer that is not the configured validator is denied identically for
// existing and absent DBs (no existence oracle) and reaches no backend.
func TestM5ForeignSignerDenied(t *testing.T) {
	f := newM5Fixture(t, "db-a")
	_ = f.backs["db-a"].CreateCollection(context.Background(), "db_m5_5", 4)
	f.srv.handlers.Registry().PinDB("db_m5_5", "db-a")
	present := f.del(t, f.bob, "db_m5_5")
	absent := f.del(t, f.bob, "db_m5_missing")
	if present.Code != http.StatusUnauthorized && present.Code != http.StatusForbidden {
		t.Fatalf("foreign signer status=%d", present.Code)
	}
	if absent.Code != present.Code {
		t.Fatalf("existence oracle: present=%d absent=%d", present.Code, absent.Code)
	}
	if f.backs["db-a"].drops.Load() != 0 || !f.backs["db-a"].has("db_m5_5") {
		t.Fatal("foreign request must not reach the backend")
	}
}
