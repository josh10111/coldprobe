package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/josh10111/coldprobe/internal/fakerunpod"
	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/store"
)

// The retry waits are package vars, shrunk once here: tests run in
// parallel and writing them per test would race.
func init() {
	// Long enough that TestFinishRun_FailureIsLoggedAtError can see the retries.
	finishRunWaits = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond}
	deleteWaits = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	deleteMargin = 150 * time.Millisecond
	restoreWaits = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
}

// The production teardown schedule, captured before init shrinks it.
var prodDeleteWaits, prodDeleteMargin = deleteWaits, deleteMargin

// The purge, every wait and the margin fit in Timeouts.Teardown.
func TestTeardownScheduleFitsBudget(t *testing.T) {
	sum := purgeTimeout + prodDeleteMargin
	for _, w := range prodDeleteWaits {
		sum += w
	}
	if budget := DefaultTimeouts().Teardown; sum >= budget {
		t.Errorf("purge + delete waits + margin = %v, want under Timeouts.Teardown (%v)", sum, budget)
	}
}

// The fake's catalog, and the GPU each pool hands out when nothing is excluded.
var (
	testCatalog = fakeCatalog()
	testGPU     = map[string]string{"AMPERE_16": "NVIDIA RTX 2000 Ada Generation", "AMPERE_24": "NVIDIA RTX A5000"}
)

// runParams is a whole protocol scaled down: 2 pools x 2 flashboot = 4
// configs. idleTimeoutS 1 is 5 ms on the fake.
func runParams() Params {
	p := DefaultParams("fake/image:latest")
	p.Pools = []string{"AMPERE_16", "AMPERE_24"}
	p.ColdStarts = 2
	p.WarmRequests = 3
	p.LevelsRPS = []float64{10, 20}
	p.LevelSeconds = 1
	p.DrainSeconds = 2
	p.JobSleepMs = 5
	p.IdleTimeoutS = 1
	p.ModelLoadS = 0
	return p
}

// harness is one fake Runpod, one database and one Runner, plus every event published.
type harness struct {
	fake  *fakerunpod.Fake
	store *store.Store
	r     *Runner

	mu     sync.Mutex
	events []Event
}

// newHarness wires a Runner to a fake. A non-empty mgmtURL replaces the
// fake's management URL.
func newHarness(t *testing.T, opts fakerunpod.Options, mgmtURL func(*fakerunpod.Fake) string) *harness {
	t.Helper()
	h := &harness{fake: fakerunpod.New(opts)}
	t.Cleanup(h.fake.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.store = st

	u := h.fake.MgmtURL()
	if mgmtURL != nil {
		u = mgmtURL(h.fake)
	}
	h.r = &Runner{
		Mgmt:    newMgmt(t, u),
		Jobs:    runpod.NewJobClient(h.fake.JobURL(), testKey, nil),
		Store:   st,
		Publish: h.publish,
		Timeouts: Timeouts{
			FirstBoot: 5 * time.Second,
			ScaleDown: 5 * time.Second,
			Cold:      5 * time.Second,
			Warm:      5 * time.Second,
			Teardown:  5 * time.Second,
		},
		WorkerPoll:  5 * time.Millisecond,
		BudgetTick:  5 * time.Millisecond,
		MetricsPoll: 10 * time.Millisecond,
		MetricsWait: 200 * time.Millisecond,
		RunSyncWait: 2 * time.Second,
	}
	// Runs before the fake and store close, so every teardown can finish.
	t.Cleanup(h.r.Wait)
	return h
}

// publish is the Runner's Publish; it holds its mutex only briefly.
func (h *harness) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, e)
}

func (h *harness) eventsOf(typ string) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Event
	for _, e := range h.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, what, 10*time.Second, cond)
}

func waitForWithin(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runToEnd starts a run, waits for it (teardown included) and returns the stored result.
func (h *harness) runToEnd(t *testing.T, p Params) store.RunDetail {
	t.Helper()
	id, err := h.r.Start(context.Background(), p, 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Wait()
	return h.getRun(t, id)
}

func (h *harness) getRun(t *testing.T, id string) store.RunDetail {
	t.Helper()
	rd, err := h.store.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rd
}

// assertCleanedUp checks no endpoint is left on the fake and every config
// that got one recorded its deletion.
func (h *harness) assertCleanedUp(t *testing.T, rd store.RunDetail) {
	t.Helper()
	if live := h.fake.LiveEndpoints(); len(live) != 0 {
		t.Errorf("live endpoints after the run: %v", live)
	}
	for _, c := range rd.Configs {
		if c.EndpointID != "" && c.DeletedAt == nil {
			t.Errorf("config %d (%s/%s) has endpoint %s but no DeletedAt", c.ID, c.Pool, c.FlashBoot, c.EndpointID)
		}
	}
}

func (h *harness) lastRunStatus() string {
	evs := h.eventsOf("run_status")
	if len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].Data.(map[string]any)["status"].(string)
}

func TestRun_HappyPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	p := runParams()

	rd := h.runToEnd(t, p)

	if rd.Status != "completed" || rd.FinishedAt == nil || rd.Error != "" {
		t.Errorf("run = %s (finished %v, error %q), want completed", rd.Status, rd.FinishedAt, rd.Error)
	}
	if s := h.lastRunStatus(); s != "completed" {
		t.Errorf("last run_status event = %q, want completed", s)
	}
	if len(rd.Configs) != 4 {
		t.Fatalf("got %d configs, want 4", len(rd.Configs))
	}
	for _, c := range rd.Configs {
		if c.Status != "done" || c.Error != "" || c.EndpointID == "" {
			t.Errorf("config %s/%s = %s (error %q, endpoint %q), want done", c.Pool, c.FlashBoot, c.Status, c.Error, c.EndpointID)
		}
		counts := map[string]int{}
		completed := 0
		for _, j := range c.Jobs {
			key := j.Phase
			if j.Phase == "load" {
				key = fmt.Sprintf("load%g", j.Level)
			}
			counts[key]++
			if j.Status == "COMPLETED" {
				completed++
			} else {
				t.Errorf("config %s/%s: %s job %s: %s", c.Pool, c.FlashBoot, j.Phase, j.Status, j.Error)
			}
			if j.GPUTypeID != "" && j.GPUTypeID != testGPU[c.Pool] {
				t.Errorf("config %s: job ran on %q, want %q or unknown", c.Pool, j.GPUTypeID, testGPU[c.Pool])
			}
			if j.RunpodJobID == "" || j.WorkerID == "" || j.WallMs <= 0 || j.SentAt.Before(j.IntendedAt) {
				t.Errorf("config %s: job not fully recorded: %+v", c.Pool, j)
			}
		}
		want := map[string]int{"first_boot": 1, "cold": 2, "warm": 3, "load10": 10, "load20": 20}
		for k, n := range want {
			if counts[k] != n {
				t.Errorf("config %s/%s: %d %s jobs, want %d (all: %v)", c.Pool, c.FlashBoot, counts[k], k, n, counts)
			}
		}
		if len(c.Jobs) > 0 && c.Jobs[0].Phase == "first_boot" && c.Jobs[0].GPUTypeID != testGPU[c.Pool] {
			t.Errorf("config %s: first_boot GPUTypeID = %q, want %q", c.Pool, c.Jobs[0].GPUTypeID, testGPU[c.Pool])
		}
		// The metrics snapshot waited until Runpod counted every completed job.
		var m runpod.Metrics
		if err := json.Unmarshal(c.RPMetrics, &m); err != nil {
			t.Errorf("config %s/%s: RPMetrics %q: %v", c.Pool, c.FlashBoot, c.RPMetrics, err)
		}
		requests := 0
		for _, b := range m.Requests {
			requests += b.Requests
		}
		if requests < completed {
			t.Errorf("config %s/%s: metrics count %d requests, want >= %d completed", c.Pool, c.FlashBoot, requests, completed)
		}
	}
	if len(h.eventsOf("job")) == 0 || len(h.eventsOf("cost")) == 0 {
		t.Errorf("want job and cost events, got %d and %d", len(h.eventsOf("job")), len(h.eventsOf("cost")))
	}
	// Event times are UTC, like the snapshot's.
	for _, e := range h.eventsOf("job") {
		if j := e.Data.(store.Job); j.SentAt.Location() != time.UTC || j.IntendedAt.Location() != time.UTC {
			t.Fatalf("job event times %v, %v: want UTC", j.IntendedAt, j.SentAt)
		}
	}
	h.assertCleanedUp(t, rd)
}

func TestRun_CancelMidRunDeletesAllEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)

	id, err := h.r.Start(context.Background(), runParams(), 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first job event", func() bool { return len(h.eventsOf("job")) > 0 })
	if !h.r.Cancel(id) {
		t.Fatal("Cancel returned false for a running run")
	}
	h.r.Wait()

	rd := h.getRun(t, id)
	if rd.Status != "cancelled" {
		t.Errorf("run status = %s, want cancelled", rd.Status)
	}
	if s := h.lastRunStatus(); s != "cancelled" {
		t.Errorf("last run_status event = %q, want cancelled", s)
	}
	if h.r.Cancel(id) {
		t.Error("Cancel returned true for a finished run")
	}
	assertConfigsCancelled(t, rd, errCancelled.Error())
	h.assertCleanedUp(t, rd)
}

