package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"recall-miner/internal/config"
)

func TestRecallAPIRegistrationAndHeartbeatUseCanonicalURLWithoutInternalKey(t *testing.T) {
	type capturedRequest struct {
		path, signature, internalKey string
	}
	requests := make(chan capturedRequest, 2)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- capturedRequest{
			path:        r.URL.Path,
			signature:   r.Header.Get("Epistula-Request-Signature"),
			internalKey: r.Header.Get("X-Internal-Api-Key"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()
	t.Setenv("RECALL_API_URL", api.URL+"/api/")
	srv := testServer(t)
	srv.cfg.RecallAPIURL = config.FromEnv().RecallAPIURL
	keypair := testKeypair(t)
	if err := srv.registerValidator(t.Context(), keypair); err != nil {
		t.Fatal(err)
	}
	if err := srv.heartbeatValidator(t.Context(), keypair); err != nil {
		t.Fatal(err)
	}
	for _, wantPath := range []string{"/api/miner/register", "/api/miner/heartbeat"} {
		request := <-requests
		if request.path != wantPath {
			t.Fatalf("path = %q, want %q", request.path, wantPath)
		}
		if request.signature == "" {
			t.Fatal("Recall API request must have an Epistula signature")
		}
		if request.internalKey != "" {
			t.Fatal("miner-local sidecar key must not be sent to the Recall API")
		}
	}
}
