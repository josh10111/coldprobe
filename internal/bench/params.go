// Package bench runs the coldprobe benchmark protocol against Runpod
// Serverless endpoints.
package bench

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/stats"
)

// Limits enforced by Validate, so a typo can't become a long, expensive run.
const (
	// MaxWorkersCeiling is Runpod's self-serve workers.max per endpoint.
	// COLDPROBE_MAX_WORKERS may set a lower ceiling.
	MaxWorkersCeiling      = 60
	DefaultMaxWorkersLimit = MaxWorkersCeiling

	maxRPS        = 20    // 20 req/s is already far past saturation for a few workers on a 500 ms job
	maxJobSleepMs = 60000 // the worker rejects anything above this

	// A level keeps up to rps x min(levelSeconds, runsyncWaitS) runsyncs
	// open; past Runpod's 400 per endpoint, 429s would be blamed on the endpoint.
	maxOpenRunsync = 400
	runsyncWaitS   = 60

	maxMatmulN = 8192 // the worker rejects anything above this
	// matmulMinCuda: the GPU image is built on CUDA 12.8.
	matmulMinCuda = "12.8"
)

// The two worker images: the default only sleeps; the GPU one adds torch for MatmulN.
const (
	syntheticRepo  = "ghcr.io/josh10111/coldprobe-worker"
	SyntheticImage = syntheticRepo + ":latest"
	GPUImage       = "ghcr.io/josh10111/coldprobe-worker-gpu:latest"
)

// FlashBoot settings the management API accepts.
var flashBootValues = []string{"OFF", "FLASHBOOT"}

// Params.ColdStartMode values. ColdNatural waits for scale-to-zero, which
// FlashBoot endpoints often never reach. ColdForced pauses the endpoint
// (workers.max 0) and restores it, so each sample is a new-worker cold
// start. Empty means natural.
const (
	ColdNatural = "natural"
	ColdForced  = "forced"
)

// Params is one benchmark run's input. Validate's errors use the json names.
// A run measures every pool (or, in card mode, card) x flashboot config,
// each on its own temporary endpoint.
type Params struct {
	Image        string    `json:"image"`           // worker image to deploy
	Pools        []string  `json:"pools"`           // GPU pool ids, e.g. AMPERE_16
	Cards        []string  `json:"cards,omitempty"` // card mode: catalog GPU ids, instead of pools
	FlashBoot    []string  `json:"flashboot"`       // OFF and/or FLASHBOOT
	MaxWorkers   int       `json:"maxWorkers"`      // workers.max on each endpoint
	IdleTimeoutS int       `json:"idleTimeoutS"`    // seconds a worker idles before scaling to zero
	ColdStarts   int       `json:"coldStarts"`      // cold-start samples per config
	WarmRequests int       `json:"warmRequests"`    // sequential warm requests per config
	LevelsRPS    []float64 `json:"levelsRps"`       // open-loop arrival rates, req/s
	LevelSeconds int       `json:"levelSeconds"`    // how long each level sends for
	DrainSeconds int       `json:"drainSeconds"`    // how long to wait for a level's stragglers
	JobSleepMs   int       `json:"jobSleepMs"`      // how long each job sleeps on the worker
	ModelLoadS   int       `json:"modelLoadS"`      // simulated model load at worker boot
	BudgetUSD    float64   `json:"budgetUsd"`       // the run aborts when the upper-bound cost passes this
	// ColdStartMode is ColdNatural, ColdForced, or "" (natural).
	ColdStartMode string `json:"coldStartMode,omitempty"`
	// MatmulN > 0 makes every job also time an fp16 NxN matmul (GPU image only).
	MatmulN int `json:"matmulN,omitempty"`
}

// DefaultParams is the UI form's starting point. Pools is empty on purpose:
// the user must pick which GPUs to pay for.
func DefaultParams(image string) Params {
	return Params{
		Image:        image,
		FlashBoot:    []string{"OFF", "FLASHBOOT"},
		MaxWorkers:   3, // Runpod's own default for a new endpoint
		IdleTimeoutS: 5,
		ColdStarts:   3,
		WarmRequests: 10,
		LevelsRPS:    []float64{1, 3, 8},
		LevelSeconds: 30,
		DrainSeconds: 60,
		JobSleepMs:   500,
		ModelLoadS:   5,
		BudgetUSD:    2.00,
		// Explicit, so /api/defaults shows the form which mode it starts in.
		ColdStartMode: ColdNatural,
	}
}

