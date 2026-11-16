package qdrant

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"recall-miner/internal/services/storage/vector"
)

const (
	stagingTenantSuffix = "::restore_staging"
	stagingStatusKey    = "staging_status"
	stagingOfKey        = "staging_of"
	stagingExpectedKey  = "staging_expected"
	stagingPhysicalKey  = "staging_physical"
	stagingDimensionKey = "staging_dimension"
	activeTenantKey     = "active_tenant"

	stagingStatusWriting   = "writing"
	stagingStatusReady     = "ready"
	stagingStatusPromoting = "promoting"
)

func stagingTenant(collectionName string) string {
	return collectionName + stagingTenantSuffix
}

func stagingRegistryID(collectionName string) string {
	return deterministicUUID(collectionName + stagingTenantSuffix)
}

// StagedResetRestore writes the full replacement set under a staging tenant,
// verifies it, then promotes it by switching the registry active-tenant
// pointer before deleting the previous generation. Concurrent Search/stats
// cannot discard an in-progress writing stage owned by this client.
func (c *Client) StagedResetRestore(ctx context.Context, collectionName string, dimension int, records []vector.RestorePoint) error {
	if dimension <= 0 {
		return fmt.Errorf("dimension must be positive")
	}
	if !c.claimStagingOwn(collectionName) {
		return fmt.Errorf("restore already in progress for %q", collectionName)
	}
	defer c.releaseStagingOwn(collectionName)

	if len(records) == 0 {
		// Restoring an empty database: there is nothing to stage, and promotion skips an empty
		// stage, so the database was never created and the restore "succeeded" with nothing
		// there. Reset it in place to an empty database of the given dimension instead.
		return c.ResetCollection(ctx, collectionName, dimension)
	}

	// Reclaim abandoned writing even while we hold ownership (fresh start).
	if err := c.recoverStaging(ctx, collectionName, true); err != nil {
		return err
	}

	var oldPhysical string
	if entry, err := c.registryEntry(ctx, collectionName); err == nil {
		oldPhysical = entry.PhysicalCollection
	} else if !strings.Contains(err.Error(), "not found") {
		return err
	}

	targetPhysical := physicalCollectionName(dimension)
	if err := c.ensureRegistryCollection(ctx); err != nil {
		return err
	}
	if err := c.ensureVectorCollection(ctx, targetPhysical, dimension); err != nil {
		return err
	}

	stg := stagingTenant(collectionName)
	_ = c.deleteByTenant(ctx, targetPhysical, stg)
	if oldPhysical != "" && oldPhysical != targetPhysical {
		_ = c.deleteByTenant(ctx, oldPhysical, stg)
	}

	if err := c.upsertStagingMark(ctx, collectionName, targetPhysical, dimension, len(records), stagingStatusWriting); err != nil {
		return err
	}

	for _, record := range records {
		if err := c.upsertTenantPoint(ctx, targetPhysical, stg, record.EmbedID, record.Vector, record.Metadata); err != nil {
			_ = c.deleteByTenant(ctx, targetPhysical, stg)
			_ = c.clearStagingMark(ctx, collectionName)
			return err
		}
	}

	count, err := c.countTenant(ctx, targetPhysical, stg)
	if err != nil {
		_ = c.deleteByTenant(ctx, targetPhysical, stg)
		_ = c.clearStagingMark(ctx, collectionName)
		return err
	}
	if count != len(records) {
		_ = c.deleteByTenant(ctx, targetPhysical, stg)
		_ = c.clearStagingMark(ctx, collectionName)
		return fmt.Errorf("staging count %d does not match restore payload %d", count, len(records))
	}

	if err := c.upsertStagingMark(ctx, collectionName, targetPhysical, dimension, len(records), stagingStatusReady); err != nil {
		return err
	}
	if err := c.upsertStagingMark(ctx, collectionName, targetPhysical, dimension, len(records), stagingStatusPromoting); err != nil {
		return err
	}

	if err := c.promoteStaging(ctx, collectionName, oldPhysical, targetPhysical, dimension, records); err != nil {
		// Leave staging mark as promoting/ready so RecoverStaging can finish.
		return err
	}
	return nil
}

