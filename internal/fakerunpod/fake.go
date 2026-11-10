// Package fakerunpod is an in-memory fake of Runpod's management and
// Serverless job APIs, for tests and COLDPROBE_FAKE=1 demo mode.
//
// It deliberately doesn't import internal/runpod: it is an independent copy
// of the API contract, so a client bug can't be mirrored into the fake.
//
// One mutex (Fake.mu) guards all state. Per-worker time.AfterFunc timers take
// it too; runsync waits without it. Close stops every timer and wakes every
// waiting handler.
package fakerunpod

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// API behaviour the fake models.
//
// Observed on the live API:
//   - Runsync returns 200 with IN_QUEUE for an unfinished job; ids start with "sync-".
//   - After the idle timeout a worker stays listed as IDLE with a null
//     uptimeSeconds, and the next job boots that same worker again.
//   - After DELETE, /workers and /metrics answer 404; runsync answered 409
//     paused (pausedDetail) or hung (Options.HangDeletedRunsync).
//   - PATCH workers.max 0 stops workers within seconds; after the restore,
//     the next job ran on a new worker with a first-boot delay.
//
// Assumed, not verified: no-stock behaviour, null gpuTypeId while
// INITIALIZING, placement under excludedTypes, instant metrics, 409 paused
// from status and purge-queue on a deleted or paused endpoint, and that a
// pause cancels queued jobs. Lowering max above 0 isn't modelled.
const (
	// defaultRunsyncWait applies when the client sends no ?wait=.
	defaultRunsyncWait = 90 * time.Second
	// The real worker's input caps.
	maxSleepMs = 60000
	maxMatmulN = 8192
	// fakeCudaInitMs is near what a real RTX 4090 measured.
	fakeCudaInitMs = 250.0
)

// fakeTFLOPS is the fp16 matmul rate a few GPUs report; only the order
// matters, so tests can tell GPUs apart. Others report defaultTFLOPS.
var fakeTFLOPS = map[string]float64{
	"NVIDIA RTX A4000": 55, "NVIDIA RTX A4500": 65, "NVIDIA RTX 2000 Ada Generation": 45,
	"NVIDIA RTX A5000": 80, "NVIDIA L4": 70,
}

const defaultTFLOPS = 50.0

// Spec defaults for create fields the body leaves out.
const (
	defaultMaxWorkers  = 3
	defaultIdleTimeout = 10 // seconds
	jobTimeoutMs       = 300000
)

// Worker and job statuses, as the APIs spell them.
const (
	initializing = "INITIALIZING"
	idle         = "IDLE"
	running      = "RUNNING"

	inQueue    = "IN_QUEUE"
	inProgress = "IN_PROGRESS"
	completed  = "COMPLETED"
	failed     = "FAILED"
	cancelled  = "CANCELLED"
)

// Options tunes the simulation. Durations are real wall-clock time.
type Options struct {
	// A worker's first boot costs ImagePull+ColdStart; later boots cost
	// FlashBootColdStart if flashboot isn't OFF, else ColdStart.
	ImagePull, ColdStart, FlashBootColdStart time.Duration
	// IdleUnit is the real length of one idleTimeout "second".
	IdleUnit time.Duration
	// NoStockPools never get a worker, and the catalog reports them NONE.
	NoStockPools []string
	// PlaceAfter > 0 places no worker on an endpoint until that long after
	// its creation; jobs queue meanwhile.
	PlaceAfter time.Duration
	// FailCreateAfter > 0 makes only the Nth create call (1-based) return 400.
	FailCreateAfter int
	// Job429Every > 0 makes every Nth job-API request return 429.
	Job429Every int
	// RejectBusyDelete makes DELETE answer 409 while jobs are queued or
	// running (unverified against the real API).
	RejectBusyDelete bool
	// HangDeletedRunsync makes a deleted endpoint's runsync hang instead of
	// answering 409, so only a /workers 404 shows it's gone.
	HangDeletedRunsync bool
	// Pools is the GPU catalog, one entry per card; empty means DefaultPools().
	Pools []FakePool
}

// FakePool is one card in the fake catalog: GPU model GPUName (the catalog
// id and workers' gpuTypeId) in pool ID. Entries sharing an ID form a pool.
type FakePool struct {
	ID, GPUName string
	// Name is the catalog's short name ("RTX A4000"); "" means GPUName.
	Name       string
	MemoryGB   int
	PricePerHr float64
	// Availability is the GPU's catalog level; "" omits it.
	Availability string
}

