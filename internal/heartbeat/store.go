package heartbeat

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"recall-miner/internal/config"
)

const maxRecentHeartbeats = 1000

// MaxListLimit caps a request-supplied heartbeat list limit.
const MaxListLimit = 1000

type Sink interface {
	Write(context.Context, Record) error
	List(context.Context, string, int) ([]Record, error)
	Close() error
}

type Store struct {
	mu     sync.RWMutex
	wg     sync.WaitGroup
	recent []Record
	log    *slog.Logger
	sink   Sink
}

type Record struct {
	ObservedAt         time.Time      `json:"observed_at"`
	ServiceName        string         `json:"service_name"`
	MachineID          string         `json:"machine_id"`
	Status             string         `json:"status"`
	CPUPercent         *float64       `json:"cpu_percent,omitempty"`
	MemoryTotalBytes   *uint64        `json:"memory_total_bytes,omitempty"`
	MemoryUsedBytes    *uint64        `json:"memory_used_bytes,omitempty"`
	MemoryUsedPercent  *float64       `json:"memory_used_percent,omitempty"`
	DiskTotalBytes     *uint64        `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes      *uint64        `json:"disk_used_bytes,omitempty"`
	DiskUsedPercent    *float64       `json:"disk_used_percent,omitempty"`
	UpstreamStatusCode *uint16        `json:"upstream_status_code,omitempty"`
	Health             map[string]any `json:"health"`
	Payload            map[string]any `json:"payload"`
}

func NewStore(cfg config.Config, logger *slog.Logger) *Store {
	sink, err := NewSink(cfg)
	if err != nil {
		sink = nil
		if logger != nil {
			logger.Warn("heartbeat storage disabled", "mode", cfg.MetricsStorage, "error", err)
		}
	}
	return &Store{
		log:  logger,
		sink: sink,
	}
}

func (s *Store) Record(ctx context.Context, serviceName, machineID string, health map[string]any) {
	if health == nil {
		health = map[string]any{}
	}
	record := Record{
		ObservedAt:  time.Now().UTC(),
		ServiceName: serviceName,
		MachineID:   machineID,
		Status:      stringValue(health["status"]),
		Health:      health,
		Payload: map[string]any{
			"name":       serviceName,
			"machine_id": machineID,
			"health":     health,
		},
	}
	metrics := mapValue(health["metrics"])
	memory := mapValue(metrics["memory"])
	disk := mapValue(metrics["disk"])
	record.CPUPercent = floatPointer(metrics["cpu_percent"])
	record.MemoryTotalBytes = uintPointer(memory["total_bytes"])
	record.MemoryUsedBytes = uintPointer(memory["used_bytes"])
	record.MemoryUsedPercent = floatPointer(memory["used_percent"])
	record.DiskTotalBytes = uintPointer(disk["total_bytes"])
	record.DiskUsedBytes = uintPointer(disk["used_bytes"])
	record.DiskUsedPercent = floatPointer(disk["used_percent"])
	record.UpstreamStatusCode = uint16Pointer(health["upstream_status_code"])

	s.mu.Lock()
	s.recent = append(s.recent, record)
	if len(s.recent) > maxRecentHeartbeats {
		s.recent = append([]Record(nil), s.recent[len(s.recent)-maxRecentHeartbeats:]...)
	}
	s.mu.Unlock()

	if s.sink != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.sink.Write(context.WithoutCancel(ctx), record); err != nil && s.log != nil {
				s.log.Warn("heartbeat storage write failed", "error", err)
			}
		}()
	}
}

func (s *Store) List(serviceName string, limit int) []Record {
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if recent := s.listRecent(serviceName, limit); len(recent) > 0 {
		return recent
	}
	if s.sink != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		records, err := s.sink.List(ctx, serviceName, limit)
		if err != nil && s.log != nil {
			s.log.Warn("heartbeat storage list failed", "error", err)
		}
		if len(records) > 0 {
			return records
		}
	}
	return nil
}

func (s *Store) Close() {
	s.wg.Wait()
	if s.sink != nil {
		if err := s.sink.Close(); err != nil && s.log != nil {
			s.log.Warn("heartbeat storage close failed", "error", err)
		}
	}
}

func (s *Store) listRecent(serviceName string, limit int) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, min(limit, len(s.recent)))
	for i := len(s.recent) - 1; i >= 0 && len(out) < limit; i-- {
		if serviceName == "" || s.recent[i].ServiceName == serviceName {
			out = append(out, s.recent[i])
		}
	}
	return out
}

func mapValue(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func stringValue(value any) string {
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
}

func floatPointer(value any) *float64 {
	switch typed := value.(type) {
	case float64:
		return &typed
	case float32:
		v := float64(typed)
		return &v
	case int:
		v := float64(typed)
		return &v
	case int64:
		v := float64(typed)
		return &v
	case json.Number:
		if v, err := typed.Float64(); err == nil {
			return &v
		}
	}
	return nil
}

func uintPointer(value any) *uint64 {
	if float := floatPointer(value); float != nil && *float >= 0 {
		v := uint64(*float)
		return &v
	}
	return nil
}

func uint16Pointer(value any) *uint16 {
	if float := floatPointer(value); float != nil && *float >= 0 {
		v := uint16(*float)
		return &v
	}
	return nil
}
