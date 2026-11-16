package qdrant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"recall-miner/internal/services/storage/vector"
)

type qdrantFake struct {
	mu          sync.Mutex
	registry    map[string]map[string]any // point id -> payload
	collections map[string]int            // physical name -> dimension
	puts        int
}

func newQdrantFake() *qdrantFake {
	return &qdrantFake{
		registry:    map[string]map[string]any{},
		collections: map[string]int{registryCollection: 1},
	}
}

func (f *qdrantFake) handler(w http.ResponseWriter, r *http.Request) {
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
		writeQJSON(w, map[string]any{"result": true})
	case r.Method == http.MethodPut && strings.Contains(path, "/points"):
		f.puts++
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		points, _ := payload["points"].([]any)
		for _, raw := range points {
			point, _ := raw.(map[string]any)
			id, _ := point["id"].(string)
			pl, _ := point["payload"].(map[string]any)
			f.registry[id] = pl
		}
		writeQJSON(w, map[string]any{"result": map[string]any{"status": "ok"}})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/points"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		ids, _ := payload["ids"].([]any)
		result := []map[string]any{}
		for _, raw := range ids {
			id, _ := raw.(string)
			if pl, ok := f.registry[id]; ok {
				result = append(result, map[string]any{"id": id, "payload": pl})
			}
		}
		writeQJSON(w, map[string]any{"result": result})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/points/delete") && strings.Contains(path, registryCollection):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		ids, _ := payload["points"].([]any)
		for _, raw := range ids {
			id, _ := raw.(string)
			delete(f.registry, id)
		}
		writeQJSON(w, map[string]any{"result": map[string]any{"status": "ok"}})
	default:
		writeQJSON(w, map[string]any{"result": true})
	}
}

func writeQJSON(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func newQClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return &Client{baseURL: srv.URL, http: srv.Client()}
}

func TestCreateCollectionRejectsDimensionChange(t *testing.T) {
	fake := newQdrantFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 2); err != nil {
		t.Fatalf("create dim 2: %v", err)
	}
	putsAfterCreate := fake.puts

	err := client.CreateCollection(context.Background(), "db_a", 3)
	if !errors.Is(err, vector.ErrDimensionImmutable) {
		t.Fatalf("expected ErrDimensionImmutable, got %v", err)
	}
	if fake.puts != putsAfterCreate {
		t.Fatalf("registry was rewritten on dimension conflict: puts %d -> %d", putsAfterCreate, fake.puts)
	}

	entry, err := client.registryEntry(context.Background(), "db_a")
	if err != nil {
		t.Fatalf("registryEntry: %v", err)
	}
	if entry.Dimension != 2 {
		t.Fatalf("dimension remapped to %d", entry.Dimension)
	}
}

func TestCreateCollectionSameDimensionIdempotent(t *testing.T) {
	fake := newQdrantFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)

	if err := client.CreateCollection(context.Background(), "db_a", 4); err != nil {
		t.Fatalf("create: %v", err)
	}
	putsAfterCreate := fake.puts
	if err := client.CreateCollection(context.Background(), "db_a", 4); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if fake.puts != putsAfterCreate {
		t.Fatalf("same-dimension create rewrote registry: puts %d -> %d", putsAfterCreate, fake.puts)
	}
}

func TestValidateRestorePointID(t *testing.T) {
	valid := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"00000000-0000-0000-0000-000000000000",
		"0",
		"1",
		"18446744073709551615",
	}
	for _, id := range valid {
		if err := ValidateRestorePointID(id); err != nil {
			t.Fatalf("ValidateRestorePointID(%q): %v", id, err)
		}
	}
	invalid := []string{
		"",
		"not-a-uuid",
		"550e8400-e29b-41d4-a716-44665544000", // too short
		"zg",
		"-1",
		"1.5",
		"1e10",
		"18446744073709551616", // overflow u64
	}
	for _, id := range invalid {
		if err := ValidateRestorePointID(id); err == nil {
			t.Fatalf("ValidateRestorePointID(%q) expected error", id)
		}
	}
}

// M5: dropping an already-absent DB reports vector.ErrCollectionNotFound so
// the storage API answers 404 (idempotent delete), not a generic error.
func TestDropCollectionAbsentIsNotFound(t *testing.T) {
	fake := newQdrantFake()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := newQClient(t, srv)
	if err := client.CreateCollection(context.Background(), "db_gone", 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := client.DropCollection(context.Background(), "db_gone"); err != nil {
		t.Fatalf("first drop: %v", err)
	}
	if err := client.DropCollection(context.Background(), "db_gone"); !errors.Is(err, vector.ErrCollectionNotFound) {
		t.Fatalf("second drop: got %v, want ErrCollectionNotFound", err)
	}
}
