package handlers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"recall-miner/internal/client"
	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

func inventoryTestHandler(t *testing.T) *Handlers {
	t.Helper()
	cfg := config.FromEnv()
	cfg.MinerAPIURL = "" // force health miss unless overridden
	httpClient := &http.Client{Timeout: time.Second}
	apiClient := client.New(httpClient, "test")
	reg := registry.New()
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), httpClient, apiClient, reg, nil)
}

func TestResourceStats_EmitsAllowlistKeys(t *testing.T) {
	t.Setenv("MINER_REGION", "us-east")
	t.Setenv("MINER_VERSION", "recall-miner/stage-test")
	// Avoid env capacity override masking disk detection assertions elsewhere.
	t.Setenv("MINER_STORAGE_CAPACITY_GB", "")
	t.Setenv("MINER_PUBLIC_STORAGE_GB", "")

	h := inventoryTestHandler(t)
	stats := h.ResourceStats()

	required := []string{
		"region", "os", "cpu", "cpu_model", "ram_gb", "storage_gb",
		"uptime_seconds", "processed_requests", "miner_version", "version",
	}
	for _, key := range required {
		if _, ok := stats[key]; !ok {
			t.Fatalf("missing allowlist key %q in %#v", key, stats)
		}
	}

	if stats["os"] != runtime.GOOS {
		t.Fatalf("os=%v want %s", stats["os"], runtime.GOOS)
	}
	if stats["region"] != "us-east" {
		t.Fatalf("region=%v", stats["region"])
	}
	if stats["miner_version"] != "recall-miner/stage-test" {
		t.Fatalf("miner_version=%v", stats["miner_version"])
	}
	if stats["version"] != stats["miner_version"] {
		t.Fatalf("version=%v miner_version=%v", stats["version"], stats["miner_version"])
	}
	if _, ok := stats["ram_gb"].(float64); !ok {
		t.Fatalf("ram_gb type %T", stats["ram_gb"])
	}
	if stats["ram_gb"].(float64) <= 0 {
		t.Fatalf("ram_gb must be > 0, got %v", stats["ram_gb"])
	}
	storageGB, ok := stats["storage_gb"].(float64)
	if !ok || storageGB <= 0 {
		t.Fatalf("storage_gb must be > 0 float, got %T %v", stats["storage_gb"], stats["storage_gb"])
	}
	uptime, ok := stats["uptime_seconds"].(int64)
	if !ok || uptime < 0 {
		t.Fatalf("uptime_seconds=%T %v", stats["uptime_seconds"], stats["uptime_seconds"])
	}
	if _, ok := stats["processed_requests"].(int64); !ok {
		t.Fatalf("processed_requests type %T", stats["processed_requests"])
	}
}

func TestResourceStats_StorageCapacityNeverEqualsUsedMapping(t *testing.T) {
	h := inventoryTestHandler(t)
	// Health OK path also sets storage_used_gb — use fake health server.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()
	h.cfg.MinerAPIURL = ts.URL

	t.Setenv("MINER_STORAGE_CAPACITY_GB", "500")
	stats := h.ResourceStats()

	capGB, ok := stats["storage_gb"].(float64)
	if !ok {
		t.Fatalf("storage_gb missing: %#v", stats)
	}
	if capGB != 500 {
		t.Fatalf("storage_gb=%v want 500 (capacity)", capGB)
	}
	used, ok := stats["storage_used_gb"]
	if !ok {
		t.Fatal("expected storage_used_gb on health OK")
	}
	usedGB, ok := used.(float64)
	if !ok {
		t.Fatalf("storage_used_gb type %T", used)
	}
	// Capacity must not be copied from used.
	if capGB == usedGB && usedGB != 500 {
		t.Fatalf("storage_gb incorrectly equals storage_used_gb (%v)", usedGB)
	}
	// Explicit: used key must not be the capacity source when used is small.
	if usedGB == 500 && capGB == 500 {
		// Ambiguous only if disk used somehow equals 500 — still capacity came from env.
		t.Log("note: used coincidentally 500; capacity still from MINER_STORAGE_CAPACITY_GB")
	}
}

