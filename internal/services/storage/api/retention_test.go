package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"recall-miner/internal/retention"
	"recall-miner/internal/services/storage/config"
	"strings"
	"testing"
)

type proofStore struct {
	*memClient
	calls  int
	record retention.Record
	err    error
}

func (p *proofStore) RetentionRecord(context.Context, string, string) (retention.Record, error) {
	p.calls++
	return p.record, p.err
}
func TestRetentionStorageAuthAndFailures(t *testing.T) {
	p := &proofStore{memClient: newMemClient(), record: retention.Record{Vector: []float32{1}, Metadata: map[string]any{"text": "hello"}}}
	h := NewServerWithClient(config.Config{InternalAPIKey: "synthetic"}, slog.New(slog.NewTextHandler(io.Discard, nil)), p)
	good := `{"embed_id":"record","nonce":"` + strings.Repeat("01", 32) + `"}`
	send := func(key, body string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/db/db_test/retention-proof", strings.NewReader(body))
		r.Header.Set("X-Internal-Api-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if send("", good) != 403 || p.calls != 0 {
		t.Fatal("unauthenticated backend read")
	}
	if send("synthetic", good) != 200 {
		t.Fatal("valid request rejected")
	}
	p.err = retention.ErrNotFound
	if send("synthetic", good) != 404 {
		t.Fatal("missing record status")
	}
	p.err = nil
	p.record.Vector = nil
	if send("synthetic", good) != 502 {
		t.Fatal("empty vector accepted")
	}
	before := p.calls
	if send("synthetic", strings.Repeat("x", 2048)) != 400 || p.calls != before {
		t.Fatal("oversized request read backend")
	}
}
