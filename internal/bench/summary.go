package bench

import (
	"maps"
	"slices"
	"time"

	"github.com/josh10111/coldprobe/internal/stats"
	"github.com/josh10111/coldprobe/internal/store"
)

// Job statuses. Each job counts as exactly one of completed, unfinished or
// failed, so Sent = Completed + Failed + Unfinished.
const (
	completed = "COMPLETED"
	// jobCancelled marks a job cut short (drain deadline, cancel, budget,
	// config deadline) or cancelled by Runpod. Such jobs were typically still
	// queued, so they count as unfinished, not failed.
	jobCancelled = "CANCELLED"
	// jobTimedOut is Runpod's execution timeout or our phase timeout. Failed.
	jobTimedOut = "TIMED_OUT"
	// jobError is an HTTP or network error: the outcome is unknown. Failed.
	jobError = "ERROR"
)

// LevelSummary is one open-loop load level. All times are milliseconds.
type LevelSummary struct {
	RPS           float64       `json:"rps"`  // offered rate
	Sent          int           `json:"sent"` // jobs scheduled at this level
	Completed     int           `json:"completed"`
	Failed        int           `json:"failed"`        // FAILED, TIMED_OUT (incl. phase timeouts), ERROR
	Unfinished    int           `json:"unfinished"`    // CANCELLED: cut short, e.g. at the drain deadline
	ThroughputRPS float64       `json:"throughputRps"` // achieved rate, completed jobs only
	Latency       stats.Summary `json:"latency"`       // from IntendedAt to completion
	Delay         stats.Summary `json:"delay"`         // Runpod's delayTime (time queued)
}

// ConfigSummary is the report for one pool x flashboot config, in ms.
// Timing summaries use COMPLETED jobs only.
type ConfigSummary struct {
	ConfigID    int64          `json:"configId"`
	FirstBootMs float64        `json:"firstBootMs"` // wall - exec of the first job: image pull + boot
	Cold        stats.Summary  `json:"cold"`        // wall - exec of cold jobs: what a cold start adds
	ColdDelay   stats.Summary  `json:"coldDelay"`   // Runpod's delayTime for the same jobs
	Warm        stats.Summary  `json:"warm"`        // wall time of warm jobs
	Levels      []LevelSummary `json:"levels"`
	Completed   int            `json:"completed"`
	Failed      int            `json:"failed"`
	Unfinished  int            `json:"unfinished"`
	CostUSD     float64        `json:"costUsd"`   // attributed cost, see Summarize
	CostPer1k   float64        `json:"costPer1k"` // CostUSD per 1,000 completed jobs
	// GPUs has one entry per GPU model the GPU worker image reported on;
	// nil for the synthetic image.
	GPUs []GPUSummary `json:"gpus,omitempty"`
}

// GPUSummary is one GPU model's numbers within a config, from COMPLETED
// jobs. Pools mix GPU models, so a per-config average would blend them.
type GPUSummary struct {
	GPUTypeID string        `json:"gpuTypeId"` // "" when Runpod never named the worker's GPU
	MatmulMs  stats.Summary `json:"matmulMs"`  // median ms per matmul, one sample per job
	TFLOPS    stats.Summary `json:"tflops"`    // fp16 TFLOP/s, one sample per job
	// USDPerTFLOPHr is the config's $/hour over p50 TFLOPS; 0 without samples.
	USDPerTFLOPHr float64 `json:"usdPerTflopHr"`
	// ColdCudaInitMs covers first_boot and cold jobs, the first on each new worker process.
	ColdCudaInitMs stats.Summary `json:"coldCudaInitMs"`
}

// Summarize turns one config's recorded jobs into its report. first_boot
// is kept out of Cold because it also pulls the image.
//
// Cost covers exec time of every job (Runpod bills what ran), boot time of
// COMPLETED first_boot and cold jobs only (a never-placed job billed $0),
// and one idle tail per cold job actually recorded, so a partial run isn't
// charged for scale-downs that never happened.
func Summarize(c store.ConfigDetail, p Params) ConfigSummary {
	s := ConfigSummary{ConfigID: c.ID, Levels: []LevelSummary{}} // [] not null in JSON

	var cold, coldDelay, warm []float64
	var sumExecMs, sumBootMs float64
	coldJobs := 0

	for _, j := range c.Jobs {
		sumExecMs += j.ExecMs
		if j.Phase == "cold" {
			coldJobs++
		}
		switch j.Status {
		case completed:
			s.Completed++
		case jobCancelled:
			s.Unfinished++
			continue
		default:
			s.Failed++
			continue
		}

		switch j.Phase {
		case "first_boot":
			s.FirstBootMs = j.WallMs - j.ExecMs
			sumBootMs += j.WallMs - j.ExecMs
		case "cold":
			cold = append(cold, j.WallMs-j.ExecMs)
			coldDelay = append(coldDelay, j.DelayMs)
			sumBootMs += j.WallMs - j.ExecMs
		case "warm":
			warm = append(warm, j.WallMs)
		}
	}
	s.Cold = stats.Summarize(cold)
	s.ColdDelay = stats.Summarize(coldDelay)
	s.Warm = stats.Summarize(warm)

	// A level the run never reached is left out rather than shown as zeros.
	for _, rps := range p.LevelsRPS {
		var jobs []store.Job
		for _, j := range c.Jobs {
			// Level is stored from this same float64, so == is exact.
			if j.Phase == "load" && j.Level == rps {
				jobs = append(jobs, j)
			}
		}
		if len(jobs) > 0 {
			s.Levels = append(s.Levels, summarizeLevel(rps, jobs))
		}
	}

	s.CostUSD = stats.AttributedCostUSD(sumExecMs, sumBootMs, coldJobs, p.IdleTimeoutS, c.PriceHr)
	s.CostPer1k = stats.PerThousand(s.CostUSD, s.Completed)
	s.GPUs = summarizeGPUs(c.Jobs, c.PriceHr)
	return s
}