// DefaultPools is a snapshot of the real Serverless GPU catalog, cards in
// catalog order; a pool's first card is where workers land. Four cards'
// availability is raised so the default pools start without a stock warning.
func DefaultPools() []FakePool {
	return []FakePool{
		{ID: "ADA_24", GPUName: "NVIDIA GeForce RTX 4090", Name: "RTX 4090", MemoryGB: 24, PricePerHr: 0.89, Availability: "HIGH"},
		{ID: "ADA_32_PRO", GPUName: "NVIDIA GeForce RTX 5090", Name: "RTX 5090", MemoryGB: 32, PricePerHr: 1.19, Availability: "LOW"},
		{ID: "ADA_48_PRO", GPUName: "NVIDIA L40", Name: "L40", MemoryGB: 48, PricePerHr: 0.82, Availability: "NONE"},
		{ID: "ADA_48_PRO", GPUName: "NVIDIA RTX 6000 Ada Generation", Name: "RTX 6000 Ada", MemoryGB: 48, PricePerHr: 0.99, Availability: "NONE"},
		{ID: "ADA_48_PRO", GPUName: "NVIDIA L40S", Name: "L40S", MemoryGB: 48, PricePerHr: 1.09, Availability: "LOW"},
		{ID: "ADA_48_PRO", GPUName: "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 2g.48gb", Name: "PRO 6000 MIG 48GB", MemoryGB: 48, PricePerHr: 1.29, Availability: "LOW"},
		{ID: "ADA_80_PRO", GPUName: "NVIDIA H100 PCIe", Name: "H100 PCIe", MemoryGB: 80, PricePerHr: 2.89, Availability: "NONE"},
		{ID: "ADA_80_PRO", GPUName: "NVIDIA H100 NVL", Name: "H100 NVL", MemoryGB: 94, PricePerHr: 3.19, Availability: "NONE"},
		{ID: "ADA_80_PRO", GPUName: "NVIDIA H100 80GB HBM3", Name: "H100 SXM", MemoryGB: 80, PricePerHr: 3.99, Availability: "LOW"},
		{ID: "AMPERE_16", GPUName: "NVIDIA RTX 2000 Ada Generation", Name: "RTX 2000 Ada", MemoryGB: 16, PricePerHr: 0.24, Availability: "HIGH"},
		{ID: "AMPERE_16", GPUName: "NVIDIA RTX A4000", Name: "RTX A4000", MemoryGB: 16, PricePerHr: 0.25, Availability: "MEDIUM"},
		{ID: "AMPERE_16", GPUName: "NVIDIA RTX A4500", Name: "RTX A4500", MemoryGB: 20, PricePerHr: 0.25, Availability: "LOW"},
		{ID: "AMPERE_16", GPUName: "NVIDIA RTX 4000 Ada Generation", Name: "RTX 4000 Ada", MemoryGB: 20, PricePerHr: 0.28, Availability: "LOW"},
		{ID: "AMPERE_24", GPUName: "NVIDIA RTX A5000", Name: "RTX A5000", MemoryGB: 24, PricePerHr: 0.27, Availability: "MEDIUM"},
		{ID: "AMPERE_24", GPUName: "NVIDIA GeForce RTX 3090", Name: "RTX 3090", MemoryGB: 24, PricePerHr: 0.5, Availability: "HIGH"},
		{ID: "AMPERE_24", GPUName: "NVIDIA L4", Name: "L4", MemoryGB: 24, PricePerHr: 0.59, Availability: "NONE"},
		{ID: "AMPERE_24", GPUName: "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb", Name: "PRO 6000 MIG 24GB", MemoryGB: 24, PricePerHr: 0.69, Availability: "MEDIUM"},
		{ID: "AMPERE_48", GPUName: "NVIDIA A40", Name: "A40", MemoryGB: 48, PricePerHr: 0.59, Availability: "MEDIUM"},
		{ID: "AMPERE_48", GPUName: "NVIDIA RTX A6000", Name: "RTX A6000", MemoryGB: 48, PricePerHr: 0.59, Availability: "NONE"},
		{ID: "AMPERE_80", GPUName: "NVIDIA A100 80GB PCIe", Name: "A100 PCIe", MemoryGB: 80, PricePerHr: 1.79, Availability: "NONE"},
		{ID: "AMPERE_80", GPUName: "NVIDIA A100-SXM4-80GB", Name: "A100 SXM", MemoryGB: 80, PricePerHr: 1.79, Availability: "NONE"},
		{ID: "BLACKWELL_180", GPUName: "NVIDIA B200", Name: "B200", MemoryGB: 180, PricePerHr: 7.99, Availability: "LOW"},
		{ID: "BLACKWELL_288", GPUName: "NVIDIA B300 SXM6 AC", Name: "B300", MemoryGB: 288, PricePerHr: 8.99, Availability: "NONE"},
		{ID: "BLACKWELL_32", GPUName: "NVIDIA RTX PRO 4500 Blackwell", Name: "RTX PRO 4500", MemoryGB: 32, PricePerHr: 0.72, Availability: "NONE"},
		{ID: "BLACKWELL_32", GPUName: "NVIDIA RTX PRO 4500 Blackwell Server Edition", Name: "RTX PRO 4500 SE", MemoryGB: 32, PricePerHr: 0.72, Availability: "NONE"},
		{ID: "BLACKWELL_96", GPUName: "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition", Name: "RTX PRO 6000 MaxQ", MemoryGB: 96, PricePerHr: 0.5, Availability: "NONE"},
		{ID: "BLACKWELL_96", GPUName: "NVIDIA RTX PRO 6000 Blackwell Server Edition", Name: "RTX PRO 6000", MemoryGB: 96, PricePerHr: 2.49, Availability: "LOW"},
		{ID: "BLACKWELL_96", GPUName: "NVIDIA RTX PRO 6000 Blackwell Workstation Edition", Name: "RTX PRO 6000 WK", MemoryGB: 96, PricePerHr: 2.59, Availability: "LOW"},
		{ID: "HOPPER_141", GPUName: "NVIDIA H200", Name: "H200 SXM", MemoryGB: 141, PricePerHr: 5.29, Availability: "LOW"},
	}
}

// DemoOptions has realistic timings for COLDPROBE_FAKE=1.
func DemoOptions() Options {
	return Options{
		ImagePull:          20 * time.Second,
		ColdStart:          8 * time.Second,
		FlashBootColdStart: 1500 * time.Millisecond,
		IdleUnit:           time.Second,
	}
}

