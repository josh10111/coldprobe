package bench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/store"
)

// Event is one live update about a run, for the UI. Type and Data:
//
//	run_status     {status, error?}
//	config_status  {status, detail?}
//	job            a store.Job
//	cost           {boundUsd, capUsd}: the budget guard's upper bound, not the spend
//	log            {msg, level?}: level "error" needs the user's attention
//
// ConfigID is set on per-config events. Seq is numbered by the publisher.
type Event struct {
	RunID    string    `json:"runId"`
	Seq      int64     `json:"seq"`
	TS       time.Time `json:"ts"`
	Type     string    `json:"type"`
	ConfigID int64     `json:"configId,omitempty"`
	Data     any       `json:"data"`
}

// ErrBudget is the cancel cause when the budget guard stops a run.
var ErrBudget = errors.New("budget would be exceeded")

// The other cancel causes, read back with context.Cause.
var (
	errCancelled      = errors.New("run cancelled")
	errConfigDeadline = errors.New("config deadline exceeded")
	// errPhaseTimeout is a sequential job's own timeout. Unlike the other
	// causes it means the job went wrong, so it's recorded as TIMED_OUT.
	errPhaseTimeout = errors.New("phase timeout exceeded")
	// errEndpointGone cancels one config whose endpoint was deleted under
	// it, as shown by a /workers 404.
	errEndpointGone = errors.New("endpoint was deleted outside this run")
)

// errMetricsGone is not a cancel cause: it marks a done config whose
// endpoint vanished during the metrics wait.
var errMetricsGone = errors.New("endpoint deleted before Runpod metrics were available")

const (
	// gpuRefresh is the longest a config's worker -> GPU map is reused.
	gpuRefresh = 10 * time.Second
	// purgeTimeout bounds teardown's purge so it can't eat the DELETE's time.
	purgeTimeout = 5 * time.Second
	// defaultBudgetTick is Runner.BudgetTick's default, also used by EstimateRun.
	defaultBudgetTick = time.Second
	// goneCheckTimeout bounds the /workers call in confirmGone.
	goneCheckTimeout = 10 * time.Second
	// goneCheckEvery is the least time between gone checks per config, and
	// the watchdog's period. With the phase polls that's at most ~18
	// requests/min per config, 72 for four, under the account's 180/min.
	goneCheckEvery = 10 * time.Second
)

// deleteWaits are the pauses between teardown's four DeleteEndpoint tries,
// covering what the transport doesn't retry (a 4xx while workers are busy,
// a short outage). deleteMargin is what a wait always leaves of
// Timeouts.Teardown for the next try. Vars so tests can shrink them.
var (
	deleteWaits  = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
	deleteMargin = 3 * time.Second
)

// restoreWaits are the pauses between tries to restore workers.max after a
// forced pause; the SDK doesn't retry a PATCH on 5xx, and repeating it is
// harmless.
var restoreWaits = []time.Duration{time.Second, 3 * time.Second}

// finishRunWaits are the pauses between FinishRun tries.
var finishRunWaits = []time.Duration{500 * time.Millisecond, 2 * time.Second}

// Runner runs benchmark runs in the background. Zero-valued knobs take the
// production defaults. Set fields before the first method call and never
// after; read settings only through methods, since the first call fills
// in the defaults.
type Runner struct {
	Mgmt  *runpod.Mgmt
	Jobs  *runpod.JobClient
	Store *store.Store

	// Publish receives every Event, from many goroutines. It must not
	// block: it's called mid-measurement. nil means no events.
	Publish func(Event)

	Timeouts Timeouts // zero value: DefaultTimeouts()

	// MaxWorkersLimit is the most workers.max a run may ask for; zero means
	// DefaultMaxWorkersLimit.
	MaxWorkersLimit int

	// Log is for what the operator must see with no UI open. nil means slog.Default().
	Log *slog.Logger

	// Polling knobs. Zero means the default in brackets; tests shrink them.
	WorkerPoll  time.Duration // /workers poll while waiting for scale-to-zero [5 s]
	BudgetTick  time.Duration // budget check, local math only [1 s]
	MetricsPoll time.Duration // /metrics poll before teardown [15 s]
	MetricsWait time.Duration // longest wait for Runpod's metrics to catch up [90 s]
	RunSyncWait time.Duration // runsync ?wait= [60 s]; a 300 s wait got a dropped response
	// ProgressEvery is how often a "still waiting" line repeats [30 s].
	ProgressEvery time.Duration

	// mu guards cancels.
	mu       sync.Mutex
	cancels  map[string]context.CancelCauseFunc // run ID -> its cancel
	wg       sync.WaitGroup
	defaults sync.Once
}

