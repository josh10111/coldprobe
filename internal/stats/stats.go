// Package stats computes timing summaries and run cost estimates.
package stats

import (
	"math"
	"slices"
	"time"
)

// Summary describes one set of measurements (milliseconds in coldprobe).
type Summary struct {
	N    int     `json:"n"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	Mean float64 `json:"mean"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// Summarize computes a Summary of vals; no samples gives the zero Summary.
func Summarize(vals []float64) Summary {
	if len(vals) == 0 {
		return Summary{}
	}

	sorted := slices.Clone(vals)
	slices.Sort(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}

	return Summary{
		N:    len(sorted),
		P50:  Percentile(sorted, 50),
		P95:  Percentile(sorted, 95),
		Mean: sum / float64(len(sorted)),
		Min:  sorted[0],
		Max:  sorted[len(sorted)-1],
	}
}

// Percentile returns the nearest-rank p-th percentile (0-100) of an already
// sorted slice. The rank is clamped to [1, n]; an empty slice returns 0.
func Percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}

	// Multiply before dividing: p/100 is inexact in binary, and 28/100*25
	// gives 7.000000000000001, which ceils to 8.
	rank := int(math.Ceil(p * float64(n) / 100))
	rank = max(1, min(rank, n))

	return sorted[rank-1]
}

// UpperBoundUSD is the most one endpoint could have cost so far: maxWorkers
// billed for the whole elapsed time. The budget guard uses it because it
// needs no polling and can only over-estimate.
func UpperBoundUSD(elapsed time.Duration, maxWorkers int, pricePerHr float64) float64 {
	return elapsed.Hours() * float64(maxWorkers) * pricePerHr
}

// AttributedCostUSD estimates one configuration's billed cost: execution
// plus cold-start time, plus one idle tail of idleTimeoutS per scale-down.
func AttributedCostUSD(sumExecMs, sumColdMs float64, scaleDowns, idleTimeoutS int, pricePerHr float64) float64 {
	busySeconds := (sumExecMs + sumColdMs) / 1000
	idleSeconds := float64(scaleDowns * idleTimeoutS)
	return (busySeconds + idleSeconds) / 3600 * pricePerHr
}

// PerThousand converts a total cost into dollars per 1,000 completed
// requests; zero completed returns 0 rather than +Inf.
func PerThousand(costUSD float64, completed int) float64 {
	if completed == 0 {
		return 0
	}
	return costUSD / float64(completed) * 1000
}
