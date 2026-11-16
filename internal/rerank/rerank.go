package rerank

import (
	"math"
	"sort"
)

func CosineResult(query []float64, candidates [][]float64, candidateIDs []string) map[string]any {
	scores := make([]float64, len(candidates))
	for i, candidate := range candidates {
		scores[i] = Cosine(query, candidate)
	}
	return map[string]any{"scores": scores, "ranked_ids": RankedIDs(scores, candidateIDs), "model": "cosine-similarity"}
}

// RankedIDs returns the candidates in descending score order. Ties keep the
// lower candidate index first (stable), which is the order the validator
// derives from the same scores (validateRankedIDsMatchScores). Without
// candidate IDs it returns candidate indexes ([]int); with IDs it returns the
// IDs ([]string). Candidates without a score sort after all scored ones.
func RankedIDs(scores []float64, candidateIDs []string) any {
	n := len(scores)
	if len(candidateIDs) > 0 {
		n = len(candidateIDs)
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		left, right := order[i], order[j]
		leftOK, rightOK := left < len(scores), right < len(scores)
		if leftOK != rightOK {
			return leftOK
		}
		if !leftOK || scores[left] == scores[right] {
			return left < right
		}
		return scores[left] > scores[right]
	})
	if len(candidateIDs) == 0 {
		return order
	}
	ranked := make([]string, 0, len(order))
	for _, idx := range order {
		ranked = append(ranked, candidateIDs[idx])
	}
	return ranked
}

func Cosine(a, b []float64) float64 {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < limit; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
