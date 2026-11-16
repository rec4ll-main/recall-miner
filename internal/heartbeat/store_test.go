package heartbeat

import (
	"context"
	"testing"
)

type limitSink struct{ limits []int }

func (s *limitSink) Write(context.Context, Record) error { return nil }
func (s *limitSink) List(_ context.Context, _ string, limit int) ([]Record, error) {
	s.limits = append(s.limits, limit)
	return nil, nil
}
func (s *limitSink) Close() error { return nil }

func TestStoreListCapsRequestedLimit(t *testing.T) {
	sink := &limitSink{}
	store := &Store{sink: sink}
	for _, limit := range []int{1 << 30, MaxListLimit + 1, MaxListLimit, 5, 0, -3} {
		store.List("", limit)
	}
	want := []int{MaxListLimit, MaxListLimit, MaxListLimit, 5, 100, 100}
	if len(sink.limits) != len(want) {
		t.Fatalf("limits = %v, want %v", sink.limits, want)
	}
	for i := range want {
		if sink.limits[i] != want[i] {
			t.Fatalf("limits = %v, want %v", sink.limits, want)
		}
	}
}
