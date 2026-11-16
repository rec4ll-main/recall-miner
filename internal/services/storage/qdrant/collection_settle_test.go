package qdrant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A collection that keeps answering 5xx is an error after a bounded wait,
// not a hang.
func TestExistingVectorCollectionPersistent5xxIsBounded(t *testing.T) {
	old := collectionSettleTimeout
	collectionSettleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { collectionSettleTimeout = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"status":{"error":"Service internal error"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	start := time.Now()
	_, err := newQClient(t, srv).existingVectorCollection(context.Background(), "recall_vec_dim_8", 8, false)
	if err == nil || !isHTTPStatus(err, http.StatusInternalServerError) {
		t.Fatalf("want the 500 after the bounded wait, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("bounded wait took %v", d)
	}
}
