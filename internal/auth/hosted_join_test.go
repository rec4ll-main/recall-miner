package auth

import (
 "encoding/json"
 "encoding/hex"
 "net/http"
 "net/http/httptest"
 "os"
 "strconv"
 "strings"
 "testing"
 "github.com/centrifuge/go-substrate-rpc-client/v4/signature"
 "github.com/subtrahend-labs/gobt/boilerplate"
)

// Consumes actual Python handler outputs from the preceding offline test run.
// The known development key is generated in memory; no chain/key files exist.
func TestHostedHandlerReceiptsSignedAndVerified(t *testing.T) {
 data,err:=os.ReadFile("../../testdata/handler-receipts.json"); if err!=nil {t.Fatal(err)}
 var receipts []struct{Status int `json:"status"`;Body json.RawMessage `json:"body"`}
 if err=json.Unmarshal(data,&receipts);err!=nil {t.Fatal(err)}
 if len(receipts)!=5 {t.Fatal("missing joined receipts")}
 kp,err:=signature.KeyringPairFromSecret("//Alice",42);if err!=nil {t.Fatal(err)}
 for _,receipt:=range receipts {t.Run(strconv.Itoa(receipt.Status),func(t *testing.T){
   requestBody:=[]byte(`{"query":"synthetic question","contexts":[]}`)
   req:=httptest.NewRequest("POST","/generate",strings.NewReader(string(requestBody)))
   req.Header.Set("Epistula-Uuid","synthetic-request-uuid")
   rec:=httptest.NewRecorder()
   NewResponseSigner(&kp,true,1<<20,nil).Wrap(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
     w.WriteHeader(receipt.Status);_,_=w.Write(receipt.Body)
   })).ServeHTTP(rec,req)
   raw,err:=hex.DecodeString(strings.TrimPrefix(rec.Header().Get(ResponseSignatureHeader),"0x"));if err!=nil||len(raw)==0 {t.Fatal("missing signature")}
   msg:=ResponseMessage("POST","/generate","",strconv.Itoa(rec.Code),bodyHash(requestBody),req.Header.Get("Epistula-Uuid"),bodyHash(rec.Body.Bytes()),rec.Header().Get(ResponseTimestampHeader),kp.Address)
   if !boilerplate.VerfySignature(kp.Address,sha256Digest([]byte(msg)),raw) {t.Fatal("invalid signature")}
   if boilerplate.VerfySignature(kp.Address,sha256Digest([]byte(msg+"tamper")),raw) {t.Fatal("tampering accepted")}
 })}
}

func TestSigningFailureCannotBecomeSignedTrustProof(t *testing.T){
 kp:=signature.KeyringPair{URI:"invalid synthetic secret",Address:"synthetic"}
 rec:=httptest.NewRecorder();req:=httptest.NewRequest("POST","/generate",nil)
 NewResponseSigner(&kp,true,1<<20,nil).Wrap(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.WriteHeader(200);_,_=w.Write([]byte(`{"answer":"synthetic"}`))})).ServeHTTP(rec,req)
 if rec.Header().Get(ResponseSignatureHeader)!="" {t.Fatal("invalid key signed")}
 // Unchanged baseline returns unsigned200; strict verifier must reject it.
 if rec.Code!=200 {t.Fatal("unexpected baseline change")}
}
