package milvus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParseInt64EmbedID(t *testing.T) {
	t.Parallel()

	valid := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1", 1},
		{"42", 42},
		{"9223372036854775807", 9223372036854775807},
		{"-1", -1},
		{"-9223372036854775808", -9223372036854775808},
	}
	for _, tc := range valid {
		got, err := parseInt64EmbedID(tc.in)
		if err != nil {
			t.Fatalf("parseInt64EmbedID(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("parseInt64EmbedID(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"",
		" ",
		"1 ",
		" 1",
		"1.0",
		"1e10",
		"0x10",
		"abc",
		"1||id>0",
		"1 && id > 0",
		`1" || tenant_id == "other`,
		"1; drop",
		"9223372036854775808",  // overflow
		"-9223372036854775809", // underflow
		"+",
		"-",
		"01a",
	}
	for _, in := range invalid {
		if _, err := parseInt64EmbedID(in); err == nil {
			t.Fatalf("parseInt64EmbedID(%q) expected error", in)
		}
	}
}

type captureServer struct {
	mu       sync.Mutex
	deletes  []map[string]any
	requests []string
}

func (s *captureServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path+" "+string(body))
	s.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/v2/vectordb/collections/list"):
		writeJSON(w, map[string]any{"code": 0, "data": []string{registryCollection, "recall_vec_dim_8"}})
	case strings.HasSuffix(r.URL.Path, "/v2/vectordb/collections/create"):
		writeJSON(w, map[string]any{"code": 0})
	case strings.HasSuffix(r.URL.Path, "/v2/vectordb/collections/load"):
		writeJSON(w, map[string]any{"code": 0})
	case strings.HasSuffix(r.URL.Path, "/v2/vectordb/entities/query"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		filter, _ := payload["filter"].(string)
		if strings.Contains(filter, "id == ") && strings.Contains(filter, registryCollection) == false {
			// not used
		}
		// Registry lookup response for any collection name.
		writeJSON(w, map[string]any{
			"code": 0,
			"data": []map[string]any{{
				"collection_name":     "tenant-a",
				"physical_collection": "recall_vec_dim_8",
				"dimension":           8,
			}},
		})
	case strings.HasSuffix(r.URL.Path, "/v2/vectordb/entities/delete"):
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		s.mu.Lock()
		s.deletes = append(s.deletes, payload)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"code": 0})
	default:
		writeJSON(w, map[string]any{"code": 0})
	}
}

func writeJSON(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return &Client{
		baseURL: srv.URL,
		http:    srv.Client(),
	}
}

func TestDeleteVectorRejectsMalformedBeforeRequest(t *testing.T) {
	t.Parallel()
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv)

	malformed := []string{
		"",
		"not-an-id",
		"1||id>0",
		`1 || tenant_id == "other"`,
		"9223372036854775808",
	}
	for _, id := range malformed {
		cap.mu.Lock()
		beforeDeletes := len(cap.deletes)
		beforeReqs := len(cap.requests)
		cap.mu.Unlock()

		err := client.DeleteVector(context.Background(), "tenant-a", id)
		if err == nil {
			t.Fatalf("DeleteVector(%q) expected error", id)
		}
		if !strings.Contains(err.Error(), "embed_id must be a valid int64") {
			t.Fatalf("DeleteVector(%q) error = %v", id, err)
		}

		cap.mu.Lock()
		if len(cap.deletes) != beforeDeletes {
			t.Fatalf("DeleteVector(%q) issued delete request: %#v", id, cap.deletes)
		}
		if len(cap.requests) != beforeReqs {
			t.Fatalf("DeleteVector(%q) contacted Milvus before rejecting: %v", id, cap.requests[beforeReqs:])
		}
		for _, req := range cap.requests {
			if strings.Contains(req, id) && id != "" {
				t.Fatalf("raw embed_id %q appeared in Milvus request: %s", id, req)
			}
		}
		cap.mu.Unlock()
	}
}

func TestDeleteVectorFormatsCanonicalNumericFilter(t *testing.T) {
	t.Parallel()
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv)

	if err := client.DeleteVector(context.Background(), "tenant-a", "42"); err != nil {
		t.Fatalf("DeleteVector: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.deletes) != 1 {
		t.Fatalf("expected 1 delete, got %#v", cap.deletes)
	}
	filter, _ := cap.deletes[0]["filter"].(string)
	want := `tenant_id == "tenant-a" && id == 42`
	if filter != want {
		t.Fatalf("filter = %q, want %q", filter, want)
	}
	rawBody, _ := json.Marshal(cap.deletes[0])
	if strings.Contains(string(rawBody), "id == 042") || strings.Contains(string(rawBody), `id == "42"`) {
		t.Fatalf("filter used non-canonical id form: %s", rawBody)
	}
}

func TestDeleteVectorKeepsTenantScopedFilter(t *testing.T) {
	t.Parallel()
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv)

	// Two tenants deleting the same numeric id must each keep their own tenant predicate.
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := client.DeleteVector(context.Background(), tenant, "7"); err != nil {
			t.Fatalf("DeleteVector(%s): %v", tenant, err)
		}
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.deletes) != 2 {
		t.Fatalf("expected 2 deletes, got %#v", cap.deletes)
	}
	for i, tenant := range []string{"tenant-a", "tenant-b"} {
		filter, _ := cap.deletes[i]["filter"].(string)
		want := `tenant_id == "` + tenant + `" && id == 7`
		if filter != want {
			t.Fatalf("delete[%d] filter = %q, want %q", i, filter, want)
		}
		// Ensure the other tenant name is not present without its own scoped call.
		other := "tenant-b"
		if tenant == "tenant-b" {
			other = "tenant-a"
		}
		if strings.Contains(filter, other) {
			t.Fatalf("filter for %s unexpectedly mentions %s: %q", tenant, other, filter)
		}
	}
}