// RecoverStaging cleans abandoned staging or finishes a ready/promoting
// staging set after an interrupt. Safe to call on read/write paths. An
// in-progress writing stage owned by this process is left untouched so
// concurrent search/stats cannot erase an active restore.
func (c *Client) RecoverStaging(ctx context.Context, collectionName string) error {
	return c.recoverStaging(ctx, collectionName, false)
}

func (c *Client) recoverStaging(ctx context.Context, collectionName string, reclaimOwnedWriting bool) error {
	if err := c.ensureRegistryCollection(ctx); err != nil {
		return err
	}
	mark, ok, err := c.loadStagingMark(ctx, collectionName)
	if err != nil {
		return err
	}
	if !ok {
		// Orphan staging tenants without a mark: delete if present under known dims.
		return c.dropOrphanStagingTenants(ctx, collectionName)
	}

	stg := stagingTenant(collectionName)
	switch mark.Status {
	case stagingStatusWriting:
		if !reclaimOwnedWriting && c.hasStagingOwn(collectionName) {
			return nil
		}
		_ = c.deleteByTenant(ctx, mark.Physical, stg)
		return c.clearStagingMark(ctx, collectionName)
	case stagingStatusReady, stagingStatusPromoting:
		if c.testAfterRecoverScroll != nil {
			// Delayed-caller window: this caller has observed a pending stage
			// but does not yet hold the promote lock.
			c.testAfterRecoverScroll()
		}
		return c.withPromoteLock(collectionName, func() error {
			return c.recoverPendingStageLocked(ctx, collectionName)
		})
	default:
		_ = c.deleteByTenant(ctx, mark.Physical, stg)
		return c.clearStagingMark(ctx, collectionName)
	}
}

type stagingMark struct {
	Physical  string
	Dimension int
	Expected  int
	Status    string
}

func (c *Client) promoteStaging(ctx context.Context, collectionName, oldPhysical, targetPhysical string, dimension int, records []vector.RestorePoint) error {
	if c.testBeforePromote != nil {
		c.testBeforePromote()
	}
	return c.withPromoteLock(collectionName, func() error {
		return c.promoteStagingLocked(ctx, collectionName, oldPhysical, targetPhysical, dimension, records)
	})
}

func (c *Client) promoteStagingLocked(ctx context.Context, collectionName, oldPhysical, targetPhysical string, dimension int, records []vector.RestorePoint) error {
	stg := stagingTenant(collectionName)

	// Fence: a concurrent promoter may have already finished. Never reselect
	// a deleted stage or reclaim a completed generation from a stale snapshot.
	mark, ok, err := c.loadStagingMark(ctx, collectionName)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if mark.Status != stagingStatusReady && mark.Status != stagingStatusPromoting {
		return nil
	}
	if targetPhysical == "" {
		targetPhysical = mark.Physical
	}
	if dimension <= 0 {
		dimension = mark.Dimension
	}

	oldTenant := collectionName
	if entry, err := c.registryEntry(ctx, collectionName); err == nil {
		oldTenant = entry.liveTenant()
		if oldPhysical == "" {
			oldPhysical = entry.PhysicalCollection
		}
	}

	stgCount, err := c.countTenant(ctx, targetPhysical, stg)
	if err != nil {
		return err
	}

	if oldTenant != stg {
		if stgCount == 0 {
			// Staging already reclaimed; do not point the registry at an empty
			// stage or delete the currently selected generation.
			return nil
		}
		// Atomic generation switch: select the verified staging tenant before
		// deleting the previous complete generation.
		if err := c.selectTenant(ctx, collectionName, targetPhysical, dimension, stg); err != nil {
			return err
		}
	}

	// Drop previous generation tenant(s). Staging is selected now, so never
	// delete stg here. Canonical collectionName may still hold the old
	// generation when recovery resumes after the switch.
	tenantsToDrop := map[string]struct{}{collectionName: {}}
	if oldTenant != "" && oldTenant != stg {
		tenantsToDrop[oldTenant] = struct{}{}
	}
	for tenant := range tenantsToDrop {
		if tenant == stg {
			continue
		}
		// Never delete the currently selected tenant.
		if entry, err := c.registryEntry(ctx, collectionName); err == nil && entry.liveTenant() == tenant {
			continue
		}
		if oldPhysical != "" {
			_ = c.deleteByTenant(ctx, oldPhysical, tenant)
		}
		if targetPhysical != "" && targetPhysical != oldPhysical {
			_ = c.deleteByTenant(ctx, targetPhysical, tenant)
		}
	}
	if oldPhysical != "" && oldPhysical != targetPhysical {
		_ = c.deleteByTenant(ctx, oldPhysical, stg)
	}

	// Compact staging down to the canonical live tenant name.
	for _, record := range records {
		if err := c.upsertTenantPoint(ctx, targetPhysical, collectionName, record.EmbedID, record.Vector, record.Metadata); err != nil {
			return err
		}
	}
	if err := c.selectTenant(ctx, collectionName, targetPhysical, dimension, collectionName); err != nil {
		return err
	}
	_ = c.deleteByTenant(ctx, targetPhysical, stg)
	return c.clearStagingMark(ctx, collectionName)
}

