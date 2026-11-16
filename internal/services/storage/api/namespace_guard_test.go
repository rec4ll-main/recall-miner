package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"recall-miner/internal/services/storage/config"
)

func TestDatabaseHandlerRejectsReservedNames(t *testing.T) {
	for _, id := range []string{"other--db_1", "__storage_owner", "other--__storage_owner"} {
		t.Run(id, func(t *testing.T) {
			called := false
			handler := databaseHandler(func(w http.ResponseWriter, r *http.Request) { called = true })
			req := httptest.NewRequest(http.MethodDelete, "/db/"+id, nil)
			req.SetPathValue("db_id", id)
			rec := httptest.NewRecorder()
			handler(rec, req)
			if called || rec.Code != http.StatusBadRequest {
				t.Fatalf("called=%v status=%d", called, rec.Code)
			}
		})
	}
	called := false
	handler := databaseHandler(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/db/db_1", nil)
	req.SetPathValue("db_id", "db_1")
	handler(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("valid database rejected")
	}
}

func TestReservedNamesBlockedOnEveryDatabaseRoute(t *testing.T) {
	server := NewServerWithClient(config.Config{InternalAPIKey: "secret"}, nil, nil)
	routes := []struct{ method, path string }{
		{"POST", "/db/other--db_1/retention-proof"},
		{"DELETE", "/db/other--db_1"},
		{"GET", "/db/other--db_1"},
		{"POST", "/db/other--db_1/restore"},
		{"POST", "/db/other--db_1/store"},
		{"DELETE", "/db/other--db_1/embed/id"},
		{"POST", "/db/other--db_1/query"},
		{"DELETE", "/db/__storage_owner"},
		{"DELETE", "/db/other%2D%2Ddb_1"},
	}
	for _, route := range routes {
		t.Run(route.method+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			req.Header.Set("X-Internal-Api-Key", "secret")
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got status %d", rec.Code)
			}
		})
	}
}
