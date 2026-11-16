package handlers

import (
 "testing"
 "recall-miner/internal/registry"
)

// A generation outage must not withdraw the unchanged shared CPU lanes.
func TestChutesIsolationPreservesSharedLanes(t *testing.T) {
 h := testHandler(t, true)
 h.cfg.GenerationBackendName = "chutes"
 h.cfg.GenerationModel = productionModel
 h.registry.Register(registry.RegisterRequest{Name:"cpu", URL:"http://cpu.invalid", Capabilities:[]string{"embedding","rerank"}})
 t4Register(t,h,productionRegistration("http://chutes.invalid"))
 h.registry.Register(registry.RegisterRequest{Name:"gen", URL:"http://legacy.invalid", Capabilities:[]string{"generate","generate_computed_probe_v1"}})
 assert := func(wantGen bool) {
  t.Helper()
  caps:=map[string]bool{}
  for _,c:=range h.AvailableCapabilities(){caps[c]=true}
  if !caps["embedding"] || !caps["rerank"] || caps["generate"]!=wantGen || caps["generate_computed_probe_v1"] {t.Fatalf("capabilities=%v",caps)}
 }
 assert(true)
 h.registry.Touch("chutes", "synthetic-machine", map[string]any{"status":"degraded"})
 assert(false)
 if _,ok:=h.chutesBackend();ok {t.Fatal("legacy generation must not become fallback")}
}