// assertConfigsCancelled checks every config was cancelled with the run's cause.
func assertConfigsCancelled(t *testing.T, rd store.RunDetail, cause string) {
	t.Helper()
	for _, c := range rd.Configs {
		if c.Status != "cancelled" || !strings.HasPrefix(c.Error, cause) {
			t.Errorf("config %s/%s = %s (%q), want cancelled with %q", c.Pool, c.FlashBoot, c.Status, c.Error, cause)
		}
	}
}

// Cancelling Start's ctx is what SIGTERM does. No pool has stock, so every
// config is stuck in its first boot; in-flight jobs must be recorded as
// CANCELLED, and Wait must return well before FirstBoot.
func TestRun_ParentContextCancelDeletesAllEndpoints(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.NoStockPools = []string{"AMPERE_16", "AMPERE_24"}
	h := newHarness(t, opts, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, err := h.r.Start(ctx, runParams(), 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for all four first-boot runsyncs to be queued: a cancel before
	// a job is sent is a different path, and a sleep proved flaky.
	waitFor(t, "four queued first-boot jobs", func() bool { return h.fake.QueuedJobs() == 4 })

	start := time.Now()
	cancel()
	h.r.Wait()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Wait took %v after cancel, want well under the 5 s first-boot timeout", d)
	}

	rd := h.getRun(t, id)
	if rd.Status != "cancelled" {
		t.Errorf("run status = %s, want cancelled", rd.Status)
	}
	for _, c := range rd.Configs {
		if len(c.Jobs) != 1 || c.Jobs[0].Phase != "first_boot" || c.Jobs[0].Status != "CANCELLED" || c.Jobs[0].Error == "" {
			t.Errorf("config %s/%s jobs = %+v, want one CANCELLED first_boot with an error", c.Pool, c.FlashBoot, c.Jobs)
		}
	}
	// A plain cancel of the parent has no cause, so the text is context.Canceled's.
	assertConfigsCancelled(t, rd, context.Canceled.Error())
	h.assertCleanedUp(t, rd)
}

// The budget is a ceiling: the guard aborts on the bound projected through
// a tick and teardown. 4 configs x 3 workers bill ~$0.0022/s, so the
// current bound would pass $0.003 at ~1.35 s; the projection fires at
// ~0.35 s. Each config row's life slightly overstates its endpoint's.
func TestRun_BudgetAbort(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	h.r.Timeouts.Teardown = time.Second
	p := runParams()
	p.BudgetUSD = 0.003

	rd := h.runToEnd(t, p)

	if rd.Status != "aborted_budget" || !strings.Contains(rd.Error, ErrBudget.Error()) {
		t.Errorf("run = %s (error %q), want aborted_budget", rd.Status, rd.Error)
	}
	spent := 0.0
	for _, c := range rd.Configs {
		if c.DeletedAt != nil {
			spent += upperBound(c.DeletedAt.Sub(c.CreatedAt), p.MaxWorkers, c.PriceHr)
		}
	}
	if spent <= 0 || spent > p.BudgetUSD {
		t.Errorf("spent (upper bound, creation to deletion) = $%.6f, want within the $%v budget", spent, p.BudgetUSD)
	}
	costs := h.eventsOf("cost")
	if len(costs) == 0 {
		t.Fatal("no cost events")
	}
	last := costs[len(costs)-1].Data.(map[string]any)
	if last["boundUsd"].(float64) > p.BudgetUSD || last["capUsd"].(float64) != p.BudgetUSD {
		t.Errorf("last cost event = %v, want the current bound still within the budget when the guard fired", last)
	}
	assertConfigsCancelled(t, rd, ErrBudget.Error())
	h.assertCleanedUp(t, rd)
}

// No stock: no worker is ever placed, so only that pool's configs fail.
func TestRun_NoStockPoolFailsOnlyThatConfig(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.NoStockPools = []string{"AMPERE_24"}
	h := newHarness(t, opts, nil)
	// A first boot takes ~50 ms on the fake; 2 s leaves room on a loaded CI runner.
	h.r.Timeouts.FirstBoot = 2 * time.Second

	rd := h.runToEnd(t, runParams())

	if rd.Status != "completed" {
		t.Errorf("run status = %s, want completed (AMPERE_16 finished)", rd.Status)
	}
	for _, c := range rd.Configs {
		switch c.Pool {
		case "AMPERE_24":
			if c.Status != "failed" || !strings.Contains(c.Error, "first boot") {
				t.Errorf("AMPERE_24/%s = %s (%q), want failed on first boot", c.FlashBoot, c.Status, c.Error)
			}
		case "AMPERE_16":
			if c.Status != "done" {
				t.Errorf("AMPERE_16/%s = %s (%q), want done", c.FlashBoot, c.Status, c.Error)
			}
		}
	}
	h.assertCleanedUp(t, rd)
}

// A config that failed for its own reason before a cancel stays failed.
// AMPERE_24 has no stock; a proxy holds every DELETE 500 ms so the cancel
// lands during its teardown, while AMPERE_16 is mid load level.
func TestRun_CancelKeepsEarlierFailures(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.NoStockPools = []string{"AMPERE_24"}
	slowDelete := func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				time.Sleep(500 * time.Millisecond)
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	h := newHarness(t, opts, slowDelete)
	h.r.Timeouts.FirstBoot = 2 * time.Second // see TestRun_NoStockPoolFailsOnlyThatConfig
	p := runParams()
	p.LevelSeconds = 10

	id, err := h.r.Start(context.Background(), p, 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both AMPERE_24 configs tearing down", func() bool {
		n := 0
		for _, e := range h.eventsOf("config_status") {
			if e.Data.(map[string]any)["status"] == "tearing_down" {
				n++
			}
		}
		return n == 2
	})
	h.r.Cancel(id)
	h.r.Wait()

	rd := h.getRun(t, id)
	if rd.Status != "cancelled" {
		t.Errorf("run status = %s, want cancelled", rd.Status)
	}
	for _, c := range rd.Configs {
		switch c.Pool {
		case "AMPERE_24":
			if c.Status != "failed" || !strings.Contains(c.Error, "first boot") {
				t.Errorf("AMPERE_24/%s = %s (%q), want failed on first boot", c.FlashBoot, c.Status, c.Error)
			}
		case "AMPERE_16":
			if c.Status != "cancelled" || c.Error != errCancelled.Error() {
				t.Errorf("AMPERE_16/%s = %s (%q), want cancelled", c.FlashBoot, c.Status, c.Error)
			}
		}
	}
	h.assertCleanedUp(t, rd)
}

// The four creates race, so exactly one config must fail, with no endpoint.
func TestRun_CreateFailureStillTearsDownOthers(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.FailCreateAfter = 2
	h := newHarness(t, opts, nil)

	rd := h.runToEnd(t, runParams())

	if rd.Status != "completed" {
		t.Errorf("run status = %s, want completed", rd.Status)
	}
	failed, done := 0, 0
	for _, c := range rd.Configs {
		switch c.Status {
		case "failed":
			failed++
			if c.EndpointID != "" || c.DeletedAt != nil || !strings.Contains(c.Error, "create") {
				t.Errorf("failed config = %+v, want no endpoint and a create error", c.Config)
			}
		case "done":
			done++
		}
	}
	if failed != 1 || done != 3 {
		t.Errorf("failed, done = %d, %d, want 1, 3", failed, done)
	}
	h.assertCleanedUp(t, rd)
}

func TestRun_RejectsInvalidParams(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	p := runParams()
	p.MaxWorkers = 61 // the default limit is 60

	id, err := h.r.Start(context.Background(), p, 2.00, testCatalog)
	if err == nil || !strings.Contains(err.Error(), "maxWorkers") || id != "" {
		t.Fatalf("Start = %q, %v, want an error naming maxWorkers", id, err)
	}
	h.r.Wait()
	runs, err := h.store.ListRuns(context.Background())
	if err != nil || len(runs) != 0 {
		t.Errorf("runs = %v, %v, want none", runs, err)
	}
	if live := h.fake.LiveEndpoints(); len(live) != 0 {
		t.Errorf("live endpoints = %v, want none created", live)
	}
}

