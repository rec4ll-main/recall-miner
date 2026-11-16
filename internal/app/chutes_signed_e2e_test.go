package app

// Chutes generation, signed end-to-end: validator-signed Epistula request ->
// real miner router (auth, replay, limits) -> exact model-proxy registration ->
// Generate -> loopback sidecar replaying receipts emitted by the actual Python
// handler (test_t4_ordinary.SignedE2EReceipts) -> response signer -> verifier.
// Known development keys (//Alice miner, //Bob validator); synthetic data only.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"
	"github.com/subtrahend-labs/gobt/boilerplate"

	"recall-miner/internal/auth"
	"recall-miner/internal/config"
)

const e2eModel = "Qwen/Qwen3.8-27B-TEE"

type e2eReceipt struct {
	Case   string          `json:"case"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type e2eSidecar struct {
	mu       sync.Mutex
	current  e2eReceipt
	payloads []map[string]any
}

func (s *e2eSidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p map[string]any
	_ = json.NewDecoder(r.Body).Decode(&p)
	s.payloads = append(s.payloads, p)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.current.Status)
	_, _ = w.Write(s.current.Body)
}

func (s *e2eSidecar) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

func mustKey(t *testing.T, uri string) signature.KeyringPair {
	t.Helper()
	kp, err := signature.KeyringPairFromSecret(uri, 42)
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

// signedGenerate builds a request exactly as the API signer does: base Epistula
// headers plus the envelope signature over the canonical request message.
func signedGenerate(t *testing.T, signer signature.KeyringPair, minerAddress string, body []byte) *http.Request {
	t.Helper()
	headers, err := boilerplate.GetEpistulaHeaders(signer, minerAddress, body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/generate", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Del("Connection")
	req.Header.Set("Content-Type", "application/json")
	msg := auth.EnvelopeMessage(http.MethodPost, "/generate", "", "application/json", auth.EnvelopeBodyHash(body),
		req.Header.Get("Epistula-Uuid"), req.Header.Get("Epistula-Timestamp"), req.Header.Get("Epistula-Signed-For"),
		req.Header.Get("Epistula-Signed-By"))
	sig, err := signature.Sign(auth.EnvelopeDigest(msg), signer.URI)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Epistula-Envelope-Signature", "0x"+hex.EncodeToString(sig))
	return req
}

func verifySignedResponse(t *testing.T, miner signature.KeyringPair, req *http.Request, reqBody []byte, rec *httptest.ResponseRecorder) {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(rec.Header().Get(auth.ResponseSignatureHeader), "0x"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("response unsigned (status %d)", rec.Code)
	}
	msg := auth.ResponseMessage(http.MethodPost, "/generate", "", strconv.Itoa(rec.Code), auth.EnvelopeBodyHash(reqBody),
		req.Header.Get("Epistula-Uuid"), auth.EnvelopeBodyHash(rec.Body.Bytes()), rec.Header().Get(auth.ResponseTimestampHeader), miner.Address)
	digest := sha256.Sum256([]byte(msg))
	if !boilerplate.VerfySignature(miner.Address, digest[:], raw) {
		t.Fatal("miner response signature rejected")
	}
	tampered := sha256.Sum256([]byte(strings.Replace(msg, strconv.Itoa(rec.Code), "200", 1) + "x"))
	if boilerplate.VerfySignature(miner.Address, tampered[:], raw) {
		t.Fatal("tampered response accepted")
	}
}

func TestChutesSignedEndToEnd(t *testing.T) {
	data, err := os.ReadFile("../../testdata/chutes-t4-receipts.json")
	if err != nil {
		t.Fatal(err)
	}
	var receipts []e2eReceipt
	if err := json.Unmarshal(data, &receipts); err != nil || len(receipts) != 7 {
		t.Fatalf("receipts: %v (%d)", err, len(receipts))
	}
	miner, validator, stranger := mustKey(t, "//Alice"), mustKey(t, "//Bob"), mustKey(t, "//Charlie")

	sidecar := &e2eSidecar{}
	sidecarServer := httptest.NewServer(sidecar)
	defer sidecarServer.Close()

	t.Setenv("CHUTES_MODEL", "")
	t.Setenv("CHUTES_BACKEND_NAME", "")
	cfg := config.FromEnv()
	cfg.GenerationEnabled = true
	cfg.GenerationBackendTimeout = 30 * time.Second
	cfg.GenerationSidecarMargin = 2 * time.Second
	cfg.InternalAPIKey = "synthetic-internal-key-not-a-secret-0123456789"
	cfg.MinerHotkey = miner.Address
	cfg.ApplicationProberHotkey = validator.Address
	cfg.EpistulaEnvelopeRequired = true
	cfg.ResponseSigningEnabled = true
	cfg.MetricsStorage = "none"
	s := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.keypair = &miner
	router := s.routes()

	// Exact registration as cmd/model-proxy posts it (see RESULT.md).
	reg, _ := json.Marshal(map[string]any{"name": "chutes", "url": sidecarServer.URL, "capabilities": []string{"generate"},
		"models": []string{e2eModel}, "metadata": map[string]any{"generation_connector": "chutes"}, "machine_id": "synthetic"})
	regReq := httptest.NewRequest(http.MethodPost, "/internal/services/register", bytes.NewReader(reg))
	regReq.Header.Set("X-Internal-Api-Key", cfg.InternalAPIKey)
	regReq.Header.Set("Content-Type", "application/json")
	regRec := httptest.NewRecorder()
	router.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusOK {
		t.Fatalf("registration %d", regRec.Code)
	}
	if caps := s.handlers.AvailableCapabilities(); strings.Join(caps, ",") != "generate" {
		t.Fatalf("advertised %v", caps)
	}

	inputs := map[string][]byte{
		"grounded_fact": []byte(`{"query":"At what temperature does water boil at sea level?","contexts":[{"id":"public-fact-1","text":"Water boils at 100 degrees Celsius at sea level."}]}`),
	}
	abstain := []byte(`{"query":"What is the synthetic project codename?","contexts":[]}`)
	for _, receipt := range receipts {
		t.Run(receipt.Case, func(t *testing.T) {
			body, ok := inputs[receipt.Case]
			if !ok {
				body = abstain
			}
			sidecar.mu.Lock()
			sidecar.current = receipt
			sidecar.mu.Unlock()
			before := sidecar.calls()
			req := signedGenerate(t, validator, miner.Address, body)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			// Existing failure mapping, unchanged: 200 and 503 pass through; other
			// sidecar failures (409 hold, 500, 504) are signed 502s.
			want := http.StatusBadGateway
			if receipt.Status == 200 || receipt.Status == 503 {
				want = receipt.Status
			}
			if rec.Code != want || sidecar.calls() != before+1 {
				t.Fatalf("status %d want %d, sidecar calls %d", rec.Code, want, sidecar.calls()-before)
			}
			verifySignedResponse(t, miner, req, body, rec)

			sidecar.mu.Lock()
			sent := sidecar.payloads[len(sidecar.payloads)-1]
			sidecar.mu.Unlock()
			ms, _ := sent["deadline_ms"].(float64)
			if sent["model"] != e2eModel || ms <= 27000 || ms > 28000 {
				t.Fatalf("sidecar payload model %v deadline_ms %v", sent["model"], sent["deadline_ms"])
			}
			if receipt.Status == 200 {
				var got, exp map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				_ = json.Unmarshal(receipt.Body, &exp)
				gj, _ := json.Marshal(got)
				ej, _ := json.Marshal(exp)
				if !bytes.Equal(gj, ej) || got["model"] != e2eModel {
					t.Fatalf("answer altered in transit:\n%s\n%s", gj, ej)
				}
			}
		})
	}

	// These requests come from a public address (httptest's 192.0.2.1), so a refusal is
	// the plain 404 the quiet gate gives anything not verifiably from the Recall API.
	refusals := []struct {
		name string
		req  func(t *testing.T) *http.Request
		code int
	}{
		{"unsigned", func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/generate", bytes.NewReader(abstain))
			r.Header.Set("Content-Type", "application/json")
			return r
		}, http.StatusNotFound},
		{"unauthorized validator", func(t *testing.T) *http.Request { return signedGenerate(t, stranger, miner.Address, abstain) }, http.StatusNotFound},
		{"wrong miner audience", func(t *testing.T) *http.Request { return signedGenerate(t, validator, stranger.Address, abstain) }, http.StatusNotFound},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			before := sidecar.calls()
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, tc.req(t))
			if rec.Code != tc.code || sidecar.calls() != before {
				t.Fatalf("status %d want %d, dispatched %d", rec.Code, tc.code, sidecar.calls()-before)
			}
		})
	}

	t.Run("replay", func(t *testing.T) {
		sidecar.mu.Lock()
		sidecar.current = receipts[1]
		sidecar.mu.Unlock()
		req := signedGenerate(t, validator, miner.Address, abstain)
		replay := req.Clone(req.Context())
		replay.Body = io.NopCloser(bytes.NewReader(abstain))
		first := httptest.NewRecorder()
		router.ServeHTTP(first, req)
		before := sidecar.calls()
		second := httptest.NewRecorder()
		router.ServeHTTP(second, replay)
		if first.Code != http.StatusOK || second.Code != http.StatusNotFound || sidecar.calls() != before {
			t.Fatalf("first %d replay %d dispatched %d", first.Code, second.Code, sidecar.calls()-before)
		}
	})

	t.Run("alternate model refused before dispatch, signed", func(t *testing.T) {
		body := []byte(`{"query":"synthetic","contexts":[],"model":"Qwen/Qwen3.8-27B"}`)
		before := sidecar.calls()
		req := signedGenerate(t, validator, miner.Address, body)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || sidecar.calls() != before {
			t.Fatalf("status %d dispatched %d", rec.Code, sidecar.calls()-before)
		}
		verifySignedResponse(t, miner, req, body, rec)
	})
}
