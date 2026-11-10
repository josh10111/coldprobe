package bench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/josh10111/coldprobe/internal/stats"
	"github.com/josh10111/coldprobe/internal/store"
)

// job builds a store.Job; times are offsets in ms from t0.
func job(phase string, level float64, status string, intendedMs, sentMs int, wall, delay, exec float64) store.Job {
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	return store.Job{Phase: phase, Level: level, Status: status, IntendedAt: ms(intendedMs), SentAt: ms(sentMs),
		WallMs: wall, DelayMs: delay, ExecMs: exec}
}

func TestSummarize(t *testing.T) {
	const L = 100_000 // the load level starts 100 s after t0
	c := store.ConfigDetail{
		Config: store.Config{ID: 7, Pool: "AMPERE_24", FlashBoot: "OFF", PriceHr: 0.75},
		Jobs: []store.Job{
			//  phase        level status       intended sent    wall   delay  exec
			job("first_boot", 0, "COMPLETED", 0, 0, 60000, 59000, 600),
			job("cold", 0, "COMPLETED", 0, 0, 8000, 7300, 550),  // wall-exec 7450
			job("cold", 0, "COMPLETED", 0, 0, 9000, 8300, 560),  // 8440
			job("cold", 0, "COMPLETED", 0, 0, 10000, 9300, 570), // 9430
			job("warm", 0, "COMPLETED", 0, 0, 600, 20, 520),
			job("warm", 0, "COMPLETED", 0, 0, 640, 40, 530),
			// The second send went out 10 ms late; that counts toward its latency.
			job("load", 4, "COMPLETED", L, L, 600, 30, 510),           // latency 600, done at L+600
			job("load", 4, "COMPLETED", L+250, L+260, 1000, 100, 520), // latency 1010, done at L+1260 (the last to finish)
			job("load", 4, "COMPLETED", L+500, L+500, 650, 50, 505),   // latency 650, done at L+1150
			job("load", 4, "FAILED", L+750, L+750, 30000, 29000, 200), // outlier; excluded from latency and throughput
			// Exec 0 and sent before the last completion: cost and window unchanged.
			job("load", 4, "ERROR", L+1000, L+1000, 5, 0, 0),         // failed
			job("load", 4, "CANCELLED", L+1250, L+1250, 40000, 0, 0), // unfinished, not failed
		},
	}
	p := validParams()
	p.IdleTimeoutS = 5
	p.ColdStarts = 5 // a partial run: only 3 cold jobs were recorded
	p.LevelsRPS = []float64{4, 16}

	got := Summarize(c, p)

	if got.ConfigID != 7 {
		t.Errorf("ConfigID = %d, want 7", got.ConfigID)
	}
	if got.FirstBootMs != 59400 {
		t.Errorf("FirstBootMs = %v, want 59400 (wall - exec)", got.FirstBootMs)
	}
	checkSummary(t, "Cold", got.Cold, stats.Summary{N: 3, P50: 8440, P95: 9430, Mean: 8440, Min: 7450, Max: 9430})
	checkSummary(t, "ColdDelay", got.ColdDelay, stats.Summary{N: 3, P50: 8300, P95: 9300, Mean: 8300, Min: 7300, Max: 9300})
	checkSummary(t, "Warm", got.Warm, stats.Summary{N: 2, P50: 600, P95: 640, Mean: 620, Min: 600, Max: 640})

	if len(got.Levels) != 1 {
		t.Fatalf("got %d levels, want 1", len(got.Levels))
	}
	lv := got.Levels[0]
	if lv.RPS != 4 || lv.Sent != 6 || lv.Completed != 3 || lv.Failed != 2 || lv.Unfinished != 1 {
		t.Errorf("level = rps %v sent %d completed %d failed %d unfinished %d, want 4/6/3/2/1",
			lv.RPS, lv.Sent, lv.Completed, lv.Failed, lv.Unfinished)
	}
	if lv.Sent != lv.Completed+lv.Failed+lv.Unfinished {
		t.Errorf("level: sent %d != completed + failed + unfinished", lv.Sent)
	}
	// 3 completed over (L+1260) - L = 1.26 s: the last completion ends the window.
	if !near(lv.ThroughputRPS, 3/1.26) {
		t.Errorf("ThroughputRPS = %v, want %v", lv.ThroughputRPS, 3/1.26)
	}
	checkSummary(t, "Latency", lv.Latency, stats.Summary{N: 3, P50: 650, P95: 1010, Mean: (600 + 1010 + 650) / 3.0, Min: 600, Max: 1010})
	checkSummary(t, "Delay", lv.Delay, stats.Summary{N: 3, P50: 50, P95: 100, Mean: 60, Min: 30, Max: 100})

	if got.Completed != 9 || got.Failed != 2 || got.Unfinished != 1 {
		t.Errorf("Completed, Failed, Unfinished = %d, %d, %d, want 9, 2, 1", got.Completed, got.Failed, got.Unfinished)
	}
	if len(c.Jobs) != got.Completed+got.Failed+got.Unfinished {
		t.Errorf("%d jobs != completed + failed + unfinished", len(c.Jobs))
	}

	// Exec over every job: 600 + 1680 + 1050 + 1735 = 5065 ms.
	// Boot (wall-exec) of first_boot + cold: 59400 + 25320 = 84720 ms.
	// Idle tail: 3 recorded cold jobs x 5 s = 15 s.
	wantCost := ((5065.0+84720.0)/1000 + 15) / 3600 * 0.75
	if !near(got.CostUSD, wantCost) {
		t.Errorf("CostUSD = %v, want %v", got.CostUSD, wantCost)
	}
	if !near(got.CostPer1k, wantCost/9*1000) {
		t.Errorf("CostPer1k = %v, want %v", got.CostPer1k, wantCost/9*1000)
	}
}