// deleteProxy answers 403 to the first fails DELETEs of each endpoint (-1:
// all). 403 because the transport retries 5xx itself.
func deleteProxy(t *testing.T, fails int) func(*fakerunpod.Fake) string {
	return func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		var mu sync.Mutex
		seen := map[string]int{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				mu.Lock()
				seen[r.URL.Path]++
				n := seen[r.URL.Path]
				mu.Unlock()
				if fails < 0 || n <= fails {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
}

// oneConfig is runParams cut to a single config and level.
func oneConfig() Params {
	p := runParams()
	p.Pools = []string{"AMPERE_16"}
	p.FlashBoot = []string{"OFF"}
	p.LevelsRPS = []float64{10}
	return p
}

// Every phase transition is logged, and long waits repeat a "still waiting" line.
func TestRun_LogLevels(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	h.r.ProgressEvery = 10 * time.Millisecond
	p := oneConfig()
	p.DrainSeconds = 2

	h.runToEnd(t, p)

	want := []struct{ prefix, level string }{
		{"first boot: waiting for a worker on AMPERE_16", "wait"},
		{"still waiting for a worker (", "wait"},
		{"first boot: worker ", "ok"},
		{"cold sample 1/2: waiting for workers to scale to zero", "wait"},
		{"cold sample 1/2: sending", ""},
		{"cold sample 1/2: done in ", "ok"},
		{"warm: 3 of 3 completed", "ok"},
		{"level 10 req/s: ", "ok"},
		{"collecting Runpod metrics (up to ", "wait"},
		{"endpoint deleted", "ok"},
	}
	var got []string
	i := 0
	for _, e := range h.eventsOf("log") {
		d := e.Data.(map[string]any)
		msg, _ := d["msg"].(string)
		level, _ := d["level"].(string)
		got = append(got, level+" | "+msg)
		if i < len(want) && strings.HasPrefix(msg, want[i].prefix) && !strings.Contains(msg, "sending for") {
			if level != want[i].level {
				t.Errorf("%q: level %q, want %q", msg, level, want[i].level)
			}
			i++
		}
	}
	if i < len(want) {
		t.Errorf("missing %q (in order) from the log events:\n%s", want[i].prefix, strings.Join(got, "\n"))
	}
}

func TestRun_PhaseLogsInOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	h.r.ProgressEvery = 10 * time.Millisecond // first boot takes ~50 ms on the fake
	p := oneConfig()
	p.DrainSeconds = 2

	h.runToEnd(t, p)

	var msgs []string
	for _, e := range h.eventsOf("log") {
		msgs = append(msgs, e.Data.(map[string]any)["msg"].(string))
	}
	want := []string{
		"first boot: waiting for a worker on AMPERE_16",
		"still waiting for a worker (",
		"cold sample 1/2: waiting for workers to scale to zero",
		"cold sample 1/2: sending",
		"cold sample 2/2: waiting for workers to scale to zero",
		"cold sample 2/2: sending",
		"warm: 3 sequential requests",
		"level 10 req/s: sending for 1 s, then draining up to 2 s",
		"collecting Runpod metrics (up to ",
		"tearing down",
	}
	// Each want line appears in order, as a prefix; others may sit between.
	i := 0
	for _, m := range msgs {
		if i < len(want) && strings.HasPrefix(m, want[i]) {
			i++
		}
	}
	if i < len(want) {
		t.Errorf("missing %q (in order) from the log events:\n%s", want[i], strings.Join(msgs, "\n"))
	}
}

// Scale-down takes ~100 ms here and a proxy holds each /metrics GET 100 ms,
// several ProgressEvery ticks each.
func TestRun_ScaleDownAndMetricsWaitsRepeat(t *testing.T) {
	t.Parallel()
	slowMetrics := func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/metrics/") {
				time.Sleep(100 * time.Millisecond)
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	h := newHarness(t, fakerunpod.TestOptions(), slowMetrics)
	h.r.ProgressEvery = 10 * time.Millisecond
	h.r.MetricsWait = 2 * time.Second
	p := oneConfig()
	p.ColdStarts = 1
	p.IdleTimeoutS = 20

	h.runToEnd(t, p)

	var msgs []string
	for _, e := range h.eventsOf("log") {
		msgs = append(msgs, e.Data.(map[string]any)["msg"].(string))
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"still waiting for workers to scale to zero (", "still waiting for Runpod metrics ("} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q line in the log events:\n%s", want, all)
		}
	}
}

// Every config status is saved before its event is published.
func TestRun_ConfigStatusIsSavedBeforeItsEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	var (
		mu       sync.Mutex
		seen     []string
		mismatch []string
	)
	h.r.Publish = func(e Event) {
		h.publish(e)
		if e.Type != "config_status" {
			return
		}
		want := e.Data.(map[string]any)["status"].(string)
		rd, err := h.store.GetRun(context.Background(), e.RunID)
		got := fmt.Sprint(err)
		for _, c := range rd.Configs {
			if c.ID == e.ConfigID {
				got = c.Status
			}
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, want)
		if got != want {
			mismatch = append(mismatch, fmt.Sprintf("event %s, row %s", want, got))
		}
	}

	h.runToEnd(t, oneConfig())

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"pending", "creating", "running", "tearing_down", "done"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("config_status events = %v, want %v", seen, want)
	}
	if len(mismatch) > 0 {
		t.Errorf("stored status behind its event: %v", mismatch)
	}
}

// A job past its phase timeout is TIMED_OUT (failed); a load job cut off
// at the drain deadline is CANCELLED (unfinished).
func TestRun_PhaseTimeoutFailsDrainCutoffIsUnfinished(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	h.r.Timeouts.Warm = 100 * time.Millisecond // shorter than every 300 ms job
	p := oneConfig()
	p.JobSleepMs = 300
	p.LevelsRPS = []float64{20}
	p.DrainSeconds = 0 // a job sent near the end of the 1 s level can't finish

	rd := h.runToEnd(t, p)
	if len(rd.Configs) != 1 {
		t.Fatalf("got %d configs, want 1", len(rd.Configs))
	}
	c := rd.Configs[0]

	timedOut, cancelled := 0, 0
	for _, j := range c.Jobs {
		switch {
		case j.Phase == "warm":
			if j.Status != "TIMED_OUT" || !strings.Contains(j.Error, "phase timeout") {
				t.Errorf("warm job = %s (%q), want TIMED_OUT with the phase timeout as the reason", j.Status, j.Error)
			}
			timedOut++
		case j.Phase == "load" && j.Status == "CANCELLED":
			cancelled++
		case j.Status != "COMPLETED":
			t.Errorf("%s job = %s (%q), want COMPLETED", j.Phase, j.Status, j.Error)
		}
	}
	if timedOut != 3 || cancelled == 0 {
		t.Fatalf("%d TIMED_OUT warm jobs and %d CANCELLED load jobs, want 3 and at least 1", timedOut, cancelled)
	}

	s := Summarize(c, p)
	if s.Failed != timedOut || s.Unfinished != cancelled {
		t.Errorf("Failed, Unfinished = %d, %d, want %d, %d", s.Failed, s.Unfinished, timedOut, cancelled)
	}
	h.assertCleanedUp(t, rd)
}

// Three refused DELETEs, then the fourth try deletes it.
func TestRun_TeardownRetriesDelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), deleteProxy(t, 3))

	rd := h.runToEnd(t, oneConfig())

	if rd.Status != "completed" || rd.Configs[0].Status != "done" {
		t.Errorf("run, config = %s, %s, want completed, done", rd.Status, rd.Configs[0].Status)
	}
	h.assertCleanedUp(t, rd)
}

// When the waits would outrun Timeouts.Teardown, the last is cut short so
// the last try still happens: here at ~1.5 s rather than 10.3 s.
// Not parallel: it sets the package vars.
func TestRun_TeardownClampsLastWaitToBudget(t *testing.T) {
	waits, margin := deleteWaits, deleteMargin
	t.Cleanup(func() { deleteWaits, deleteMargin = waits, margin })
	deleteWaits = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 10 * time.Second}
	deleteMargin = 500 * time.Millisecond
	h := newHarness(t, fakerunpod.TestOptions(), deleteProxy(t, 3))
	h.r.Timeouts.Teardown = 2 * time.Second

	rd := h.runToEnd(t, oneConfig())

	if c := rd.Configs[0]; c.Status != "done" || c.Error != "" {
		t.Errorf("config %s (%q), want done with the endpoint deleted on the last try", c.Status, c.Error)
	}
	h.assertCleanedUp(t, rd)
}

// When every delete fails after a good measurement, the config stays done
// and the run completed, but the error names the endpoint, an error log
// says so, and DeletedAt stays nil.
func TestRun_TeardownFailureIsLoudAndJanitorCleansUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), deleteProxy(t, -1))

	rd := h.runToEnd(t, oneConfig())

	c := rd.Configs[0]
	if rd.Status != "completed" || c.Status != "done" || c.DeletedAt != nil ||
		!strings.Contains(c.Error, "not deleted") || !strings.Contains(c.Error, c.EndpointID) {
		t.Errorf("run %s, config %s, DeletedAt %v, error %q; want completed, done, and an error naming endpoint %s",
			rd.Status, c.Status, c.DeletedAt, c.Error, c.EndpointID)
	}
	loud := false
	for _, e := range h.eventsOf("log") {
		d := e.Data.(map[string]any)
		if strings.Contains(d["msg"].(string), "NOT DELETED") && d["level"] == "error" {
			loud = true
		}
	}
	if !loud {
		t.Error("no error-level log event saying the endpoint was NOT DELETED")
	}
	// The endpoint's run is finished in h.store, so the rule deletes it though it's young.
	skip, err := SkipRule(context.Background(), h.store, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Janitor(context.Background(), newMgmt(t, h.fake.MgmtURL()), true, skip); err != nil {
		t.Fatal(err)
	}
	if live := h.fake.LiveEndpoints(); len(live) != 0 {
		t.Errorf("live endpoints after the janitor: %v", live)
	}
}

