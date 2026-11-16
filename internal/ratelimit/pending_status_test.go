package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPendingCapacityAndRetainedFailureStatuses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retained   int
		completion int
		wantAfter  int
	}{{"pending success", 0, 200, 200}, {"pending failure", 0, 401, 429}, {"mixed success", 1, 200, 200}, {"mixed failure", 1, 403, 429}} {
		t.Run(tc.name, func(t *testing.T) {
			per := NewWindow(tc.retained+1, time.Minute, 10)
			for i := 0; i < tc.retained; i++ {
				per.Record("203.0.113.1")
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan int, 1)
			h := FailureGuard{PerSource: per}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Hold") == "1" {
					close(entered)
					<-release
					w.WriteHeader(tc.completion)
					return
				}
				w.WriteHeader(200)
			}))
			go func() {
				r := httptest.NewRequest("GET", "/", nil)
				r.RemoteAddr = "203.0.113.1:1"
				r.Header.Set("X-Hold", "1")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				done <- w.Code
			}()
			<-entered
			got := send(h, "203.0.113.1", true)
			close(release)
			<-done
			if got != 503 {
				t.Errorf("pending capacity: got %d want503", got)
			}
			if got := send(h, "203.0.113.1", true); got != tc.wantAfter {
				t.Errorf("after completion:got %d want%d", got, tc.wantAfter)
			}
		})
	}
}

func TestAuthenticationSettlementGenerationAndDuplicates(t *testing.T) {
	for _, evict := range []bool{false, true} {
		w := NewWindow(2, time.Minute, 1)
		now := time.Unix(100, 0)
		fixedClock(w, &now)
		_, _, old := w.reserveAuth("A")
		now = now.Add(time.Minute)
		if evict {
			_, _, other := w.reserveAuth("B")
			other(true)
		}
		_, _, current := w.reserveAuth("A")
		old(false)
		old(true)
		if w.entries["A"].failures != 0 || w.entries["A"].count != 1 {
			t.Fatal("old settlement changed replacement")
		}
		current(false)
		current(false)
		current(true)
		if w.entries["A"].failures != 1 || w.entries["A"].count != 1 {
			t.Fatal("duplicate settlement changed retained charge")
		}
		_, _, second := w.reserveAuth("A")
		if out, _, _ := w.reserveAuth("A"); out != PendingFull {
			t.Fatal("mixed saturation not pending")
		}
		second(false)
		if out, _, _ := w.reserveAuth("A"); out != OverLimit {
			t.Fatal("completed failures must retain abuse status")
		}
	}
}

func TestConcurrentMixedSettlementsKeepExactAccounting(t *testing.T) {
	w := NewWindow(100, time.Minute, 1)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		out, _, settle := w.reserveAuth("A")
		if out != Allowed {
			t.Fatal(out)
		}
		wg.Add(1)
		go func(success bool) { defer wg.Done(); settle(success); settle(!success) }(i%2 == 0)
	}
	wg.Wait()
	if w.entries["A"].count != 50 || w.entries["A"].failures != 50 {
		t.Fatal("mixed settlements lost or duplicated charge")
	}
}