// recoverPendingStageLocked rolls a ready/promoting stage forward. It runs
// under the per-collection promote lock and re-reads the marker, registry, and
// staging records there, so a delayed caller can never promote a snapshot of
// a stage that another promoter already completed (or that a later restore
// has since replaced).
func (c *Client) recoverPendingStageLocked(ctx context.Context, collectionName string) error {
	mark, ok, err := c.loadStagingMark(ctx, collectionName)
	if err != nil {
		return err
	}
	if !ok || (mark.Status != stagingStatusReady && mark.Status != stagingStatusPromoting) {
		// Another promoter finished, or a new restore is still writing.
		return nil
	}
	var oldPhysical string
	if entry, err := c.registryEntry(ctx, collectionName); err == nil {
		oldPhysical = entry.PhysicalCollection
	} else if !strings.Contains(err.Error(), "not found") {
		return err
	}
	stg := stagingTenant(collectionName)
	records, err := c.scrollTenantRecords(ctx, mark.Physical, stg)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return c.finishEmptyStagingLocked(ctx, collectionName, mark.Physical, stg)
	}
	return c.promoteStagingLocked(ctx, collectionName, oldPhysical, mark.Physical, mark.Dimension, records)
}

// selectTenant publishes a new active tenant. It takes the per-collection
// selection lock exclusively, so it waits for in-flight reads that resolved
// the previous selection; generations are only reclaimed after this returns.
func (c *Client) selectTenant(ctx context.Context, collectionName, physical string, dimension int, activeTenant string) error {
	if c.testBeforeSelect != nil {
		c.testBeforeSelect(activeTenant)
	}
	mu := c.selectLockFor(collectionName)
	mu.Lock()
	defer mu.Unlock()
	return c.ensureLiveRegistry(ctx, collectionName, physical, dimension, activeTenant)
}

func (c *Client) finishEmptyStagingLocked(ctx context.Context, collectionName, physical, stg string) error {
	mark, ok, err := c.loadStagingMark(ctx, collectionName)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if mark.Status != stagingStatusReady && mark.Status != stagingStatusPromoting {
		return nil
	}
	// Re-check under the promote lock after a concurrent promoter may have
	// already switched and cleaned up.
	stgCount, err := c.countTenant(ctx, physical, stg)
	if err != nil {
		return err
	}
	if stgCount > 0 {
		return fmt.Errorf("staging mark ready/promoting but caller saw empty staging")
	}
	if entry, err := c.registryEntry(ctx, collectionName); err == nil && entry.liveTenant() == stg {
		return fmt.Errorf("staging mark ready/promoting but staging tenant is empty")
	}
	_ = c.deleteByTenant(ctx, physical, stg)
	return c.clearStagingMark(ctx, collectionName)
}

