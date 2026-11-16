package handlers
import("context";"encoding/json";"io";"log/slog";"net/http";"strings";"testing";"recall-miner/internal/client";"recall-miner/internal/config";"recall-miner/internal/registry")
func TestChutesOnlyRouting(t *testing.T){
 for _,tc:=range []struct{name,backend,provider,advertised,requested,returned string;want bool}{
 {"selected","chutes","chutes","fixture/model","","fixture/model",true},
 {"no name fallback","other","chutes","fixture/model","","fixture/model",false},
 {"no connector fallback","chutes","vllm","fixture/model","","fixture/model",false},
 {"missing model","chutes","chutes","other/model","","fixture/model",false},
 {"requested alternate","chutes","chutes","fixture/model","other/model","fixture/model",false},
 {"returned alternate","chutes","chutes","fixture/model","","other/model",false},
 }{t.Run(tc.name,func(t *testing.T){
 cfg:=config.FromEnv();cfg.GenerationModel="fixture/model";calls:=0
 hc:=&http.Client{Transport:fakeHostedTransport(func(r *http.Request)(*http.Response,error){calls++;var p map[string]any;if err:=json.NewDecoder(r.Body).Decode(&p);err!=nil{t.Fatal(err)};if p["probe_contract"]!="computed-probe-v1"{t.Fatal("lost contract")};return &http.Response{StatusCode:200,Body:io.NopCloser(strings.NewReader(`{"model":"`+tc.returned+`"}`)),Header:http.Header{}},nil})}
 reg:=registry.New();reg.Register(registry.RegisterRequest{Name:tc.backend,URL:"http://fixture.invalid",Capabilities:[]string{"generate"},Models:[]string{tc.advertised},Metadata:map[string]any{"generation_connector":tc.provider}})
 h:=New(cfg,slog.New(slog.NewTextHandler(io.Discard,nil)),hc,client.New(hc,"synthetic"),reg,nil)
 _,err:=h.generate(context.Background(),generateRequest{Query:"synthetic",Model:tc.requested,ProbeContract:"computed-probe-v1"})
 if (err==nil)!=tc.want{t.Fatal(err)}
 wantCalls:=0;if tc.want||tc.name=="returned alternate"{wantCalls=1};if calls!=wantCalls{t.Fatal("unexpected dispatch",calls)}
 })}
}