func TestSummarize_ThroughputWindowStartsAtFirstSent(t *testing.T) {
	const L = 100_000
	c := store.ConfigDetail{Jobs: []store.Job{
		//  phase  level status      intended sent   wall   delay exec
		job("load", 4, "FAILED", L, L, 30000, 29000, 0),
		job("load", 4, "COMPLETED", L+250, L+250, 600, 30, 510), // done at L+850
	}}
	p := validParams()
	p.LevelsRPS = []float64{4}

	lv := Summarize(c, p).Levels[0]
	// 1 completed over (L+850) - L = 0.85 s.
	if !near(lv.ThroughputRPS, 1/0.85) {
		t.Errorf("ThroughputRPS = %v, want %v", lv.ThroughputRPS, 1/0.85)
	}
}

// A FAILED cold job still pays an idle tail but no boot time.
func TestSummarize_FailedColdJobCountsScaleDown(t *testing.T) {
	c := store.ConfigDetail{
		Config: store.Config{PriceHr: 0.72},
		Jobs: []store.Job{
			job("cold", 0, "FAILED", 0, 0, 300000, 0, 0),
		},
	}
	p := validParams()
	p.IdleTimeoutS = 5

	got := Summarize(c, p)
	// No exec, no boot; one idle tail of 5 s at $0.72/h = $0.001.
	if !near(got.CostUSD, 0.001) {
		t.Errorf("CostUSD = %v, want 0.001 (one 5 s idle tail)", got.CostUSD)
	}
}