// setDefaults fills every zero knob with its production default, once,
// before any run goroutine exists.
func (r *Runner) setDefaults() {
	if r.Timeouts == (Timeouts{}) {
		r.Timeouts = DefaultTimeouts()
	}
	// Filled on its own: tests set the other timeouts and leave this.
	if r.Timeouts.NoWorkerWarn == 0 {
		r.Timeouts.NoWorkerWarn = DefaultTimeouts().NoWorkerWarn
	}
	if r.WorkerPoll == 0 {
		r.WorkerPoll = 5 * time.Second
	}
	if r.BudgetTick == 0 {
		r.BudgetTick = defaultBudgetTick
	}
	if r.MetricsPoll == 0 {
		r.MetricsPoll = 15 * time.Second
	}
	if r.MetricsWait == 0 {
		r.MetricsWait = 90 * time.Second
	}
	if r.RunSyncWait == 0 {
		r.RunSyncWait = 60 * time.Second
	}
	if r.ProgressEvery == 0 {
		r.ProgressEvery = 30 * time.Second
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.MaxWorkersLimit == 0 {
		r.MaxWorkersLimit = DefaultMaxWorkersLimit
	}
}

// Validate is Params.Validate with this Runner's MaxWorkersLimit, so the
// server, Estimate and Start refuse the same params.
func (r *Runner) Validate(p Params, capUSD float64, catalog []runpod.Pool) error {
	r.defaults.Do(r.setDefaults)
	return p.Validate(capUSD, r.MaxWorkersLimit, catalog)
}

// Limits is ParamLimits for this Runner's MaxWorkersLimit.
func (r *Runner) Limits() Limits {
	r.defaults.Do(r.setDefaults)
	return ParamLimits(r.MaxWorkersLimit)
}

// runState is what one run's goroutines share. Only meter changes after
// Start, under its own lock.
type runState struct {
	id   string
	p    Params
	plan []ConfigEstimate // one per config, in endpoint-name order, with its price
	// excluded maps each card to its endpoint's excludedTypes; empty in pool mode.
	excluded map[string][]string
	meter    meter
}

// configRun is one config's state. row and metricsErr belong to the
// config's goroutine; the fields under mu are shared with load sends.
type configRun struct {
	run     *runState
	idx     int          // position in run.plan; part of the endpoint name
	row     store.Config // what's saved to the store
	created time.Time    // just before CreateEndpoint; metrics start here
	span    int          // this endpoint's entry in run.meter
	// metricsErr is why a done config has no Runpod metrics.
	metricsErr error
	// cancel cancels this config only, with errEndpointGone. Set before any job.
	cancel context.CancelCauseFunc

	mu          sync.Mutex
	completed   int               // COMPLETED jobs so far, for the metrics wait
	gpuTypes    map[string]string // worker ID -> GPU type, see gpuType
	gpuFetched  time.Time
	goneChecked time.Time // last confirmGone or watchGone /workers call
}

// Start checks p, records the run and starts it in the background,
// returning the run ID once the row is written. ctx must be the app's root
// context, never a request's. Cancelling it cancels the run; endpoints are
// still deleted, and Wait waits for that. Don't call Start once Wait has begun.
func (r *Runner) Start(ctx context.Context, p Params, capUSD float64, catalog []runpod.Pool) (string, error) {
	// The same path as the confirmed estimate, so the stored EstMaxUSD matches.
	est, err := r.Estimate(p, capUSD, catalog)
	if err != nil {
		return "", err
	}
	// Card mode has no pools; store [] rather than null for the UI.
	if p.Pools == nil {
		p.Pools = []string{}
	}
	params, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	// The plan carries prices and exclusions, so the catalog is never re-read.
	rs := &runState{id: newRunID(), p: p, plan: est.Configs, excluded: excludedTypes(p, catalog)}
	run := store.Run{
		ID: rs.id, CreatedAt: time.Now(), Status: "running", Params: params,
		BudgetUSD: p.BudgetUSD, EstMaxUSD: est.MaxSpendUSD,
	}
	if err := r.Store.CreateRun(ctx, run); err != nil {
		return "", err
	}

	// Cancelled by Cancel, the budget guard, or ctx itself.
	runCtx, cancel := context.WithCancelCause(ctx)
	r.mu.Lock()
	if r.cancels == nil {
		r.cancels = map[string]context.CancelCauseFunc{}
	}
	r.cancels[rs.id] = cancel
	r.mu.Unlock()

	r.wg.Go(func() {
		// Unregister, then release runCtx, both after run has read context.Cause.
		defer cancel(nil)
		defer r.unregister(rs.id)
		r.run(runCtx, cancel, rs)
	})
	return rs.id, nil
}

// Estimate checks p and returns the most a run with p could cost, with this
// Runner's timeouts. Start stores the same figure.
func (r *Runner) Estimate(p Params, capUSD float64, catalog []runpod.Pool) (Estimate, error) {
	r.defaults.Do(r.setDefaults)
	if err := r.Validate(p, capUSD, catalog); err != nil {
		return Estimate{}, err
	}
	return EstimateRun(p, r.Timeouts, catalog)
}

// excludedTypes maps each of p's cards to the excludedTypes that pin an
// endpoint to it.
func excludedTypes(p Params, catalog []runpod.Pool) map[string][]string {
	out := map[string][]string{}
	for _, id := range p.Cards {
		pool, _, _ := CardPool(catalog, id)
		out[id] = ExcludedFor(pool, id)
	}
	return out
}

// Cancel cancels a running run and reports whether it was running. It
// doesn't wait for the teardown.
func (r *Runner) Cancel(runID string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[runID]
	r.mu.Unlock()
	if ok {
		cancel(errCancelled)
	}
	return ok
}

// Wait blocks until every run has finished, teardown included.
func (r *Runner) Wait() { r.wg.Wait() }

func (r *Runner) unregister(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, runID)
}