// RejectBusyDelete refuses DELETE while work is queued or running. Teardown
// purges the queue first, so the retries only need to outlast the running job.
func TestRun_TeardownPurgesQueueAndRetriesBusyDelete(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.RejectBusyDelete = true
	h := newHarness(t, opts, nil)
	p := oneConfig()
	p.MaxWorkers = 1
	p.JobSleepMs = 200 // one worker serves 5 jobs/s; 20 req/s builds a queue
	p.LevelsRPS = []float64{20}
	p.LevelSeconds = 5

	id, err := h.r.Start(context.Background(), p, 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a backlog of queued jobs", func() bool { return h.fake.QueuedJobs() >= 10 })
	h.r.Cancel(id)
	h.r.Wait()

	rd := h.getRun(t, id)
	if c := rd.Configs[0]; strings.Contains(c.Error, "not deleted") {
		t.Errorf("config error = %q, want the endpoint deleted", c.Error)
	}
	h.assertCleanedUp(t, rd)
}

// An endpoint deleted under a run makes runsync 409 "paused" and /workers
// 404. The config must stop at once and say why; its sibling finishes done.
func TestRun_EndpointDeletedMidRunFailsFast(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	p := oneConfig()
	p.Pools = []string{"AMPERE_16", "AMPERE_24"}
	p.LevelSeconds = 5

	id, gone, took := h.deleteMidLoad(t, p)
	// The first 409 triggers the check at once.
	if took > 2*time.Second {
		t.Errorf("config stopped %v after the delete, want well within the 5 s level (and goneCheckEvery)", took)
	}
	h.r.Wait()
	// The error carries both the runsync 409 and the /workers 404.
	h.assertOneGone(t, id, gone, "409", "Endpoint is paused", "404")
	// The purge before the delete gets a 409 too; that's expected.
	for _, e := range h.eventsOf("log") {
		if msg := e.Data.(map[string]any)["msg"].(string); strings.Contains(msg, "purge queue before delete") {
			t.Errorf("log %q: teardown's purge 409 on a gone endpoint shouldn't be logged", msg)
		}
	}
}

// When the deleted endpoint's runsync hangs instead, only the watchdog can
// see it: the config is cancelled, its hung jobs end CANCELLED, and the
// sibling finishes done.
func TestRun_EndpointDeletedWithHungRunsyncsFailsViaWatchdog(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.HangDeletedRunsync = true
	h := newHarness(t, opts, nil)
	p := oneConfig()
	p.Pools = []string{"AMPERE_16", "AMPERE_24"}
	p.LevelSeconds = 16

	id, gone, took := h.deleteMidLoad(t, p)
	if took > goneCheckEvery+3*time.Second {
		t.Errorf("config stopped %v after the delete, want within goneCheckEvery (%v) plus slack", took, goneCheckEvery)
	}
	h.r.Wait()
	h.assertOneGone(t, id, gone, "404")

	var cancelled, errored int
	for _, e := range h.eventsOf("job") {
		j := e.Data.(store.Job)
		if j.ConfigID != gone {
			continue
		}
		switch {
		case j.Status == jobCancelled && strings.Contains(j.Error, errEndpointGone.Error()):
			cancelled++
		case j.Status == jobError:
			errored++
		}
	}
	if cancelled == 0 || errored != 0 {
		t.Errorf("gone config: %d jobs CANCELLED with %q, %d ERROR; want some CANCELLED, none ERROR", cancelled, errEndpointGone, errored)
	}
}

// deleteMidLoad starts p and deletes the endpoint of the first config to
// send a load job. It returns the run ID, that config's ID, and how long
// after the delete it began tearing down.
func (h *harness) deleteMidLoad(t *testing.T, p Params) (id string, gone int64, took time.Duration) {
	t.Helper()
	id, err := h.r.Start(context.Background(), p, 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the load level", func() bool {
		for _, e := range h.eventsOf("job") {
			if e.Data.(store.Job).Phase == "load" {
				gone = e.ConfigID
				return true
			}
		}
		return false
	})
	var ep string
	for _, c := range h.getRun(t, id).Configs {
		if c.ID == gone {
			ep = c.EndpointID
		}
	}
	if ep == "" {
		t.Fatalf("config %d has no endpoint ID", gone)
	}
	if err := newMgmt(t, h.fake.MgmtURL()).DeleteEndpoint(context.Background(), ep); err != nil {
		t.Fatal(err)
	}
	deleted := time.Now()
	waitForWithin(t, "the deleted config's teardown", 30*time.Second, func() bool {
		for _, e := range h.eventsOf("config_status") {
			if e.ConfigID == gone && e.Data.(map[string]any)["status"] == "tearing_down" {
				return true
			}
		}
		return false
	})
	return id, gone, time.Since(deleted)
}

// assertOneGone checks config gone failed with errEndpointGone and every
// string in want, its sibling is done, and nothing was left up.
func (h *harness) assertOneGone(t *testing.T, id string, gone int64, want ...string) {
	t.Helper()
	rd := h.getRun(t, id)
	if rd.Status != "completed" {
		t.Errorf("run %s (%q), want completed: the sibling finished", rd.Status, rd.Error)
	}
	for _, c := range rd.Configs {
		if c.ID != gone {
			if c.Status != "done" || c.Error != "" {
				t.Errorf("sibling config %s (%q), want done, no error", c.Status, c.Error)
			}
			continue
		}
		ok := c.Status == "failed" && strings.Contains(c.Error, errEndpointGone.Error())
		for _, w := range want {
			ok = ok && strings.Contains(c.Error, w)
		}
		if !ok {
			t.Errorf("config %s (%q); want failed with %q and %q", c.Status, c.Error, errEndpointGone, want)
		}
	}
	h.assertCleanedUp(t, rd)
}

// deleteOnFirst deletes the endpoint on the first management GET whose
// path contains part, then forwards that GET, which now 404s.
func deleteOnFirst(t *testing.T, part string) func(*fakerunpod.Fake) string {
	return func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		direct := newMgmt(t, f.MgmtURL())
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, part) {
				once.Do(func() {
					for _, id := range f.LiveEndpoints() {
						if err := direct.DeleteEndpoint(context.Background(), id); err != nil {
							t.Error(err)
						}
					}
				})
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
}

// assertGoneFast checks the run ended well within the phase's 20 s wait,
// failed with errEndpointGone.
func (h *harness) assertGoneFast(t *testing.T, id string, took time.Duration) {
	t.Helper()
	if took > 5*time.Second {
		t.Errorf("run took %v, want well within the 20 s wait it was in", took)
	}
	rd := h.getRun(t, id)
	c := rd.Configs[0]
	if rd.Status != "failed" || c.Status != "failed" || !strings.Contains(c.Error, "deleted outside this run") || !strings.Contains(c.Error, "404") {
		t.Errorf("run %s, config %s (%q); want both failed, saying the endpoint was deleted outside this run, with the 404", rd.Status, c.Status, c.Error)
	}
	h.assertCleanedUp(t, rd)
}

// Deleted while waiting for scale-to-zero: the config stops at once rather
// than polling 404s. The first /workers GET is gpuType's, the next the wait's.
func TestRun_EndpointDeletedDuringScaleDownFailsFast(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), deleteOnFirst(t, "/workers"))
	h.r.Timeouts.ScaleDown = 20 * time.Second
	start := time.Now()
	id, err := h.r.Start(context.Background(), oneConfig(), 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Wait()
	h.assertGoneFast(t, id, time.Since(start))
}

// Deleted while waiting for metrics: the config stays done, the wait stops
// at once, and Error says why there are no metrics.
func TestRun_EndpointDeletedDuringMetricsStaysDone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), deleteOnFirst(t, "/metrics/"))
	h.r.MetricsWait = 5 * time.Second
	id, err := h.r.Start(context.Background(), oneConfig(), 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	var metricsAt time.Time
	waitFor(t, "the metrics wait", func() bool {
		for _, e := range h.eventsOf("log") {
			if strings.HasPrefix(e.Data.(map[string]any)["msg"].(string), "collecting Runpod metrics") {
				metricsAt = time.Now()
				return true
			}
		}
		return false
	})
	h.r.Wait()

	if took := time.Since(metricsAt); took > 2*time.Second {
		t.Errorf("run took %v after the metrics wait began, want well within its 5 s", took)
	}
	rd := h.getRun(t, id)
	c := rd.Configs[0]
	if rd.Status != "completed" || c.Status != "done" || c.Error != errMetricsGone.Error() || len(c.RPMetrics) != 0 {
		t.Errorf("run %s, config %s (%q, rpMetrics %s); want completed, done, %q, no metrics", rd.Status, c.Status, c.Error, c.RPMetrics, errMetricsGone)
	}
	logged := false
	for _, e := range h.eventsOf("log") {
		msg := e.Data.(map[string]any)["msg"].(string)
		logged = logged || (strings.HasPrefix(msg, errMetricsGone.Error()) && strings.Contains(msg, "404"))
	}
	if !logged {
		t.Errorf("no log line starting %q with the 404", errMetricsGone)
	}
	h.assertCleanedUp(t, rd)
}