func (c *Client) selectedGenerationReadable(ctx context.Context, collectionName, stg string) bool {
	entry, err := c.registryEntry(ctx, collectionName)
	if err != nil {
		return false
	}
	live := entry.liveTenant()
	if live == stg {
		// Generation switch already published the replacement set.
		return true
	}
	count, err := c.countTenant(ctx, entry.PhysicalCollection, live)
	if err != nil {
		return false
	}
	if count > 0 {
		return true
	}
	// Live empty: only treat as readable when staging also has nothing left to
	// promote (genuinely empty DB). Otherwise the switch still needs to finish.
	stgCount, err := c.countTenant(ctx, entry.PhysicalCollection, stg)
	if err != nil {
		return false
	}
	return stgCount == 0
}

func (c *Client) ensureLiveRegistry(ctx context.Context, collectionName, physical string, dimension int, activeTenant string) error {
	if activeTenant == "" {
		activeTenant = collectionName
	}
	return c.do(ctx, http.MethodPut, "/collections/"+registryCollection+"/points?wait=true", map[string]any{
		"points": []map[string]any{{
			"id":     deterministicUUID(collectionName),
			"vector": []float64{0},
			"payload": map[string]any{
				"collection_name":     collectionName,
				"physical_collection": physical,
				"dimension":           dimension,
				activeTenantKey:       activeTenant,
			},
		}},
	}, &genericResponse{})
}

func (c *Client) upsertTenantPoint(ctx context.Context, physical, tenant, embedID string, value []float64, metadata map[string]any) error {
	physicalID := pointIDForTenant(tenant, embedID)
	payload := map[string]any{
		tenantPayloadKey: tenant,
		embedPayloadKey:  embedID,
	}
	if metadata != nil {
		payload["metadata"] = metadata
	}
	return c.do(ctx, http.MethodPut, "/collections/"+physical+"/points?wait=true", map[string]any{
		"points": []map[string]any{{
			"id":      physicalID,
			"vector":  value,
			"payload": payload,
		}},
	}, &genericResponse{})
}

func (c *Client) deleteByTenant(ctx context.Context, physical, tenant string) error {
	return c.do(ctx, http.MethodPost, "/collections/"+physical+"/points/delete?wait=true", map[string]any{
		"filter": tenantFilter(tenant),
	}, &genericResponse{})
}

func (c *Client) countTenant(ctx context.Context, physical, tenant string) (int, error) {
	var response countResponse
	if err := c.do(ctx, http.MethodPost, "/collections/"+physical+"/points/count", map[string]any{
		"exact":  true,
		"filter": tenantFilter(tenant),
	}, &response); err != nil {
		return 0, err
	}
	return response.Result.Count, nil
}

func (c *Client) scrollTenantRecords(ctx context.Context, physical, tenant string) ([]vector.RestorePoint, error) {
	out := []vector.RestorePoint{}
	var offset any
	for {
		payload := map[string]any{
			"limit":        64,
			"with_payload": true,
			"with_vector":  true,
			"filter":       tenantFilter(tenant),
		}
		if offset != nil {
			payload["offset"] = offset
		}
		var response scrollResponse
		if err := c.do(ctx, http.MethodPost, "/collections/"+physical+"/points/scroll", payload, &response); err != nil {
			return nil, err
		}
		for _, point := range response.Result.Points {
			embedID := externalEmbedID(point.ID, point.Payload)
			meta, _ := point.Payload["metadata"].(map[string]any)
			out = append(out, vector.RestorePoint{
				EmbedID:  embedID,
				Vector:   append([]float64(nil), point.Vector...),
				Metadata: meta,
			})
		}
		if response.Result.NextPageOffset == nil {
			break
		}
		offset = response.Result.NextPageOffset
	}
	return out, nil
}