// run is one run, start to finish, on its own goroutine.
func (r *Runner) run(ctx context.Context, cancel context.CancelCauseFunc, rs *runState) {
	// Store writes must survive cancellation: that's when final statuses land.
	bg := context.WithoutCancel(ctx)
	r.emit(rs.id, 0, "run_status", map[string]any{"status": "running"})

	configs, err := r.createConfigs(bg, rs)
	if err != nil {
		r.finishRun(bg, rs.id, "failed", err.Error())
		return
	}

	// The budget guard stops once every config has finished.
	stopBudget := make(chan struct{})
	budgetDone := make(chan struct{})
	go func() {
		defer close(budgetDone)
		r.guardBudget(ctx, cancel, rs, stopBudget)
	}()

	// A config's failure is recorded on its row and never stops the others.
	var wg sync.WaitGroup
	for _, c := range configs {
		wg.Go(func() { r.runConfig(ctx, c) })
	}
	wg.Wait()

	close(stopBudget)
	<-budgetDone // after this, nothing else can cancel ctx with a cause

	status, msg := finalStatus(context.Cause(ctx), configs)
	r.finishRun(bg, rs.id, status, msg)
}

// finalStatus decides how a run ended. cause is nil unless the run was cancelled.
func finalStatus(cause error, configs []*configRun) (status, msg string) {
	switch {
	case errors.Is(cause, ErrBudget):
		return "aborted_budget", cause.Error()
	case cause != nil: // Cancel, or the parent ctx (SIGTERM)
		return "cancelled", cause.Error()
	}
	// A config is cancelled only when cause is set, handled above.
	for _, c := range configs {
		if c.row.Status == "done" {
			return "completed", ""
		}
	}
	return "failed", "every config failed; see each config's error"
}

// finishRun saves the run's final status and publishes it. Saving is
// retried: a row left "running" makes the server refuse every later run
// until a restart.
func (r *Runner) finishRun(ctx context.Context, runID, status, msg string) {
	err := r.Store.FinishRun(ctx, runID, status, msg)
	for _, wait := range finishRunWaits {
		if err == nil {
			break
		}
		time.Sleep(wait)
		err = r.Store.FinishRun(ctx, runID, status, msg)
	}
	if err != nil {
		r.Log.Error("save run status failed; the run stays \"running\" in the database, so new runs get 409 until a restart",
			"run", runID, "status", status, "err", err)
		r.emit(runID, 0, "log", map[string]any{"msg": "save run status: " + err.Error(), "level": "error"})
	}
	data := map[string]any{"status": status}
	if msg != "" {
		data["error"] = msg
	}
	r.emit(runID, 0, "run_status", data)
}

// createConfigs writes one pending config row per pool (or card) x
// flashboot, so even a config whose create fails has a row to show why.
func (r *Runner) createConfigs(ctx context.Context, rs *runState) ([]*configRun, error) {
	var out []*configRun
	for i, plan := range rs.plan {
		c := &configRun{run: rs, idx: i, row: store.Config{
			RunID: rs.id, Pool: plan.Pool, Card: plan.Card, FlashBoot: plan.FlashBoot, PriceHr: plan.PriceHr,
			Status: "pending", CreatedAt: time.Now(),
		}}
		id, err := r.Store.CreateConfig(ctx, c.row)
		if err != nil {
			return nil, err
		}
		c.row.ID = id
		out = append(out, c)
		r.emitStatus(c, "pending", "")
	}
	return out, nil
}

// runConfig runs one config and records how it ended. It is cancelled only
// when its error is the run's own cause; a config that stopped for its own
// reason stays failed. A good measurement whose endpoint couldn't be
// deleted stays done, with the teardown error in Error.
func (r *Runner) runConfig(ctx context.Context, c *configRun) {
	err, teardownErr := r.measure(ctx, c)
	c.row.Status, c.row.Error = "done", ""
	switch {
	case err != nil:
		c.row.Status, c.row.Error = "failed", errors.Join(err, teardownErr).Error()
		if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
			c.row.Status = "cancelled"
		}
	case c.metricsErr != nil || teardownErr != nil:
		c.row.Error = errors.Join(c.metricsErr, teardownErr).Error()
	}
	r.saveConfig(ctx, c)
	r.emitStatus(c, c.row.Status, c.row.Error)
}

// measure creates c's endpoint, runs the protocol on it and deletes it.
// teardownErr is non-nil if the endpoint may still exist, kept apart so a
// good measurement survives a failed delete. Teardown is deferred right
// after the create, so it runs however measure returns, panics included.
func (r *Runner) measure(ctx context.Context, c *configRun) (err, teardownErr error) {
	// Bounded by the same worst case EstimateRun bills for.
	deadline := r.Timeouts.ConfigDeadline(c.run.p)
	ctx, cancel := context.WithTimeoutCause(ctx, deadline, errConfigDeadline)
	defer cancel()
	ctx, c.cancel = context.WithCancelCause(ctx)
	defer c.cancel(nil)

	if err = context.Cause(ctx); err != nil {
		return err, nil
	}
	if err = r.createEndpoint(ctx, c); err != nil {
		return err, nil
	}
	// Teardown uses its own uncancellable context.
	defer func() { teardownErr = r.teardown(ctx, c) }()

	// The watchdog is stopped before teardown's own DELETE makes /workers 404.
	watchCtx, stopWatch := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		r.watchGone(watchCtx, c, c.row.EndpointID)
	}()
	defer func() { stopWatch(); <-watched }()

	// The create ignored cancellation; check before sending any job.
	if err = context.Cause(ctx); err != nil {
		return err, nil
	}
	return r.protocol(ctx, c), nil
}

