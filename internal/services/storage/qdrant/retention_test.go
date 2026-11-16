package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"recall-miner/internal/retention"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRetentionRejectsBackendIdentityAndShapeViolations(t *testing.T) {
	fake := newNSFake()
	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		if req["with_vector"] == true && strings.HasSuffix(r.URL.Path, "/points") {
			if mode.Load() == 5 {
				w.Write([]byte(strings.Repeat("x", retention.MaxRecordBytes+1)))
				return
			}
			p := map[string]any{"id": pointIDForTenant("db_a", "42"), "vector": []float32{1, 0}, "payload": map[string]any{"tenant_id": "db_a", "embed_id": "42", "metadata": map[string]any{"text": "private"}}}
			switch mode.Load() {
			case 1:
				p["payload"].(map[string]any)["tenant_id"] = "db_b"
			case 2:
				p["payload"].(map[string]any)["embed_id"] = "43"
			case 3:
				p["id"] = "foreign"
			case 4:
				p["vector"] = []float32{}
			case 6:
				p["vector"] = []float32{1}
			}
			points := []any{p}
			if mode.Load() == 7 {
				points = append(points, p)
			}
			if mode.Load() == 8 {
				points = nil
			}
			json.NewEncoder(w).Encode(map[string]any{"result": points})
			return
		}
		fake.handler(w, r)
	}))
	defer srv.Close()
	c := newQClient(t, srv)
	ctx := context.Background()
	if e := c.CreateCollection(ctx, "db_a", 2); e != nil {
		t.Fatal(e)
	}
	if rec, e := c.RetentionRecord(ctx, "db_a", "42"); e != nil || len(rec.Vector) != 2 {
		t.Fatalf("valid record: %v", e)
	}
	for i := int32(1); i <= 8; i++ {
		mode.Store(i)
		if _, e := c.RetentionRecord(ctx, "db_a", "42"); e == nil {
			t.Errorf("accepted malformed backend mode %d", i)
		}
	}
}