// TestOptions keeps the same proportions, scaled down to milliseconds.
func TestOptions() Options {
	return Options{
		ImagePull:          20 * time.Millisecond,
		ColdStart:          30 * time.Millisecond,
		FlashBootColdStart: 5 * time.Millisecond,
		IdleUnit:           5 * time.Millisecond,
	}
}

// Fake is the running fake: two HTTP servers sharing one in-memory state.
// Two because the real APIs are two hosts, and their paths would overlap
// ambiguously in one ServeMux.
type Fake struct {
	opts Options
	mgmt *httptest.Server
	jobs *httptest.Server

	closeOnce sync.Once
	// closing wakes waiting runsync handlers, so Close needn't wait them out.
	closing chan struct{}

	// mu guards every field below and every endpoint, worker and job.
	mu       sync.Mutex
	closed   bool
	lastID   int // last number used in any ID
	creates  int // create calls so far, for FailCreateAfter
	jobCalls int // job-API requests so far, for Job429Every
	// See CatalogCalls and SetCatalogDown.
	catalogCalls int
	catalogDown  bool
	// availability holds SetAvailability's overrides.
	availability map[string]string
	// noPlace holds BlockPlacement's pools.
	noPlace   map[string]bool
	endpoints map[string]*endpoint
	// deleted endpoints answer the job API differently (see noEndpoint).
	deleted map[string]bool
	// calls is every request in arrival order, kept for the fake's lifetime.
	calls []string
}

type endpoint struct {
	id, name, typ, image, pool, flashboot string
	// gpu is the card every worker lands on: the pool's first card not excluded.
	gpu                      string
	excludedTypes            []string
	maxWorkers, idleTimeoutS int
	scaling                  json.RawMessage // echoed back as sent
	createdAt                time.Time

	// pulled is set once a worker has booted: later boots skip ImagePull.
	pulled bool
	// placeArmed is set once holdPlacement has armed its timer.
	placeArmed bool
	workers    []*worker
	queue      []*job          // jobs waiting for a worker, oldest first
	jobs       map[string]*job // every job ever submitted, for /status

	// History for /metrics, oldest first.
	jobEvents  []event
	coldStarts []event
}

type worker struct {
	id, gpuTypeID string
	status        string // initializing, idle or running
	// bootedAt is zero while there's no container (booting or stopped).
	bootedAt time.Time

	// timer is the worker's one pending event: boot done, job done or idle
	// shutdown. Replacing it cancels the old event.
	timer *time.Timer
}

type job struct {
	id, status, workerID string
	sleepMs              int
	matmulN              int    // 0: no matmul, so no GPU keys in the output
	gpu                  string // the worker's GPU, for the output's device
	inputErr             string // non-empty: the worker rejects the input
	submitted            time.Time
	delayMs, execMs      int64
	bootedAt             time.Time // the worker's, for the output
	// done is closed when the job reaches a terminal status.
	done chan struct{}
}

// event is one thing /metrics counts: a finished job or a cold start.
type event struct {
	at      time.Time
	ms      int64 // job: execution time; cold start: its duration
	delayMs int64 // jobs only
	failed  bool  // jobs only
}

// New starts both servers. Call Close when done.
func New(o Options) *Fake {
	// len, not == nil: AddEndpoint needs a first pool.
	if len(o.Pools) == 0 {
		o.Pools = DefaultPools()
	}
	f := &Fake{opts: o, closing: make(chan struct{}), availability: map[string]string{},
		noPlace: map[string]bool{}, endpoints: map[string]*endpoint{}, deleted: map[string]bool{}}

	mgmt := http.NewServeMux()
	mgmt.HandleFunc("POST /v2/serverless", f.createEndpoint)
	mgmt.HandleFunc("GET /v2/serverless", f.listEndpoints)
	mgmt.HandleFunc("PATCH /v2/serverless/{id}", f.updateEndpoint)
	mgmt.HandleFunc("DELETE /v2/serverless/{id}", f.deleteEndpoint)
	mgmt.HandleFunc("GET /v2/serverless/{id}/workers", f.listWorkers)
	mgmt.HandleFunc("GET /v2/serverless/{id}/metrics/{series}", f.metrics)
	mgmt.HandleFunc("GET /v2/catalog/gpus", f.listGPUs)

	jobs := http.NewServeMux()
	jobs.HandleFunc("POST /v2/{id}/runsync", f.runSync)
	jobs.HandleFunc("GET /v2/{id}/status/{jobId}", f.status)
	jobs.HandleFunc("POST /v2/{id}/purge-queue", f.purgeQueue)

	f.mgmt = httptest.NewServer(requireAuth(f.record(mgmt)))
	f.jobs = httptest.NewServer(requireAuth(f.record(f.throttle(jobs))))
	return f
}

// MgmtURL is the base for runpod.NewMgmt (no /v2: the SDK's paths have it).
func (f *Fake) MgmtURL() string { return f.mgmt.URL }

// JobURL is the base for runpod.NewJobClient (with /v2, like the real one).
func (f *Fake) JobURL() string { return f.jobs.URL + "/v2" }

// LiveEndpoints returns the IDs of endpoints not yet deleted, sorted.
func (f *Fake) LiveEndpoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.endpoints))
}

// QueuedJobs returns how many jobs are waiting for a worker, across all
// endpoints, so a test can wait on a runsync reaching the fake.
func (f *Fake) QueuedJobs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ep := range f.endpoints {
		n += len(ep.queue)
	}
	return n
}

// Calls returns every request, oldest first, as "METHOD path"; a PATCH
// also carries its body.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CatalogCalls returns how many catalog requests have arrived, retries included.
func (f *Fake) CatalogCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogCalls
}