func (r *Runner) createEndpoint(ctx context.Context, c *configRun) error {
	p := c.run.p
	r.setStatus(ctx, c, "creating")
	// Taken before the call so the budget bound measured from here is an upper bound.
	c.created = time.Now()
	// WithoutCancel: a cancel mid-POST could lose the ID of an endpoint
	// Runpod already created. The client's 60 s timeout still bounds it.
	spec := runpod.EndpointSpec{
		Name:         endpointName(c.run.id, c.idx),
		Image:        p.Image,
		Pool:         c.row.Pool,
		FlashBoot:    c.row.FlashBoot,
		MaxWorkers:   p.MaxWorkers,
		IdleTimeoutS: p.IdleTimeoutS,
		Env:          map[string]string{"MODEL_LOAD_SECONDS": strconv.Itoa(p.ModelLoadS)},
		// Card mode only: nil or empty uses the whole pool.
		ExcludedTypes: c.run.excluded[c.row.Card],
	}
	// Only matmul runs need a CUDA floor.
	if p.MatmulN > 0 {
		spec.MinCudaVersion = matmulMinCuda
	}
	id, err := r.Mgmt.CreateEndpoint(context.WithoutCancel(ctx), spec)
	if err != nil {
		// A timeout or 5xx can hide a created endpoint; the janitor finds it by prefix.
		return fmt.Errorf("create endpoint: %w", err)
	}
	c.row.EndpointID = id
	c.span = c.run.meter.open(c.created, c.row.PriceHr)
	r.setStatus(ctx, c, "running")
	return nil
}

// label names c's GPU for people: the card in card mode, else the pool.
func (c *configRun) label() string {
	if c.row.Card != "" {
		return c.row.Card
	}
	return c.row.Pool
}

// protocol runs the measurements on c's endpoint, in order. A failed job is
// recorded and the protocol goes on; it stops early when ctx is done or the
// first boot never completes.
func (r *Runner) protocol(ctx context.Context, c *configRun) error {
	p, t := c.run.p, r.Timeouts

	// Long waits repeat a "still waiting" line so the UI never sits silent.
	r.logAt(c, logWait, "first boot: waiting for a worker on %s", c.label())
	stop := r.waiting(c, "a worker")
	stopWarn := r.warnIfNoWorker(ctx, c)
	first := r.timedJob(ctx, c, "first_boot", t.FirstBoot)
	stopWarn()
	stop()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	// No worker ever ran the first job; every later phase would time out too.
	if first.Status != completed {
		r.logAt(c, logError, "first boot: %s: %s", first.Status, first.Error)
		return fmt.Errorf("first boot did not complete (%s: %s): no worker ran a job, e.g. the pool has no stock", first.Status, first.Error)
	}
	r.logAt(c, logOK, "first boot: worker %s connected, first request answered in %s", first.WorkerID, secsTenth(first.WallMs))

	for i := range p.ColdStarts {
		sample := fmt.Sprintf("cold sample %d/%d", i+1, p.ColdStarts)
		var err error
		if p.ColdStartMode == ColdForced {
			var restoreErr error
			err, restoreErr = r.forceNewWorker(ctx, c, sample)
			// A cancel wins: a sample on a cancelled ctx would be a junk CANCELLED job.
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if restoreErr != nil {
				return restoreErr
			}
		} else {
			r.logAt(c, logWait, "%s: waiting for workers to scale to zero", sample)
			err = r.waitScaledToZero(ctx, c)
		}
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			// A sample on a warm worker would be a fake cold start. Don't
			// retry either: each try keeps a possibly billing worker up.
			r.logAt(c, logNote, "cold starts %d to %d of %d skipped: %v", i+1, p.ColdStarts, p.ColdStarts, err)
			break
		}
		r.logf(c, "%s: sending", sample)
		stop := r.waiting(c, "a worker")
		j := r.timedJob(ctx, c, "cold", t.Cold)
		stop()
		if err := context.Cause(ctx); err != nil {
			return err
		}
		r.logJob(c, sample, j)
	}

	r.logf(c, "warm: %d sequential requests", p.WarmRequests)
	warmOK := 0
	for range p.WarmRequests {
		if r.timedJob(ctx, c, "warm", t.Warm).Status == completed {
			warmOK++
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
	}
	r.logAt(c, countLevel(warmOK, p.WarmRequests), "warm: %d of %d completed", warmOK, p.WarmRequests)

	for _, rps := range p.LevelsRPS {
		r.loadLevel(ctx, c, rps)
		if err := context.Cause(ctx); err != nil {
			return err
		}
	}

	// Every measurement is done, so nothing in the metrics wait fails the config.
	r.snapshotMetrics(ctx, c)
	return nil
}

// forceNewWorker pauses c's endpoint (workers.max 0), waits for zero, then
// restores workers.max, so the next job needs a new worker.
//
// waitErr means zero may not have been reached: the caller skips the rest
// of the cold samples. restoreErr means max may still be 0: the caller
// fails the config. The restore runs even after a failed pause, which may
// still have reached Runpod; restoring an unpaused endpoint is a no-op.
func (r *Runner) forceNewWorker(ctx context.Context, c *configRun, sample string) (waitErr, restoreErr error) {
	id, n := c.row.EndpointID, c.run.p.MaxWorkers
	r.logAt(c, logWait, "%s: pausing the endpoint to force a new worker (workers.max 0)", sample)
	if waitErr = r.Mgmt.SetMaxWorkers(ctx, id, 0); waitErr != nil {
		c.noteGone(waitErr)
	} else {
		r.logAt(c, logWait, "%s: endpoint paused, waiting for workers to scale to zero", sample)
		waitErr = r.waitScaledToZero(ctx, c)
	}
	r.logf(c, "%s: restoring workers.max to %d", sample, n)
	if err := r.restoreMaxWorkers(ctx, id, n); err != nil {
		if is404(err) {
			// Deleted under us: noteGone cancels the config with errEndpointGone.
			c.noteGone(err)
			return waitErr, err
		}
		restoreErr = fmt.Errorf("%s: could not restore workers.max to %d after pausing the endpoint, so it may still be paused; no more jobs sent: %w", sample, n, err)
		r.logAt(c, logError, "%v", restoreErr)
	}
	return waitErr, restoreErr
}