// One runsync 404 while /workers still answers costs only that job. The
// proxy 404s the 4th runsync, the first warm job.
func TestRun_StrayRunsync404CostsOneJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	target, _ := url.Parse(strings.TrimSuffix(h.fake.JobURL(), "/v2"))
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runsync") {
			mu.Lock()
			n++
			stray := n == 4
			mu.Unlock()
			if stray {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	h.r.Jobs = runpod.NewJobClient(srv.URL+"/v2", testKey, nil)

	rd := h.runToEnd(t, oneConfig())

	c := rd.Configs[0]
	if rd.Status != "completed" || c.Status != "done" || c.Error != "" {
		t.Errorf("run %s, config %s (%q); want completed, done, no error", rd.Status, c.Status, c.Error)
	}
	var errored []store.Job
	for _, e := range h.eventsOf("job") {
		if j := e.Data.(store.Job); j.Status == jobError {
			errored = append(errored, j)
		}
	}
	if len(errored) != 1 || !strings.Contains(errored[0].Error, "404") {
		t.Errorf("ERROR jobs = %+v, want exactly the one 404", errored)
	}
	h.assertCleanedUp(t, rd)
}

func TestMeter_UpperBound(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var m meter
	a := m.open(t0, 0.60)
	m.open(t0, 1.20)
	m.close(a, t0.Add(30*time.Minute)) // deleted: frozen at 30 min

	// At t0+1h, MaxWorkers 1 -> 2 billed: 0.5 h x 2 x $0.60 + 1 h x 2 x $1.20.
	got := m.upperBoundUSD(t0.Add(time.Hour), 1)
	if !near(got, 3.00) {
		t.Errorf("upperBoundUSD = %v, want 3.00", got)
	}
}

func TestEndpointName(t *testing.T) {
	id := newRunID()
	name := endpointName(id, 3)
	// e.g. bench-coldprobe-20261107-150405-a1b2-3: 38 characters.
	if !strings.HasPrefix(name, runpod.NamePrefix+id+"-") || !strings.HasSuffix(name, "-3") || len(name) >= 64 {
		t.Errorf("endpointName = %q (%d chars), want prefix+runID-3 under 64 chars", name, len(name))
	}
	if len(id) != len("20261107-150405-a1b2") {
		t.Errorf("newRunID = %q, want YYYYMMDD-HHMMSS-xxxx", id)
	}
}

// A cancel while CreateEndpoint is in flight must not orphan the endpoint.
// A proxy holds every create response 300 ms after the fake created it.
func TestRun_CancelDuringCreateStillDeletes(t *testing.T) {
	t.Parallel()
	slowCreate := func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ModifyResponse = func(res *http.Response) error {
			if res.Request.Method == http.MethodPost {
				time.Sleep(300 * time.Millisecond)
			}
			return nil
		}
		srv := httptest.NewServer(proxy)
		t.Cleanup(srv.Close)
		return srv.URL
	}
	h := newHarness(t, fakerunpod.TestOptions(), slowCreate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, err := h.r.Start(ctx, runParams(), 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "four endpoints on the fake", func() bool { return len(h.fake.LiveEndpoints()) == 4 })
	cancel() // the four create responses are still held by the proxy
	h.r.Wait()

	rd := h.getRun(t, id)
	if rd.Status != "cancelled" {
		t.Errorf("run status = %s, want cancelled", rd.Status)
	}
	for _, c := range rd.Configs {
		if len(c.Jobs) != 0 {
			t.Errorf("config %s/%s ran %d jobs after the cancel, want none", c.Pool, c.FlashBoot, len(c.Jobs))
		}
	}
	h.assertCleanedUp(t, rd)
}

// A run cancelled on its first event creates no endpoint at all.
func TestRun_CancelBeforeCreateCreatesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	var once sync.Once
	h.r.Publish = func(e Event) {
		once.Do(func() { h.r.Cancel(e.RunID) })
		h.publish(e)
	}

	rd := h.runToEnd(t, runParams())

	if rd.Status != "cancelled" {
		t.Errorf("run status = %s, want cancelled", rd.Status)
	}
	assertConfigsCancelled(t, rd, errCancelled.Error())
	for _, c := range rd.Configs {
		if c.EndpointID != "" {
			t.Errorf("config %s/%s created endpoint %s after the cancel, want none", c.Pool, c.FlashBoot, c.EndpointID)
		}
	}
	if n := len(h.fake.LiveEndpoints()); n != 0 {
		t.Errorf("live endpoints = %d, want 0", n)
	}
}

// Estimate and Start must use the default timeouts before the first Start,
// and be safe to call concurrently (setDefaults writes the fields).
func TestRunner_EstimateUsesDefaultsAndIsRaceFree(t *testing.T) {
	t.Parallel()
	p := runParams()
	want, err := EstimateRun(p, DefaultTimeouts(), testCatalog)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh Runner, no Start yet: Estimate needs no clients.
	got, err := (&Runner{}).Estimate(p, 2.00, testCatalog)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Estimate before any Start = %+v, %v, want %+v (DefaultTimeouts)", got, err, want)
	}

	// Each run is cancelled at once; -race checks the defaults handling.
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	h.r.Timeouts = Timeouts{}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ests []Estimate
		ids  []string
	)
	for range 2 {
		wg.Go(func() {
			id, err := h.r.Start(context.Background(), p, 2.00, testCatalog)
			if err != nil {
				t.Error(err)
				return
			}
			h.r.Cancel(id)
			mu.Lock()
			ids = append(ids, id)
			mu.Unlock()
		})
	}
	for range 4 {
		wg.Go(func() {
			est, err := h.r.Estimate(p, 2.00, testCatalog)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			ests = append(ests, est)
			mu.Unlock()
		})
	}
	wg.Wait()
	h.r.Wait()

	for _, est := range ests {
		if !reflect.DeepEqual(est, want) {
			t.Errorf("concurrent Estimate = %+v, want %+v", est, want)
		}
	}
	for _, id := range ids {
		rd := h.getRun(t, id)
		if rd.EstMaxUSD != want.MaxSpendUSD {
			t.Errorf("run %s EstMaxUSD = %v, want %v (the estimate shown)", id, rd.EstMaxUSD, want.MaxSpendUSD)
		}
		h.assertCleanedUp(t, rd)
	}
}

// A FinishRun that keeps failing is retried, then logged at ERROR. A closed
// database fails at once, so taking at least the sum of finishRunWaits
// proves the retries.
func TestFinishRun_FailureIsLoggedAtError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	var buf bytes.Buffer
	h.r.Log = slog.New(slog.NewTextHandler(&buf, nil))
	h.store.Close()

	start := time.Now()
	h.r.finishRun(context.Background(), "20261107-150405-a1b2", "completed", "")
	took := time.Since(start)

	var waits time.Duration
	for _, w := range finishRunWaits {
		waits += w
	}
	if took < waits {
		t.Errorf("finishRun took %v, want at least %v: it should retry after each of finishRunWaits", took, waits)
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "20261107-150405-a1b2") || !strings.Contains(out, "closed") {
		t.Errorf("log = %q, want an ERROR line naming the run and the store error", out)
	}
	if s := h.lastRunStatus(); s != "completed" {
		t.Errorf("last run_status event = %q, want completed: the UI still learns the outcome", s)
	}
}