// SetCatalogDown makes the catalog answer 503 with Retry-After: 0.
func (f *Fake) SetCatalogDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catalogDown = down
}

// SetAvailability overrides the catalog level for card in pool, or every
// card in pool when card is "". Placement is unchanged.
func (f *Fake) SetAvailability(pool, card, level string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.opts.Pools {
		if p.ID == pool && (card == "" || p.GPUName == card) {
			f.availability[availabilityKey(p.ID, p.GPUName)] = level
		}
	}
}

// BlockPlacement makes pool never get a new worker, while the catalog still
// reports stock, so the runner's no-worker warning can be shown.
func (f *Fake) BlockPlacement(pool string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noPlace[pool] = true
}

// availabilityKey includes the pool: a custom catalog may list a card twice.
func availabilityKey(pool, card string) string { return pool + "/" + card }

// AddEndpoint seeds an endpoint with any name, bypassing the client's prefix
// guard, using the spec defaults and the first pool.
func (f *Fake) AddEndpoint(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	scaling := json.RawMessage(`{"type":"QUEUE_DELAY","queueDelay":4}`)
	pool := f.opts.Pools[0].ID
	ep := f.newEndpoint(name, "QUEUE", "fake/image:latest", pool, "OFF",
		defaultMaxWorkers, defaultIdleTimeout, scaling)
	ep.gpu, _ = f.cardFor(pool, nil)
	return ep.id
}

// Close stops every timer and wakes waiting handlers before closing the
// servers, whose Close waits for in-flight requests. Safe to call twice.
func (f *Fake) Close() {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.closed = true
		for _, ep := range f.endpoints {
			for _, w := range ep.workers {
				w.stop()
			}
		}
		f.mu.Unlock()
		close(f.closing)
		f.mgmt.Close()
		f.jobs.Close()
	})
}

// Simulation. Everything below runs with f.mu held.

// newID returns a fresh ID like "ep-0007"; zero-padding keeps string order.
func (f *Fake) newID(kind string) string {
	f.lastID++
	return fmt.Sprintf("%s-%04d", kind, f.lastID)
}

func (f *Fake) newEndpoint(name, typ, image, pool, flashboot string, maxWorkers, idleTimeoutS int, scaling json.RawMessage) *endpoint {
	if flashboot == "" {
		flashboot = "OFF"
	}
	ep := &endpoint{
		id: f.newID("ep"), name: name, typ: typ, image: image, pool: pool, flashboot: flashboot,
		maxWorkers: maxWorkers, idleTimeoutS: idleTimeoutS, scaling: scaling,
		createdAt: time.Now().UTC(), jobs: map[string]*job{},
	}
	f.endpoints[ep.id] = ep
	return ep
}

// cardFor returns the card an endpoint on pool lands on, or a problem
// detail: unknown pool, an excluded id not in the pool, or every card
// excluded. It reads only f.opts, so it needs no lock.
func (f *Fake) cardFor(pool string, excluded []string) (card, problem string) {
	var cards []string
	for _, p := range f.opts.Pools {
		if p.ID == pool {
			cards = append(cards, p.GPUName)
		}
	}
	if len(cards) == 0 {
		return "", "unknown gpu pool " + strconv.Quote(pool)
	}
	for _, id := range excluded {
		if !slices.Contains(cards, id) {
			return "", "gpu.excludedTypes: " + strconv.Quote(id) + " is not a GPU type in pool " + strconv.Quote(pool)
		}
	}
	for _, c := range cards {
		if !slices.Contains(excluded, c) {
			return c, ""
		}
	}
	return "", "gpu.excludedTypes excludes every GPU type in pool " + strconv.Quote(pool)
}

// after arms w's one timer to run fn in d, replacing any pending event. A
// callback that fired but lost the race for f.mu checks it is still w's
// current timer, so a stale idle shutdown can't kill a now-busy worker.
func (f *Fake) after(w *worker, d time.Duration, fn func()) {
	// After Close, arm nothing, or the timer would outlive the Fake.
	if f.closed {
		return
	}
	w.stop()
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.closed || w.timer != t {
			return
		}
		w.timer = nil
		fn()
	})
	w.timer = t
}

// stop cancels w's pending event, if any.
func (w *worker) stop() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// submit queues a job and lets dispatch place it.
func (f *Fake) submit(ep *endpoint, input json.RawMessage) *job {
	j := &job{id: f.newID("sync"), status: inQueue, submitted: time.Now(), done: make(chan struct{})}
	j.sleepMs, j.matmulN, j.inputErr = parseInput(input)
	ep.jobs[j.id] = j
	ep.queue = append(ep.queue, j)
	f.dispatch(ep)
	return j
}

// parseInput applies the real worker's input checks. A bad input still
// waits for a worker, then fails. The fake acts as the GPU image whenever
// matmul_n > 0.
func parseInput(raw json.RawMessage) (sleepMs, matmulN int, errMsg string) {
	// Fields unmarshal separately so each gets its own message.
	var in struct {
		SleepMs *int            `json:"sleep_ms"`
		MatmulN json.RawMessage `json:"matmul_n"`
	}
	// *int rejects bools, floats and strings, like the worker's isinstance check.
	if json.Unmarshal(raw, &in) != nil || in.SleepMs == nil || *in.SleepMs < 0 || *in.SleepMs > maxSleepMs {
		return 0, 0, fmt.Sprintf("sleep_ms must be an integer between 0 and %d", maxSleepMs)
	}
	if in.MatmulN != nil { // absent means 0
		// A JSON null leaves n nil and fails, as in the worker.
		var n *int
		if json.Unmarshal(in.MatmulN, &n) != nil || n == nil || *n < 0 || *n > maxMatmulN {
			return 0, 0, fmt.Sprintf("matmul_n must be an integer between 0 and %d", maxMatmulN)
		}
		matmulN = *n
	}
	return *in.SleepMs, matmulN, ""
}