// restoreMaxWorkers sets workers.max back to n, with retries (not on 404).
// It runs uncancellable, bounded by restoreTimeout, so a cancel during the
// pause can't stop the restore.
func (r *Runner) restoreMaxWorkers(ctx context.Context, id string, n int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	err := r.Mgmt.SetMaxWorkers(ctx, id, n)
	for _, wait := range restoreWaits {
		if err == nil || is404(err) || !waitUntil(ctx, time.Now().Add(wait)) {
			break
		}
		err = r.Mgmt.SetMaxWorkers(ctx, id, n)
	}
	return err
}

// waitScaledToZero polls /workers until no worker is active, for at most
// Timeouts.ScaleDown. The first check is immediate.
func (r *Runner) waitScaledToZero(ctx context.Context, c *configRun) error {
	ctx, cancel := context.WithTimeout(ctx, r.Timeouts.ScaleDown)
	defer cancel()
	defer r.waiting(c, "workers to scale to zero")()
	var lastErr error
	for {
		ws, err := r.Mgmt.Workers(ctx, c.row.EndpointID)
		if err == nil && ws.Active() == 0 {
			return nil
		}
		if err != nil {
			lastErr = err
			c.noteGone(err)
		}
		if !waitUntil(ctx, time.Now().Add(r.WorkerPoll)) {
			return fmt.Errorf("workers still active after %v (last /workers error: %v)", r.Timeouts.ScaleDown, lastErr)
		}
	}
}

// timedJob sends one sequential job, intended now, bounded by the phase
// timeout with errPhaseTimeout as its cause.
func (r *Runner) timedJob(ctx context.Context, c *configRun, phase string, timeout time.Duration) store.Job {
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errPhaseTimeout)
	defer cancel()
	return r.runJob(ctx, c, phase, 0, time.Now())
}

// loadLevel offers rps jobs a second for LevelSeconds (open loop), then
// gives the stragglers up to DrainSeconds. Sends still waiting are
// cancelled and their jobs purged so they can't run during the next level.
func (r *Runner) loadLevel(ctx context.Context, c *configRun, rps float64) {
	p := c.run.p
	level := time.Duration(p.LevelSeconds) * time.Second
	drain := time.Duration(p.DrainSeconds) * time.Second
	sendCtx, cancel := context.WithTimeout(ctx, level+drain)
	defer cancel()

	r.logf(c, "level %v req/s: sending for %d s, then draining up to %d s", rps, p.LevelSeconds, p.DrainSeconds)
	var mu sync.Mutex
	sent, ok := 0, 0
	OpenLoop(sendCtx, rps, level, func(ctx context.Context, intended time.Time) {
		j := r.runJob(ctx, c, "load", rps, intended)
		mu.Lock()
		sent++
		if j.Status == completed {
			ok++
		}
		mu.Unlock()
	})

	if ctx.Err() != nil {
		return // the run is ending; teardown deletes the queue with the endpoint
	}
	r.logAt(c, countLevel(ok, sent), "level %v req/s: %d of %d completed", rps, ok, sent)
	if err := r.Jobs.PurgeQueue(ctx, c.row.EndpointID); err != nil {
		r.logAt(c, logError, "purge queue after the %v req/s level: %v", rps, err)
	}
}

// runJob sends one job, waits for it, and records it. It runs concurrently
// during load levels.
func (r *Runner) runJob(ctx context.Context, c *configRun, phase string, level float64, intended time.Time) store.Job {
	j := store.Job{ConfigID: c.row.ID, Phase: phase, Level: level, IntendedAt: intended, SentAt: time.Now()}
	res, err := r.Jobs.RunSync(ctx, c.row.EndpointID, jobInput(c.run.p), r.RunSyncWait)
	// Measured first, so the lookups and database write never count as latency.
	j.WallMs = float64(time.Since(j.SentAt)) / float64(time.Millisecond)

	j.RunpodJobID, j.WorkerID = res.ID, res.WorkerID
	j.DelayMs, j.ExecMs = float64(res.DelayTime), float64(res.ExecutionTime)
	j.Status, j.Error = jobOutcome(ctx, res, err)
	if j.Status == completed {
		j.MatmulMs, j.TFLOPS, j.CudaInitMs = gpuOutput(res.Output)
	}
	// Only a 404 or 409 on the runsync POST itself (no job ID yet): a /status
	// 404 could mean an unknown job. After jobOutcome, so this job is ERROR.
	if res.ID == "" {
		r.confirmGone(ctx, c, err)
	}
	j.GPUTypeID = r.gpuType(ctx, c, res.WorkerID)

	// WithoutCancel: a job cut short by a cancel must still be saved.
	id, err := r.Store.InsertJob(context.WithoutCancel(ctx), j)
	if err != nil {
		r.logAt(c, logError, "save %s job: %v", phase, err)
	}
	j.ID = id
	if j.Status == completed {
		c.mu.Lock()
		c.completed++
		c.mu.Unlock()
	}
	// UTC only now: UTC() drops the monotonic reading WallMs used.
	j.IntendedAt, j.SentAt = j.IntendedAt.UTC(), j.SentAt.UTC()
	r.emit(c.run.id, c.row.ID, "job", j)
	return j
}

