package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"recall-miner/internal/retention"
)

// RetentionRecord reads only the selected generation and checks both tenant and
// logical record identity. No legacy ID fallback may cross a tenant boundary.
func (c *Client) RetentionRecord(ctx context.Context, db, id string) (retention.Record, error) {
	var zero retention.Record
	entry, release, err := c.resolveForRead(ctx, db)
	if err != nil {
		return zero, err
	}
	defer release()
	live := entry.liveTenant()
	physical := pointIDForTenant(live, id)
	body, _ := json.Marshal(map[string]any{"ids": []string{physical}, "with_vector": true, "with_payload": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/collections/"+url.PathEscape(entry.PhysicalCollection)+"/points", bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("retention backend HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, retention.MaxRecordBytes+1))
	if err != nil || len(b) > retention.MaxRecordBytes {
		return zero, retention.ErrInvalid
	}
	var out struct {
		Result []struct {
			ID      string         `json:"id"`
			Vector  []float32      `json:"vector"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &out) != nil {
		return zero, retention.ErrInvalid
	}
	if len(out.Result) == 0 {
		return zero, retention.ErrNotFound
	}
	if len(out.Result) != 1 {
		return zero, retention.ErrInvalid
	}
	p := out.Result[0]
	if len(p.Vector) != entry.Dimension || len(p.Vector) == 0 || len(p.Vector) > retention.MaxDimensions {
		return zero, retention.ErrInvalid
	}
	if p.ID != physical || p.Payload[tenantPayloadKey] != live || p.Payload[embedPayloadKey] != id {
		return zero, retention.ErrNotFound
	}
	meta, ok := p.Payload["metadata"].(map[string]any)
	if !ok && p.Payload["metadata"] != nil {
		return zero, retention.ErrInvalid
	}
	return retention.Record{Vector: p.Vector, Metadata: meta}, nil
}