// dispatch hands queued jobs to warm idle workers, then boots workers for
// jobs with none on the way. Unlike the real QUEUE_DELAY scaler, it scales
// up at once.
func (f *Fake) dispatch(ep *endpoint) {
	for len(ep.queue) > 0 {
		w := ep.warmIdle()
		if w == nil {
			break
		}
		j := ep.queue[0]
		ep.queue = ep.queue[1:]
		f.startJob(ep, w, j)
	}
	if slices.Contains(f.opts.NoStockPools, ep.pool) || f.noPlace[ep.pool] {
		return
	}
	if f.holdPlacement(ep) {
		return
	}
	// Reuse a stopped worker first: it still counts toward workers.max.
	for ep.count(initializing) < len(ep.queue) {
		w := ep.stopped()
		if w == nil {
			if len(ep.workers) >= ep.maxWorkers {
				break
			}
			w = &worker{id: f.newID("w"), gpuTypeID: ep.gpu}
			ep.workers = append(ep.workers, w)
		}
		f.boot(ep, w)
	}
}

// holdPlacement reports whether ep is still inside Options.PlaceAfter, and
// arms one timer to dispatch ep when the hold ends.
func (f *Fake) holdPlacement(ep *endpoint) bool {
	wait := f.opts.PlaceAfter - time.Since(ep.createdAt)
	if wait <= 0 {
		return false
	}
	if !ep.placeArmed && !f.closed {
		ep.placeArmed = true
		time.AfterFunc(wait, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.closed || f.endpoints[ep.id] != ep {
				return
			}
			f.dispatch(ep)
		})
	}
	return true
}

// boot starts w's container, new or stopped.
func (f *Fake) boot(ep *endpoint, w *worker) {
	w.status = initializing
	boot := f.opts.ImagePull + f.opts.ColdStart
	switch {
	case !ep.pulled:
	case ep.flashboot != "OFF":
		boot = f.opts.FlashBootColdStart
	default:
		boot = f.opts.ColdStart
	}
	f.after(w, boot, func() {
		ep.pulled = true
		w.bootedAt = time.Now()
		ep.coldStarts = append(ep.coldStarts, event{at: w.bootedAt, ms: boot.Milliseconds()})
		f.workerFree(ep, w)
	})
}

// startJob runs j on w. delayTime includes any cold start.
func (f *Fake) startJob(ep *endpoint, w *worker, j *job) {
	w.status = running
	j.status = inProgress
	j.workerID = w.id
	j.gpu = w.gpuTypeID
	j.bootedAt = w.bootedAt
	j.delayMs = time.Since(j.submitted).Milliseconds()
	f.after(w, time.Duration(j.sleepMs)*time.Millisecond, func() {
		j.execMs = int64(j.sleepMs)
		if j.inputErr != "" {
			f.finish(ep, j, failed)
		} else {
			f.finish(ep, j, completed)
		}
		f.workerFree(ep, w)
	})
}

// workerFree gives w the next queued job or starts its idle countdown.
func (f *Fake) workerFree(ep *endpoint, w *worker) {
	w.status = idle
	f.dispatch(ep)
	if w.status == idle {
		// The idle timeout stops the container but keeps the worker listed.
		f.after(w, time.Duration(ep.idleTimeoutS)*f.opts.IdleUnit, func() { w.bootedAt = time.Time{} })
	}
}

// finish moves j to a terminal status and wakes anyone waiting on it.
// Cancelled jobs don't count in /metrics.
func (f *Fake) finish(ep *endpoint, j *job, status string) {
	j.status = status
	close(j.done)
	if status != cancelled {
		ep.jobEvents = append(ep.jobEvents, event{
			at: time.Now(), ms: j.execMs, delayMs: j.delayMs, failed: status == failed,
		})
	}
}

// shutdown stops every worker and cancels every unfinished job.
func (f *Fake) shutdown(ep *endpoint) {
	for _, w := range ep.workers {
		w.stop()
	}
	ep.workers = nil
	ep.queue = nil
	for _, j := range ep.jobs {
		if !terminal(j.status) {
			f.finish(ep, j, cancelled)
		}
	}
}

// up reports whether w's container is running (booted, not stopped).
func (w *worker) up() bool { return !w.bootedAt.IsZero() }

// warmIdle returns an IDLE worker whose container is up, or nil.
func (ep *endpoint) warmIdle() *worker {
	for _, w := range ep.workers {
		if w.status == idle && w.up() {
			return w
		}
	}
	return nil
}

// stopped returns an IDLE worker whose container was stopped, or nil.
func (ep *endpoint) stopped() *worker {
	for _, w := range ep.workers {
		if w.status == idle && !w.up() {
			return w
		}
	}
	return nil
}

func (ep *endpoint) count(status string) int {
	n := 0
	for _, w := range ep.workers {
		if w.status == status {
			n++
		}
	}
	return n
}

func terminal(status string) bool {
	return status == completed || status == failed || status == cancelled
}

// HTTP plumbing.