func TestResourceStats_NoIPOrURLInStrings(t *testing.T) {
	t.Setenv("MINER_REGION", "https://evil.example.com")
	t.Setenv("MINER_VERSION", "http://bad")
	h := inventoryTestHandler(t)
	stats := h.ResourceStats()

	// Unsafe region env → omitted (or not the leaky value). Default only applies when unset.
	if region, ok := stats["region"].(string); ok {
		if strings.Contains(region, "://") || strings.Contains(region, "evil.example") {
			t.Fatalf("leaky region emitted: %q", region)
		}
	}
	ver, _ := stats["miner_version"].(string)
	if strings.Contains(ver, "://") || strings.HasPrefix(strings.ToLower(ver), "http") {
		t.Fatalf("leaky miner_version emitted: %q", ver)
	}

	// Inject leaky GPU metadata — must be omitted.
	h.registry.Register(registry.RegisterRequest{
		Name:         "gpu-sidecar",
		URL:          "http://127.0.0.1:9",
		Capabilities: []string{"generate"},
		Metadata:     map[string]any{"gpu_model": "10.0.0.5", "gpu": "node.internal"},
	})
	stats = h.ResourceStats()
	if g, ok := stats["gpu"]; ok && g != nil && g != "" {
		gs, _ := g.(string)
		if strings.Contains(gs, "10.0.0.5") || strings.Contains(gs, ".internal") {
			t.Fatalf("leaky gpu emitted: %q", gs)
		}
	}

	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, bad := range []string{"://", "127.0.0.1", "10.0.0.5"} {
		// URL of registry is not in resource_stats; ensure payload strings clean.
		if strings.Contains(s, bad) {
			// allow only if somehow in non-string — fail hard
			t.Fatalf("resource_stats JSON contains %q: %s", bad, s)
		}
	}
}

func TestResourceStats_GPUEmittedWhenSafeMetadataPresent(t *testing.T) {
	h := inventoryTestHandler(t)
	h.registry.Register(registry.RegisterRequest{
		Name:         "gpu-sidecar",
		URL:          "http://127.0.0.1:9",
		Capabilities: []string{"generate"},
		Metadata:     map[string]any{"gpu_model": "RTX 4090"},
	})
	stats := h.ResourceStats()
	if stats["gpu"] != "RTX 4090" || stats["gpu_model"] != "RTX 4090" {
		t.Fatalf("gpu not emitted from safe metadata: %#v", stats)
	}
}

func TestResourceStats_GPUOmittedWhenUnknown(t *testing.T) {
	h := inventoryTestHandler(t)
	stats := h.ResourceStats()
	// On hosts without nvidia / metadata, gpu keys must be absent (not invented).
	if _, hasNvidia := inventoryNvidiaGPUModel(), false; false {
		_ = hasNvidia
	}
	if inventoryNvidiaGPUModel() == "" {
		if _, ok := stats["gpu"]; ok {
			t.Fatalf("gpu must be omitted when unknown, got %v", stats["gpu"])
		}
		if _, ok := stats["gpu_model"]; ok {
			t.Fatalf("gpu_model must be omitted when unknown, got %v", stats["gpu_model"])
		}
	}
}

func TestIncProcessedRequests(t *testing.T) {
	h := inventoryTestHandler(t)
	if h.processedRequestsCount() != 0 {
		t.Fatalf("start count=%d", h.processedRequestsCount())
	}
	h.IncProcessedRequests()
	h.IncProcessedRequests()
	stats := h.ResourceStats()
	if stats["processed_requests"].(int64) != 2 {
		t.Fatalf("processed_requests=%v", stats["processed_requests"])
	}
}

func TestInventoryStringSafe(t *testing.T) {
	safe := []string{"us-east", "linux", "AMD EPYC", "RTX 4090", "1.2.3", "recall-miner/1.0"}
	for _, s := range safe {
		if !inventoryStringSafe(s) {
			t.Fatalf("expected safe: %q", s)
		}
	}
	unsafe := []string{
		"https://x.com", "http://a", "10.0.0.5", "node.internal",
		"evil.example.com", "host:8080", "ws://x",
	}
	for _, s := range unsafe {
		if inventoryStringSafe(s) {
			t.Fatalf("expected unsafe: %q", s)
		}
	}
}

func TestStorageCapacityNotFromUsed(t *testing.T) {
	// Direct helper contract: capacity env wins; used is separate helper.
	t.Setenv("MINER_STORAGE_CAPACITY_GB", "123")
	capGB := inventoryStorageCapacityGB()
	if capGB != 123 {
		t.Fatalf("capacity=%v", capGB)
	}
	used, ok := inventoryDiskUsedGB()
	if ok && used == 123 && capGB == 123 {
		t.Log("coincidental equality possible; sources remain distinct helpers")
	}
	// Ensure mapping function does not read storage_used key — covered by buildInventoryStats
	h := inventoryTestHandler(t)
	stats := h.buildInventoryStats()
	if _, ok := stats["storage_used_gb"]; ok {
		t.Fatal("buildInventoryStats must not emit storage_used_gb (capacity path only)")
	}
	if stats["storage_gb"].(float64) != 123 {
		t.Fatalf("storage_gb=%v", stats["storage_gb"])
	}
}
