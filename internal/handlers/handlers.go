package handlers

import (
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"recall-miner/internal/client"
	"recall-miner/internal/config"
	"recall-miner/internal/heartbeat"
	"recall-miner/internal/registry"
)

type Handlers struct {
	cfg               config.Config
	log               *slog.Logger
	httpClient        *http.Client
	apiClient         *client.Client
	registry          *registry.Registry
	heartbeats        *heartbeat.Store
	configMu          sync.RWMutex
	configVersion     int
	sidecarConfig     map[string]config.SidecarConfig
	processedRequests atomic.Uint64
}

func New(cfg config.Config, logger *slog.Logger, httpClient *http.Client, apiClient *client.Client, registry *registry.Registry, heartbeats *heartbeat.Store) *Handlers {
	return &Handlers{
		cfg:           cfg,
		log:           logger,
		httpClient:    httpClient,
		apiClient:     apiClient,
		registry:      registry,
		heartbeats:    heartbeats,
		sidecarConfig: cfg.SidecarConfig,
	}
}

// ResourceStats samples this process and local filesystem directly. The advertised
// endpoint may be behind NAT, a proxy or TLS termination and is not a self-health
// destination. These observations do not establish inbound service readiness.
func (h *Handlers) ResourceStats() map[string]any {
	return h.resourceStats(inventoryDiskUsedGB)
}

func (h *Handlers) resourceStats(diskUsed func() (float64, bool)) map[string]any {
	stats := h.buildInventoryStats()
	// Preserve the existing placeholder counters; /health never measured databases
	// or vectors. Actual capability readiness remains the healthy registry's job.
	stats["dbs_count"] = 0
	stats["vectors_count"] = 0
	if used, ok := diskUsed(); ok {
		stats["storage_used_gb"] = used
		stats["checked_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	// A failed local sample is unknown, not zero usage or a refreshed old value.
	return stats
}

func (h *Handlers) Registry() *registry.Registry { return h.registry }

// AvailableCapabilities returns only the capabilities backed by a healthy
// registered sidecar, so registration and heartbeats never advertise a
// capability the miner cannot serve. `db`, `embedding`, and `rerank` each
// require a healthy sidecar advertising that capability; `generate`
// additionally requires GENERATION_ENABLED.
func (h *Handlers) AvailableCapabilities() []string {
	now := time.Now()
	caps := []string{}
	if h.hasHealthyCapability("db", now) {
		caps = append(caps, "db")
	}
	if h.hasHealthyCapability("embedding", now) {
		caps = append(caps, "embedding")
	}
	if h.hasHealthyCapability("rerank", now) {
		caps = append(caps, "rerank")
	}
	// generate uses the same selection as routing: only the configured Chutes
	// backend advertising the configured model, never any other generate sidecar.
	if _, ok := h.chutesGenerationReady(); h.cfg.GenerationEnabled && ok {
		caps = append(caps, "generate")
	}
	return caps
}

func (h *Handlers) hasHealthyCapability(capability string, now time.Time) bool {
	_ = now
	return len(h.registry.ByCapabilityHealthy(capability)) > 0
}
