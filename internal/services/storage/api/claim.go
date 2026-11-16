package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"recall-miner/internal/services/storage/vector"
)

// storageClaimDB holds one record naming the db service that uses this storage (under its
// STORAGE_PREFIX). Two db services with the same prefix on one Qdrant would act on each other's
// databases: a delete or restore through one would reach the other's copy.
const (
	storageClaimDB    = "__storage_owner"
	storageClaimID    = "5f0c6b5e-7c1a-4d2e-9b3f-000000000001"
	storageClaimFresh = 3 * time.Minute
	storageClaimEvery = time.Minute
)

// StorageClaim records this db service as the user of its storage and refuses to share it.
type StorageClaim struct {
	client vector.Client
	owner  string // this service's public URL: unique per db service, stable across restarts
	prefix string
	log    *slog.Logger
	now    func() time.Time
}

// SharedStorageError means another db service uses this storage under the same prefix.
type SharedStorageError struct{ msg string }

func (e *SharedStorageError) Error() string { return e.msg }

func errSharedStorage(other, prefix string) error {
	if prefix == "" {
		return &SharedStorageError{fmt.Sprintf("this storage is already used by another db service (%s). Each miner needs its own storage: give every miner that shares one Qdrant a different STORAGE_PREFIX", other)}
	}
	return &SharedStorageError{fmt.Sprintf("this storage is already used by another db service (%s) with the same STORAGE_PREFIX %q. Give every miner that shares one Qdrant a different STORAGE_PREFIX", other, prefix)}
}

// Check reads the current claim and, unless another db service refreshed it recently, writes
// this service's claim. It returns an error when the storage is in use by another service.
func (c *StorageClaim) Check(ctx context.Context) error {
	if err := c.client.CreateCollection(ctx, storageClaimDB, 1); err != nil {
		return fmt.Errorf("storage claim: %w", err)
	}
	hits, err := c.client.Search(ctx, storageClaimDB, []float64{1}, 1)
	if err != nil {
		return fmt.Errorf("storage claim: %w", err)
	}
	now := c.now()
	if len(hits) > 0 {
		other, _ := hits[0].Metadata["owner"].(string)
		seen, _ := strconv.ParseInt(fmt.Sprint(hits[0].Metadata["seen_unix"]), 10, 64)
		if other != "" && other != c.owner && now.Sub(time.Unix(seen, 0)) < storageClaimFresh {
			return errSharedStorage(other, c.prefix)
		}
	}
	return c.client.RestoreVector(ctx, storageClaimDB, storageClaimID, []float64{1},
		map[string]any{"owner": c.owner, "seen_unix": strconv.FormatInt(now.Unix(), 10)})
}

// Keep refreshes the claim until ctx ends. A conflict found later (two services started at the
// same moment) is returned so the caller stops serving.
func (c *StorageClaim) Keep(ctx context.Context) error {
	t := time.NewTicker(storageClaimEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := c.Check(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if isSharedStorage(err) {
				return err
			}
			if c.log != nil {
				c.log.Warn("storage claim refresh failed; retrying", "error", err)
			}
		}
	}
}

func isSharedStorage(err error) bool {
	var shared *SharedStorageError
	return errors.As(err, &shared)
}
