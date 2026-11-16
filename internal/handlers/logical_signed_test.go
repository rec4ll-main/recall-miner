package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"net/http"
	"net/http/httptest"
	"recall-miner/internal/auth"
	"recall-miner/internal/registry"
	"testing"
	"time"
)

func TestLogicalSignedRequestReachesSidecarScoped(t *testing.T) {
	kp, e := signature.KeyringPairFromSecret("//Alice", 42)
	if e != nil {
		t.Fatal(e)
	}
	h := testHandler(t, true)
	h.cfg.GenerationBackendName = "chutes"
	h.cfg.GenerationModel = "synthetic-model"
	keys := []string{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		keys = append(keys, p["request_key"].(string))
		json.NewEncoder(w).Encode(map[string]any{"model": "synthetic-model", "answer": "synthetic"})
	}))
	defer backend.Close()
	h.registry.Register(registry.RegisterRequest{Name: "chutes", URL: backend.URL, Capabilities: []string{"generate"}, Models: []string{"synthetic-model"}, Metadata: map[string]any{"generation_connector": "chutes"}})
	handler := auth.Validator(kp.Address, kp.Address, true, 1<<20, nil, nil, http.HandlerFunc(h.Generate))
	body := []byte(`{"query":"public synthetic","request_key":"o1:fixed","model":"synthetic-model"}`)
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/generate", bytes.NewReader(body))
		timestamp := fmt.Sprint(time.Now().UnixMilli())
		id := fmt.Sprintf("envelope-%d", i)
		sum := sha256.Sum256(body)
		bh := hex.EncodeToString(sum[:])
		message := fmt.Sprintf("%s.%s.%s.%s", bh, id, timestamp, kp.Address)
		sig, e := signature.Sign([]byte(message), kp.URI)
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Epistula-Version", "2")
		r.Header.Set("Epistula-Timestamp", timestamp)
		r.Header.Set("Epistula-Uuid", id)
		r.Header.Set("Epistula-Signed-By", kp.Address)
		r.Header.Set("Epistula-Signed-For", kp.Address)
		r.Header.Set("Epistula-Request-Signature", "0x"+hex.EncodeToString(sig))
		r.Header.Set("X-Verified-Hotkey", "forged")
		env := auth.EnvelopeMessage("POST", "/generate", "", "application/json", bh, id, timestamp, kp.Address, kp.Address)
		digest := sha256.Sum256([]byte(env))
		es, e := signature.Sign(digest[:], kp.URI)
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Epistula-Envelope-Signature", "0x"+hex.EncodeToString(es))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status %d %s", w.Code, w.Body.String())
		}
	}
	want, _ := scopeGenerationRequestKey(kp.Address, "o1:fixed")
	if len(keys) != 2 || keys[0] != want || keys[1] != want {
		t.Fatal("envelope UUID altered logical scope", keys)
	}
	// Fake sidecar only observes forwarding. Real duplicate rejection is proven by the Python ledger suite.
}
