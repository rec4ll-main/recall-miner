package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recall-miner/internal/services/storage/vector"
)

type searchRecorder struct {
	*memClient
	topKs []int
}

func (c *searchRecorder) Search(_ context.Context, _ string, _ []float64, topK int) ([]vector.SearchHit, error) {
	c.topKs = append(c.topKs, topK)
	return nil, nil
}

func doQuery(client vector.Client, payload string) *httptest.ResponseRecorder {
	s := &Server{client: client, collections: map[string]collectionRecord{}}
	req := httptest.NewRequest(http.MethodPost, "/db/db1/query", strings.NewReader(payload))
	req.SetPathValue("db_id", "db1")
	rr := httptest.NewRecorder()
	s.queryCollection(rr, req)
	return rr
}

func TestQueryPassesBoundedTopK(t *testing.T) {
	cases := map[string]int{
		`{"vector":[1,2],"top_k":1}`:   1,
		`{"vector":[1,2],"top_k":100}`: 100,
		`{"vector":[1,2]}`:             10,
	}
	for payload, want := range cases {
		client := &searchRecorder{memClient: newMemClient()}
		rr := doQuery(client, payload)
		if rr.Code != http.StatusOK {
			t.Fatalf("payload %s: status %d body %s", payload, rr.Code, rr.Body.String())
		}
		if len(client.topKs) != 1 || client.topKs[0] != want {
			t.Fatalf("payload %s: backend top_k %v, want %d", payload, client.topKs, want)
		}
	}
}

func TestQueryRejectsInvalidRequestsBeforeBackend(t *testing.T) {
	for _, payload := range []string{
		`{"vector":[1,2],"top_k":-1}`,
		`{"vector":[1,2],"top_k":101}`,
		`{"vector":[1,2],"top_k":5,"filter":{"tenant":"other"}}`,
		`{"vector":[1,2],"top_k":5,"namespace":"other"}`,
	} {
		client := &searchRecorder{memClient: newMemClient()}
		rr := doQuery(client, payload)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("payload %s: status %d body %s", payload, rr.Code, rr.Body.String())
		}
		if len(client.topKs) != 0 {
			t.Fatalf("payload %s: backend searched %v", payload, client.topKs)
		}
	}
}

func TestQueryIgnoresOtherUnknownFields(t *testing.T) {
	client := &searchRecorder{memClient: newMemClient()}
	rr := doQuery(client, `{"vector":[1,2],"top_k":3,"extra":true}`)
	if rr.Code != http.StatusOK || len(client.topKs) != 1 || client.topKs[0] != 3 {
		t.Fatalf("status %d body %s topKs %v", rr.Code, rr.Body.String(), client.topKs)
	}
}

func TestQueryRejectsOversizedBody(t *testing.T) {
	client := &searchRecorder{memClient: newMemClient()}
	payload := `{"vector":[` + strings.Repeat("0,", maxQueryBodyBytes/2) + `0]}`
	rr := doQuery(client, payload)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", rr.Code)
	}
	if len(client.topKs) != 0 {
		t.Fatalf("backend searched %v", client.topKs)
	}
}
