package pgsql

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"recall-miner/internal/services/storage/config"
	"recall-miner/internal/services/storage/vector"
)

const collectionsTable = "recall_miner_vector_collections"

var identifierPattern = regexp.MustCompile(`[^a-zA-Z0-9_]`)

type Client struct {
	dsn string
}

func NewClient(cfg config.Config) *Client {
	return &Client{dsn: cfg.PgSQLDSN}
}

func (c *Client) CreateCollection(ctx context.Context, collectionName string, dimension int) error {
	if dimension <= 0 {
		return fmt.Errorf("dimension must be positive")
	}

	pool, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := ensureSchema(ctx, pool); err != nil {
		return err
	}

	table := dimensionTableName(dimension)
	_, err = pool.Exec(ctx,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, embedding vector(%d) NOT NULL, metadata JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`, quoteIdent(table), dimension),
	)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s (tenant_id)`, quoteIdent(table+"_tenant_idx"), quoteIdent(table))); err != nil {
		return err
	}

	// Dimension is immutable for an existing logical database. The WHERE clause
	// makes the conflict update a no-op when dimensions differ so concurrent
	// remaps cannot race past a read-only check.
	var stored int
	err = pool.QueryRow(ctx,
		fmt.Sprintf(`INSERT INTO %s (collection_name, table_name, dimension) VALUES ($1, $2, $3)
			ON CONFLICT (collection_name) DO UPDATE SET table_name = EXCLUDED.table_name
			WHERE %s.dimension = EXCLUDED.dimension
			RETURNING dimension`, quoteIdent(collectionsTable), quoteIdent(collectionsTable)),
		collectionName, table, dimension,
	).Scan(&stored)
	if err == nil {
		if stored != dimension {
			return vector.DimensionConflict(collectionName, stored, dimension)
		}
		return nil
	}
	if err != pgx.ErrNoRows {
		return err
	}
	var existing int
	if lookupErr := pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT dimension FROM %s WHERE collection_name = $1`, quoteIdent(collectionsTable)),
		collectionName,
	).Scan(&existing); lookupErr != nil {
		return lookupErr
	}
	return vector.DimensionConflict(collectionName, existing, dimension)
}

func (c *Client) DropCollection(ctx context.Context, collectionName string) error {
	pool, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := ensureSchema(ctx, pool); err != nil {
		return err
	}

	table, err := c.collectionTable(ctx, pool, collectionName)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1`, quoteIdent(table)), collectionName); err != nil {
		return err
	}
	_, err = pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE collection_name = $1`, quoteIdent(collectionsTable)), collectionName)
	return err
}

func (c *Client) GetCollectionInfo(ctx context.Context, collectionName string) (vector.CollectionInfo, error) {
	pool, err := c.open(ctx)
	if err != nil {
		return vector.CollectionInfo{}, err
	}
	defer pool.Close()

	if err := ensureSchema(ctx, pool); err != nil {
		return vector.CollectionInfo{}, err
	}

	var dimension int
	err = pool.QueryRow(ctx, fmt.Sprintf(`SELECT dimension FROM %s WHERE collection_name = $1`, quoteIdent(collectionsTable)), collectionName).Scan(&dimension)
	if err != nil {
		return vector.CollectionInfo{}, err
	}
	return vector.CollectionInfo{Name: collectionName, Dimension: dimension}, nil
}

func (c *Client) GetCollectionStats(ctx context.Context, collectionName string) (int, error) {
	pool, err := c.open(ctx)
	if err != nil {
		return 0, err
	}
	defer pool.Close()

	if err := ensureSchema(ctx, pool); err != nil {
		return 0, err
	}

	table, err := c.collectionTable(ctx, pool, collectionName)
	if err != nil {
		return 0, err
	}

	var count int
	err = pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1`, quoteIdent(table)), collectionName).Scan(&count)
	return count, err
}