// summarizeGPUs groups COMPLETED jobs with GPU numbers by gpuTypeId; nil
// when none have any. A job with no gpuTypeId borrows one from another job
// of the same worker, else it stays in a "" group.
//
// Cold CUDA init counts only first_boot and cold jobs, each the first on a
// new worker process. "First job per worker ID" would miss most of them,
// because Runpod reuses the worker ID after scale-to-zero.
func summarizeGPUs(jobs []store.Job, priceHr float64) []GPUSummary {
	gpuOf := map[string]string{} // worker ID -> GPU
	for _, j := range jobs {
		if j.GPUTypeID != "" {
			gpuOf[j.WorkerID] = j.GPUTypeID
		}
	}

	type samples struct{ matmul, tflops, coldInit []float64 }
	groups := map[string]*samples{}
	for _, j := range jobs {
		if j.Status != completed || (j.MatmulMs == nil && j.TFLOPS == nil && j.CudaInitMs == nil) {
			continue
		}
		gpu := j.GPUTypeID
		if gpu == "" {
			gpu = gpuOf[j.WorkerID]
		}
		g := groups[gpu]
		if g == nil {
			g = &samples{}
			groups[gpu] = g
		}
		if j.MatmulMs != nil {
			g.matmul = append(g.matmul, *j.MatmulMs)
		}
		if j.TFLOPS != nil {
			g.tflops = append(g.tflops, *j.TFLOPS)
		}
		if j.CudaInitMs != nil && (j.Phase == "first_boot" || j.Phase == "cold") {
			g.coldInit = append(g.coldInit, *j.CudaInitMs)
		}
	}

	var out []GPUSummary // nil with no groups, so the JSON omits it
	for _, gpu := range slices.Sorted(maps.Keys(groups)) {
		g := groups[gpu]
		sum := GPUSummary{
			GPUTypeID:      gpu,
			MatmulMs:       stats.Summarize(g.matmul),
			TFLOPS:         stats.Summarize(g.tflops),
			ColdCudaInitMs: stats.Summarize(g.coldInit),
		}
		// +Inf would make encoding/json fail the whole snapshot.
		if sum.TFLOPS.P50 > 0 {
			sum.USDPerTFLOPHr = priceHr / sum.TFLOPS.P50
		}
		out = append(out, sum)
	}
	return out
}

// summarizeLevel summarizes one load level's jobs. Latency runs from
// IntendedAt, not SentAt, to correct for coordinated omission.
func summarizeLevel(rps float64, jobs []store.Job) LevelSummary {
	lv := LevelSummary{RPS: rps, Sent: len(jobs)}
	var latency, delay []float64
	var firstIntended, lastDone time.Time

	for _, j := range jobs {
		// The window starts at the first due send over all jobs: starting at
		// the first completed one would inflate throughput when early jobs failed.
		if firstIntended.IsZero() || j.IntendedAt.Before(firstIntended) {
			firstIntended = j.IntendedAt
		}
		switch j.Status {
		case completed:
			lv.Completed++
		case jobCancelled:
			lv.Unfinished++
			continue
		default:
			lv.Failed++
			continue
		}
		done := j.SentAt.Add(msToDuration(j.WallMs))
		latency = append(latency, durationToMs(done.Sub(j.IntendedAt)))
		delay = append(delay, j.DelayMs)

		if done.After(lastDone) {
			lastDone = done
		}
	}
	lv.Latency = stats.Summarize(latency)
	lv.Delay = stats.Summarize(delay)

	// No completed jobs gives a negative window and 0, not NaN/Inf.
	if window := lastDone.Sub(firstIntended).Seconds(); window > 0 {
		lv.ThroughputRPS = float64(lv.Completed) / window
	}
	return lv
}

// durationToMs keeps sub-millisecond precision, unlike d.Milliseconds().
func durationToMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func msToDuration(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }
