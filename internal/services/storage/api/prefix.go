package api

import (
	"context"
	"fmt"
	"strings"

	"recall-miner/internal/retention"
	"recall-miner/internal/services/storage/vector"
)

// qdrantFeatures are the optional backend features the storage API uses; the Qdrant client has
// all of them, and STORAGE_PREFIX is allowed with Qdrant only.
type qdrantFeatures interface {
	vector.Client
	StagedResetRestore(context.Context, string, int, []vector.RestorePoint) error
	ResetCollection(context.Context, string, int) error
	RetentionRecord(context.Context, string, string) (retention.Record, error)
	ValidateRestorePointID(string) error
}

// prefixedClient keeps one miner's databases apart from other miners that share the same
// Qdrant: every database is stored under "<prefix>--<db id>", and only names with this
// miner's prefix are listed. Without it, a delete or restore through one miner reached the
// same stored database as every other miner sharing the Qdrant.
type prefixedClient struct {
	inner  qdrantFeatures
	prefix string // "<STORAGE_PREFIX>--"
}

func newPrefixedClient(inner vector.Client, prefix string) (vector.Client, error) {
	if prefix == "" {
		return inner, nil
	}
	q, ok := inner.(qdrantFeatures)
	if !ok {
		return nil, fmt.Errorf("STORAGE_PREFIX is supported with DB_ENGINE=qdrant only")
	}
	return &prefixedClient{inner: q, prefix: prefix + "--"}, nil
}

func (c *prefixedClient) name(db string) string { return c.prefix + db }

func (c *prefixedClient) CreateCollection(ctx context.Context, db string, dimension int) error {
	return c.inner.CreateCollection(ctx, c.name(db), dimension)
}
func (c *prefixedClient) DropCollection(ctx context.Context, db string) error {
	return c.inner.DropCollection(ctx, c.name(db))
}
func (c *prefixedClient) GetCollectionInfo(ctx context.Context, db string) (vector.CollectionInfo, error) {
	info, err := c.inner.GetCollectionInfo(ctx, c.name(db))
	info.Name = strings.TrimPrefix(info.Name, c.prefix)
	return info, err
}
func (c *prefixedClient) GetCollectionStats(ctx context.Context, db string) (int, error) {
	return c.inner.GetCollectionStats(ctx, c.name(db))
}
func (c *prefixedClient) ListCollections(ctx context.Context) ([]string, error) {
	all, err := c.inner.ListCollections(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, n := range all {
		if strings.HasPrefix(n, c.prefix) {
			out = append(out, strings.TrimPrefix(n, c.prefix))
		}
	}
	return out, nil
}
func (c *prefixedClient) InsertVector(ctx context.Context, db string, v []float64, md map[string]any) (string, error) {
	return c.inner.InsertVector(ctx, c.name(db), v, md)
}
func (c *prefixedClient) RestoreVector(ctx context.Context, db, embedID string, v []float64, md map[string]any) error {
	return c.inner.RestoreVector(ctx, c.name(db), embedID, v, md)
}
func (c *prefixedClient) DeleteVector(ctx context.Context, db, embedID string) error {
	return c.inner.DeleteVector(ctx, c.name(db), embedID)
}
func (c *prefixedClient) Search(ctx context.Context, db string, v []float64, topK int) ([]vector.SearchHit, error) {
	return c.inner.Search(ctx, c.name(db), v, topK)
}
func (c *prefixedClient) StagedResetRestore(ctx context.Context, db string, dimension int, points []vector.RestorePoint) error {
	return c.inner.StagedResetRestore(ctx, c.name(db), dimension, points)
}
func (c *prefixedClient) ResetCollection(ctx context.Context, db string, dimension int) error {
	return c.inner.ResetCollection(ctx, c.name(db), dimension)
}
func (c *prefixedClient) RetentionRecord(ctx context.Context, db, id string) (retention.Record, error) {
	return c.inner.RetentionRecord(ctx, c.name(db), id)
}
func (c *prefixedClient) ValidateRestorePointID(id string) error {
	return c.inner.ValidateRestorePointID(id)
}
