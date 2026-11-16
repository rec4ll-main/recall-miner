package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRefundReclaimsSuccessfulOverlappingReservations(t *testing.T) {
	for _, creatorFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(creatorFirst), func(t *testing.T) {
			w := NewWindow(2, time.Minute, 1)
			now := time.Unix(100, 0)
			fixedClock(w, &now)
			out, _, first := w.reserve("A")
			if out != Allowed {
				t.Fatal(out)
			}
			out, _, second := w.reserve("A")
			if out != Allowed {
				t.Fatal(out)
			}
			if creatorFirst {
				first()
				second()
			} else {
				second()
				first()
			}
			if len(w.entries) != 0 || w.expiry.Len() != 0 {
				t.Fatalf("successful reservations retained entries=%d expiry=%d", len(w.entries), w.expiry.Len())
			}
			// No clock advance: a new source must immediately get the freed slot.
			if out, _ := w.Record("B"); out != Allowed {
				t.Fatalf("retry from new source: %v", out)
			}
		})
	}
}

func TestRefundPreservesFailuresAndPendingReservations(t *testing.T) {
	w := NewWindow(3, time.Minute, 1)
	w.Record("A") // A failed authentication keeps this charge.
	_, _, first := w.reserve("A")
	_, _, second := w.reserve("A")
	first()
	if w.entries["A"].count != 2 {
		t.Fatal("refund erased failure or pending work")
	}
	second()
	if w.entries["A"].count != 1 || w.expiry.Len() != 1 {
		t.Fatal("refund erased failed-auth charge")
	}
	if out, _ := w.Record("B"); out != TableFull {
		t.Fatal("failed-auth bucket lost capacity accounting")
	}
	for i := 0; i < 2; i++ {
		if out, _ := w.Record("A"); out != Allowed {
			t.Fatal(out)
		}
	}
	if out, _ := w.Record("A"); out != OverLimit {
		t.Fatal("failed-auth limit bypassed")
	}
}

func TestRefundDoesNotDeleteReplacementGeneration(t *testing.T) {
	for _, evict := range []bool{false, true} {
		t.Run(fmt.Sprint(evict), func(t *testing.T) {
			w := NewWindow(1, time.Minute, 1)
			now := time.Unix(100, 0)
			fixedClock(w, &now)
			_, _, oldRefund := w.reserve("A")
			now = now.Add(time.Minute)
			if evict {
				_, _, otherRefund := w.reserve("B")
				otherRefund()
			}
			if out, _ := w.Record("A"); out != Allowed {
				t.Fatal(out)
			}
			oldRefund()
			oldRefund()
			if out, retry := w.Record("A"); out != OverLimit || retry != time.Minute {
				t.Fatalf("late refund changed replacement: %v %s", out, retry)
			}
		})
	}
}

func TestRefundChurnAndConcurrentRetryRemainBounded(t *testing.T) {
	w := NewWindow(1, time.Minute, 1)
	for i := 0; i < 1000; i++ {
		out, _, refund := w.reserve(fmt.Sprint(i))
		if out != Allowed {
			t.Fatal(out)
		}
		var wg sync.WaitGroup
		for n := 0; n < 2; n++ {
			wg.Add(1)
			go func() { defer wg.Done(); refund() }()
		}
		wg.Wait()
		if len(w.entries) != 0 || w.expiry.Len() != 0 {
			t.Fatal("refund metadata retained")
		}
	}
	// Reusing a key at the same clock value still creates a distinct generation.
	now := time.Unix(100, 0)
	fixedClock(w, &now)
	_, _, oldRefund := w.reserve("A")
	oldRefund()
	w.Record("A")
	oldRefund()
	if out, _ := w.Record("A"); out != OverLimit {
		t.Fatal("duplicate refund credited replacement")
	}
}