// jobInput is the input every job of a run sends; matmul_n only when on.
func jobInput(p Params) map[string]any {
	input := map[string]any{"sleep_ms": p.JobSleepMs}
	if p.MatmulN > 0 {
		input["matmul_n"] = p.MatmulN
	}
	return input
}

// gpuOutput reads the GPU image's optional numbers from a job's output;
// each is nil when absent, and all are nil for malformed output.
func gpuOutput(raw json.RawMessage) (matmulMs, tflops, cudaInitMs *float64) {
	var out struct {
		MatmulMs   *float64 `json:"matmul_ms"`
		TFLOPS     *float64 `json:"tflops"`
		CudaInitMs *float64 `json:"cuda_init_ms"`
	}
	// Unmarshal may have filled some fields before failing.
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, nil
	}
	return out.MatmulMs, out.TFLOPS, out.CudaInitMs
}

// jobOutcome turns RunSync's result into the job's status and error text:
// Runpod's status on success, TIMED_OUT past the phase timeout, CANCELLED
// when ctx ended for any other reason (kept, so a cancel doesn't drop
// exactly the slow jobs), and ERROR for HTTP or network failures.
func jobOutcome(ctx context.Context, res runpod.JobResult, err error) (status, msg string) {
	switch {
	case err == nil:
		return res.Status, res.Error
	case ctx.Err() != nil:
		msg = err.Error()
		// ctx.Err() only says "context canceled"; the cause says why.
		cause := context.Cause(ctx)
		if !errors.Is(err, cause) {
			msg += " (" + cause.Error() + ")"
		}
		if errors.Is(cause, errPhaseTimeout) {
			return jobTimedOut, msg
		}
		return jobCancelled, msg
	default:
		return jobError, err.Error()
	}
}

// is404 reports whether err is a Runpod API answer with status 404.
func is404(err error) bool {
	var apiErr *runpod.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// isGoneHint reports whether err may mean the endpoint was deleted: 404,
// or the 409 "paused" a deleted endpoint's job API returns. A live endpoint
// at max 0 also answers 409, so it's only a hint. Never for DELETE, where
// 409 means busy.
func isGoneHint(err error) bool {
	var apiErr *runpod.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusConflict)
}

// noteGone cancels c's config with errEndpointGone if err is a 404 from a
// management call on the endpoint itself, so the config stops within
// seconds instead of failing through every remaining phase.
func (c *configRun) noteGone(err error) {
	if is404(err) {
		c.cancel(fmt.Errorf("%w (%v)", errEndpointGone, err))
	}
}

// confirmGone is noteGone for a runsync 404 or 409. Neither is proof on
// its own, so it cancels only if /workers also 404s; otherwise only the
// job is lost. Rate-limited with watchGone via claimGoneCheck, and on ctx
// so a cancel never waits for it.
func (r *Runner) confirmGone(ctx context.Context, c *configRun, err error) {
	if !isGoneHint(err) {
		return
	}
	if !c.claimGoneCheck() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, goneCheckTimeout)
	defer cancel()
	if _, werr := r.Mgmt.Workers(ctx, c.row.EndpointID); is404(werr) {
		c.cancel(fmt.Errorf("%w (%v; confirmed by %v)", errEndpointGone, err, werr))
	}
}

// claimGoneCheck reports whether the caller may make a gone-check /workers
// call now, and records the claim.
func (c *configRun) claimGoneCheck() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.goneChecked) < goneCheckEvery {
		return false
	}
	c.goneChecked = time.Now()
	return true
}

// watchGone is the config's watchdog: every goneCheckEvery it asks /workers
// about endpoint id and cancels the config on a 404; other errors are
// ignored. Needed because a deleted endpoint's runsync can simply hang.
func (r *Runner) watchGone(ctx context.Context, c *configRun, id string) {
	t := time.NewTicker(goneCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !c.claimGoneCheck() {
			continue // confirmGone just checked
		}
		wctx, cancel := context.WithTimeout(ctx, goneCheckTimeout)
		_, err := r.Mgmt.Workers(wctx, id)
		cancel()
		c.noteGone(err)
	}
}

// gpuType returns the GPU model workerID runs on ("" if unknown), from a
// per-config copy of /workers refreshed at most every gpuRefresh: a call
// per job would blow the account's 180 requests/min. The caller that finds
// the copy stale claims the refresh and calls without the lock; others
// read the old copy meanwhile.
func (r *Runner) gpuType(ctx context.Context, c *configRun, workerID string) string {
	if workerID == "" {
		return ""
	}
	c.mu.Lock()
	types := c.gpuTypes
	refresh := time.Since(c.gpuFetched) >= gpuRefresh
	if refresh {
		// Set even on failure, so a failing API isn't hit once per job.
		c.gpuFetched = time.Now()
	}
	c.mu.Unlock()
	if refresh {
		if ws, err := r.Mgmt.Workers(ctx, c.row.EndpointID); err == nil {
			types = ws.GPUTypes
			c.mu.Lock()
			c.gpuTypes = types
			c.mu.Unlock()
		}
	}
	return types[workerID] // a nil map reads as ""
}

