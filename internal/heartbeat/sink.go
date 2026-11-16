package heartbeat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"recall-miner/internal/config"
)

const (
	StorageNone       = "none"
	StorageDuckDB     = "duckdb"
	StorageClickHouse = "clickhouse"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func NewSink(cfg config.Config) (Sink, error) {
	switch strings.ToLower(cfg.MetricsStorage) {
	case "", StorageDuckDB:
		return NewDuckDBSink(cfg.MetricsDuckDBPath)
	case StorageNone:
		return NoopSink{}, nil
	case StorageClickHouse:
		if cfg.ClickHouseURL == "" {
			return nil, fmt.Errorf("CLICKHOUSE_URL is required when METRICS_STORAGE=clickhouse")
		}
		return NewClickHouseSink(cfg.ClickHouseURL, cfg.ClickHouseDB), nil
	default:
		return nil, fmt.Errorf("unsupported metrics storage mode %q", cfg.MetricsStorage)
	}
}

func healthJSON(record Record) (string, error) {
	return marshalJSONString(record.Health)
}

func payloadJSON(record Record) (string, error) {
	return marshalJSONString(record.Payload)
}

func marshalJSONString(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func parseRecordJSON(healthRaw, payloadRaw string, record *Record) {
	if healthRaw != "" {
		_ = json.Unmarshal([]byte(healthRaw), &record.Health)
	}
	if payloadRaw != "" {
		_ = json.Unmarshal([]byte(payloadRaw), &record.Payload)
	}
	if record.Health == nil {
		record.Health = map[string]any{}
	}
	if record.Payload == nil {
		record.Payload = map[string]any{}
	}
}

func safeIdentifier(value, fallback string) string {
	if identifierPattern.MatchString(value) {
		return value
	}
	return fallback
}

func observedAtString(observedAt time.Time) string {
	return observedAt.UTC().Format("2006-01-02 15:04:05.000")
}
