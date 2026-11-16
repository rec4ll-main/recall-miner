package handlers

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"recall-miner/internal/config"
	"recall-miner/internal/registry"
	"testing"
	"time"
)

type selfHealthTransport func(*http.Request) (*http.Response, error)

func (f selfHealthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSelfHealthStatsNeverDialAdvertisedEndpoint(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: selfHealthTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("public WAN hairpin unavailable")
	})}
	h := New(config.Config{MinerAPIURL: "https://192.0.2.1:443"}, slog.New(slog.NewTextHandler(io.Discard, nil)), client, nil, registry.New(), nil)
	for _, endpoint := range []string{"https://192.0.2.1:443", "https://198.51.100.2:7443", "https://proxy.example.invalid/miner", ""} {
		h.cfg.MinerAPIURL = endpoint
		h.IncProcessedRequests()
		stats := h.ResourceStats()
		if _, ok := stats["os"]; !ok {
			t.Fatal("local inventory lost")
		}
		if stats["processed_requests"] != h.processedRequestsCount() {
			t.Fatal("stale counter")
		}
		if h.cfg.MinerAPIURL != endpoint {
			t.Fatal("advertised endpoint changed")
		}
	}
	if calls != 0 {
		t.Fatalf("public self calls=%d", calls)
	}
}
func TestSelfHealthLocalSampleFailureDoesNotRefreshOldUsage(t *testing.T) {
	h := New(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, registry.New(), nil)
	first := h.resourceStats(func() (float64, bool) { return 12.5, true })
	if first["storage_used_gb"] != 12.5 {
		t.Fatal(first)
	}
	if _, err := time.Parse(time.RFC3339Nano, first["checked_at"].(string)); err != nil {
		t.Fatal(err)
	}
	h.IncProcessedRequests()
	failed := h.resourceStats(func() (float64, bool) { return 0, false })
	for _, key := range []string{"storage_used_gb", "checked_at"} {
		if _, ok := failed[key]; ok {
			t.Fatalf("unknown sample advertised %s", key)
		}
	}
	if failed["processed_requests"] != int64(1) || failed["os"] == nil {
		t.Fatal("failure discarded live inventory")
	}
	next := h.resourceStats(func() (float64, bool) { return 4.0, true })
	if next["storage_used_gb"] != 4.0 || first["storage_used_gb"] != 12.5 {
		t.Fatal("samples reused or mutated")
	}
}
func TestSelfHealthDoesNotOverrideLocalCapabilityFailure(t *testing.T) {
	reg := registry.New()
	h := New(config.Config{GenerationEnabled: true, GenerationBackendName: "sidecar", GenerationModel: "fixture/model"}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, reg, nil)
	reg.Register(registry.RegisterRequest{Name: "sidecar", URL: "http://loopback-helper.invalid:9200", Capabilities: []string{"generate"}, Models: []string{"fixture/model"}, Metadata: map[string]any{"generation_connector":"chutes"}})
	if len(h.AvailableCapabilities()) != 1 {
		t.Fatal("fixture missing healthy capability")
	}
	reg.Touch("sidecar", "", map[string]any{"status": "failed"})
	h.resourceStats(func() (float64, bool) { return 4, true })
	if len(h.AvailableCapabilities()) != 0 {
		t.Fatal("local inventory masked failed service")
	}
	reg.Unregister("sidecar")
	if len(h.AvailableCapabilities()) != 0 {
		t.Fatal("removed service advertised")
	}
}