// snapshotMetrics saves Runpod's own metrics for c's endpoint, before
// teardown makes /metrics 404. Runpod's buckets lag, so it polls until
// Runpod counts at least our completed requests, for at most MetricsWait.
// Problems are only logged; a 404 stops the wait and sets c.metricsErr.
func (r *Runner) snapshotMetrics(ctx context.Context, c *configRun) {
	ctx, cancel := context.WithTimeout(ctx, r.MetricsWait)
	defer cancel()
	r.logAt(c, logWait, "collecting Runpod metrics (up to %s)", secs(r.MetricsWait))
	stop := r.waiting(c, "Runpod metrics")
	c.mu.Lock()
	want := c.completed
	c.mu.Unlock()

	var (
		last    runpod.Metrics
		have    bool
		lastErr error
	)
	for {
		m, err := r.Mgmt.Metrics(ctx, c.row.EndpointID, c.created)
		if err != nil {
			lastErr = err
			if is404(err) {
				c.metricsErr = errMetricsGone
				break
			}
		} else {
			last, have = m, true
			if requestCount(m) >= want {
				break
			}
		}
		if !waitUntil(ctx, time.Now().Add(r.MetricsPoll)) {
			break
		}
	}
	// Not deferred, so no "still waiting" line lands after the outcome lines.
	stop()

	if c.metricsErr != nil {
		r.logAt(c, logNote, "%v (%v)", c.metricsErr, lastErr)
	}
	if !have {
		if c.metricsErr == nil {
			r.logAt(c, logNote, "no Runpod metrics saved: %v", lastErr)
		}
		return
	}
	if n := requestCount(last); n < want {
		r.logAt(c, logNote, "Runpod metrics count %d requests, we completed %d; saved them anyway", n, want)
	}
	b, err := json.Marshal(last)
	if err != nil {
		r.logAt(c, logError, "encode Runpod metrics: %v", err)
		return
	}
	c.row.RPMetrics = b
}

// requestCount is the number of requests across all of m's buckets.
func requestCount(m runpod.Metrics) int {
	n := 0
	for _, b := range m.Requests {
		n += b.Requests
	}
	return n
}

// teardown deletes c's endpoint and returns an error only if it may still
// exist. It runs uncancellable, bounded by Timeouts.Teardown, since ctx is
// often already cancelled here. It purges the queue first (best effort),
// then tries the delete four times so running jobs can finish; a wait is
// cut short to leave deleteMargin for the next try.
func (r *Runner) teardown(ctx context.Context, c *configRun) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.Timeouts.Teardown)
	defer cancel()
	r.setStatus(ctx, c, "tearing_down")
	r.logf(c, "tearing down")
	id := c.row.EndpointID

	purgeCtx, cancelPurge := context.WithTimeout(ctx, purgeTimeout)
	// A 404 or 409 means the endpoint is likely gone already; DELETE's 404 counts as success.
	if err := r.Jobs.PurgeQueue(purgeCtx, id); err != nil && !isGoneHint(err) {
		r.logAt(c, logError, "purge queue before delete: %v", err)
	}
	cancelPurge()

	deadline, _ := ctx.Deadline() // always set: WithTimeout above
	tries := len(deleteWaits) + 1
	var err error
	for i := range tries {
		if err = r.Mgmt.DeleteEndpoint(ctx, id); err == nil {
			now := time.Now().UTC()
			c.row.DeletedAt = &now
			c.run.meter.close(c.span, now)
			r.logAt(c, logOK, "endpoint deleted")
			return nil
		}
		r.logAt(c, logError, "delete endpoint %s, attempt %d of %d: %v", id, i+1, tries, err)
		if i == len(deleteWaits) {
			break // out of tries
		}
		wait := min(deleteWaits[i], max(0, time.Until(deadline)-deleteMargin))
		if !waitUntil(ctx, time.Now().Add(wait)) {
			break // Timeouts.Teardown is used up
		}
	}
	r.emit(c.run.id, c.row.ID, "log", map[string]any{"level": "error", "msg": fmt.Sprintf(
		"ENDPOINT NOT DELETED: %s may still be running and billing. Delete it in the Runpod console, "+
			"or run `coldprobe janitor -delete` (the next run's start and startup run it too). Last error: %v", id, err)})
	return fmt.Errorf("teardown: endpoint %s not deleted: %w", id, err)
}

// guardBudget stops the run before its cost can pass the budget. Every
// BudgetTick it publishes the meter's bound, then aborts if the bound
// projected one tick plus stopTail ahead passes the budget: live endpoints
// bill until their teardown finishes, so this makes the budget a ceiling.
func (r *Runner) guardBudget(ctx context.Context, cancel context.CancelCauseFunc, rs *runState, stop <-chan struct{}) {
	t := time.NewTicker(r.BudgetTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		bound := rs.meter.upperBoundUSD(now, rs.p.MaxWorkers)
		r.emit(rs.id, 0, "cost", map[string]any{"boundUsd": bound, "capUsd": rs.p.BudgetUSD})
		tail := r.BudgetTick + stopTail(rs.p, r.Timeouts)
		if rs.meter.upperBoundUSD(now.Add(tail), rs.p.MaxWorkers) > rs.p.BudgetUSD {
			cancel(ErrBudget)
			return
		}
	}
}

// meter records when each of a run's endpoints existed, so the budget
// guard can bound spend without asking Runpod.
type meter struct {
	mu    sync.Mutex
	spans []span
}

// span is one endpoint's life. end is zero while it still exists.
type span struct {
	start, end time.Time
	priceHr    float64
}

