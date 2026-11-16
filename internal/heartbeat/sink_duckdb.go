package heartbeat

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/marcboeker/go-duckdb"
)

type DuckDBSink struct {
	db *sql.DB
}

func NewDuckDBSink(path string) (*DuckDBSink, error) {
	if path == "" {
		path = "/var/lib/recall-miner/heartbeats.duckdb"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	sink := &DuckDBSink{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sink.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return sink, nil
}

func (s *DuckDBSink) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS service_heartbeats (
    observed_at TIMESTAMP,
    service_name TEXT,
    machine_id TEXT,
    status TEXT,
    cpu_percent DOUBLE,
    memory_total_bytes UBIGINT,
    memory_used_bytes UBIGINT,
    memory_used_percent DOUBLE,
    disk_total_bytes UBIGINT,
    disk_used_bytes UBIGINT,
    disk_used_percent DOUBLE,
    upstream_status_code USMALLINT,
    health_json TEXT,
    payload_json TEXT
)`)
	return err
}

func (s *DuckDBSink) Write(ctx context.Context, record Record) error {
	health, err := healthJSON(record)
	if err != nil {
		return err
	}
	payload, err := payloadJSON(record)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO service_heartbeats (
    observed_at, service_name, machine_id, status,
    cpu_percent, memory_total_bytes, memory_used_bytes, memory_used_percent,
    disk_total_bytes, disk_used_bytes, disk_used_percent,
    upstream_status_code, health_json, payload_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ObservedAt.UTC(), record.ServiceName, record.MachineID, record.Status,
		record.CPUPercent, record.MemoryTotalBytes, record.MemoryUsedBytes, record.MemoryUsedPercent,
		record.DiskTotalBytes, record.DiskUsedBytes, record.DiskUsedPercent,
		record.UpstreamStatusCode, health, payload)
	return err
}

func (s *DuckDBSink) List(ctx context.Context, serviceName string, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `
SELECT observed_at, service_name, machine_id, status,
       cpu_percent, memory_total_bytes, memory_used_bytes, memory_used_percent,
       disk_total_bytes, disk_used_bytes, disk_used_percent,
       upstream_status_code, health_json, payload_json
FROM service_heartbeats`
	args := []any{}
	if serviceName != "" {
		query += " WHERE service_name = ?"
		args = append(args, serviceName)
	}
	query += " ORDER BY observed_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []Record{}
	for rows.Next() {
		record, err := scanSQLRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *DuckDBSink) Close() error {
	return s.db.Close()
}

type sqlScanner interface {
	Scan(dest ...any) error
}

func scanSQLRecord(row sqlScanner) (Record, error) {
	var record Record
	var cpu, memoryUsedPercent, diskUsedPercent sql.NullFloat64
	var memoryTotal, memoryUsed, diskTotal, diskUsed sql.NullInt64
	var upstreamStatus sql.NullInt64
	var healthRaw, payloadRaw string
	if err := row.Scan(
		&record.ObservedAt, &record.ServiceName, &record.MachineID, &record.Status,
		&cpu, &memoryTotal, &memoryUsed, &memoryUsedPercent,
		&diskTotal, &diskUsed, &diskUsedPercent,
		&upstreamStatus, &healthRaw, &payloadRaw,
	); err != nil {
		return Record{}, err
	}
	if cpu.Valid {
		record.CPUPercent = &cpu.Float64
	}
	if memoryTotal.Valid && memoryTotal.Int64 >= 0 {
		v := uint64(memoryTotal.Int64)
		record.MemoryTotalBytes = &v
	}
	if memoryUsed.Valid && memoryUsed.Int64 >= 0 {
		v := uint64(memoryUsed.Int64)
		record.MemoryUsedBytes = &v
	}
	if memoryUsedPercent.Valid {
		record.MemoryUsedPercent = &memoryUsedPercent.Float64
	}
	if diskTotal.Valid && diskTotal.Int64 >= 0 {
		v := uint64(diskTotal.Int64)
		record.DiskTotalBytes = &v
	}
	if diskUsed.Valid && diskUsed.Int64 >= 0 {
		v := uint64(diskUsed.Int64)
		record.DiskUsedBytes = &v
	}
	if diskUsedPercent.Valid {
		record.DiskUsedPercent = &diskUsedPercent.Float64
	}
	if upstreamStatus.Valid && upstreamStatus.Int64 >= 0 && upstreamStatus.Int64 <= 65535 {
		v := uint16(upstreamStatus.Int64)
		record.UpstreamStatusCode = &v
	}
	parseRecordJSON(healthRaw, payloadRaw, &record)
	if record.ObservedAt.IsZero() {
		return Record{}, fmt.Errorf("heartbeat record missing observed_at")
	}
	return record, nil
}
