package heartbeat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ClickHouseSink struct {
	client      *http.Client
	url         string
	db          string
	mu          sync.Mutex
	initialized bool
}

func NewClickHouseSink(clickHouseURL, db string) *ClickHouseSink {
	return &ClickHouseSink{client: &http.Client{Timeout: 10 * time.Second}, url: clickHouseURL, db: safeIdentifier(db, "recall_ts")}
}

func (s *ClickHouseSink) Write(ctx context.Context, record Record) error {
	if err := s.ensureInitialized(ctx); err != nil {
		return err
	}
	row, err := s.row(record)
	if err != nil {
		return err
	}
	return s.post(ctx, fmt.Sprintf("INSERT INTO %s.service_heartbeats FORMAT JSONEachRow", s.db), row)
}

func (s *ClickHouseSink) List(ctx context.Context, serviceName string, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	if err := s.ensureInitialized(ctx); err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`SELECT observed_at, service_name, machine_id, status, cpu_percent, memory_total_bytes, memory_used_bytes, memory_used_percent, disk_total_bytes, disk_used_bytes, disk_used_percent, upstream_status_code, health_json, payload_json FROM %s.service_heartbeats`, s.db)
	if serviceName != "" {
		query += " WHERE service_name = " + quoteClickHouseString(serviceName)
	}
	query += " ORDER BY observed_at DESC LIMIT " + strconv.Itoa(limit) + " FORMAT JSONEachRow"
	body, err := s.query(ctx, query)
	if err != nil {
		return nil, err
	}
	return parseClickHouseRows(body)
}

func (s *ClickHouseSink) Close() error { return nil }

func (s *ClickHouseSink) ensureInitialized(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized {
		return nil
	}
	if err := s.post(ctx, "CREATE DATABASE IF NOT EXISTS "+s.db, nil); err != nil {
		return err
	}
	if err := s.post(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.service_heartbeats (
    observed_at DateTime64(3),
    service_name LowCardinality(String),
    machine_id String,
    status LowCardinality(String),
    cpu_percent Nullable(Float64),
    memory_total_bytes Nullable(UInt64),
    memory_used_bytes Nullable(UInt64),
    memory_used_percent Nullable(Float64),
    disk_total_bytes Nullable(UInt64),
    disk_used_bytes Nullable(UInt64),
    disk_used_percent Nullable(Float64),
    upstream_status_code Nullable(UInt16),
    health_json String,
    payload_json String
)
ENGINE = MergeTree()
ORDER BY (service_name, observed_at)
TTL observed_at + INTERVAL 90 DAY DELETE`, s.db), nil); err != nil {
		return err
	}
	s.initialized = true
	return nil
}

func (s *ClickHouseSink) row(record Record) ([]byte, error) {
	health, err := healthJSON(record)
	if err != nil {
		return nil, err
	}
	payload, err := payloadJSON(record)
	if err != nil {
		return nil, err
	}
	row := map[string]any{
		"observed_at":          observedAtString(record.ObservedAt),
		"service_name":         record.ServiceName,
		"machine_id":           record.MachineID,
		"status":               record.Status,
		"cpu_percent":          record.CPUPercent,
		"memory_total_bytes":   record.MemoryTotalBytes,
		"memory_used_bytes":    record.MemoryUsedBytes,
		"memory_used_percent":  record.MemoryUsedPercent,
		"disk_total_bytes":     record.DiskTotalBytes,
		"disk_used_bytes":      record.DiskUsedBytes,
		"disk_used_percent":    record.DiskUsedPercent,
		"upstream_status_code": record.UpstreamStatusCode,
		"health_json":          health,
		"payload_json":         payload,
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func (s *ClickHouseSink) post(ctx context.Context, query string, body []byte) error {
	_, err := s.do(ctx, query, body)
	return err
}

func (s *ClickHouseSink) query(ctx context.Context, query string) ([]byte, error) {
	return s.do(ctx, query, nil)
}

func (s *ClickHouseSink) do(ctx context.Context, query string, body []byte) ([]byte, error) {
	requestURL, err := url.Parse(s.url)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "text/plain")
	q := request.URL.Query()
	q.Set("database", s.db)
	q.Set("query", query)
	request.URL.RawQuery = q.Encode()
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(response.Body); err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("clickhouse returned status %d: %s", response.StatusCode, out.String())
	}
	return out.Bytes(), nil
}

func parseClickHouseRows(body []byte) ([]Record, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	records := []Record{}
	for scanner.Scan() {
		var row struct {
			ObservedAt         string   `json:"observed_at"`
			ServiceName        string   `json:"service_name"`
			MachineID          string   `json:"machine_id"`
			Status             string   `json:"status"`
			CPUPercent         *float64 `json:"cpu_percent"`
			MemoryTotalBytes   *uint64  `json:"memory_total_bytes"`
			MemoryUsedBytes    *uint64  `json:"memory_used_bytes"`
			MemoryUsedPercent  *float64 `json:"memory_used_percent"`
			DiskTotalBytes     *uint64  `json:"disk_total_bytes"`
			DiskUsedBytes      *uint64  `json:"disk_used_bytes"`
			DiskUsedPercent    *float64 `json:"disk_used_percent"`
			UpstreamStatusCode *uint16  `json:"upstream_status_code"`
			HealthJSON         string   `json:"health_json"`
			PayloadJSON        string   `json:"payload_json"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, err
		}
		observedAt, err := time.Parse("2006-01-02 15:04:05.000", row.ObservedAt)
		if err != nil {
			return nil, err
		}
		record := Record{ObservedAt: observedAt.UTC(), ServiceName: row.ServiceName, MachineID: row.MachineID, Status: row.Status, CPUPercent: row.CPUPercent, MemoryTotalBytes: row.MemoryTotalBytes, MemoryUsedBytes: row.MemoryUsedBytes, MemoryUsedPercent: row.MemoryUsedPercent, DiskTotalBytes: row.DiskTotalBytes, DiskUsedBytes: row.DiskUsedBytes, DiskUsedPercent: row.DiskUsedPercent, UpstreamStatusCode: row.UpstreamStatusCode}
		parseRecordJSON(row.HealthJSON, row.PayloadJSON, &record)
		records = append(records, record)
	}
	return records, scanner.Err()
}

func quoteClickHouseString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "\\'") + "'"
}
