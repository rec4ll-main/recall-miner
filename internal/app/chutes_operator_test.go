package app
import("encoding/json"; "net/http"; "net/http/httptest"; "bytes"; "os"; "io"; "log/slog"; "strings"; "testing"; "time"; "recall-miner/internal/config")
// Operator-only harness: never enabled by default. Synthetic public identities only.
func TestOperatorChutesTwoCases(t *testing.T) {
 sidecarURL := os.Getenv("CHUTES_SYNTHETIC_SIDECAR")
 if sidecarURL == "" { t.Skip("operator opt-in absent") }
 if sidecarURL != "http://127.0.0.1:9200" { t.Fatal("isolated loopback sidecar required") }
 miner, validator := mustKey(t,"//Alice"), mustKey(t,"//Bob")
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
	reg, _ := json.Marshal(map[string]any{"name": "chutes", "url": sidecarURL, "capabilities": []string{"generate"},
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

 cases := []struct{name, body string}{
 {"grounded_fact",`{"query":"What is the capital of France?","contexts":[{"id":"public_fact","text":"Paris is the capital of France."}],"temperature":0}`},
 
 }
 for _,c := range cases {
  body:=[]byte(c.body); req:=signedGenerate(t,validator,miner.Address,body); rec:=httptest.NewRecorder()
  router.ServeHTTP(rec,req)
  // Emit synthetic result before assertions; never retry on failure/uncertainty.
  t.Logf("case=%s router_status=%d",c.name,rec.Code)
  verifySignedResponse(t,miner,req,body,rec)
  t.Logf("case=%s signature_verified=true request_uuid=%s",c.name,req.Header.Get("Epistula-Uuid"))
  if rec.Code!=200 {t.Fatal("non-success; stop without retry")}
  var result struct { Model string `json:"model"`; Answer string `json:"answer"`; Abstained bool `json:"abstained"`; Citations []struct{ID string `json:"context_id"`} `json:"citations"` }
  if json.Unmarshal(rec.Body.Bytes(),&result)!=nil || result.Model!=e2eModel {t.Fatal("invalid result/model")}
  if c.name=="grounded_fact" {
   if result.Abstained || !strings.Contains(strings.ToLower(result.Answer),"paris") || len(result.Citations)!=1 || result.Citations[0].ID!="public_fact" {t.Fatal("grounded answer assertion failed")}
  } else if !result.Abstained || len(result.Citations)!=0 || result.Answer!="I cannot answer from the provided context." {t.Fatal("abstention assertion failed")}
  t.Logf("case=%s semantic_assertions=true",c.name)
 }
}
