// Contract tests: the real internal/runpod clients against the fake. An
// external test package, so fake.go itself never imports the client.
package fakerunpod_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/josh10111/coldprobe/internal/fakerunpod"
	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/runpod/go-sdk/v2/transport"
)

// testKey is any non-empty string: the fake only checks a Bearer token exists.
const testKey = "fake-runpod-test-key" // gitleaks:allow (test fixture)

// sleep10 is the job input every test sends.
var sleep10 = map[string]int{"sleep_ms": 10}

// start returns a fake and the two real clients pointed at it; cleanup
// also checks Close doesn't hang.
func start(t *testing.T, o fakerunpod.Options) (*fakerunpod.Fake, *runpod.Mgmt, *runpod.JobClient) {
	t.Helper()
	f := fakerunpod.New(o)
	t.Cleanup(f.Close)
	m, err := runpod.NewMgmt(f.MgmtURL(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	return f, m, runpod.NewJobClient(f.JobURL(), testKey, nil)
}

// create makes an endpoint the way the benchmark does.
func create(t *testing.T, m *runpod.Mgmt, pool, flashboot string, idleTimeoutS int) string {
	t.Helper()
	id, err := m.CreateEndpoint(context.Background(), runpod.EndpointSpec{
		Name:         runpod.NamePrefix + "test",
		Image:        "ghcr.io/example/worker:1",
		Pool:         pool,
		FlashBoot:    flashboot,
		MaxWorkers:   1,
		IdleTimeoutS: idleTimeoutS,
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	return id
}

// runSync sends one sleep10 job and requires it to complete.
func runSync(t *testing.T, jc *runpod.JobClient, id string) runpod.JobResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := jc.RunSync(ctx, id, sleep10, 5*time.Second)
	if err != nil {
		t.Fatalf("runsync: %v", err)
	}
	if res.Status != "COMPLETED" {
		t.Fatalf("status = %q, want COMPLETED (%+v)", res.Status, res)
	}
	return res
}

// waitForZero polls Workers until Active() is 0, failing after budget.
func waitForZero(t *testing.T, m *runpod.Mgmt, id string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		w, err := m.Workers(context.Background(), id)
		if err != nil {
			t.Fatalf("workers: %v", err)
		}
		if w.Active() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %d active workers after %v", w.Active(), budget)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestContract_CreateRunDelete(t *testing.T) {
	o := fakerunpod.TestOptions()
	f, m, jc := start(t, o)
	ctx := context.Background()

	// The two cheapest pools by their dearest card: GPUNames follow Cards,
	// MemoryGB is the smallest card's, PricePerHr the dearest's.
	pools, err := m.Pools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantPools := []runpod.Pool{
		{ID: "AMPERE_16", GPUNames: []string{"RTX 2000 Ada", "RTX A4000", "RTX A4500", "RTX 4000 Ada"}, MemoryGB: 16, PricePerHr: 0.28},
		{ID: "AMPERE_48", GPUNames: []string{"A40", "RTX A6000"}, MemoryGB: 48, PricePerHr: 0.59},
	}
	if len(pools) < 2 || !slices.EqualFunc(pools[:2], wantPools, func(a, b runpod.Pool) bool {
		return a.ID == b.ID && slices.Equal(a.GPUNames, b.GPUNames) && a.MemoryGB == b.MemoryGB && a.PricePerHr == b.PricePerHr
	}) {
		t.Errorf("pools = %+v, want %+v first", pools, wantPools)
	}

	// A long idle timeout keeps the worker around for the /workers check.
	id := create(t, m, "AMPERE_16", "OFF", 1000)
	res := runSync(t, jc, id)

	if minDelay := (o.ImagePull + o.ColdStart).Milliseconds(); res.DelayTime < minDelay {
		t.Errorf("delayTime = %d ms, want >= %d (first boot pays pull + cold start)", res.DelayTime, minDelay)
	}
	if res.ExecutionTime < 10 {
		t.Errorf("executionTime = %d ms, want >= 10", res.ExecutionTime)
	}
	if res.WorkerID == "" || !strings.HasPrefix(res.ID, "sync-") {
		t.Errorf("want a workerId and a sync- job id, like the real runsync: %+v", res)
	}
	var out struct {
		SleptMs  int     `json:"slept_ms"`
		BootedAt float64 `json:"booted_at"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || out.SleptMs != 10 || out.BootedAt <= 0 {
		t.Errorf("output = %s (err %v), want slept_ms 10 and a booted_at", res.Output, err)
	}

	w, err := m.Workers(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if w.Total != 1 || w.Idle != 1 || w.GPUTypes[res.WorkerID] != "NVIDIA RTX 2000 Ada Generation" {
		t.Errorf("workers = %+v, want 1 idle worker %s on NVIDIA RTX 2000 Ada Generation", w, res.WorkerID)
	}

	eps, err := m.ListEndpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].ID != id || !strings.HasPrefix(eps[0].Name, runpod.NamePrefix) {
		t.Errorf("list = %+v, want just %s", eps, id)
	}

	if err := m.DeleteEndpoint(ctx, id); err != nil {
		t.Fatal(err)
	}
	if live := f.LiveEndpoints(); len(live) != 0 {
		t.Errorf("live after delete = %v, want none", live)
	}
	// A second delete is a 404, which Mgmt treats as success.
	if err := m.DeleteEndpoint(ctx, id); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// After the idle timeout the worker stays listed as IDLE, so Total stays 1
// while Active() is 0, and the next job reuses it with a cold start.
func TestContract_ScalesToZero(t *testing.T) {
	o := fakerunpod.TestOptions()
	_, m, jc := start(t, o)
	const idle = 4 // 4 × 5 ms = 20 ms idle
	id := create(t, m, "AMPERE_16", "OFF", idle)
	first := runSync(t, jc, id)
	waitForZero(t, m, id, time.Second) // generous: timers fire late under -race

	w, err := m.Workers(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if w.Total != 1 || w.Idle != 1 || w.Active() != 0 {
		t.Errorf("after scale-down: total %d idle %d active %d, want 1, 1, 0 (worker stays listed)", w.Total, w.Idle, w.Active())
	}

	second := runSync(t, jc, id)
	if second.WorkerID != first.WorkerID {
		t.Errorf("second job ran on %s, want the listed worker %s reused", second.WorkerID, first.WorkerID)
	}
	if cold := o.ColdStart.Milliseconds(); second.DelayTime < cold {
		t.Errorf("second delay = %d ms, want >= %d (a stopped worker boots again)", second.DelayTime, cold)
	}
}

func TestContract_FlashBootFasterSecondColdStart(t *testing.T) {
	// ColdStart is raised for slack: timers fire late under -race.
	o := fakerunpod.TestOptions()
	o.ColdStart = 100 * time.Millisecond

	for _, flashboot := range []string{"FLASHBOOT", "OFF"} {
		t.Run(flashboot, func(t *testing.T) {
			_, m, jc := start(t, o)
			id := create(t, m, "AMPERE_16", flashboot, 1)
			first := runSync(t, jc, id)
			waitForZero(t, m, id, time.Second)

			// The stopped worker is still listed, so it's the one that boots again.
			second := runSync(t, jc, id)
			if second.WorkerID != first.WorkerID {
				t.Fatalf("second job ran on %s, want the stopped worker %s booted again", second.WorkerID, first.WorkerID)
			}
			cold := o.ColdStart.Milliseconds()
			if flashboot == "FLASHBOOT" && second.DelayTime >= cold {
				t.Errorf("flashboot second delay = %d ms, want < %d", second.DelayTime, cold)
			}
			if flashboot == "OFF" && second.DelayTime < cold {
				t.Errorf("no-flashboot second delay = %d ms, want >= %d", second.DelayTime, cold)
			}
		})
	}
}

// workers.max 0 pauses the endpoint: runsync answers 409 while /workers
// answers 200, and after the restore the next job lands on a new worker
// with a first-boot delay.
func TestContract_PauseForcesNewWorker(t *testing.T) {
	o := fakerunpod.TestOptions()
	f, m, jc := start(t, o)
	ctx := context.Background()
	// A long idle timeout: without the pause the worker would stay warm.
	id := create(t, m, "AMPERE_16", "FLASHBOOT", 1000)
	first := runSync(t, jc, id)

	if err := m.SetMaxWorkers(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	w, err := m.Workers(ctx, id)
	if err != nil || w.Active() != 0 {
		t.Fatalf("workers while paused: %+v, %v; want 200 with 0 active", w, err)
	}
	var apiErr *runpod.APIError
	if _, err := jc.RunSync(ctx, id, sleep10, time.Second); !errors.As(err, &apiErr) ||
		apiErr.StatusCode != http.StatusConflict || !strings.Contains(apiErr.Detail, "Endpoint is paused") {
		t.Fatalf("runsync while paused: %v, want 409 Endpoint is paused", err)
	}

	if err := m.SetMaxWorkers(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	second := runSync(t, jc, id)
	if second.WorkerID == first.WorkerID {
		t.Errorf("job after restore ran on %s again, want a new worker", second.WorkerID)
	}
	if boot := (o.ImagePull + o.ColdStart).Milliseconds(); second.DelayTime < boot {
		t.Errorf("delay after restore = %d ms, want >= %d (a first boot)", second.DelayTime, boot)
	}

	var patches []string
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "PATCH ") {
			patches = append(patches, c)
		}
	}
	want := []string{
		"PATCH /v2/serverless/" + id + ` {"workers":{"max":0}}`,
		"PATCH /v2/serverless/" + id + ` {"workers":{"max":1}}`,
	}
	if !slices.Equal(patches, want) {
		t.Errorf("PATCH calls = %q, want %q", patches, want)
	}
}

func TestContract_NoStockRunSyncNotTerminal(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.NoStockPools = []string{"AMPERE_24"}
	_, m, jc := start(t, o)
	id := create(t, m, "AMPERE_24", "OFF", 5)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// wait (50 ms) < ctx (200 ms): runsync returns the queued job, then
	// the client polls /status until the deadline.
	res, err := jc.RunSync(ctx, id, sleep10, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if res.ID == "" || res.Status != "IN_QUEUE" {
		t.Errorf("last result = %+v, want an id and IN_QUEUE", res)
	}
}

// count429 sits under the SDK's retry transport and counts the 429s that reach it.
type count429 struct{ n atomic.Int32 }

func (c *count429) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		c.n.Add(1)
	}
	return resp, err
}

func TestContract_429IsRetried(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.Job429Every = 2
	f, m, _ := start(t, o)
	counter := &count429{}
	// The same transport NewJobClient uses, with the counter as its base.
	jc := runpod.NewJobClient(f.JobURL(), testKey, &http.Client{Transport: &transport.Retry{Base: counter}})
	since := time.Now()
	id := create(t, m, "AMPERE_16", "OFF", 1000)
	for range 4 {
		runSync(t, jc, id)
	}

	// Requests 2, 4 and 6 of the 7 (4 jobs + 3 retries) are 429s.
	if n := counter.n.Load(); n != 3 {
		t.Errorf("saw %d 429s, want 3", n)
	}
	// The 429 must come before a job exists, or the retry would duplicate it.
	met, err := m.Metrics(context.Background(), id, since)
	if err != nil {
		t.Fatal(err)
	}
	if n := sumRequests(met); n != 4 {
		t.Errorf("fake ran %d jobs, want 4", n)
	}
}

func TestContract_CreateMissingScalingIs400(t *testing.T) {
	f, _, _ := start(t, fakerunpod.TestOptions())

	body := `{"name":"bench-coldprobe-x","type":"QUEUE","image":"img","gpu":{"pools":["AMPERE_16"]}}`
	post := func(auth bool) *http.Response {
		req, err := http.NewRequest(http.MethodPost, f.MgmtURL()+"/v2/serverless", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer "+testKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post(false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want 401", resp.StatusCode)
	}

	resp = post(true)
	defer resp.Body.Close()
	var problem struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || problem.Title == "" || problem.Status != 400 {
		t.Errorf("got %d %+v, want 400 problem JSON with a title", resp.StatusCode, problem)
	}
	if !strings.Contains(problem.Detail, "scaling") {
		t.Errorf("detail = %q, want it to name the missing field", problem.Detail)
	}
}

func TestContract_FailCreateAfter(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.FailCreateAfter = 2
	f, m, _ := start(t, o)
	create(t, m, "AMPERE_16", "OFF", 5)

	_, err := m.CreateEndpoint(context.Background(), runpod.EndpointSpec{
		Name: runpod.NamePrefix + "b", Image: "img", Pool: "AMPERE_16", MaxWorkers: 1, IdleTimeoutS: 5,
	})
	var apiErr *runpod.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("second create err = %v, want a 400 *APIError", err)
	}

	create(t, m, "AMPERE_16", "OFF", 5)
	if n := len(f.LiveEndpoints()); n != 2 {
		t.Errorf("live endpoints = %d, want 2", n)
	}
}

func TestContract_MetricsReflectJobs(t *testing.T) {
	_, m, jc := start(t, fakerunpod.TestOptions())
	since := time.Now()
	id := create(t, m, "AMPERE_16", "OFF", 1000)
	for range 3 {
		runSync(t, jc, id)
	}

	met, err := m.Metrics(context.Background(), id, since)
	if err != nil {
		t.Fatal(err)
	}
	if n := sumRequests(met); n != 3 {
		t.Errorf("requests = %d, want 3 (%+v)", n, met.Requests)
	}
	completed, colds := 0, 0
	for _, b := range met.Requests {
		completed += b.Completed
		if b.ExecutionTimeMs.Min < 10 || b.DelayTimeMs.Max < b.DelayTimeMs.Min {
			t.Errorf("bucket stats look wrong: %+v", b)
		}
	}
	for _, b := range met.ColdStarts {
		colds += b.Count
	}
	if completed != 3 || colds != 1 {
		t.Errorf("completed = %d, cold starts = %d; want 3 and 1 (one worker, reused)", completed, colds)
	}
}

func TestContract_PurgeAndDeleteDropQueuedJobs(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.NoStockPools = []string{"AMPERE_16"}
	_, m, jc := start(t, o)
	id := create(t, m, "AMPERE_16", "OFF", 5)
	ctx := context.Background()

	queue := func() string {
		c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		res, _ := jc.RunSync(c, id, sleep10, 10*time.Millisecond)
		if res.ID == "" {
			t.Fatalf("no job id from a queued runsync: %+v", res)
		}
		return res.ID
	}

	purged := queue()
	if err := jc.PurgeQueue(ctx, id); err != nil {
		t.Fatal(err)
	}
	if res, err := jc.Status(ctx, id, purged); err != nil || res.Status != "CANCELLED" {
		t.Errorf("after purge: %+v, %v; want CANCELLED", res, err)
	}

	queue()
	if err := m.DeleteEndpoint(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Like the real job API after a delete: 409 "paused", not 404.
	for _, call := range []struct {
		name string
		err  error
	}{
		{"runsync", func() error { _, err := jc.RunSync(ctx, id, sleep10, time.Second); return err }()},
		{"status", func() error { _, err := jc.Status(ctx, id, purged); return err }()},
		{"purge-queue", jc.PurgeQueue(ctx, id)},
	} {
		var apiErr *runpod.APIError
		if !errors.As(call.err, &apiErr) || apiErr.StatusCode != http.StatusConflict || !strings.Contains(apiErr.Detail, "Endpoint is paused") {
			t.Errorf("%s after delete: %v, want 409 Endpoint is paused", call.name, call.err)
		}
	}
	var apiErr *runpod.APIError
	if _, err := m.Workers(ctx, id); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("workers after delete: %v, want 404", err)
	}
	// An ID that never existed is still a 404.
	if _, err := jc.Status(ctx, "nope", purged); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status of an unknown endpoint: %v, want 404", err)
	}
}

// Close must wake a runsync parked on a queued job, or it hangs for the whole wait.
func TestClose_WakesWaitingRunSync(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.NoStockPools = []string{"AMPERE_16"}
	f, m, jc := start(t, o)
	id := create(t, m, "AMPERE_16", "OFF", 5)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The goroutine must not touch t.
	type result struct {
		res runpod.JobResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := jc.RunSync(ctx, id, sleep10, time.Minute)
		done <- result{res, err}
	}()
	time.Sleep(50 * time.Millisecond) // let the request reach the handler

	closed := make(chan struct{})
	go func() {
		f.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close hung on a waiting runsync")
	}
	// The result must carry the job ID, proving the woken handler answered.
	got := <-done
	if got.err == nil {
		t.Error("RunSync against a closed fake returned no error")
	}
	if got.res.ID == "" {
		t.Errorf("result has no job id (%+v); the runsync handler didn't answer", got.res)
	}
}

func TestAddEndpoint_BypassesPrefix(t *testing.T) {
	f, m, _ := start(t, fakerunpod.TestOptions())
	id := f.AddEndpoint("keep-me")
	eps, err := m.ListEndpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].ID != id || eps[0].Name != "keep-me" {
		t.Errorf("list = %+v, want [%s keep-me]", eps, id)
	}
	if live := f.LiveEndpoints(); !slices.Equal(live, []string{id}) {
		t.Errorf("live = %v, want [%s]", live, id)
	}
}

// The catalog reports each pool's availability, and a no-stock pool as NONE.
func TestCatalog_Availability(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.NoStockPools = []string{"AMPERE_24"}
	_, m, _ := start(t, o)
	pools, err := m.Pools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range pools {
		got[p.ID] = p.Availability
	}
	if got["AMPERE_16"] != "HIGH" || got["AMPERE_24"] != "NONE" || got["AMPERE_48"] != "MEDIUM" {
		t.Errorf("availability = %v, want AMPERE_16 HIGH, AMPERE_24 NONE (no stock), AMPERE_48 MEDIUM", got)
	}
}

func TestNew_EmptyPoolsUsesDefaults(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.Pools = []fakerunpod.FakePool{}
	f, m, _ := start(t, o)
	f.AddEndpoint("seeded")
	pools, err := m.Pools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, p := range pools {
		cards += len(p.Cards)
	}
	if len(pools) != 13 || cards != len(fakerunpod.DefaultPools()) || cards != 29 {
		t.Errorf("got %d pools, %d cards; want 13 pools, the 29 default cards", len(pools), cards)
	}
}

func sumRequests(m runpod.Metrics) int {
	n := 0
	for _, b := range m.Requests {
		n += b.Requests
	}
	return n
}

// runSyncInput sends one job and returns the result, whatever its status.
func runSyncInput(t *testing.T, jc *runpod.JobClient, id string, input any) runpod.JobResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := jc.RunSync(ctx, id, input, 5*time.Second)
	if err != nil {
		t.Fatalf("runsync: %v", err)
	}
	return res
}

// A matmul job gets the GPU image's extra output keys at its GPU's rate;
// a job without matmul_n gets exactly the synthetic worker's two keys.
func TestContract_MatmulOutput(t *testing.T) {
	_, m, jc := start(t, fakerunpod.TestOptions())
	type gpuOut struct {
		MatmulMs   float64 `json:"matmul_ms"`
		TFLOPS     float64 `json:"tflops"`
		CudaInitMs float64 `json:"cuda_init_ms"`
		Device     string  `json:"device"`
	}
	const n = 4096
	got := map[string]gpuOut{}
	for pool, gpu := range map[string]string{"AMPERE_16": "NVIDIA RTX 2000 Ada Generation", "AMPERE_24": "NVIDIA RTX A5000"} {
		id := create(t, m, pool, "OFF", 1000)
		res := runSyncInput(t, jc, id, map[string]int{"sleep_ms": 0, "matmul_n": n})
		var out gpuOut
		if err := json.Unmarshal(res.Output, &out); err != nil || res.Status != "COMPLETED" {
			t.Fatalf("%s: %s %s (err %v)", pool, res.Status, res.Output, err)
		}
		// ms = 2n^3 / (tflops x 1e12) x 1000.
		wantMs := 2 * float64(n*n*n) / (out.TFLOPS * 1e9)
		if out.TFLOPS <= 0 || out.CudaInitMs <= 0 || out.Device != gpu || math.Abs(out.MatmulMs-wantMs) > 1e-9 {
			t.Errorf("%s: output %+v, want device %s and matmul_ms %v", pool, out, gpu, wantMs)
		}
		got[pool] = out

		var keys map[string]any
		_ = json.Unmarshal(runSync(t, jc, id).Output, &keys)
		if k := slices.Sorted(maps.Keys(keys)); !slices.Equal(k, []string{"booted_at", "slept_ms"}) {
			t.Errorf("%s: sleep-only output keys = %q, want booted_at and slept_ms only", pool, k)
		}
	}
	if got["AMPERE_24"].TFLOPS <= got["AMPERE_16"].TFLOPS {
		t.Errorf("A5000 %v TFLOPS, RTX 2000 Ada %v: want the A5000 faster", got["AMPERE_24"].TFLOPS, got["AMPERE_16"].TFLOPS)
	}
}

func TestContract_BadMatmulNFails(t *testing.T) {
	_, m, jc := start(t, fakerunpod.TestOptions())
	id := create(t, m, "AMPERE_16", "OFF", 1000)
	for _, bad := range []any{8193, -1, 1.5, "4096", true, nil} {
		res := runSyncInput(t, jc, id, map[string]any{"sleep_ms": 0, "matmul_n": bad})
		if res.Status != "FAILED" || !strings.Contains(res.Error, "matmul_n must be an integer between 0 and 8192") {
			t.Errorf("matmul_n %v: %s %q, want FAILED naming matmul_n", bad, res.Status, res.Error)
		}
	}
}

// gpu.minCudaVersion is refused next to a non-empty gpu.allowedCudaVersions.
func TestContract_CudaVersionFields(t *testing.T) {
	f, m, _ := start(t, fakerunpod.TestOptions())
	_, err := m.CreateEndpoint(context.Background(), runpod.EndpointSpec{
		Name: runpod.NamePrefix + "cuda", Image: "img", Pool: "AMPERE_16", FlashBoot: "OFF",
		MaxWorkers: 1, IdleTimeoutS: 5, MinCudaVersion: "12.8",
	})
	if err != nil {
		t.Errorf("create with minCudaVersion: %v", err)
	}

	body := `{"name":"bench-coldprobe-x","type":"QUEUE","image":"img","scaling":{"type":"QUEUE_DELAY","queueDelay":4},
		"gpu":{"pools":["AMPERE_16"],"minCudaVersion":"12.8","allowedCudaVersions":["12.8"]}}`
	req, _ := http.NewRequest(http.MethodPost, f.MgmtURL()+"/v2/serverless", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("both CUDA fields: status = %d, want 400", resp.StatusCode)
	}
}

// Every worker of an endpoint with excludedTypes reports the pinned card.
func TestFake_ExcludedTypesNarrowsGPUType(t *testing.T) {
	_, m, jc := start(t, fakerunpod.TestOptions())
	ctx := context.Background()
	id, err := m.CreateEndpoint(ctx, runpod.EndpointSpec{
		Name: runpod.NamePrefix + "test", Image: "ghcr.io/example/worker:1",
		Pool: "AMPERE_16", FlashBoot: "OFF", MaxWorkers: 1, IdleTimeoutS: 1000,
		ExcludedTypes: []string{"NVIDIA RTX A4000", "NVIDIA RTX 2000 Ada Generation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := runSync(t, jc, id)
	w, err := m.Workers(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.GPUTypes[res.WorkerID]; got != "NVIDIA RTX A4500" {
		t.Errorf("gpuTypeId = %q, want NVIDIA RTX A4500 (the first card left)", got)
	}
}

// The pool's availability roll-up follows SetAvailability (best card wins).
func TestFake_SetAvailability(t *testing.T) {
	f, m, _ := start(t, fakerunpod.TestOptions())
	f.SetAvailability("AMPERE_16", "NVIDIA RTX 2000 Ada Generation", "NONE")
	f.SetAvailability("AMPERE_24", "", "LOW")
	f.SetAvailability("NO_SUCH_POOL", "", "HIGH") // ignored
	pools, err := m.Pools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range pools {
		if p.ID != "AMPERE_16" && p.ID != "AMPERE_24" {
			continue
		}
		got[p.ID] = p.Availability
		for _, c := range p.Cards {
			got[c.ID] = c.Availability
		}
	}
	want := map[string]string{
		"AMPERE_16": "MEDIUM", "NVIDIA RTX 2000 Ada Generation": "NONE", "NVIDIA RTX A4000": "MEDIUM",
		"NVIDIA RTX A4500": "LOW", "NVIDIA RTX 4000 Ada Generation": "LOW",
		"AMPERE_24": "LOW", "NVIDIA RTX A5000": "LOW", "NVIDIA GeForce RTX 3090": "LOW", "NVIDIA L4": "LOW",
		"NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb": "LOW",
	}
	if !maps.Equal(got, want) {
		t.Errorf("availability = %v\nwant %v", got, want)
	}
}

// An exclusion not in the pool is a 400, and so is excluding every card.
func TestFake_ExcludedTypesRejectsBadLists(t *testing.T) {
	_, m, _ := start(t, fakerunpod.TestOptions())
	for _, excluded := range [][]string{
		{"NVIDIA RTX A5000"}, // an AMPERE_24 card
		{"NVIDIA RTX A4000", "NVIDIA RTX A4500", "NVIDIA RTX 2000 Ada Generation", "NVIDIA RTX 4000 Ada Generation"},
	} {
		_, err := m.CreateEndpoint(context.Background(), runpod.EndpointSpec{
			Name: runpod.NamePrefix + "test", Image: "img", Pool: "AMPERE_16", FlashBoot: "OFF",
			MaxWorkers: 1, IdleTimeoutS: 5, ExcludedTypes: excluded,
		})
		var apiErr *runpod.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			t.Errorf("excluded %q: err = %v, want a 400 APIError", excluded, err)
		}
	}
}

// gpu.excludedTypes is echoed back, and omitted when nothing was excluded.
func TestFake_EchoesExcludedTypes(t *testing.T) {
	f, m, _ := start(t, fakerunpod.TestOptions())
	excluded := []string{"NVIDIA RTX A4000", "NVIDIA RTX 2000 Ada Generation"}
	pinned, err := m.CreateEndpoint(context.Background(), runpod.EndpointSpec{
		Name: runpod.NamePrefix + "pinned", Image: "img", Pool: "AMPERE_16", FlashBoot: "OFF",
		MaxWorkers: 1, IdleTimeoutS: 5, ExcludedTypes: excluded,
	})
	if err != nil {
		t.Fatal(err)
	}
	whole := create(t, m, "AMPERE_16", "OFF", 5)

	req, err := http.NewRequest(http.MethodGet, f.MgmtURL()+"/v2/serverless", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Endpoints []struct {
			ID  string                     `json:"id"`
			GPU map[string]json.RawMessage `json:"gpu"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]json.RawMessage{}
	for _, ep := range list.Endpoints {
		got[ep.ID] = ep.GPU
	}
	var echoed []string
	if err := json.Unmarshal(got[pinned]["excludedTypes"], &echoed); err != nil || !slices.Equal(echoed, excluded) {
		t.Errorf("pinned gpu = %s, want excludedTypes %q", got[pinned], excluded)
	}
	if _, ok := got[whole]["excludedTypes"]; ok {
		t.Errorf("whole-pool gpu = %s, want no excludedTypes key", got[whole])
	}
}

// BlockPlacement keeps jobs queued but leaves catalog stock alone.
func TestFake_BlockPlacementKeepsCatalog(t *testing.T) {
	f, m, jc := start(t, fakerunpod.TestOptions())
	f.BlockPlacement("AMPERE_24")
	id := create(t, m, "AMPERE_24", "OFF", 5)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if res, err := jc.RunSync(ctx, id, sleep10, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || res.Status != "IN_QUEUE" {
		t.Errorf("runsync = %+v, %v; want IN_QUEUE until the deadline", res, err)
	}
	if ws, err := m.Workers(context.Background(), id); err != nil || ws.Total != 0 {
		t.Errorf("workers = %+v, %v; want none", ws, err)
	}
	pools, err := m.Pools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pools {
		if p.ID == "AMPERE_24" && p.Availability != "HIGH" {
			t.Errorf("AMPERE_24 availability = %q, want HIGH (unchanged)", p.Availability)
		}
	}
}

// PlaceAfter holds back the first worker, then places it without another request.
func TestFake_PlaceAfterDelaysFirstWorker(t *testing.T) {
	o := fakerunpod.TestOptions()
	o.PlaceAfter = 150 * time.Millisecond
	_, m, jc := start(t, o)
	id := create(t, m, "AMPERE_16", "OFF", 5)

	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := jc.RunSync(ctx, id, sleep10, 2*time.Second)
	if err != nil || res.Status != "COMPLETED" {
		t.Fatalf("runsync = %+v, %v; want COMPLETED", res, err)
	}
	if took := time.Since(begin); took < 150*time.Millisecond {
		t.Errorf("first job done after %v, want at least PlaceAfter (150 ms)", took)
	}
}
