package heartbeat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"recall-miner/internal/config"
)

func TestNewSinkNone(t *testing.T) {
	sink, err := NewSink(config.Config{MetricsStorage: StorageNone})
	if err != nil {
		t.Fatalf("NewSink none error = %v", err)
	}
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("expected NoopSink, got %T", sink)
	}
}

func TestDuckDBSinkRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeats.duckdb")
	sink, err := NewDuckDBSink(path)
	if err != nil {
		t.Fatalf("NewDuckDBSink error = %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	cpu := 12.5
	memoryTotal := uint64(1024)
	upstreamStatus := uint16(200)
	record := Record{
		ObservedAt:         time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC),
		ServiceName:        "db",
		MachineID:          "machine-1",
		Status:             "ok",
		CPUPercent:         &cpu,
		MemoryTotalBytes:   &memoryTotal,
		UpstreamStatusCode: &upstreamStatus,
		Health:             map[string]any{"status": "ok"},
		Payload:            map[string]any{"name": "db"},
	}
	if err := sink.Write(context.Background(), record); err != nil {
		t.Fatalf("Write error = %v", err)
	}

	records, err := sink.List(context.Background(), "db", 10)
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	got := records[0]
	if got.ServiceName != "db" || got.MachineID != "machine-1" || got.Status != "ok" {
		t.Fatalf("record = %#v", got)
	}
	if got.CPUPercent == nil || *got.CPUPercent != cpu {
		t.Fatalf("cpu_percent = %#v", got.CPUPercent)
	}
	if got.MemoryTotalBytes == nil || *got.MemoryTotalBytes != memoryTotal {
		t.Fatalf("memory_total_bytes = %#v", got.MemoryTotalBytes)
	}
	if got.UpstreamStatusCode == nil || *got.UpstreamStatusCode != upstreamStatus {
		t.Fatalf("upstream_status_code = %#v", got.UpstreamStatusCode)
	}
}

func TestClickHouseSinkRetriesDDLAfterTransientFailure(t *testing.T) {
	var calls atomic.Int32
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 1 {
			http.Error(w, "transient ddl failure", http.StatusInternalServerError)
			return
		}
		ready.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := NewClickHouseSink(server.URL, "recall_ts")
	cpu := 1.0
	record := Record{
		ObservedAt:  time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC),
		ServiceName: "db",
		MachineID:   "machine-1",
		Status:      "ok",
		CPUPercent:  &cpu,
	}

	if err := sink.Write(context.Background(), record); err == nil {
		t.Fatal("expected first Write to fail on transient DDL failure")
	}
	if ready.Load() {
		t.Fatal("second call should not have happened before the retry assertion")
	}
	if err := sink.Write(context.Background(), record); err != nil {
		t.Fatalf("expected second Write to retry DDL and succeed: %v", err)
	}
	if !ready.Load() {
		t.Fatal("expected DDL retried on subsequent Write")
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("expected at least 3 calls (failed DDL db + failed DDL table + successful retry db), got %d", got)
	}
}

func TestClickHouseSinkDoesNotRetryAfterSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sink := NewClickHouseSink(server.URL, "recall_ts")
	cpu := 1.0
	record := Record{
		ObservedAt:  time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC),
		ServiceName: "db",
		MachineID:   "machine-1",
		Status:      "ok",
		CPUPercent:  &cpu,
	}
	if err := sink.Write(context.Background(), record); err != nil {
		t.Fatalf("expected first Write to succeed: %v", err)
	}
	before := calls.Load()
	if err := sink.Write(context.Background(), record); err != nil {
		t.Fatalf("expected second Write to succeed: %v", err)
	}
	// After successful init only the INSERT should hit the server: one call
	// per Write, no re-running of the DDL.
	if got := calls.Load(); got != before+1 {
		t.Fatalf("expected exactly %d calls after success, got %d", before+1, got)
	}
}