// Limits maps a Params JSON name to its [lo, hi] range, both inclusive.
// "pools" counts pools or cards, "levelsRps" counts levels, and "rps" is
// each level's rate, which must be above lo.
type Limits map[string][2]int

// ParamLimits returns the ranges Validate checks; the UI gets the same table.
func ParamLimits(maxWorkers int) Limits {
	return Limits{
		"maxWorkers":   {1, maxWorkers},
		"idleTimeoutS": {1, 3600},
		"coldStarts":   {1, 10},
		"warmRequests": {1, 50},
		"levelSeconds": {1, 300},
		"drainSeconds": {0, 300},
		"jobSleepMs":   {0, maxJobSleepMs},
		"modelLoadS":   {0, 300},
		"matmulN":      {0, maxMatmulN},
		"pools":        {1, 2},
		"levelsRps":    {1, 3},
		"rps":          {0, maxRPS},
	}
}

// Validate checks p before anything is created or billed, reporting every
// problem at once. capUSD caps the budget, maxWorkers caps workers.max, and
// catalog resolves card mode's cards to pools.
func (p Params) Validate(capUSD float64, maxWorkers int, catalog []runpod.Pool) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	lim := ParamLimits(maxWorkers)

	if strings.TrimSpace(p.Image) == "" {
		add("image is required")
	}
	if len(p.Cards) > 0 {
		for _, err := range cardErrors(p, lim["pools"], catalog) {
			add("%w", err)
		}
	} else {
		if r, n := lim["pools"], len(p.Pools); n < r[0] || n > r[1] {
			add("pools: pick %d or %d, got %d", r[0], r[1], n)
		}
		// A duplicate would create a second identical, billing endpoint.
		if d := firstDuplicate(p.Pools); d != "" {
			add("pools: duplicate %s", d)
		}
	}
	if n := len(p.FlashBoot); n < 1 || n > 2 {
		add("flashboot: pick 1 or 2, got %d", n)
	}
	if d := firstDuplicate(p.FlashBoot); d != "" {
		add("flashboot: duplicate %s", d)
	}
	for _, fb := range p.FlashBoot {
		if fb != flashBootValues[0] && fb != flashBootValues[1] {
			add("flashboot: %q is not one of %v", fb, flashBootValues)
		}
	}
	if r, n := lim["levelsRps"], len(p.LevelsRPS); n < r[0] || n > r[1] {
		add("levelsRps: give %d to %d levels, got %d", r[0], r[1], n)
	}
	levels := make([]string, len(p.LevelsRPS))
	for i, rps := range p.LevelsRPS {
		levels[i] = strconv.FormatFloat(rps, 'g', -1, 64)
		// !(in range) also rejects NaN.
		if r := lim["rps"]; !(rps > float64(r[0]) && rps <= float64(r[1])) {
			add("levelsRps: %v must be above %d and at most %d", rps, r[0], r[1])
		}
		if open := rps * float64(min(p.LevelSeconds, runsyncWaitS)); open > maxOpenRunsync {
			add("levelsRps: %v req/s x %d s (levelSeconds, capped at the %d s runsync wait) keeps up to %v requests open; Runpod allows %d per endpoint",
				rps, min(p.LevelSeconds, runsyncWaitS), runsyncWaitS, open, maxOpenRunsync)
		}
	}
	// The summary groups jobs by level, so a duplicate would show twice.
	if d := firstDuplicate(levels); d != "" {
		add("levelsRps: duplicate %s", d)
	}
	if m := p.ColdStartMode; m != "" && m != ColdNatural && m != ColdForced {
		add("coldStartMode: %q is not one of [%s %s]", m, ColdNatural, ColdForced)
	}
	if !(p.BudgetUSD > 0 && p.BudgetUSD <= capUSD) {
		add("budgetUsd: %v must be above 0 and at most the cap, %v", p.BudgetUSD, capUSD)
	}

	ints := []struct {
		field string
		v     int
	}{
		{"maxWorkers", p.MaxWorkers},
		{"idleTimeoutS", p.IdleTimeoutS},
		{"coldStarts", p.ColdStarts},
		{"warmRequests", p.WarmRequests},
		{"levelSeconds", p.LevelSeconds},
		{"drainSeconds", p.DrainSeconds},
		{"jobSleepMs", p.JobSleepMs},
		{"modelLoadS", p.ModelLoadS},
		{"matmulN", p.MatmulN},
	}
	for _, f := range ints {
		if r := lim[f.field]; f.v < r[0] || f.v > r[1] {
			add("%s: %d must be between %d and %d", f.field, f.v, r[0], r[1])
		}
	}

	// The synthetic image has no torch and would fail every matmul job.
	if img := strings.TrimSpace(p.Image); p.MatmulN > 0 && isSyntheticImage(img) {
		add("matmulN: needs the GPU image %s; %s only sleeps", GPUImage, img)
	}

	return errors.Join(errs...)
}

