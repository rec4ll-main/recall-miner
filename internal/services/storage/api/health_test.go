package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recall-miner/internal/services/storage/config"
)

// /health needs no internal key, so it must never echo the backend address or any
// credential an operator embedded in it.
func TestHealthUnauthenticatedOmitsBackendAddress(t *testing.T) {
	const secret = "s3cr3t-fixture"
	for _, cfg := range []config.Config{
		{DBEngine: "qdrant", QdrantURL: "https://user:" + secret + "@db.example.invalid:6333"},
		{DBEngine: "milvus", MilvusURI: "https://root:" + secret + "@milvus.example.invalid:19530?token=" + secret},
	} {
		cfg.InternalAPIKey = "synthetic"
		h := NewServerWithClient(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), newMemClient())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: /health status = %d, want 200", cfg.DBEngine, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, secret) || strings.Contains(body, "example.invalid") {
			t.Fatalf("%s: /health leaked the backend address: %s", cfg.DBEngine, body)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decode: %v", cfg.DBEngine, err)
		}
		if _, ok := got["backend_url"]; ok {
			t.Fatalf("%s: /health still has backend_url: %v", cfg.DBEngine, got)
		}
		if got["status"] != "ok" || got["engine"] != cfg.DBEngine {
			t.Fatalf("%s: unexpected health body: %v", cfg.DBEngine, got)
		}
	}
}
