package qdrant

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// raceFake is a fresh Qdrant node (no collections at all, not even the
// registry) that reproduces the create race deterministically: a GET for a
// missing collection is held until `callers` GETs for it have arrived, so
// every caller decides "not found" before anyone creates it. The first PUT
// then takes `createFor` to finish; like Qdrant 1.12, while it is in
// progress a GET answers 500 ("0 of 0 read operations failed") and another
// PUT answers 409 "already exists" (seen in an end-to-end run).
type raceFake struct {
	*qdrantFake
	callers   int
	createFor time.Duration
	bmu       sync.Mutex
	arrived   map[string]int
	released  map[string]chan struct{}
	creating  map[string]bool
	conflicts int // 409s answered (guarded by bmu)
}

func newRaceFake(callers int) *raceFake {
	f := &raceFake{qdrantFake: newQdrantFake(), callers: callers, createFor: 150 * time.Millisecond,
		arrived: map[string]int{}, released: map[string]chan struct{}{}, creating: map[string]bool{}}
	f.collections = map[string]int{} // fresh node
	return f
}

func (f *raceFake) handler(w http.ResponseWriter, r *http.Request) {
	isCollection := strings.HasPrefix(r.URL.Path, "/collections/") &&
		!strings.Contains(strings.TrimPrefix(r.URL.Path, "/collections/"), "/")
	name := strings.TrimPrefix(r.URL.Path, "/collections/")
	exists := func() bool { f.mu.Lock(); defer f.mu.Unlock(); _, ok := f.collections[name]; return ok }
	conflict := func() {
		f.conflicts++ // caller holds bmu
		http.Error(w, `{"status":{"error":"Wrong input: Collection `+"`"+name+"`"+` already exists!"}}`, http.StatusConflict)
	}
	switch {
	case isCollection && r.Method == http.MethodPut:
		// create-or-conflict is decided atomically under bmu
		f.bmu.Lock()
		if exists() || f.creating[name] {
			conflict()
			f.bmu.Unlock()
			return
		}
		if f.createFor == 0 {
			f.qdrantFake.handler(w, r)
			f.bmu.Unlock()
			return
		}
		f.creating[name] = true
		f.bmu.Unlock()
		time.Sleep(f.createFor)
		f.bmu.Lock()
		f.qdrantFake.handler(w, r)
		delete(f.creating, name)
		f.bmu.Unlock()
		return
	case isCollection && r.Method == http.MethodGet:
		f.bmu.Lock()
		if f.creating[name] {
			f.bmu.Unlock()
			http.Error(w, `{"status":{"error":"Service internal error: 0 of 0 read operations failed"}}`, http.StatusInternalServerError)
			return
		}
		if exists() {
			f.bmu.Unlock()
			break
		}
		ch, ok := f.released[name]
		if !ok {
			ch = make(chan struct{})
			f.released[name] = ch
		}
		f.arrived[name]++
		if f.arrived[name] == f.callers {
			close(ch)
		}
		f.bmu.Unlock()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
		}
		http.Error(w, `{"status":{"error":"Not found: Collection `+"`"+name+"`"+` doesn't exist!"}}`, http.StatusNotFound)
		return
	}
	f.qdrantFake.handler(w, r)
}

func (f *raceFake) conflictCount() int {
	f.bmu.Lock()
	defer f.bmu.Unlock()
	return f.conflicts
}

func runConcurrentCreates(t *testing.T, names []string, dimension int) (*raceFake, *Client) {
	t.Helper()
	fake := newRaceFake(len(names))
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			errs[i] = client.CreateCollection(context.Background(), name, dimension)
		}(i, name)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent create %q on a fresh node: %v", names[i], err)
		}
	}
	return fake, client
}

// First database creates racing on a fresh
// miner node returned 502 because the losers' collection PUT got 409.
func TestCreateCollectionConcurrentOnFreshNode(t *testing.T) {
	names := make([]string, 8)
	for i := range names {
		names[i] = fmt.Sprintf("db_race_%d", i)
	}
	fake, client := runConcurrentCreates(t, names, 384)
	if n := fake.conflictCount(); n < len(names)-1 {
		t.Fatalf("only %d create conflicts occurred; the race was not exercised", n)
	}
	for _, name := range names {
		entry, err := client.registryEntry(context.Background(), name)
		if err != nil || entry.Dimension != 384 || entry.PhysicalCollection != physicalCollectionName(384) {
			t.Fatalf("registry entry for %q: %+v, %v", name, entry, err)
		}
	}
}

func TestCreateCollectionConcurrentSameDatabase(t *testing.T) {
	names := []string{"db_same", "db_same", "db_same", "db_same"}
	fake, client := runConcurrentCreates(t, names, 384)
	if n := fake.conflictCount(); n < len(names)-1 {
		t.Fatalf("only %d create conflicts occurred; the race was not exercised", n)
	}
	if entry, err := client.registryEntry(context.Background(), "db_same"); err != nil || entry.Dimension != 384 {
		t.Fatalf("registry entry: %+v, %v", entry, err)
	}
}

// "Already exists" is only success when the existing collection matches:
// a concurrent creator with another dimension is still an error.
func TestEnsureVectorCollectionConflictWithOtherDimensionFails(t *testing.T) {
	fake := newRaceFake(1)
	fake.createFor = 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/collections/recall_vec_dim_8" {
			fake.mu.Lock()
			fake.collections["recall_vec_dim_8"] = 16 // created meanwhile with another size
			fake.mu.Unlock()
		}
		fake.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)
	err := client.ensureVectorCollection(context.Background(), "recall_vec_dim_8", 8)
	if err == nil || !strings.Contains(err.Error(), "dimension 16") {
		t.Fatalf("want dimension mismatch error, got %v", err)
	}
}

// Conflict recovery accepts only a collection whose vector size is known and
// matches; an existing collection reporting no size is an error.
func TestEnsureVectorCollectionConflictWithUnknownDimensionFails(t *testing.T) {
	fake := newRaceFake(1)
	fake.createFor = 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/collections/recall_vec_dim_8" {
			fake.mu.Lock()
			fake.collections["recall_vec_dim_8"] = 0 // created meanwhile; size not reported
			fake.mu.Unlock()
		}
		fake.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	err := newQClient(t, srv).ensureVectorCollection(context.Background(), "recall_vec_dim_8", 8)
	if err == nil || !strings.Contains(err.Error(), "size is unknown") {
		t.Fatalf("want unknown-size error, got %v", err)
	}
	if fake.conflictCount() != 1 {
		t.Fatalf("conflicts %d, want 1", fake.conflictCount())
	}
}