// cardErrors checks card mode: 1 or 2 cards, no duplicates, not mixed with
// pools, and every card in the catalog.
func cardErrors(p Params, count [2]int, catalog []runpod.Pool) []error {
	var errs []error
	if len(p.Pools) > 0 {
		errs = append(errs, errors.New("cards: pick pools or cards, not both"))
	}
	if n := len(p.Cards); n > count[1] {
		errs = append(errs, fmt.Errorf("cards: pick %d or %d, got %d", count[0], count[1], n))
	}
	if d := firstDuplicate(p.Cards); d != "" {
		errs = append(errs, fmt.Errorf("cards: duplicate %s", d))
	}
	for _, id := range p.Cards {
		if _, _, ok := CardPool(catalog, id); !ok {
			errs = append(errs, fmt.Errorf("cards: unknown card %q (see GET /api/gpus)", id))
		}
	}
	return errs
}

// isSyntheticImage reports whether image is the synthetic repo under any
// tag, digest or neither, but not coldprobe-worker-gpu.
func isSyntheticImage(image string) bool {
	rest, ok := strings.CutPrefix(image, syntheticRepo)
	return ok && (rest == "" || rest[0] == ':' || rest[0] == '@')
}

// firstDuplicate returns the first value that appears twice in vals, or "".
func firstDuplicate(vals []string) string {
	seen := map[string]bool{}
	for _, v := range vals {
		if seen[v] {
			return v
		}
		seen[v] = true
	}
	return ""
}

// Timeouts are the longest each phase may take. They feed the worst-case
// estimate, so they're generous: a real cold start took ~55 s.
type Timeouts struct {
	FirstBoot time.Duration // image pull + boot + first job
	ScaleDown time.Duration // waiting for workers to reach zero before a cold sample
	Cold      time.Duration // one cold-start job
	Warm      time.Duration // one warm job (a warm runsync took ~1.1 s for a 1 s sleep)
	Teardown  time.Duration // deleting the endpoint at the end
	// NoWorkerWarn is how long a first boot may go without a worker before
	// the runner warns the pool may be out of stock. Zero means 90 s.
	NoWorkerWarn time.Duration
}

// DefaultTimeouts are the production values.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		FirstBoot: 600 * time.Second,
		ScaleDown: 180 * time.Second,
		Cold:      300 * time.Second,
		Warm:      60 * time.Second,
		Teardown:  30 * time.Second,
		// A pool with stock places a worker within seconds; one without
		// showed nothing for 300 s.
		NoWorkerWarn: 90 * time.Second,
	}
}

// ConfigDeadline is the longest one config may run if every phase uses its
// whole timeout: the config's ctx deadline and the estimate's billed time.
func (t Timeouts) ConfigDeadline(p Params) time.Duration {
	d := t.FirstBoot
	d += time.Duration(p.ColdStarts) * (t.ScaleDown + t.Cold)
	d += time.Duration(p.WarmRequests) * t.Warm
	perLevel := time.Duration(p.LevelSeconds+p.DrainSeconds) * time.Second
	d += time.Duration(len(p.LevelsRPS)) * perLevel
	return d
}

// restoreTimeout bounds restoring workers.max after a forced pause. It runs
// on an uncancellable ctx, past the config deadline, so stopTail counts it.
const restoreTimeout = 15 * time.Second

