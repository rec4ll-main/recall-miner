package vector

import (
	"context"
	"errors"
	"fmt"
)

// ErrDimensionImmutable is returned when CreateCollection would change an
// existing logical database's dimension. Non-reset restore must not remap.
var ErrDimensionImmutable = errors.New("collection dimension is immutable")

// DimensionConflict formats ErrDimensionImmutable with collection context.
func DimensionConflict(collectionName string, existing, requested int) error {
	return fmt.Errorf("%w: collection %q already exists with dimension %d (requested %d)", ErrDimensionImmutable, collectionName, existing, requested)
}

// ErrCollectionNotFound is returned when a logical database (collection)
// does not exist, e.g. it was already deleted. The storage API maps it to
// 404 so a repeated delete is idempotent (M5).
var ErrCollectionNotFound = errors.New("collection not found")

// CollectionNotFound wraps ErrCollectionNotFound with collection context.
func CollectionNotFound(collectionName string) error {
	return fmt.Errorf("%w: %q", ErrCollectionNotFound, collectionName)
}

type Client interface {
	CreateCollection(ctx context.Context, collectionName string, dimension int) error
	DropCollection(ctx context.Context, collectionName string) error
	GetCollectionInfo(ctx context.Context, collectionName string) (CollectionInfo, error)
	GetCollectionStats(ctx context.Context, collectionName string) (int, error)
	ListCollections(ctx context.Context) ([]string, error)
	InsertVector(ctx context.Context, collectionName string, vector []float64, metadata map[string]any) (string, error)
	RestoreVector(ctx context.Context, collectionName, embedID string, vector []float64, metadata map[string]any) error
	DeleteVector(ctx context.Context, collectionName, embedID string) error
	Search(ctx context.Context, collectionName string, vector []float64, topK int) ([]SearchHit, error)
}

type CollectionInfo struct {
	Name      string
	Dimension int
}

type SearchHit struct {
	ID string
	// Distance is cosine similarity: 1 = same direction, higher = closer.
	// Every backend (Qdrant, Milvus, pgvector) must report it this way; the
	// query API returns it as "distance" for compatibility.
	Distance float64
	Metadata map[string]any
}


// RestorePoint is one identity-preserving vector used by staged reset restore.
type RestorePoint struct {
	EmbedID  string
	Vector   []float64
	Metadata map[string]any
}