// requireAuth rejects requests without a Bearer token; any token passes.
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeProblem(w, http.StatusUnauthorized, "Unauthorized", "missing Bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// record appends each request to f.calls, with a PATCH's body.
func (f *Fake) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		line := r.Method + " " + r.URL.Path
		if r.Method == http.MethodPatch {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			line += " " + string(bytes.TrimSpace(b))
		}
		f.mu.Lock()
		f.calls = append(f.calls, line)
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// throttle implements Job429Every before the handler, so a 429'd runsync
// leaves no job behind.
func (f *Fake) throttle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.jobCalls++
		limited := f.opts.Job429Every > 0 && f.jobCalls%f.opts.Job429Every == 0
		f.mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "0")
			writeProblem(w, http.StatusTooManyRequests, "Too Many Requests", "fake rate limit (Job429Every)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON sends v as JSON. Handlers call it after releasing f.mu: a slow
// client can stall the write.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeProblem sends an RFC 9457 problem document, Runpod's error format.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": title, "status": status, "detail": detail})
}

func notFound(w http.ResponseWriter, what string) {
	writeProblem(w, http.StatusNotFound, "Not Found", what+" not found")
}

// pausedDetail is the real job API's text for a deleted endpoint; the fake
// also uses it for a paused one.
const pausedDetail = "Endpoint is paused (max_workers=0). Set max_workers > 0 to accept work."

// noEndpoint answers 409 paused for a deleted endpoint, else 404.
func noEndpoint(w http.ResponseWriter, gone bool) {
	if gone {
		writeProblem(w, http.StatusConflict, "Conflict", pausedDetail)
		return
	}
	notFound(w, "endpoint")
}

// pagination is the single-page answer: the fake never splits a list.
var pagination = map[string]any{"nextCursor": nil, "hasNextPage": false}

// Management API.

// createRequest is the part of CreateEndpointRequest the fake reads.
// Pointers tell "absent" from 0.
type createRequest struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Scaling   json.RawMessage `json:"scaling"`
	Image     string          `json:"image"`
	Flashboot string          `json:"flashboot"`
	GPU       struct {
		Pools               []string `json:"pools"`
		ExcludedTypes       []string `json:"excludedTypes"`
		MinCudaVersion      string   `json:"minCudaVersion"`
		AllowedCudaVersions []string `json:"allowedCudaVersions"`
	} `json:"gpu"`
	Workers struct {
		Max         *int `json:"max"`
		IdleTimeout *int `json:"idleTimeout"`
	} `json:"workers"`
}

// missing lists the required fields the body lacks.
func (c createRequest) missing() []string {
	var out []string
	if c.Name == "" {
		out = append(out, "name")
	}
	if c.Type == "" {
		out = append(out, "type")
	}
	if len(c.Scaling) == 0 || string(c.Scaling) == "null" {
		out = append(out, "scaling")
	}
	if c.Image == "" {
		out = append(out, "image")
	}
	if len(c.GPU.Pools) == 0 {
		out = append(out, "gpu.pools")
	}
	return out
}

func (f *Fake) createEndpoint(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.creates++
	failNow := f.opts.FailCreateAfter > 0 && f.creates == f.opts.FailCreateAfter
	f.mu.Unlock()
	if failNow {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "fake failure (FailCreateAfter)")
		return
	}

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "invalid JSON: "+err.Error())
		return
	}
	if m := req.missing(); len(m) > 0 {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "missing required field(s): "+strings.Join(m, ", "))
		return
	}
	// Accepted, but not modelled: every fake GPU meets any CUDA version.
	if req.GPU.MinCudaVersion != "" && len(req.GPU.AllowedCudaVersions) > 0 {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "gpu.minCudaVersion and a non-empty gpu.allowedCudaVersions are mutually exclusive")
		return
	}
	// coldprobe sends one pool.
	pool := req.GPU.Pools[0]
	gpu, problem := f.cardFor(pool, req.GPU.ExcludedTypes)
	if problem != "" {
		writeProblem(w, http.StatusBadRequest, "Bad Request", problem)
		return
	}

	maxWorkers, idleTimeoutS := defaultMaxWorkers, defaultIdleTimeout
	if req.Workers.Max != nil {
		maxWorkers = *req.Workers.Max
	}
	if req.Workers.IdleTimeout != nil {
		idleTimeoutS = *req.Workers.IdleTimeout
	}
	f.mu.Lock()
	ep := f.newEndpoint(req.Name, req.Type, req.Image, pool, req.Flashboot, maxWorkers, idleTimeoutS, req.Scaling)
	ep.gpu, ep.excludedTypes = gpu, req.GPU.ExcludedTypes
	body := endpointJSON(ep)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, body)
}

// endpointJSON is the spec's Endpoint, with required slices as [].
func endpointJSON(ep *endpoint) map[string]any {
	// excludedTypes is echoed as sent, like the real API, and omitted when empty.
	gpu := map[string]any{"pools": []string{ep.pool}}
	if len(ep.excludedTypes) > 0 {
		gpu["excludedTypes"] = ep.excludedTypes
	}
	return map[string]any{
		"gpu":            gpu,
		"id":             ep.id,
		"name":           ep.name,
		"type":           ep.typ,
		"image":          ep.image,
		"scaling":        ep.scaling,
		"flashboot":      ep.flashboot,
		"timeout":        jobTimeoutMs,
		"workers":        map[string]int{"min": 0, "max": ep.maxWorkers, "idleTimeout": ep.idleTimeoutS},
		"dataCenterIds":  []string{},
		"networkVolumes": []string{},
		"mounts":         map[string][]any{"global": {}, "network": {}},
		"createdAt":      ep.createdAt.Format(time.RFC3339),
	}
}

