package pgsql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"recall-miner/internal/services/storage/vector"
)

func TestTableNameSanitizesCollectionName(t *testing.T) {
	got := tableName(`DB-123; DROP TABLE users;`)
	if got != "recall_vec_db_123__drop_table_users" {
		t.Fatalf("tableName = %q", got)
	}
}

func TestQuoteIdentEscapesDoubleQuotes(t *testing.T) {
	got := quoteIdent(`a"b`)
	if got != `"a""b"` {
		t.Fatalf("quoteIdent = %q", got)
	}
}

func TestVectorLiteralAndParseVector(t *testing.T) {
	literal := vectorLiteral([]float64{1, 2.5, -3})
	if literal != "[1,2.5,-3]" {
		t.Fatalf("vectorLiteral = %q", literal)
	}

	parsed := parseVector(literal)
	if !reflect.DeepEqual(parsed, []float64{1, 2.5, -3}) {
		t.Fatalf("parseVector = %#v", parsed)
	}
}

func TestTableNameLength(t *testing.T) {
	got := tableName(strings.Repeat("a", 100))
	if len(got) > len("recall_vec_")+40 {
		t.Fatalf("tableName too long: %q", got)
	}
}

func TestDimensionConflictError(t *testing.T) {
	err := vector.DimensionConflict("db_x", 384, 1024)
	if err == nil || !strings.Contains(err.Error(), "already exists with dimension 384") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !errors.Is(err, vector.ErrDimensionImmutable) {
		t.Fatalf("expected ErrDimensionImmutable wrap, got %v", err)
	}
}

func TestCreateCollectionRejectsDimensionChange(t *testing.T) {
	dsn := os.Getenv("RECALL_TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:test@127.0.0.1:55440/postgres?sslmode=disable"
	}
	ctx := context.Background()
	client := &Client{dsn: dsn}

	// Ensure extension exists for this disposable instance.
	pool, err := client.open(ctx)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		pool.Close()
		t.Skipf("vector extension unavailable: %v", err)
	}
	pool.Close()

	name := fmt.Sprintf("db_dim_test_%d", time.Now().UnixNano())
	if err := client.CreateCollection(ctx, name, 4); err != nil {
		t.Fatalf("create dim 4: %v", err)
	}
	t.Cleanup(func() { _ = client.DropCollection(context.Background(), name) })

	// Seed one vector so delete-after-reject proves the original mapping remains.
	id, err := client.InsertVector(ctx, name, []float64{1, 2, 3, 4}, map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	err = client.CreateCollection(ctx, name, 8)
	if !errors.Is(err, vector.ErrDimensionImmutable) {
		t.Fatalf("expected ErrDimensionImmutable, got %v", err)
	}

	info, err := client.GetCollectionInfo(ctx, name)
	if err != nil {
		t.Fatalf("GetCollectionInfo: %v", err)
	}
	if info.Dimension != 4 {
		t.Fatalf("dimension remapped to %d", info.Dimension)
	}

	// Same-dimension create remains idempotent and does not drop data.
	if err := client.CreateCollection(ctx, name, 4); err != nil {
		t.Fatalf("same-dimension create: %v", err)
	}
	count, err := client.GetCollectionStats(ctx, name)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1 (id %s)", count, id)
	}

	if err := client.DropCollection(ctx, name); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := client.GetCollectionInfo(ctx, name); err == nil {
		t.Fatal("expected not found after drop")
	}
}

// Search must report cosine similarity (higher = closer), like Qdrant and Milvus.
func TestSearchReportsCosineSimilarity(t *testing.T) {
	dsn := os.Getenv("RECALL_TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:test@127.0.0.1:55440/postgres?sslmode=disable"
	}
	ctx := context.Background()
	client := &Client{dsn: dsn}
	pool, err := client.open(ctx)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		pool.Close()
		t.Skipf("vector extension unavailable: %v", err)
	}
	pool.Close()

	name := fmt.Sprintf("db_sim_test_%d", time.Now().UnixNano())
	if err := client.CreateCollection(ctx, name, 2); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = client.DropCollection(context.Background(), name) })

	same, err := client.InsertVector(ctx, name, []float64{2, 0}, nil)
	if err != nil {
		t.Fatalf("insert same: %v", err)
	}
	right, err := client.InsertVector(ctx, name, []float64{0, 3}, nil)
	if err != nil {
		t.Fatalf("insert orthogonal: %v", err)
	}
	opposite, err := client.InsertVector(ctx, name, []float64{-1, 0}, nil)
	if err != nil {
		t.Fatalf("insert opposite: %v", err)
	}

	hits, err := client.Search(ctx, name, []float64{1, 0}, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	want := []struct {
		id  string
		sim float64
	}{{same, 1}, {right, 0}, {opposite, -1}}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want %d", len(hits), len(want))
	}
	for i, w := range want {
		if hits[i].ID != w.id {
			t.Fatalf("hit %d id = %s, want %s (order must be closest first)", i, hits[i].ID, w.id)
		}
		if d := hits[i].Distance - w.sim; d > 1e-6 || d < -1e-6 {
			t.Fatalf("hit %d distance = %v, want cosine similarity %v", i, hits[i].Distance, w.sim)
		}
	}
}