func (c *Client) ListCollections(ctx context.Context) ([]string, error) {
	pool, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	if err := ensureSchema(ctx, pool); err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT collection_name FROM %s ORDER BY collection_name`, quoteIdent(collectionsTable)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	collections := []string{}
	for rows.Next() {
		var collection string
		if err := rows.Scan(&collection); err != nil {
			return nil, err
		}
		collections = append(collections, collection)
	}
	return collections, rows.Err()
}

func (c *Client) InsertVector(ctx context.Context, collectionName string, value []float64, metadata map[string]any) (string, error) {
	id := newUUID()
	if err := c.RestoreVector(ctx, collectionName, id, value, metadata); err != nil {
		return "", err
	}
	return id, nil
}

func (c *Client) RestoreVector(ctx context.Context, collectionName, id string, value []float64, metadata map[string]any) error {
	pool, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	table, err := c.collectionTable(ctx, pool, collectionName)
	if err != nil {
		return err
	}

	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	tag, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (id, tenant_id, embedding, metadata) VALUES ($1, $2, $3::vector, $4::jsonb) ON CONFLICT (id) DO UPDATE SET embedding = EXCLUDED.embedding, metadata = EXCLUDED.metadata WHERE %s.tenant_id = EXCLUDED.tenant_id`, quoteIdent(table), quoteIdent(table)), id, collectionName, vectorLiteral(value), string(metadataJSON))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("embed id %q belongs to another collection", id)
	}
	return nil
}

func (c *Client) DeleteVector(ctx context.Context, collectionName, embedID string) error {
	pool, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	table, err := c.collectionTable(ctx, pool, collectionName)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND id = $2`, quoteIdent(table)), collectionName, embedID)
	return err
}

func (c *Client) Search(ctx context.Context, collectionName string, value []float64, topK int) ([]vector.SearchHit, error) {
	pool, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	table, err := c.collectionTable(ctx, pool, collectionName)
	if err != nil {
		return nil, err
	}

	// <=> is cosine distance; return cosine similarity (higher = closer) so every
	// backend reports "distance" the same way (see vector.SearchHit).
	rows, err := pool.Query(ctx,
		fmt.Sprintf(`SELECT id, metadata, 1 - (embedding <=> $1::vector) AS distance FROM %s WHERE tenant_id = $2 ORDER BY embedding <=> $1::vector LIMIT $3`, quoteIdent(table)),
		vectorLiteral(value), collectionName, topK,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hits := []vector.SearchHit{}
	for rows.Next() {
		var hit vector.SearchHit
		var metadata []byte
		if err := rows.Scan(&hit.ID, &metadata, &hit.Distance); err != nil {
			return nil, err
		}
		if len(metadata) > 0 {
			_ = json.Unmarshal(metadata, &hit.Metadata)
		}
		if hit.Metadata == nil {
			hit.Metadata = map[string]any{}
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func (c *Client) open(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (collection_name TEXT PRIMARY KEY, table_name TEXT NOT NULL, dimension INT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`, quoteIdent(collectionsTable)))
	return err
}

func (c *Client) collectionTable(ctx context.Context, pool *pgxpool.Pool, collectionName string) (string, error) {
	var table string
	err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT table_name FROM %s WHERE collection_name = $1`, quoteIdent(collectionsTable)), collectionName).Scan(&table)
	if err == pgx.ErrNoRows {
		return "", vector.CollectionNotFound(collectionName)
	}
	return table, err
}

func tableName(collectionName string) string {
	name := strings.ToLower(identifierPattern.ReplaceAllString(collectionName, "_"))
	name = strings.Trim(name, "_")
	if name == "" {
		name = "collection"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return "recall_vec_" + name
}

func dimensionTableName(dimension int) string {
	if dimension <= 0 {
		dimension = 1
	}
	return "recall_vec_dim_" + strconv.Itoa(dimension)
}

func quoteIdent(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func vectorLiteral(values []float64) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			value = 0
		}
		parts = append(parts, strconv.FormatFloat(value, 'g', -1, 64))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func parseVector(value string) []float64 {
	trimmed := strings.Trim(value, "[]")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	result := make([]float64, 0, len(parts))
	for _, part := range parts {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err == nil {
			result = append(result, parsed)
		}
	}
	return result
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