func (f *Fake) listEndpoints(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	list := []map[string]any{}
	for _, id := range slices.Sorted(maps.Keys(f.endpoints)) {
		list = append(list, endpointJSON(f.endpoints[id]))
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": list, "pagination": pagination})
}

// updateEndpoint changes workers.max; the rest of the body is ignored.
// Max 0 pauses the endpoint: workers stop and are dropped, jobs are
// cancelled, and the image cache is forgotten so the next boot costs a
// first boot, as observed.
func (f *Fake) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workers struct {
			Max *int `json:"max"`
		} `json:"workers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "invalid JSON: "+err.Error())
		return
	}
	if req.Workers.Max != nil && *req.Workers.Max < 0 {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "workers.max must not be negative")
		return
	}
	f.mu.Lock()
	ep := f.endpoints[r.PathValue("id")]
	if ep == nil {
		f.mu.Unlock()
		notFound(w, "endpoint")
		return
	}
	if req.Workers.Max != nil {
		ep.maxWorkers = *req.Workers.Max
		if ep.maxWorkers == 0 {
			f.shutdown(ep)
			ep.pulled = false
		}
		f.dispatch(ep) // a raised max may make room for queued jobs
	}
	body := endpointJSON(ep)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, body)
}

func (f *Fake) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ep := f.endpoints[r.PathValue("id")]
	busy := ep != nil && f.opts.RejectBusyDelete && (len(ep.queue) > 0 || ep.count(running) > 0)
	if ep != nil && !busy {
		f.shutdown(ep)
		delete(f.endpoints, ep.id)
		f.deleted[ep.id] = true
	}
	f.mu.Unlock()
	switch {
	case ep == nil:
		notFound(w, "endpoint")
	case busy:
		writeProblem(w, http.StatusConflict, "Conflict", "endpoint has jobs queued or running (RejectBusyDelete)")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// listWorkers lists every worker, stopped ones included, in summary.total.
func (f *Fake) listWorkers(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ep := f.endpoints[r.PathValue("id")]
	if ep == nil {
		f.mu.Unlock()
		notFound(w, "endpoint")
		return
	}
	workers := []map[string]any{}
	for _, wk := range ep.workers {
		// uptimeSeconds is null without a container.
		var uptime any
		if wk.up() {
			uptime = int(time.Since(wk.bootedAt).Seconds())
		}
		// gpuTypeId is null while INITIALIZING.
		var gpu any
		if wk.status != initializing {
			gpu = wk.gpuTypeID
		}
		workers = append(workers, map[string]any{
			"id": wk.id, "status": wk.status, "isStale": false, "gpuCount": 1, "gpuTypeId": gpu,
			"uptimeSeconds": uptime,
		})
	}
	summary := map[string]int{
		"running": ep.count(running), "idle": ep.count(idle), "initializing": ep.count(initializing),
		"throttled": 0, "unhealthy": 0, "total": len(ep.workers),
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers, "summary": summary, "endpointVersion": 1})
}

// listGPUs is the catalog; query parameters are ignored.
func (f *Fake) listGPUs(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.catalogCalls++
	down := f.catalogDown
	overrides := maps.Clone(f.availability)
	f.mu.Unlock()
	if down {
		w.Header().Set("Retry-After", "0")
		writeProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "fake catalog outage (SetCatalogDown)")
		return
	}
	gpus := []map[string]any{}
	for _, p := range f.opts.Pools {
		g := map[string]any{
			"id": p.GPUName, "name": cmp.Or(p.Name, p.GPUName), "manufacturer": "NVIDIA", "memory": p.MemoryGB,
			"pool": p.ID, "secure": true, "community": false,
			"price":    map[string]float64{"secure": p.PricePerHr, "community": p.PricePerHr, "serverless": p.PricePerHr},
			"maxCount": map[string]int{"secure": 1, "community": 0},
		}
		if level, ok := overrides[availabilityKey(p.ID, p.GPUName)]; ok {
			p.Availability = level
		}
		if slices.Contains(f.opts.NoStockPools, p.ID) {
			p.Availability = "NONE"
		}
		if p.Availability != "" {
			g["availability"] = p.Availability
		}
		gpus = append(gpus, g)
	}
	writeJSON(w, http.StatusOK, map[string]any{"gpus": gpus})
}

// metrics serves /metrics/requests and /metrics/cold-starts in 1-minute
// buckets as a single page. Only interval=1m is supported.
func (f *Fake) metrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("interval") != "1m" {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "the fake only supports interval=1m")
		return
	}
	var since time.Time
	if s := q.Get("startTime"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Bad Request", "startTime: "+err.Error())
			return
		}
		since = t
	}
	series := r.PathValue("series")
	if series != "requests" && series != "cold-starts" {
		notFound(w, "metrics series")
		return
	}

	f.mu.Lock()
	ep := f.endpoints[r.PathValue("id")]
	if ep == nil {
		f.mu.Unlock()
		notFound(w, "endpoint")
		return
	}
	records := []map[string]any{}
	switch series {
	case "requests":
		for _, b := range byMinute(ep.jobEvents, since) {
			var exec, delay []int64
			nFailed := 0
			for _, e := range b {
				exec = append(exec, e.ms)
				delay = append(delay, e.delayMs)
				if e.failed {
					nFailed++
				}
			}
			records = append(records, map[string]any{
				"time": minuteOf(b[0].at), "requests": len(b), "completed": len(b) - nFailed,
				"failed": nFailed, "retries": 0,
				"executionTimeMs": durationStats(exec), "delayTimeMs": durationStats(delay),
			})
		}
	case "cold-starts":
		for _, b := range byMinute(ep.coldStarts, since) {
			var ms []int64
			for _, e := range b {
				ms = append(ms, e.ms)
			}
			records = append(records, map[string]any{
				"time": minuteOf(b[0].at), "count": len(b), "durationMs": durationStats(ms),
			})
		}
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"startTime": since.UTC(), "endTime": time.Now().UTC(), "interval": "1m",
		"records": records, "pagination": pagination,
	})
}

// minuteOf is the start of t's 1-minute bucket, in UTC.
func minuteOf(t time.Time) time.Time { return t.UTC().Truncate(time.Minute) }

// byMinute splits time-ordered events into 1-minute buckets from since.
// Empty minutes get no bucket, as in the real API.
func byMinute(events []event, since time.Time) [][]event {
	var out [][]event
	for _, e := range events {
		m := minuteOf(e.at)
		if m.Before(since) {
			continue
		}
		if len(out) == 0 || !minuteOf(out[len(out)-1][0].at).Equal(m) {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], e)
	}
	return out
}

// durationStats is EndpointDurationStats for one non-empty bucket, with
// nearest-rank percentiles (internal/stats isn't imported, to stay independent).
func durationStats(ms []int64) map[string]any {
	slices.Sort(ms)
	var total int64
	for _, v := range ms {
		total += v
	}
	rank := func(p int) int64 {
		// ceil(p*n/100)-th smallest, in integer math.
		return ms[(p*len(ms)+99)/100-1]
	}
	return map[string]any{
		"min": ms[0], "max": ms[len(ms)-1], "total": total,
		"p70": rank(70), "p90": rank(90), "p95": rank(95), "p98": rank(98),
	}
}

// Job API.

// runSync submits a job and holds the request until it finishes or ?wait=
// ms pass; an unfinished job returns its id and status.
func (f *Fake) runSync(w http.ResponseWriter, r *http.Request) {
	wait := defaultRunsyncWait
	if s := r.URL.Query().Get("wait"); s != "" {
		ms, err := strconv.Atoi(s)
		if err != nil || ms < 0 {
			writeProblem(w, http.StatusBadRequest, "Bad Request", "wait must be a whole number of ms")
			return
		}
		wait = time.Duration(ms) * time.Millisecond
	}
	var body struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "invalid JSON: "+err.Error())
		return
	}

	f.mu.Lock()
	ep := f.endpoints[r.PathValue("id")]
	if ep == nil {
		gone := f.deleted[r.PathValue("id")]
		f.mu.Unlock()
		if gone && f.opts.HangDeletedRunsync {
			select {
			case <-r.Context().Done():
			case <-f.closing:
			}
			return
		}
		noEndpoint(w, gone)
		return
	}
	if ep.maxWorkers == 0 { // paused
		f.mu.Unlock()
		writeProblem(w, http.StatusConflict, "Conflict", pausedDetail)
		return
	}
	j := f.submit(ep, body.Input)
	f.mu.Unlock()

	// Wait without the lock: the timers that move the job along need it.
	select {
	case <-j.done:
	case <-time.After(wait):
	case <-r.Context().Done():
	case <-f.closing:
	}

	f.mu.Lock()
	out := jobJSON(j)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) status(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ep := f.endpoints[r.PathValue("id")]
	if ep == nil {
		gone := f.deleted[r.PathValue("id")]
		f.mu.Unlock()
		noEndpoint(w, gone)
		return
	}
	j := ep.jobs[r.PathValue("jobId")]
	if j == nil {
		f.mu.Unlock()
		notFound(w, "job")
		return
	}
	body := jobJSON(j)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, body)
}

// purgeQueue cancels jobs not yet on a worker. Running jobs carry on.
func (f *Fake) purgeQueue(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ep := f.endpoints[r.PathValue("id")]
	if ep == nil {
		gone := f.deleted[r.PathValue("id")]
		f.mu.Unlock()
		noEndpoint(w, gone)
		return
	}
	n := len(ep.queue)
	for _, j := range ep.queue {
		f.finish(ep, j, cancelled)
	}
	ep.queue = nil
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"removed": n, "status": "completed"})
}

// jobJSON is a job as runsync and status report it. delayTime appears once
// a worker takes it, executionTime and output or error once it finishes.
func jobJSON(j *job) map[string]any {
	out := map[string]any{"id": j.id, "status": j.status}
	if j.workerID != "" {
		out["workerId"] = j.workerID
		out["delayTime"] = j.delayMs
	}
	switch j.status {
	case completed:
		out["executionTime"] = j.execMs
		// booted_at is the worker's boot time, the same for each of its jobs.
		output := map[string]any{
			"slept_ms":  j.sleepMs,
			"booted_at": float64(j.bootedAt.UnixMicro()) / 1e6,
		}
		// An n x n matmul is 2n^3 FLOPs: 2n^3 / (tflops x 1e9) ms.
		if j.matmulN > 0 {
			tflops := fakeTFLOPS[j.gpu]
			if tflops == 0 {
				tflops = defaultTFLOPS
			}
			n := float64(j.matmulN)
			output["matmul_ms"] = 2 * n * n * n / (tflops * 1e9)
			output["tflops"] = tflops
			output["cuda_init_ms"] = fakeCudaInitMs
			output["device"] = j.gpu
		}
		out["output"] = output
	case failed:
		out["executionTime"] = j.execMs
		out["error"] = j.inputErr
	}
	return out
}