// open starts a span and returns its index, for close.
func (m *meter) open(start time.Time, priceHr float64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, span{start: start, priceHr: priceHr})
	return len(m.spans) - 1
}

// close ends span i: its endpoint is deleted.
func (m *meter) close(i int, end time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans[i].end = end
}

// upperBoundUSD is the most the run's endpoints could have cost by at,
// each billed for maxWorkers+1 workers until deletion or at. A future at
// projects only the live ones.
func (m *meter) upperBoundUSD(at time.Time, maxWorkers int) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0.0
	for _, s := range m.spans {
		end := s.end
		if end.IsZero() {
			end = at
		}
		total += upperBound(end.Sub(s.start), maxWorkers, s.priceHr)
	}
	return total
}

// setStatus saves c's new status before publishing it, so a snapshot
// fetched after the event is never behind it.
func (r *Runner) setStatus(ctx context.Context, c *configRun, status string) {
	c.row.Status = status
	r.saveConfig(ctx, c)
	r.emitStatus(c, status, "")
}

func (r *Runner) saveConfig(ctx context.Context, c *configRun) {
	if err := r.Store.UpdateConfig(context.WithoutCancel(ctx), c.row); err != nil {
		r.logAt(c, logError, "save config: %v", err)
	}
}

func (r *Runner) emit(runID string, configID int64, typ string, data any) {
	if r.Publish == nil {
		return
	}
	r.Publish(Event{RunID: runID, TS: time.Now().UTC(), Type: typ, ConfigID: configID, Data: data})
}

func (r *Runner) emitStatus(c *configRun, status, detail string) {
	data := map[string]any{"status": status}
	if detail != "" {
		data["detail"] = detail
	}
	r.emit(c.run.id, c.row.ID, "config_status", data)
}

func (r *Runner) logf(c *configRun, format string, args ...any) {
	r.emit(c.run.id, c.row.ID, "log", map[string]any{"msg": fmt.Sprintf(format, args...)})
}

// Log levels the UI colours by: ok green, wait and note amber, error red.
// "warn" is reserved for the no-worker warning, which the UI also toasts.
const (
	logOK    = "ok"
	logWait  = "wait"
	logNote  = "note"
	logError = "error"
)

func (r *Runner) logAt(c *configRun, level, format string, args ...any) {
	r.emit(c.run.id, c.row.ID, "log", map[string]any{"level": level, "msg": fmt.Sprintf(format, args...)})
}

// waiting logs "still waiting for <what>" every ProgressEvery until stop is
// called; stop waits for the goroutine, so no late line lands.
func (r *Runner) waiting(c *configRun, what string) (stop func()) {
	start := time.Now()
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(r.ProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				r.logAt(c, logWait, "still waiting for %s (%s)", what, secs(time.Since(start)))
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// warnIfNoWorker warns once if c's first boot still has no worker placed
// NoWorkerWarn after creation, rather than leaving the user watching
// "still waiting" lines until the 600 s FirstBoot timeout. One /workers
// read at the deadline is enough: a placed worker is still active then.
// A failed read means no warning. stop waits for the goroutine.
func (r *Runner) warnIfNoWorker(ctx context.Context, c *configRun) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		if !waitUntil(ctx, c.created.Add(r.Timeouts.NoWorkerWarn)) {
			return
		}
		ws, err := r.Mgmt.Workers(ctx, c.row.EndpointID)
		// ctx.Err: stop was called while /workers was answering.
		if err != nil || ws.Active() > 0 || ctx.Err() != nil {
			return
		}
		r.emit(c.run.id, c.row.ID, "log", map[string]any{
			"level": "warn", "msg": noWorkerMsg(c.label(), r.Timeouts.NoWorkerWarn)})
	}()
	return func() {
		cancel()
		<-exited
	}
}

// noWorkerMsg is the warning for a first boot with no worker placed on label.
func noWorkerMsg(label string, d time.Duration) string {
	return fmt.Sprintf("No worker placed on %s after %s. The pool may be out of stock; Cancel stops the run.", label, secs(d))
}

func (r *Runner) logJob(c *configRun, what string, j store.Job) {
	if j.Status == completed {
		r.logAt(c, logOK, "%s: done in %s", what, secsTenth(j.WallMs))
		return
	}
	r.logAt(c, logError, "%s: %s: %s", what, j.Status, j.Error)
}

func countLevel(ok, of int) string {
	if ok == of {
		return logOK
	}
	return logNote
}

// secsTenth formats milliseconds as seconds to a tenth, e.g. "12.3 s".
func secsTenth(ms float64) string {
	return fmt.Sprintf("%.1f s", ms/1000)
}

// secs formats d as whole seconds, e.g. "90 s".
func secs(d time.Duration) string {
	return fmt.Sprintf("%.0f s", d.Seconds())
}

// runIDTime is the time layout at the start of a run ID.
const runIDTime = "20060102-150405"

// runIDLen is a run ID's length: runIDTime, "-", 4 hex digits.
const runIDLen = len(runIDTime) + 5

// newRunID returns e.g. "20261107-150405-a1b2": UTC to the second, so IDs
// sort by start time, plus random hex against same-second collisions.
func newRunID() string {
	var b [2]byte
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format(runIDTime) + "-" + hex.EncodeToString(b[:])
}

// endpointName is e.g. "bench-coldprobe-20261107-150405-a1b2-3".
func endpointName(runID string, idx int) string {
	return runpod.NamePrefix + runID + "-" + strconv.Itoa(idx)
}
