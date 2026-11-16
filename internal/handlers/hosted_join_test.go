package handlers

import (
 "encoding/json"
 "encoding/hex"
 "crypto/sha256"
 "bytes"
 "io"
 "log/slog"
 "net/http"
 "net/http/httptest"
 "os"
 "strconv"
 "strings"
 "testing"
 "time"
 "github.com/centrifuge/go-substrate-rpc-client/v4/signature"
 "github.com/subtrahend-labs/gobt/boilerplate"
 "recall-miner/internal/auth"
 "recall-miner/internal/client"
 "recall-miner/internal/config"
 "recall-miner/internal/registry"
)
type fakeHostedTransport func(*http.Request)(*http.Response,error)
func(f fakeHostedTransport)RoundTrip(r *http.Request)(*http.Response,error){return f(r)}
func TestActualMinerHandlerJoinsPythonReceiptsAndSigning(t *testing.T){
 data,err:=os.ReadFile("../../testdata/handler-receipts.json");if err!=nil{t.Fatal(err)}
 var receipts []struct{Status int `json:"status"`;Body json.RawMessage `json:"body"`}
 if err=json.Unmarshal(data,&receipts);err!=nil{t.Fatal(err)}
 kp,err:=signature.KeyringPairFromSecret("//Alice",42);if err!=nil{t.Fatal(err)}
 for _,receipt:=range receipts{t.Run(strconv.Itoa(receipt.Status),func(t *testing.T){
  calls:=0
  hc:=&http.Client{Transport:fakeHostedTransport(func(r *http.Request)(*http.Response,error){
   calls++;if r.URL.Path!="/generate"{t.Fatal("unexpected path")}
   return &http.Response{StatusCode:receipt.Status,Body:io.NopCloser(bytes.NewReader(receipt.Body)),Header:http.Header{}},nil
  })}
  cfg:=config.FromEnv();cfg.GenerationEnabled=true;cfg.GenerationBackendTimeout=time.Second;cfg.GenerationModel="fixture/model"
  reg:=registry.New();reg.Register(registry.RegisterRequest{Name:cfg.GenerationBackendName,URL:"http://fixture.invalid",Capabilities:[]string{"generate"},Models:[]string{cfg.GenerationModel},Metadata:map[string]any{"generation_connector":"chutes"}})
  h:=New(cfg,slog.New(slog.NewTextHandler(io.Discard,nil)),hc,client.New(hc,"synthetic-internal-key"),reg,nil)
  body:=[]byte(`{"query":"synthetic question","contexts":[]}`)
  req:=httptest.NewRequest("POST","/generate",bytes.NewReader(body));req.Header.Set("Epistula-Uuid","synthetic-joined-request")
  rec:=httptest.NewRecorder();auth.NewResponseSigner(&kp,true,1<<20,nil).Wrap(http.HandlerFunc(h.Generate)).ServeHTTP(rec,req)
  expected:=http.StatusBadGateway;if receipt.Status==200{expected=200};if receipt.Status==503{expected=503}
  if rec.Code!=expected||calls!=1{t.Fatalf("status %d expected %d calls %d",rec.Code,expected,calls)}
  msg:=auth.ResponseMessage("POST","/generate","",strconv.Itoa(rec.Code),auth.EnvelopeBodyHash(body),req.Header.Get("Epistula-Uuid"),auth.EnvelopeBodyHash(rec.Body.Bytes()),rec.Header().Get(auth.ResponseTimestampHeader),kp.Address)
  raw,err:=hex.DecodeString(strings.TrimPrefix(rec.Header().Get(auth.ResponseSignatureHeader),"0x"));if err!=nil{t.Fatal(err)}
  digest:=sha256.Sum256([]byte(msg));if !boilerplate.VerfySignature(kp.Address,digest[:],raw){t.Fatal("joined signature rejected")}
 })}
}
