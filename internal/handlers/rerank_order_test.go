package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"recall-miner/internal/client"
	"recall-miner/internal/config"
	"recall-miner/internal/registry"
)

// The validator probe shape (texts, no candidate_ids) through a rerank
// sidecar that returns scores in index order: ranked_ids must be the
// descending-score permutation the validator expects.
func TestRerankTextsRankedIDsDescendingWithoutCandidateIDs(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"index": 0, "score": 0.10}, {"index": 1, "score": 0.95},
			{"index": 2, "score": 0.40}, {"index": 3, "score": 0.95},
		})
	}))
	defer sidecar.Close()
	reg := registry.New()
	reg.Register(registry.RegisterRequest{Name: "rr", URL: sidecar.URL, Capabilities: []string{"rerank"}})
	reg.Touch("rr", "", map[string]any{"status": "ok"})
	h := &Handlers{cfg: config.Config{}, registry: reg, apiClient: client.New(sidecar.Client(), "k")}

	res, err := h.rerank(context.Background(), rerankRequest{Query: "q", Texts: []string{"a", "b", "c", "d"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res["ranked_ids"], []int{1, 3, 2, 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked_ids = %v, want %v (scores %v)", got, want, res["scores"])
	}
}
