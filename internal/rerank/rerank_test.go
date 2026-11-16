package rerank

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// validatorExpectedOrder mirrors recall-web validateRankedIDsMatchScores: the
// descending-score permutation, ties broken by the lower candidate index.
func validatorExpectedOrder(scores []float64) []int {
	expected := make([]int, len(scores))
	for i := range expected {
		expected[i] = i
	}
	sort.SliceStable(expected, func(i, j int) bool {
		if scores[expected[i]] == scores[expected[j]] {
			return expected[i] < expected[j]
		}
		return scores[expected[i]] > scores[expected[j]]
	})
	return expected
}

func TestRankedIDsWithoutCandidateIDsSortsByScore(t *testing.T) {
	// The validator rerank probe sends no candidate_ids; before the fix the
	// miner returned the identity order [0 1 2] here.
	got := RankedIDs([]float64{0.1, 0.9, 0.5}, nil)
	if want := []int{1, 2, 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRankedIDsTiesKeepLowerIndexFirst(t *testing.T) {
	got := RankedIDs([]float64{0.5, 0.9, 0.5, 0.9, 0}, nil)
	if want := []int{1, 3, 0, 2, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	gotIDs := RankedIDs([]float64{0.5, 0.9, 0.5, 0.9}, []string{"a", "b", "c", "d"})
	if want := []string{"b", "d", "a", "c"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("ids: got %v want %v", gotIDs, want)
	}
}

func TestRankedIDsMissingScoresSortLast(t *testing.T) {
	got := RankedIDs([]float64{0.2, 0.8}, []string{"a", "b", "c"})
	if want := []string{"b", "a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRankedIDsEmpty(t *testing.T) {
	if got := RankedIDs(nil, nil); len(got.([]int)) != 0 {
		t.Fatalf("got %v", got)
	}
}

// Contract with the validator's check, on random scores with many ties.
func TestRankedIDsMatchesValidatorContract(t *testing.T) {
	rng := rand.New(rand.NewSource(20261008))
	for iter := 0; iter < 2000; iter++ {
		n := 1 + rng.Intn(20)
		scores := make([]float64, n)
		for i := range scores {
			scores[i] = float64(rng.Intn(5)) / 4 // coarse values => frequent ties
			if rng.Intn(3) == 0 {
				scores[i] = rng.Float64()
			}
		}
		got := RankedIDs(scores, nil).([]int)
		if want := validatorExpectedOrder(scores); !reflect.DeepEqual(got, want) {
			t.Fatalf("scores %v: got %v want %v", scores, got, want)
		}
		res := CosineResult([]float64{1, 0}, [][]float64{{1, 0}, {0, 1}}, nil)
		if !reflect.DeepEqual(res["ranked_ids"], validatorExpectedOrder(res["scores"].([]float64))) {
			t.Fatalf("cosine result breaks the contract: %v", res)
		}
	}
}