func (c *Client) upsertStagingMark(ctx context.Context, collectionName, physical string, dimension, expected int, status string) error {
	return c.do(ctx, http.MethodPut, "/collections/"+registryCollection+"/points?wait=true", map[string]any{
		"points": []map[string]any{{
			"id":     stagingRegistryID(collectionName),
			"vector": []float64{0},
			"payload": map[string]any{
				stagingOfKey:        collectionName,
				stagingPhysicalKey:  physical,
				stagingDimensionKey: dimension,
				stagingExpectedKey:  expected,
				stagingStatusKey:    status,
			},
		}},
	}, &genericResponse{})
}

func (c *Client) clearStagingMark(ctx context.Context, collectionName string) error {
	return c.do(ctx, http.MethodPost, "/collections/"+registryCollection+"/points/delete?wait=true", map[string]any{
		"points": []string{stagingRegistryID(collectionName)},
	}, &genericResponse{})
}

func (c *Client) loadStagingMark(ctx context.Context, collectionName string) (stagingMark, bool, error) {
	var response pointsResponse
	if err := c.do(ctx, http.MethodPost, "/collections/"+registryCollection+"/points", map[string]any{
		"ids":          []string{stagingRegistryID(collectionName)},
		"with_payload": true,
		"with_vector":  false,
	}, &response); err != nil {
		return stagingMark{}, false, err
	}
	if len(response.Result) == 0 {
		return stagingMark{}, false, nil
	}
	payload := response.Result[0].Payload
	mark := stagingMark{}
	mark.Physical, _ = payload[stagingPhysicalKey].(string)
	mark.Status, _ = payload[stagingStatusKey].(string)
	if dim, ok := numberToInt(payload[stagingDimensionKey]); ok {
		mark.Dimension = dim
	}
	if expected, ok := numberToInt(payload[stagingExpectedKey]); ok {
		mark.Expected = expected
	}
	if mark.Physical == "" || mark.Status == "" || mark.Dimension <= 0 {
		return stagingMark{}, false, nil
	}
	return mark, true, nil
}

func (c *Client) dropOrphanStagingTenants(ctx context.Context, collectionName string) error {
	stg := stagingTenant(collectionName)
	// Best-effort: try common physical collections from live registry only.
	// Never delete the active tenant if it happens to be the staging name.
	if entry, err := c.registryEntry(ctx, collectionName); err == nil {
		if entry.liveTenant() != stg {
			_ = c.deleteByTenant(ctx, entry.PhysicalCollection, stg)
		}
	}
	return nil
}

func (c *Client) withPromoteLock(collectionName string, fn func() error) error {
	mu := c.promoteLockFor(collectionName)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func (c *Client) selectLockFor(collectionName string) *sync.RWMutex {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	if c.selectLocks == nil {
		c.selectLocks = map[string]*sync.RWMutex{}
	}
	mu, ok := c.selectLocks[collectionName]
	if !ok {
		mu = &sync.RWMutex{}
		c.selectLocks[collectionName] = mu
	}
	return mu
}

func (c *Client) promoteLockFor(collectionName string) *sync.Mutex {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	if c.promoteLocks == nil {
		c.promoteLocks = map[string]*sync.Mutex{}
	}
	mu, ok := c.promoteLocks[collectionName]
	if !ok {
		mu = &sync.Mutex{}
		c.promoteLocks[collectionName] = mu
	}
	return mu
}

func (c *Client) claimStagingOwn(collectionName string) bool {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	if c.stagingOwn == nil {
		c.stagingOwn = map[string]struct{}{}
	}
	if _, ok := c.stagingOwn[collectionName]; ok {
		return false
	}
	c.stagingOwn[collectionName] = struct{}{}
	return true
}

func (c *Client) releaseStagingOwn(collectionName string) {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	delete(c.stagingOwn, collectionName)
}

func (c *Client) hasStagingOwn(collectionName string) bool {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	_, ok := c.stagingOwn[collectionName]
	return ok
}
