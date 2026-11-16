package main
import("testing";"net/http";"io";"strings";"context";"encoding/json")
type fixtureTransport func(*http.Request)(*http.Response,error)
func(f fixtureTransport)RoundTrip(r *http.Request)(*http.Response,error){return f(r)}
func TestChutesRegistrationAndGenerationHealth(t *testing.T){
 t.Setenv("SERVICE_NAME","chutes")
 t.Setenv("UPSTREAM_URL","http://127.0.0.1:19201")
 t.Setenv("UPSTREAM_HEALTH_PATH","/health/generation")
 t.Setenv("CAPABILITIES","generate")
 t.Setenv("MODELS","Qwen/Qwen3.8-27B-TEE")
 t.Setenv("METADATA",`{"generation_connector":"chutes"}`)
 t.Setenv("INTERNAL_API_KEY","synthetic-only")
 cfg:=configFromEnv()
 if cfg.ServiceName!="chutes"||cfg.Metadata["generation_connector"]!="chutes"||len(cfg.Capabilities)!=1||cfg.Capabilities[0]!="generate"{t.Fatal("registration config mismatch")}
 status:=200; healthCalls:=0; posts:=0
 c:=&http.Client{Transport:fixtureTransport(func(r *http.Request)(*http.Response,error){
  code:=200
  if r.Method=="GET" {healthCalls++;if r.URL.String()!="http://127.0.0.1:19201/health/generation"{t.Fatal(r.URL)};code=status
  }else{posts++;var p map[string]any;if err:=json.NewDecoder(r.Body).Decode(&p);err!=nil{t.Fatal(err)};if p["name"]!="chutes"{t.Fatal(p)};if h,ok:=p["health"].(map[string]any);ok {want:="ok";if status!=200{want="degraded"};if h["status"]!=want{t.Fatal(h)}}}
  return &http.Response{StatusCode:code,Status:http.StatusText(code),Body:io.NopCloser(strings.NewReader("{}"))},nil
 })}
 if err:=register(context.Background(),c,cfg);err!=nil{t.Fatal(err)}
 for _,s:=range []int{200,503}{status=s;if err:=heartbeat(context.Background(),c,cfg);err!=nil{t.Fatal(err)}}
 if healthCalls!=2||posts!=3{t.Fatal(healthCalls,posts)}
}