// Levels encode as [], and nothing is NaN or Inf.
func TestSummarize_NoJobs(t *testing.T) {
	got := Summarize(store.ConfigDetail{}, validParams())
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"levels":[]`) {
		t.Errorf("json = %s, want \"levels\":[]", b)
	}
}

func checkSummary(t *testing.T, name string, got, want stats.Summary) {
	t.Helper()
	if got.N != want.N || !near(got.P50, want.P50) || !near(got.P95, want.P95) || !near(got.Mean, want.Mean) ||
		!near(got.Min, want.Min) || !near(got.Max, want.Max) {
		t.Errorf("%s = %+v, want %+v", name, got, want)
	}
}

// gpuJob is a COMPLETED job with the GPU image's numbers; negative means absent.
func gpuJob(phase, worker, gpu string, matmul, tflops, cudaInit float64) store.Job {
	j := job(phase, 0, "COMPLETED", 0, 0, 1000, 10, 900)
	j.WorkerID, j.GPUTypeID = worker, gpu
	ptr := func(v float64) *float64 {
		if v < 0 {
			return nil
		}
		return &v
	}
	j.MatmulMs, j.TFLOPS, j.CudaInitMs = ptr(matmul), ptr(tflops), ptr(cudaInit)
	return j
}

func TestSummarize_GPUs(t *testing.T) {
	const a5000, mig = "NVIDIA RTX A5000", "NVIDIA RTX PRO 6000 Blackwell MIG 1g.24gb"
	failed := job("load", 0, "FAILED", 0, 0, 30000, 0, 0)
	failed.WorkerID, failed.GPUTypeID = "w1", a5000
	c := store.ConfigDetail{
		Config: store.Config{PriceHr: 0.75},
		Jobs: []store.Job{
			//     phase        worker gpu    matmul tflops cudaInit
			gpuJob("first_boot", "w1", a5000, 2.0, 70, 300),
			gpuJob("cold", "w1", a5000, 2.2, 62.5, 280), // same worker ID, new process
			gpuJob("cold", "w2", mig, 10, 13.7, 260),
			gpuJob("warm", "w1", a5000, 1.8, 76, 280),
			gpuJob("warm", "w1", a5000, 2.0, 68, 280),
			// No gpuTypeId: borrows mig from w2's other job.
			gpuJob("load", "w2", "", 11, 12.5, 260),
			// w3's GPU is never known: its own "" group, never mixed in.
			gpuJob("load", "w3", "", 4, 34, 240),
			failed,
		},
	}
	got := Summarize(c, validParams()).GPUs

	if len(got) != 3 || got[0].GPUTypeID != "" || got[1].GPUTypeID != a5000 || got[2].GPUTypeID != mig {
		t.Fatalf("GPUs = %+v, want \"\", %s, %s in that order", got, a5000, mig)
	}
	unknown, a, m := got[0], got[1], got[2]

	checkSummary(t, "A5000 matmulMs", a.MatmulMs, stats.Summary{N: 4, P50: 2.0, P95: 2.2, Mean: 2.0, Min: 1.8, Max: 2.2})
	checkSummary(t, "A5000 tflops", a.TFLOPS, stats.Summary{N: 4, P50: 68, P95: 76, Mean: 69.125, Min: 62.5, Max: 76})
	checkSummary(t, "A5000 coldCudaInitMs", a.ColdCudaInitMs, stats.Summary{N: 2, P50: 280, P95: 300, Mean: 290, Min: 280, Max: 300})
	if !near(a.USDPerTFLOPHr, 0.75/68) {
		t.Errorf("A5000 usdPerTflopHr = %v, want 0.75/68 (price / p50 tflops)", a.USDPerTFLOPHr)
	}

	checkSummary(t, "MIG matmulMs", m.MatmulMs, stats.Summary{N: 2, P50: 10, P95: 11, Mean: 10.5, Min: 10, Max: 11})
	checkSummary(t, "MIG tflops", m.TFLOPS, stats.Summary{N: 2, P50: 12.5, P95: 13.7, Mean: 13.1, Min: 12.5, Max: 13.7})
	checkSummary(t, "MIG coldCudaInitMs", m.ColdCudaInitMs, stats.Summary{N: 1, P50: 260, P95: 260, Mean: 260, Min: 260, Max: 260})
	if !near(m.USDPerTFLOPHr, 0.06) {
		t.Errorf("MIG usdPerTflopHr = %v, want 0.75/12.5 = 0.06", m.USDPerTFLOPHr)
	}

	checkSummary(t, "unknown tflops", unknown.TFLOPS, stats.Summary{N: 1, P50: 34, P95: 34, Mean: 34, Min: 34, Max: 34})
	if unknown.ColdCudaInitMs.N != 0 {
		t.Errorf("unknown coldCudaInitMs = %+v, want none (load jobs aren't cold)", unknown.ColdCudaInitMs)
	}
}

// matmulN 0 reports only cuda_init_ms: no matmul numbers and no $/TFLOP-hr.
func TestSummarize_GPUsCudaInitOnly(t *testing.T) {
	c := store.ConfigDetail{
		Config: store.Config{PriceHr: 0.58},
		Jobs: []store.Job{
			gpuJob("first_boot", "w1", "NVIDIA RTX A4000", -1, -1, 300),
			gpuJob("warm", "w1", "NVIDIA RTX A4000", -1, -1, 300),
		},
	}
	got := Summarize(c, validParams())
	if len(got.GPUs) != 1 {
		t.Fatalf("GPUs = %+v, want one", got.GPUs)
	}
	g := got.GPUs[0]
	if g.MatmulMs.N != 0 || g.TFLOPS.N != 0 || g.USDPerTFLOPHr != 0 || g.ColdCudaInitMs.N != 1 || g.ColdCudaInitMs.P50 != 300 {
		t.Errorf("GPU = %+v, want only one cold cuda init of 300", g)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("json: %v", err)
	}
}

// Without GPU numbers (synthetic image) there's no gpus key.
func TestSummarize_NoGPUsKeyWithoutMatmul(t *testing.T) {
	c := store.ConfigDetail{Jobs: []store.Job{
		job("first_boot", 0, "COMPLETED", 0, 0, 60000, 59000, 600),
		job("warm", 0, "COMPLETED", 0, 0, 600, 20, 520),
	}}
	got := Summarize(c, validParams())
	b, _ := json.Marshal(got)
	if got.GPUs != nil || strings.Contains(string(b), "gpus") {
		t.Errorf("json = %s, want no gpus", b)
	}
}