// stopTail is how long an endpoint can outlive its config's deadline or a
// cancel. EstimateRun and the budget guard both add it, so they agree.
func stopTail(p Params, t Timeouts) time.Duration {
	if p.ColdStartMode == ColdForced {
		return t.Teardown + restoreTimeout
	}
	return t.Teardown
}

// ConfigEstimate is the worst case for one pool (or card) x flashboot
// config. In card mode Pool is the card's pool and Card the card.
type ConfigEstimate struct {
	Pool      string `json:"pool"`
	Card      string `json:"card,omitempty"`
	FlashBoot string `json:"flashboot"`
	// PriceHr is the pool's highest price, or the card's own in card mode.
	PriceHr float64 `json:"priceHr"`
	// Availability is the catalog's stock level: HIGH, MEDIUM, LOW, NONE or "".
	Availability string  `json:"availability"`
	WorstCaseUSD float64 `json:"worstCaseUsd"`
}

// Estimate is what the UI shows before the user confirms a run.
type Estimate struct {
	Configs      []ConfigEstimate `json:"configs"`
	WorstCaseUSD float64          `json:"worstCaseUsd"` // every config runs to its deadline with all workers up
	CapUSD       float64          `json:"capUsd"`       // the run's budget (Params.BudgetUSD)
	MaxSpendUSD  float64          `json:"maxSpendUsd"`  // min(WorstCaseUSD, CapUSD)
	// GuardStopsAfterS is how long the run can go with every endpoint alive
	// before the budget guard aborts it. The bound grows with wall clock,
	// so this is in effect a time limit.
	GuardStopsAfterS int `json:"guardStopsAfterS"`
}

// EstimateRun computes the most a run could cost: every endpoint billed for
// MaxWorkers+1 workers until its deadline plus stopTail. A pool or card
// missing from catalog is an error rather than a silent $0. MaxSpendUSD is
// a real ceiling because the budget guard aborts on the same bound.
func EstimateRun(p Params, t Timeouts, catalog []runpod.Pool) (Estimate, error) {
	est := Estimate{Configs: []ConfigEstimate{}, CapUSD: p.BudgetUSD}
	billed := t.ConfigDeadline(p) + stopTail(p, t)
	perSecond := 0.0 // the guard's bound with every endpoint alive, $/s

	targets, err := configTargets(p, catalog)
	if err != nil {
		return Estimate{}, err
	}
	for _, target := range targets {
		for _, fb := range p.FlashBoot {
			c := target
			c.FlashBoot = fb
			c.WorstCaseUSD = upperBound(billed, p.MaxWorkers, c.PriceHr)
			est.Configs = append(est.Configs, c)
			est.WorstCaseUSD += c.WorstCaseUSD
			perSecond += upperBound(time.Hour, p.MaxWorkers, c.PriceHr) / 3600
		}
	}
	est.MaxSpendUSD = min(est.WorstCaseUSD, est.CapUSD)
	// The guard aborts a tick plus stopTail early (guardBudget).
	if perSecond > 0 {
		tail := (defaultBudgetTick + stopTail(p, t)).Seconds()
		est.GuardStopsAfterS = max(0, int(p.BudgetUSD/perSecond-tail))
	}
	return est, nil
}

// configTargets is one ConfigEstimate per pool or card, filled from the catalog.
func configTargets(p Params, catalog []runpod.Pool) ([]ConfigEstimate, error) {
	var out []ConfigEstimate
	for _, id := range p.Pools {
		pool, ok := findPool(catalog, id)
		if !ok {
			return nil, fmt.Errorf("estimate: no price for pool %q", id)
		}
		out = append(out, ConfigEstimate{Pool: id, PriceHr: pool.PricePerHr, Availability: pool.Availability})
	}
	for _, id := range p.Cards {
		pool, card, ok := CardPool(catalog, id)
		if !ok {
			return nil, fmt.Errorf("estimate: no price for card %q", id)
		}
		out = append(out, ConfigEstimate{Pool: pool.ID, Card: id, PriceHr: card.PriceHr, Availability: card.Availability})
	}
	return out, nil
}

// upperBound is the most one endpoint can cost over d. It bills max+1
// workers because Runpod was seen briefly running two on a max-1 endpoint.
func upperBound(d time.Duration, maxWorkers int, priceHr float64) float64 {
	return stats.UpperBoundUSD(d, maxWorkers+1, priceHr)
}
