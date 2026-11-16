package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDBCreateRequiresDimension(t *testing.T) {
	h := &Handlers{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/db/create", strings.NewReader(`{"name":"docs"}`))

	h.DBCreate(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestDBQueryRequiresTextOrVector(t *testing.T) {
	h := &Handlers{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/db/db-1/query", strings.NewReader(`{"top_k":1}`))
	request.SetPathValue("db_id", "db-1")

	h.DBQuery(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestDBQueryRejectsUnsupportedScopeFields(t *testing.T) {
	for _, payload := range []string{
		`{"text":"q","top_k":5,"filter":{"tenant":"other"}}`,
		`{"text":"q","top_k":5,"namespace":"other"}`,
	} {
		h := &Handlers{}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/db/db-1/query", strings.NewReader(payload))
		request.SetPathValue("db_id", "db-1")

		h.DBQuery(recorder, request)

		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "not supported") {
			t.Fatalf("payload %s: status %d body %s", payload, recorder.Code, recorder.Body.String())
		}
	}
}

func TestDBQueryRejectsTopKOutOfRange(t *testing.T) {
	for _, payload := range []string{
		`{"text":"q","top_k":-1}`,
		`{"text":"q","top_k":101}`,
		`{"text":"q","top_k":1000000}`,
		`{"text":"q","top_k":2.5}`,
	} {
		h := &Handlers{}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/db/db-1/query", strings.NewReader(payload))
		request.SetPathValue("db_id", "db-1")

		h.DBQuery(recorder, request)

		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("payload %s: status %d body %s", payload, recorder.Code, recorder.Body.String())
		}
	}
}

func TestDBQueryAcceptsTopKBoundsAndIgnoresOtherUnknownFields(t *testing.T) {
	// Valid top_k values pass validation and reach the "text or vector"
	// check, which proves they were not rejected as out of range.
	for _, payload := range []string{
		`{"top_k":1}`,
		`{"top_k":100}`,
		`{"top_k":0}`,
		`{"top_k":5,"extra":"ignored"}`,
	} {
		h := &Handlers{}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/db/db-1/query", strings.NewReader(payload))
		request.SetPathValue("db_id", "db-1")

		h.DBQuery(recorder, request)

		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "text or vector is required") {
			t.Fatalf("payload %s: status %d body %s", payload, recorder.Code, recorder.Body.String())
		}
	}
}

func TestDBQueryRejectsOversizedBody(t *testing.T) {
	h := &Handlers{}
	recorder := httptest.NewRecorder()
	payload := `{"text":"` + strings.Repeat("a", maxQueryBodyBytes) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/db/db-1/query", strings.NewReader(payload))
	request.SetPathValue("db_id", "db-1")

	h.DBQuery(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, recorder.Code)
	}
}
