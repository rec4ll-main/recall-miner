package registry

import (
	"testing"
	"time"
)

func TestHealthy_FreshWithNoStatus(t *testing.T) {
	info := ServiceInfo{Health: map[string]any{}, lastSeenTime: time.Now()}
	if !info.Healthy(time.Now()) {
		t.Fatal("fresh service with no status should be healthy")
	}
}

func TestHealthy_NilHealthFresh(t *testing.T) {
	info := ServiceInfo{lastSeenTime: time.Now()}
	if !info.Healthy(time.Now()) {
		t.Fatal("nil health with fresh lastSeen should be healthy")
	}
}

func TestHealthy_StatusOk(t *testing.T) {
	info := ServiceInfo{Health: map[string]any{"status": "ok"}, lastSeenTime: time.Now()}
	if !info.Healthy(time.Now()) {
		t.Fatal("status ok should be healthy")
	}
}

func TestHealthy_DegradedStatus(t *testing.T) {
	info := ServiceInfo{Health: map[string]any{"status": "degraded"}, lastSeenTime: time.Now()}
	if info.Healthy(time.Now()) {
		t.Fatal("degraded status should be unhealthy")
	}
}

func TestHealthy_NonStringStatus(t *testing.T) {
	info := ServiceInfo{Health: map[string]any{"status": 200}, lastSeenTime: time.Now()}
	if info.Healthy(time.Now()) {
		t.Fatal("non-string status should be unhealthy")
	}
}

func TestHealthy_Stale(t *testing.T) {
	info := ServiceInfo{Health: map[string]any{"status": "ok"}, lastSeenTime: time.Now().Add(-2 * serviceMaxAge)}
	if info.Healthy(time.Now()) {
		t.Fatal("stale service should be unhealthy")
	}
}

func TestByCapabilityHealthyExcludesDegradedAndSorts(t *testing.T) {
	reg := New()
	reg.Register(RegisterRequest{Name: "cpu", URL: "http://cpu", Capabilities: []string{"embedding"}, Models: []string{"BAAI/bge-small-en-v1.5"}})
	reg.Register(RegisterRequest{Name: "gpu", URL: "http://gpu", Capabilities: []string{"embedding"}, Models: []string{"BAAI/bge-small-en-v1.5"}})
	if _, ok := reg.Touch("cpu", "", map[string]any{"status": "degraded"}); !ok {
		t.Fatal("touch cpu")
	}
	if _, ok := reg.Touch("gpu", "", map[string]any{"status": "ok"}); !ok {
		t.Fatal("touch gpu")
	}

	all := reg.ByCapability("embedding")
	if len(all) != 2 {
		t.Fatalf("ByCapability = %d", len(all))
	}
	healthy := reg.ByCapabilityHealthy("embedding")
	if len(healthy) != 1 || healthy[0].Name != "gpu" {
		t.Fatalf("ByCapabilityHealthy = %#v", healthy)
	}

	// Recovery: degraded becomes eligible again.
	if _, ok := reg.Touch("cpu", "", map[string]any{"status": "ok"}); !ok {
		t.Fatal("touch cpu ok")
	}
	healthy = reg.ByCapabilityHealthy("embedding")
	if len(healthy) != 2 || healthy[0].Name != "cpu" || healthy[1].Name != "gpu" {
		t.Fatalf("expected stable name order cpu,gpu got %#v", healthy)
	}
}

func TestByCapabilityHealthyAllDegraded(t *testing.T) {
	reg := New()
	reg.Register(RegisterRequest{Name: "a", URL: "http://a", Capabilities: []string{"rerank"}})
	reg.Touch("a", "", map[string]any{"status": "degraded"})
	if got := reg.ByCapabilityHealthy("rerank"); len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}
