package stats

import (
	"math"
	"slices"
	"testing"
	"time"
)

const eps = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < eps
}

func TestPercentile(t *testing.T) {
	oneToTen := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	oneTo25 := make([]float64, 25)
	for i := range oneTo25 {
		oneTo25[i] = float64(i + 1)
	}

	tests := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"p50 of 1..10", oneToTen, 50, 5},
		{"p95 of 1..10", oneToTen, 95, 10},
		{"p100 of 1..10", oneToTen, 100, 10},
		{"p0 is the first element", oneToTen, 0, 1},
		{"p150 clamps to last", oneToTen, 150, 10},
		{"single element", []float64{42}, 50, 42},
		{"empty", nil, 50, 0},
		// p/100*n gives 7.000000000000001 here, which ceils to 8.
		{"p28 of 1..25 (float order)", oneTo25, 28, 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Percentile(tt.sorted, tt.p)
			if !almostEqual(got, tt.want) {
				t.Errorf("Percentile(%v, %v) = %v, want %v", tt.sorted, tt.p, got, tt.want)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	t.Run("nil input gives zero Summary", func(t *testing.T) {
		if got := Summarize(nil); got != (Summary{}) {
			t.Errorf("Summarize(nil) = %+v, want zero Summary", got)
		}
	})

	t.Run("unsorted input", func(t *testing.T) {
		in := []float64{3, 1, 2}
		got := Summarize(in)
		want := Summary{N: 3, P50: 2, P95: 3, Mean: 2, Min: 1, Max: 3}

		if got.N != want.N ||
			!almostEqual(got.P50, want.P50) || !almostEqual(got.P95, want.P95) ||
			!almostEqual(got.Mean, want.Mean) ||
			!almostEqual(got.Min, want.Min) || !almostEqual(got.Max, want.Max) {
			t.Errorf("Summarize(%v) = %+v, want %+v", in, got, want)
		}

		if !slices.Equal(in, []float64{3, 1, 2}) {
			t.Errorf("Summarize modified its input: %v", in)
		}
	})
}

func TestUpperBoundUSD(t *testing.T) {
	// 10 min = 1/6 h; 1/6 h x 2 workers x $0.72/h = $0.24.
	got := UpperBoundUSD(10*time.Minute, 2, 0.72)
	if !almostEqual(got, 0.24) {
		t.Errorf("UpperBoundUSD = %v, want 0.24", got)
	}
}

func TestAttributedCostUSD(t *testing.T) {
	tests := []struct {
		name         string
		sumExecMs    float64
		sumColdMs    float64
		scaleDowns   int
		idleTimeoutS int
		pricePerHr   float64
		want         float64
	}{
		{"one hour of execution", 3_600_000, 0, 0, 0, 0.72, 0.72},
		{"one hour of cold start", 0, 3_600_000, 0, 0, 0.72, 0.72},
		{"idle tail before scale-down", 0, 0, 2, 5, 0.72, 0.002},
		{"exec and cold add up", 1_800_000, 1_800_000, 0, 0, 0.72, 0.72},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AttributedCostUSD(tt.sumExecMs, tt.sumColdMs, tt.scaleDowns, tt.idleTimeoutS, tt.pricePerHr)
			if !almostEqual(got, tt.want) {
				t.Errorf("AttributedCostUSD = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPerThousand(t *testing.T) {
	tests := []struct {
		name      string
		costUSD   float64
		completed int
		want      float64
	}{
		{"$0.50 over 250 requests", 0.5, 250, 2.0},
		{"zero completed does not divide by zero", 1, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PerThousand(tt.costUSD, tt.completed)
			if !almostEqual(got, tt.want) {
				t.Errorf("PerThousand(%v, %v) = %v, want %v", tt.costUSD, tt.completed, got, tt.want)
			}
		})
	}
}
