package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyNumericLookupUsesUint64WithoutPrecisionLoss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			IDs []json.RawMessage `json:"ids"`
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
		}
		expected := []string{`"550e8400-e29b-41d4-a716-446655440000"`, `42`, `18446744073709551615`, `0`}
		if len(req.IDs) != len(expected) {
			t.Errorf("ids length %d", len(req.IDs))
			return
		}
		for i, b := range req.IDs {
			if string(b) != expected[i] {
				t.Errorf("wire ID %d: %s", i, b)
			}
		}
		w.Write([]byte(`{"result":[]}`))
	}))
	defer srv.Close()
	c := newQClient(t, srv)
	if _, e := c.getPoints(context.Background(), "physical", []string{"550e8400-e29b-41d4-a716-446655440000", "42", "18446744073709551615", "0"}); e != nil {
		t.Fatal(e)
	}
}

// Legacy points predate tenant-derived UUIDs. A failed cleanup must be visible
// to the caller, and retry must converge on one replacement without ID rounding.
func TestLegacyNumericMigrationCleanupRetry(t *testing.T) {
	for _, id := range []string{"42", "18446744073709551615"} {
		t.Run(id, func(t *testing.T) {
			fake := newNSFake()
			legacy, replacement, reject := true, false, true
			deletes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/collections/recall_vec_dim_2/points") {
					switch {
					case r.Method == http.MethodPut:
						replacement = true
						writeQJSON(w, map[string]any{"result": map[string]any{"status": "ok"}})
						return
					case strings.HasSuffix(r.URL.Path, "/points/delete"):
						raw, _ := io.ReadAll(r.Body)
						if !bytes.Contains(raw, []byte(`"has_id"`)) {
							r.Body = io.NopCloser(bytes.NewReader(raw))
							fake.handler(w, r)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(raw))
						deletes++
						var body struct {
							Filter struct {
								Must []struct {
									Key   string `json:"key"`
									Match struct {
										Value string `json:"value"`
									} `json:"match"`
									HasID []json.RawMessage `json:"has_id"`
								} `json:"must"`
							} `json:"filter"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if len(body.Filter.Must) != 2 || body.Filter.Must[0].Key != tenantPayloadKey || body.Filter.Must[0].Match.Value != "db_a" || len(body.Filter.Must[1].HasID) != 1 || string(body.Filter.Must[1].HasID[0]) != id {
							t.Error("cleanup must use exact numeric ID and tenant filter")
							http.Error(w, "invalid filter", 400)
							return
						}
						if reject {
							http.Error(w, "transient cleanup failure", 503)
							return
						}
						legacy = false
						writeQJSON(w, map[string]any{"result": map[string]any{"status": "ok"}})
						return
					case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points"):
						rows := []map[string]any{}
						if legacy {
							rows = append(rows, map[string]any{"id": json.Number(id), "payload": map[string]any{tenantPayloadKey: "db_a"}})
						}
						if replacement {
							rows = append(rows, map[string]any{"id": pointIDForTenant("db_a", id), "payload": map[string]any{tenantPayloadKey: "db_a", embedPayloadKey: id}})
						}
						writeQJSON(w, map[string]any{"result": rows})
						return
					}
				}
				// Keep the namespace fake in charge of registry and generation behavior.
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				fake.handler(w, r)
			}))
			defer srv.Close()
			c := newQClient(t, srv)
			ctx := context.Background()
			if err := c.CreateCollection(ctx, "db_a", 2); err != nil {
				t.Fatal(err)
			}
			if err := c.RestoreVector(ctx, "db_a", id, []float64{1, 0}, map[string]any{"version": "new"}); err == nil {
				t.Fatal("cleanup failure incorrectly reported as successful restore")
			}
			if !legacy || !replacement {
				t.Fatal("expected retryable legacy and replacement state")
			}
			reject = false
			if err := c.RestoreVector(ctx, "db_a", id, []float64{1, 0}, nil); err != nil {
				t.Fatal(err)
			}
			if legacy || !replacement || deletes != 2 {
				t.Fatalf("migration did not converge: legacy=%v replacement=%v deletes=%d", legacy, replacement, deletes)
			}
			if err := c.RestoreVector(ctx, "db_a", id, []float64{1, 0}, nil); err != nil {
				t.Fatal(err)
			}
			if deletes != 2 {
				t.Fatal("idempotent retry repeated legacy deletion")
			}
		})
	}
}