// While one /workers refresh is stuck, another job's gpuType returns the old
// copy at once, and the stuck result is still used when it arrives.
func TestGPUType_RefreshDoesNotBlockOtherSends(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"workers":[{"id":"w1","status":"RUNNING","gpuTypeId":"G1","gpuCount":1,"uptimeSeconds":5}],
			"summary":{"running":1,"idle":0,"initializing":0,"throttled":0,"unhealthy":0,"total":1},"endpointVersion":1}`)
	}))
	defer ts.Close()
	r := &Runner{Mgmt: newMgmt(t, ts.URL)}
	c := &configRun{row: store.Config{EndpointID: "ep"}}

	first := make(chan string)
	go func() { first <- r.gpuType(context.Background(), c, "w1") }()
	<-entered

	done := make(chan string, 1)
	go func() { done <- r.gpuType(context.Background(), c, "w1") }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("second gpuType = %q, want \"\" (the copy is still empty)", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("second gpuType blocked behind the first one's /workers call")
	}
	close(release)
	if got := <-first; got != "G1" {
		t.Errorf("first gpuType = %q, want G1 from its own refresh", got)
	}
}

// forcedConfig is oneConfig in forced mode. The idle timeout equals
// ScaleDown, so only the pause can produce the cold samples.
func forcedConfig() Params {
	p := oneConfig()
	p.ColdStartMode = ColdForced
	p.IdleTimeoutS = 1000
	return p
}

// callSeq maps the fake's call log to letters: P pause, R restore, W GET
// /workers, S runsync, D DELETE. Other calls are dropped.
func callSeq(calls []string) string {
	var b strings.Builder
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "PATCH ") && strings.HasSuffix(c, `"max":0}}`):
			b.WriteByte('P')
		case strings.HasPrefix(c, "PATCH "):
			b.WriteByte('R')
		case strings.HasPrefix(c, "GET ") && strings.HasSuffix(c, "/workers"):
			b.WriteByte('W')
		case strings.HasSuffix(c, "/runsync"):
			b.WriteByte('S')
		case strings.HasPrefix(c, "DELETE "):
			b.WriteByte('D')
		}
	}
	return b.String()
}

// Forced mode: each cold sample is preceded by pause, wait, restore, and no
// runsync is sent while paused, so the config never sees the 409 and ends done.
func TestRun_ForcedColdLoopPausesWaitsRestoresThenSends(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	rd := h.runToEnd(t, forcedConfig())

	c := rd.Configs[0]
	if rd.Status != "completed" || c.Status != "done" || c.Error != "" {
		t.Fatalf("run %s, config %s (%q), want completed and done with no error", rd.Status, c.Status, c.Error)
	}
	workers := map[string]bool{}
	cold := 0
	for _, j := range c.Jobs {
		if j.Status != "COMPLETED" {
			t.Errorf("%s job %s: %s", j.Phase, j.Status, j.Error)
		}
		if j.Phase == "first_boot" || j.Phase == "cold" {
			workers[j.WorkerID] = true
		}
		if j.Phase == "cold" {
			cold++
		}
	}
	if cold != 2 || len(workers) != 3 {
		t.Errorf("%d cold jobs on %d distinct workers with the first boot, want 2 cold, each on a new worker (3)", cold, len(workers))
	}

	// Per sample: P, at least one W, R, then S. No S between a P and its R.
	seq := callSeq(h.fake.Calls())
	if n := len(regexp.MustCompile(`PW+RW*S`).FindAllString(seq, -1)); n != 2 || strings.Count(seq, "P") != 2 {
		t.Errorf("call sequence %s: want 2 x pause, wait, restore, send", seq)
	}

	var msgs []string
	for _, e := range h.eventsOf("log") {
		msgs = append(msgs, e.Data.(map[string]any)["msg"].(string))
	}
	want := []string{
		"cold sample 1/2: pausing the endpoint to force a new worker",
		"cold sample 1/2: endpoint paused, waiting for workers to scale to zero",
		"cold sample 1/2: restoring workers.max to 3",
		"cold sample 1/2: sending",
		"cold sample 2/2: pausing the endpoint to force a new worker",
	}
	i := 0
	for _, m := range msgs {
		if i < len(want) && strings.HasPrefix(m, want[i]) {
			i++
		}
	}
	if i < len(want) {
		t.Errorf("missing %q (in order) from the log events:\n%s", want[i], strings.Join(msgs, "\n"))
	}
	h.assertCleanedUp(t, rd)
}

// pauseHooks says what pauseProxy does to the forced-mode PATCHes; zero fields do nothing.
type pauseHooks struct {
	failPause   int    // answer the pause (PATCH max 0) with this status, without forwarding it
	failRestore int    // answer every restore (PATCH max > 0) with this status, without forwarding it
	onWait      func() // called on each GET /workers after a pause attempt
	preRestore  func() // called before a restore is forwarded
	postRestore func() // called after a restore is forwarded and answered
}

// pauseProxy fronts the fake's management API and applies hk. Use 4xx or a
// PATCH 5xx for the fail fields: a GET's 5xx is retried by the transport.
func pauseProxy(t *testing.T, hk pauseHooks) func(*fakerunpod.Fake) string {
	return func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		var mu sync.Mutex
		paused := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				var body bytes.Buffer
				_, _ = body.ReadFrom(r.Body)
				r.Body = io.NopCloser(&body)
				isPause := strings.Contains(body.String(), `"max":0}`)
				mu.Lock()
				paused = true
				mu.Unlock()
				switch {
				case isPause && hk.failPause != 0:
					http.Error(w, "pause refused", hk.failPause)
				case !isPause && hk.failRestore != 0:
					http.Error(w, "restore refused", hk.failRestore)
				case !isPause:
					if hk.preRestore != nil {
						hk.preRestore()
					}
					proxy.ServeHTTP(w, r)
					if hk.postRestore != nil {
						hk.postRestore()
					}
				default:
					proxy.ServeHTTP(w, r)
				}
				return
			}
			mu.Lock()
			wait := paused && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/workers")
			mu.Unlock()
			if wait && hk.onWait != nil {
				hk.onWait()
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
}

// onlyFirstBoot fails the test for any job but the first boot.
func onlyFirstBoot(t *testing.T, c store.ConfigDetail) {
	t.Helper()
	for _, j := range c.Jobs {
		if j.Phase != "first_boot" {
			t.Errorf("a %s job (%s) was recorded after the pause", j.Phase, j.Status)
		}
	}
}

// A cancel during the zero wait still restores workers.max before teardown,
// and no job is sent after the pause.
func TestRun_ForcedCancelMidWaitStillRestores(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, fakerunpod.TestOptions(), pauseProxy(t, pauseHooks{onWait: cancel}))
	id, err := h.r.Start(ctx, forcedConfig(), 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Wait()

	rd := h.getRun(t, id)
	if rd.Status != "cancelled" || rd.Configs[0].Status != "cancelled" {
		t.Errorf("run %s, config %s, want both cancelled", rd.Status, rd.Configs[0].Status)
	}
	// Pause, restore, DELETE; the wait's W may be cancelled before forwarding.
	seq := callSeq(h.fake.Calls())
	if !regexp.MustCompile(`^[^P]*PW*R[^S]*D$`).MatchString(seq) {
		t.Errorf("call sequence %s: want pause, wait, restore, then delete, and no runsync after the pause", seq)
	}
	onlyFirstBoot(t, rd.Configs[0])
	h.assertCleanedUp(t, rd)
}

// A cancel during the restore: the restore completes, then the cancel wins
// and no cold job is sent.
func TestRun_ForcedCancelDuringRestoreSendsNoSample(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, fakerunpod.TestOptions(), pauseProxy(t, pauseHooks{postRestore: cancel}))
	id, err := h.r.Start(ctx, forcedConfig(), 2.00, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Wait()

	rd := h.getRun(t, id)
	if rd.Status != "cancelled" || rd.Configs[0].Status != "cancelled" {
		t.Errorf("run %s, config %s, want both cancelled", rd.Status, rd.Configs[0].Status)
	}
	onlyFirstBoot(t, rd.Configs[0])
	h.assertCleanedUp(t, rd)
}

// The restore runs even after a failed pause (here never forwarded). The
// cold samples are skipped and the config carries on to done.
func TestRun_ForcedFailedPauseStillRestores(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), pauseProxy(t, pauseHooks{failPause: http.StatusInternalServerError}))
	rd := h.runToEnd(t, forcedConfig())

	c := rd.Configs[0]
	if rd.Status != "completed" || c.Status != "done" || c.Error != "" {
		t.Errorf("run %s, config %s (%q), want completed and done", rd.Status, c.Status, c.Error)
	}
	for _, j := range c.Jobs {
		if j.Phase == "cold" {
			t.Errorf("a cold job was sent after a failed pause")
		}
	}
	// The first boot's runsync, then a restore, with no pause between.
	if seq := callSeq(h.fake.Calls()); !regexp.MustCompile(`^SW*R`).MatchString(seq) || strings.Contains(seq, "P") {
		t.Errorf("call sequence %s: want the restore straight after the first boot, and no pause reaching the fake", seq)
	}
	h.assertCleanedUp(t, rd)
}

// Deleted while paused: the restore's 404 isn't retried, and the config
// fails with errEndpointGone, not "could not restore".
func TestRun_ForcedEndpointDeletedWhilePausedIsGone(t *testing.T) {
	t.Parallel()
	var h *harness
	var once sync.Once
	deleteIt := func() {
		once.Do(func() {
			direct := newMgmt(t, h.fake.MgmtURL())
			for _, id := range h.fake.LiveEndpoints() {
				if err := direct.DeleteEndpoint(context.Background(), id); err != nil {
					t.Error(err)
				}
			}
		})
	}
	h = newHarness(t, fakerunpod.TestOptions(), pauseProxy(t, pauseHooks{preRestore: deleteIt}))
	rd := h.runToEnd(t, forcedConfig())

	c := rd.Configs[0]
	if c.Status != "failed" || !strings.Contains(c.Error, "deleted outside this run") || !strings.Contains(c.Error, "404") {
		t.Errorf("config %s (%q), want failed, saying the endpoint was deleted outside this run, with the 404", c.Status, c.Error)
	}
	if seq := callSeq(h.fake.Calls()); strings.Count(seq, "R") != 1 {
		t.Errorf("call sequence %s: want exactly one restore try (a 404 isn't retried)", seq)
	}
	onlyFirstBoot(t, c)
	h.assertCleanedUp(t, rd)
}

// A restore that keeps failing fails the config and sends no more jobs;
// teardown still deletes the endpoint.
func TestRun_ForcedRestoreFailureFailsConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), pauseProxy(t, pauseHooks{failRestore: http.StatusForbidden}))
	rd := h.runToEnd(t, forcedConfig())

	c := rd.Configs[0]
	if rd.Status != "failed" || c.Status != "failed" || !strings.Contains(c.Error, "could not restore workers.max to 3") || !strings.Contains(c.Error, "403") {
		t.Errorf("run %s, config %s (%q), want both failed, saying the restore failed, with the 403", rd.Status, c.Status, c.Error)
	}
	onlyFirstBoot(t, c)
	seq := callSeq(h.fake.Calls())
	if i := strings.Index(seq, "P"); i < 0 || strings.Contains(seq[i:], "S") {
		t.Errorf("call sequence %s: want a pause and no runsync after it", seq)
	}
	h.assertCleanedUp(t, rd)
}

// A paused endpoint's runsync 409 never cancels the config, because its
// /workers still answers 200.
func TestConfirmGone_PausedEndpointIsNotGone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	bg := context.Background()
	id, err := h.r.Mgmt.CreateEndpoint(bg, runpod.EndpointSpec{Name: runpod.NamePrefix + "paused", Image: "img", Pool: "AMPERE_16", MaxWorkers: 1, IdleTimeoutS: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.Mgmt.SetMaxWorkers(bg, id, 0); err != nil {
		t.Fatal(err)
	}
	_, runErr := h.r.Jobs.RunSync(bg, id, map[string]any{"sleep_ms": 5}, time.Second)
	if !isGoneHint(runErr) {
		t.Fatalf("runsync on a paused endpoint = %v, want a 409 hint", runErr)
	}

	ctx, cancel := context.WithCancelCause(bg)
	defer cancel(nil)
	c := &configRun{cancel: cancel}
	c.row.EndpointID = id
	h.r.confirmGone(ctx, c, runErr)
	if cause := context.Cause(ctx); cause != nil {
		t.Errorf("config cancelled with %v, want a paused endpoint not taken for a deleted one", cause)
	}
}

// matmul_n is sent only when MatmulN is set.
func TestJobInput(t *testing.T) {
	p := validParams()
	p.JobSleepMs = 500
	if b, _ := json.Marshal(jobInput(p)); string(b) != `{"sleep_ms":500}` {
		t.Errorf("MatmulN 0: input = %s, want {\"sleep_ms\":500}", b)
	}
	p.MatmulN = 4096
	if b, _ := json.Marshal(jobInput(p)); string(b) != `{"matmul_n":4096,"sleep_ms":500}` {
		t.Errorf("MatmulN 4096: input = %s", b)
	}
}

func TestGPUOutput(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name                 string
		raw                  string
		matmul, tflops, init *float64
	}{
		{"synthetic image", `{"slept_ms":500,"booted_at":1.5}`, nil, nil, nil},
		{"GPU image, matmul off", `{"slept_ms":0,"booted_at":1.5,"cuda_init_ms":230.5,"device":"NVIDIA RTX A5000"}`, nil, nil, f(230.5)},
		{"GPU image, matmul on", `{"slept_ms":0,"booted_at":1.5,"cuda_init_ms":230.5,"device":"X","matmul_ms":1.25,"tflops":110}`, f(1.25), f(110), f(230.5)},
		{"no output", ``, nil, nil, nil},
		{"not an object", `"done"`, nil, nil, nil},
		// Nothing of a half-read output is kept.
		{"wrong type", `{"matmul_ms":1.25,"tflops":"fast"}`, nil, nil, nil},
	}
	eq := func(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	for _, tt := range tests {
		m, tf, ci := gpuOutput(json.RawMessage(tt.raw))
		if !eq(m, tt.matmul) || !eq(tf, tt.tflops) || !eq(ci, tt.init) {
			t.Errorf("%s: got %v %v %v, want %v %v %v", tt.name, m, tf, ci, tt.matmul, tt.tflops, tt.init)
		}
	}
}

// A MatmulN run sets a CUDA 12.8 floor, sends matmul_n and stores the
// matmul numbers; a MatmulN 0 run does none of it.
func TestRun_MatmulN(t *testing.T) {
	t.Parallel()
	for _, matmulN := range []int{0, 1024} {
		t.Run(fmt.Sprint(matmulN), func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var creates, inputs []string
			// record proxies target, saving every POST body whose path ends in suffix.
			record := func(target string, suffix string, into *[]string) string {
				u, _ := url.Parse(target)
				proxy := httputil.NewSingleHostReverseProxy(u)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, suffix) {
						b, _ := io.ReadAll(r.Body)
						r.Body = io.NopCloser(bytes.NewReader(b))
						mu.Lock()
						*into = append(*into, string(b))
						mu.Unlock()
					}
					proxy.ServeHTTP(w, r)
				}))
				t.Cleanup(srv.Close)
				return srv.URL
			}
			h := newHarness(t, fakerunpod.TestOptions(), func(f *fakerunpod.Fake) string {
				return record(f.MgmtURL(), "/serverless", &creates)
			})
			jobsURL := record(strings.TrimSuffix(h.fake.JobURL(), "/v2"), "/runsync", &inputs)
			h.r.Jobs = runpod.NewJobClient(jobsURL+"/v2", testKey, nil)

			p := oneConfig()
			p.MatmulN = matmulN
			if matmulN > 0 {
				p.Image = GPUImage
			}
			rd := h.runToEnd(t, p)
			if rd.Status != "completed" {
				t.Fatalf("run = %s (%s), want completed", rd.Status, rd.Error)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(creates) != 1 {
				t.Fatalf("got %d creates, want 1", len(creates))
			}
			hasFloor := strings.Contains(creates[0], `"minCudaVersion":"12.8"`)
			if hasFloor != (matmulN > 0) || strings.Contains(creates[0], "allowedCudaVersions") {
				t.Errorf("create body = %s, want minCudaVersion 12.8 only when matmulN > 0", creates[0])
			}
			for _, in := range inputs {
				if hasN := strings.Contains(in, `"matmul_n":1024`); hasN != (matmulN > 0) {
					t.Fatalf("runsync body = %s, want matmul_n only when matmulN > 0", in)
				}
			}
			for _, j := range rd.Configs[0].Jobs {
				if j.Status != completed {
					continue
				}
				got := j.MatmulMs != nil && j.TFLOPS != nil && j.CudaInitMs != nil
				none := j.MatmulMs == nil && j.TFLOPS == nil && j.CudaInitMs == nil
				if (matmulN > 0 && !got) || (matmulN == 0 && !none) {
					t.Errorf("%s job: matmul %v tflops %v cudaInit %v, want all set iff matmulN > 0", j.Phase, j.MatmulMs, j.TFLOPS, j.CudaInitMs)
				}
			}

			// No gpus without matmul. With it, every group has the RTX 2000
			// Ada's rate, and cold CUDA init is the first boot plus 2 cold jobs.
			gpus := Summarize(rd.Configs[0], p).GPUs
			if matmulN == 0 && gpus != nil {
				t.Errorf("gpus = %+v, want nil", gpus)
			}
			if matmulN > 0 {
				coldInits := 0
				for _, g := range gpus {
					if g.TFLOPS.P50 != 45 || (g.GPUTypeID != "" && g.GPUTypeID != "NVIDIA RTX 2000 Ada Generation") {
						t.Errorf("gpu group %+v, want NVIDIA RTX 2000 Ada Generation (or \"\") at 45 TFLOPS", g)
					}
					coldInits += g.ColdCudaInitMs.N
				}
				if len(gpus) == 0 || gpus[len(gpus)-1].GPUTypeID != "NVIDIA RTX 2000 Ada Generation" || coldInits != 3 {
					t.Errorf("gpus = %+v, want an NVIDIA RTX 2000 Ada Generation group and 3 cold CUDA inits in all", gpus)
				}
			}
			h.assertCleanedUp(t, rd)
		})
	}
}

// recordCreates saves every create-endpoint body sent to the fake.
func recordCreates(t *testing.T, mu *sync.Mutex, into *[]string) func(*fakerunpod.Fake) string {
	return func(f *fakerunpod.Fake) string {
		u, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(u)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/serverless") {
				b, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(b))
				mu.Lock()
				*into = append(*into, string(b))
				mu.Unlock()
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
}

func createGPU(t *testing.T, body string) (pools []string, excluded []string, hasExcluded bool) {
	t.Helper()
	var b struct {
		GPU map[string]json.RawMessage `json:"gpu"`
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("create body %s: %v", body, err)
	}
	json.Unmarshal(b.GPU["pools"], &pools)
	raw, hasExcluded := b.GPU["excludedTypes"]
	if hasExcluded {
		json.Unmarshal(raw, &excluded)
	}
	return pools, excluded, hasExcluded
}

// Card mode: the endpoint uses the card's pool minus every other card, and
// every job ran on the card.
func TestRun_CardModeCreatesNarrowedEndpoint(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var creates []string
	h := newHarness(t, fakerunpod.TestOptions(), recordCreates(t, &mu, &creates))

	const card = "NVIDIA RTX A4500"
	// The card is made cheaper than its pool, so the row's price is visibly
	// the card's. The catalog is a copy: testCatalog is shared.
	catalog := slices.Clone(testCatalog)
	for i := range catalog {
		catalog[i].Cards = slices.Clone(catalog[i].Cards)
		for j := range catalog[i].Cards {
			if catalog[i].Cards[j].ID == card {
				catalog[i].Cards[j].PriceHr = 0.42
			}
		}
	}
	p := oneConfig()
	p.Pools, p.Cards = nil, []string{card}
	id, err := h.r.Start(context.Background(), p, 2.00, catalog)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Wait()
	rd := h.getRun(t, id)
	if rd.Status != "completed" {
		t.Fatalf("run = %s (%s), want completed", rd.Status, rd.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("got %d creates, want 1", len(creates))
	}
	pool, _, _ := CardPool(catalog, card)
	pools, excluded, _ := createGPU(t, creates[0])
	if !slices.Equal(pools, []string{"AMPERE_16"}) || !slices.Equal(excluded, ExcludedFor(pool, card)) {
		t.Errorf("create gpu = pools %v excluded %v, want [AMPERE_16] and %v", pools, excluded, ExcludedFor(pool, card))
	}
	c := rd.Configs[0]
	if c.Card != card || c.Pool != "AMPERE_16" || c.PriceHr != 0.42 {
		t.Errorf("config = card %q pool %q price %v, want %q, AMPERE_16, 0.42 (the card's price, not the pool's 0.28)", c.Card, c.Pool, c.PriceHr, card)
	}
	// A job the GPU cache hadn't learned yet has ""; the rest must be the card.
	onCard := 0
	for _, j := range c.Jobs {
		if j.Status == completed && j.GPUTypeID != "" && j.GPUTypeID != card {
			t.Errorf("%s job ran on %q, want %q", j.Phase, j.GPUTypeID, card)
		}
		if j.GPUTypeID == card {
			onCard++
		}
	}
	if onCard == 0 {
		t.Errorf("no job reported gpuTypeId %q", card)
	}
	h.assertCleanedUp(t, rd)
}

// A pool's only card excludes nothing, so gpu.excludedTypes is omitted.
func TestRun_CardModeSingleCardPool(t *testing.T) {
	t.Parallel()
	const card = "NVIDIA GeForce RTX 4090"
	var mu sync.Mutex
	var creates []string
	h := newHarness(t, fakerunpod.TestOptions(), recordCreates(t, &mu, &creates))
	catalog, err := h.r.Mgmt.Pools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pool, _, ok := CardPool(catalog, card)
	if !ok || len(pool.Cards) != 1 {
		t.Fatalf("CardPool(4090) = %+v, %v, want a one-card ADA_24", pool, ok)
	}

	p := oneConfig()
	p.Pools, p.Cards = nil, []string{card}
	id, err := h.r.Start(context.Background(), p, 2.00, catalog)
	if err != nil {
		t.Fatal(err)
	}
	h.r.Wait()
	rd := h.getRun(t, id)
	if rd.Status != "completed" {
		t.Fatalf("run = %s (%s), want completed", rd.Status, rd.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("got %d creates, want 1", len(creates))
	}
	pools, excluded, has := createGPU(t, creates[0])
	if !slices.Equal(pools, []string{"ADA_24"}) || has || len(excluded) != 0 {
		t.Errorf("create gpu = pools %v excluded %v (present %v), want [ADA_24] and no excludedTypes", pools, excluded, has)
	}
	if c := rd.Configs[0]; c.Card != card || c.Pool != "ADA_24" || c.PriceHr != 0.89 {
		t.Errorf("config = card %q pool %q price %v, want %q, ADA_24, 0.89", c.Card, c.Pool, c.PriceHr, card)
	}
	h.assertCleanedUp(t, rd)
}

// warnLogs is every log event with level "warn", in publish order.
func (h *harness) warnLogs() []Event {
	var out []Event
	for _, e := range h.eventsOf("log") {
		if e.Data.(map[string]any)["level"] == "warn" {
			out = append(out, e)
		}
	}
	return out
}

func TestNoWorkerMsg(t *testing.T) {
	t.Parallel()
	got := noWorkerMsg("AMPERE_24", 90*time.Second)
	want := "No worker placed on AMPERE_24 after 90 s. The pool may be out of stock; Cancel stops the run."
	if got != want {
		t.Errorf("noWorkerMsg = %q, want %q", got, want)
	}
}

// A worker placed after NoWorkerWarn gets exactly one warn log naming the
// pool or card; the run still completes. 150 ms stands in for 90 s.
func TestRun_NoWorkerWarnAfter90s(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, card, label string }{
		{"pool", "", "AMPERE_16"},
		{"card", "NVIDIA RTX A4500", "NVIDIA RTX A4500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := fakerunpod.TestOptions()
			opts.PlaceAfter = 600 * time.Millisecond
			h := newHarness(t, opts, nil)
			h.r.Timeouts.NoWorkerWarn = 150 * time.Millisecond
			p := oneConfig()
			if tc.card != "" {
				p.Pools, p.Cards = nil, []string{tc.card}
			}

			rd := h.runToEnd(t, p)

			if rd.Status != "completed" {
				t.Fatalf("run = %s (%s), want completed once the worker is placed", rd.Status, rd.Error)
			}
			warns := h.warnLogs()
			if len(warns) != 1 {
				t.Fatalf("got %d warn logs, want exactly 1: %v", len(warns), warns)
			}
			w := warns[0]
			if w.ConfigID == 0 || w.ConfigID != rd.Configs[0].ID {
				t.Errorf("warn configId = %d, want config %d", w.ConfigID, rd.Configs[0].ID)
			}
			msg, _ := w.Data.(map[string]any)["msg"].(string)
			if msg != noWorkerMsg(tc.label, 150*time.Millisecond) {
				t.Errorf("warn msg = %q, want %q", msg, noWorkerMsg(tc.label, 150*time.Millisecond))
			}
			if !strings.HasSuffix(msg, "s. The pool may be out of stock; Cancel stops the run.") {
				t.Errorf("warn msg = %q, want it to end with the fixed sentence", msg)
			}
			h.assertCleanedUp(t, rd)
		})
	}
}

// A worker placed before the deadline but slow to boot gets no warn
// (real case scaled 5 ms per second: placed 400 ms, warn 450, booted 1400).
func TestRun_NoWarnWhenPlacedBeforeTimeout(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.PlaceAfter = 400 * time.Millisecond
	opts.ImagePull = 1000 * time.Millisecond
	h := newHarness(t, opts, nil)
	h.r.Timeouts.NoWorkerWarn = 450 * time.Millisecond

	rd := h.runToEnd(t, oneConfig())

	if rd.Status != "completed" {
		t.Fatalf("run = %s (%s), want completed", rd.Status, rd.Error)
	}
	if warns := h.warnLogs(); len(warns) != 0 {
		t.Errorf("got warn logs %v, want none: the worker was placed before the deadline", warns)
	}
	// The first boot outlasted the deadline, so the slow-boot case was tested.
	for _, j := range rd.Configs[0].Jobs {
		if j.Phase == "first_boot" && j.DelayMs < 450 {
			t.Errorf("first boot delayMs = %v, want > 450 (the warn deadline)", j.DelayMs)
		}
	}
}

// A card-mode run with no pools stores "pools":[], and a pool-mode run has no "cards".
func TestRun_StoredParamsPoolsIsArray(t *testing.T) {
	t.Parallel()
	h := newHarness(t, fakerunpod.TestOptions(), nil)
	h.r.Timeouts.FirstBoot = 50 * time.Millisecond // the run itself doesn't matter
	cardRun := oneConfig()
	cardRun.Pools, cardRun.Cards = nil, []string{"NVIDIA RTX A4500"}

	for _, tc := range []struct {
		name      string
		p         Params
		want, not string
	}{
		{"card mode", cardRun, `"pools":[]`, `"pools":null`},
		{"pool mode", oneConfig(), `"pools":["AMPERE_16"]`, `"cards"`},
	} {
		id, err := h.r.Start(context.Background(), tc.p, 2.00, testCatalog)
		if err != nil {
			t.Fatal(err)
		}
		h.r.Cancel(id)
		h.r.Wait()
		params := string(h.getRun(t, id).Params)
		if !strings.Contains(params, tc.want) || strings.Contains(params, tc.not) {
			t.Errorf("%s: stored params %s, want %s and no %s", tc.name, params, tc.want, tc.not)
		}
	}
}
