package server_test

// These tests drive the real handler and Runner against the fake Runpod.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/josh10111/coldprobe/internal/bench"
	"github.com/josh10111/coldprobe/internal/fakerunpod"
	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/server"
	"github.com/josh10111/coldprobe/internal/store"
)

type api struct {
	url   string
	store *store.Store
	fake  *fakerunpod.Fake
}

// testTimeouts are short phase timeouts.
var testTimeouts = bench.Timeouts{
	FirstBoot: 5 * time.Second, ScaleDown: 5 * time.Second,
	Cold: 5 * time.Second, Warm: 5 * time.Second, Teardown: 5 * time.Second,
}

type apiConfig struct {
	static   fs.FS
	timeouts bench.Timeouts // zero leaves the Runner's defaults
	fake     fakerunpod.Options
	addr     string
	catTTL   time.Duration // 0 here and below means the server default
	catLimit time.Duration
	catRetry time.Duration
	catStale time.Duration
	capUSD   float64 // 0 is 2
	maxWork  int
	mgmtURL  func(*fakerunpod.Fake) string // a proxy in front of the fake's management API; nil is the fake itself
	log      *slog.Logger                  // nil discards
}

// newAPI wires the handler to a fake Runpod, a temp database and a Runner.
func newAPI(t *testing.T, static fs.FS) *api {
	t.Helper()
	return newAPIWith(t, apiConfig{static: static, timeouts: testTimeouts, fake: fakerunpod.TestOptions()})
}

func newAPIWith(t *testing.T, c apiConfig) *api {
	t.Helper()
	// Cleanups run LIFO, so the fake closes last and every teardown finishes against it.
	fake := fakerunpod.New(c.fake)
	t.Cleanup(fake.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	mgmtURL := fake.MgmtURL()
	if c.mgmtURL != nil {
		mgmtURL = c.mgmtURL(fake)
	}
	mgmt, err := runpod.NewMgmt(mgmtURL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	hub := server.NewHub()
	r := &bench.Runner{
		Mgmt:        mgmt,
		Jobs:        runpod.NewJobClient(fake.JobURL(), "test-key", nil),
		Store:       st,
		Publish:     hub.Publish,
		Timeouts:    c.timeouts,
		WorkerPoll:  5 * time.Millisecond,
		BudgetTick:  5 * time.Millisecond,
		MetricsPoll: 10 * time.Millisecond,
		MetricsWait: 200 * time.Millisecond,
		RunSyncWait: 2 * time.Second,

		MaxWorkersLimit: c.maxWork,
	}
	t.Cleanup(r.Wait)

	log := c.log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ts := httptest.NewServer(server.New(server.Deps{
		// t.Context() is cancelled before cleanups, like SIGTERM in main.
		RootCtx: t.Context(),
		Store:   st, Runner: r, Mgmt: mgmt, Hub: hub,
		CapUSD: cmp.Or(c.capUSD, 2), Image: "fake/image:latest", Static: c.static, Addr: c.addr,
		CatalogTTL: c.catTTL, CatalogTimeout: c.catLimit, CatalogRetry: c.catRetry, CatalogMaxStale: c.catStale,
		Log: log,
	}))
	t.Cleanup(ts.Close)
	return &api{url: ts.URL, store: st, fake: fake}
}

// smallParams is one config that finishes in ~1.5 s, well under the hub's buffer.
func smallParams() bench.Params {
	p := bench.DefaultParams("fake/image:latest")
	p.Pools = []string{"AMPERE_16"}
	p.FlashBoot = []string{"OFF"}
	p.ColdStarts = 1
	p.WarmRequests = 2
	p.LevelsRPS = []float64{10}
	p.LevelSeconds = 1
	p.DrainSeconds = 1
	p.JobSleepMs = 5
	p.IdleTimeoutS = 1
	p.ModelLoadS = 0
	return p
}

// do sends a request with an optional JSON body.
func (a *api) do(t *testing.T, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	return send(t, a.req(t, method, path, body))
}

// req builds a request as the UI sends it: JSON, no Origin header.
func (a *api) req(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(mustJSON(t, body))
	}
	req, err := http.NewRequest(method, a.url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func send(t *testing.T, req *http.Request) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type runView struct {
	Run     store.Run `json:"run"`
	Configs []struct {
		Config  store.Config        `json:"config"`
		Summary bench.ConfigSummary `json:"summary"`
		Jobs    []store.Job         `json:"jobs"`
	} `json:"configs"`
}

type problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

func (a *api) startRun(t *testing.T, p bench.Params) string {
	t.Helper()
	code, _, body := a.do(t, "POST", "/api/runs", p)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/runs = %d %s", code, body)
	}
	var res struct{ ID string }
	decode(t, body, &res)
	id := res.ID
	if id == "" {
		t.Fatalf("no id in %s", body)
	}
	return id
}

func TestAPI_EstimateAndStartAndGet(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})

	code, _, body := a.do(t, "POST", "/api/runs/estimate", smallParams())
	if code != http.StatusOK {
		t.Fatalf("estimate = %d %s", code, body)
	}
	var est bench.Estimate
	decode(t, body, &est)
	if est.MaxSpendUSD <= 0 || len(est.Configs) != 1 {
		t.Fatalf("estimate = %+v, want maxSpendUsd > 0 and 1 config", est)
	}

	id := a.startRun(t, smallParams())

	// Poll until the run ends; 15 s is ten times what it takes.
	var v runView
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, _, body = a.do(t, "GET", "/api/runs/"+id, nil)
		if code != http.StatusOK {
			t.Fatalf("GET run = %d %s", code, body)
		}
		v = runView{}
		decode(t, body, &v)
		if v.Run.Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run still running after 15 s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if v.Run.ID != id || v.Run.Status != "completed" {
		t.Fatalf("run = %+v, want %s completed", v.Run, id)
	}
	if len(v.Configs) != 1 {
		t.Fatalf("got %d configs, want 1", len(v.Configs))
	}
	c := v.Configs[0]
	if c.Config.Status != "done" || c.Summary.ConfigID != c.Config.ID {
		t.Errorf("config %+v, summary for config %d", c.Config, c.Summary.ConfigID)
	}
	if c.Summary.Completed == 0 || c.Summary.FirstBootMs <= 0 || c.Summary.Warm.N != 2 || len(c.Summary.Levels) != 1 {
		t.Errorf("summary not populated: %+v", c.Summary)
	}
	if n := c.Summary.Completed + c.Summary.Failed + c.Summary.Unfinished; len(c.Jobs) != n {
		t.Errorf("%d jobs, summary counts %d", len(c.Jobs), n)
	}

	code, _, body = a.do(t, "GET", "/api/runs", nil)
	var runs []store.Run
	decode(t, body, &runs)
	if code != http.StatusOK || len(runs) != 1 || runs[0].ID != id {
		t.Errorf("GET /api/runs = %d %s", code, body)
	}
}

// A Runner with zero Timeouts runs with DefaultTimeouts, so the estimate must use them too.
func TestAPI_EstimateUsesDefaultTimeouts(t *testing.T) {
	t.Parallel()
	a := newAPIWith(t, apiConfig{static: fstest.MapFS{}, fake: fakerunpod.TestOptions()})
	p := smallParams()

	code, _, body := a.do(t, "POST", "/api/runs/estimate", p)
	if code != http.StatusOK {
		t.Fatalf("estimate = %d %s", code, body)
	}
	var got bench.Estimate
	decode(t, body, &got)
	// 0.28 is AMPERE_16's dearest card in the fake's catalog.
	want, err := bench.EstimateRun(p, bench.DefaultTimeouts(), []runpod.Pool{{ID: "AMPERE_16", PricePerHr: 0.28}})
	if err != nil {
		t.Fatal(err)
	}
	if got.WorstCaseUSD != want.WorstCaseUSD {
		t.Errorf("worstCaseUsd = %v, want %v (DefaultTimeouts)", got.WorstCaseUSD, want.WorstCaseUSD)
	}
}

func TestAPI_RejectsMaxWorkersAboveLimit(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	p := smallParams()
	p.MaxWorkers = 61

	code, h, body := a.do(t, "POST", "/api/runs", p)
	if code != http.StatusBadRequest || h.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("got %d %q %s, want 400 problem", code, h.Get("Content-Type"), body)
	}
	var pr problem
	decode(t, body, &pr)
	if pr.Status != 400 || !strings.Contains(pr.Detail, "maxWorkers") {
		t.Errorf("problem = %+v, want status 400 and detail naming maxWorkers", pr)
	}
	if runs, _ := a.store.ListRuns(t.Context()); len(runs) != 0 {
		t.Errorf("a rejected run was stored: %+v", runs)
	}
}

func TestAPI_Problems(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{"index.html": {Data: []byte("spa")}})
	unknownPool := smallParams()
	unknownPool.Pools = []string{"NO_SUCH_POOL"}
	// Valid params plus one misspelt field.
	var unknownField map[string]any
	decode(t, mustJSON(t, smallParams()), &unknownField)
	unknownField["levelRps"] = []float64{1}
	stringAllow := runBody(t, smallParams(), false)
	stringAllow["allowLowStock"] = "yes"

	tests := []struct {
		name, method, path string
		body               any
		want               int
		detail             string
	}{
		{"unknown pool on start", "POST", "/api/runs", unknownPool, 400, "NO_SUCH_POOL"},
		{"unknown pool on estimate", "POST", "/api/runs/estimate", unknownPool, 400, "NO_SUCH_POOL"},
		{"bad JSON", "POST", "/api/runs", "not an object", 400, ""},
		{"unknown field", "POST", "/api/runs", unknownField, 400, `unknown field "levelRps"`},
		{"allowLowStock not a boolean", "POST", "/api/runs", stringAllow, 400, "invalid JSON body"},
		{"missing run", "GET", "/api/runs/nope", nil, 404, ""},
		{"events for missing run", "GET", "/api/runs/nope/events", nil, 404, ""},
		{"cancel a run that isn't active", "POST", "/api/runs/nope/cancel", nil, 404, ""},
		{"unknown API path", "GET", "/api/nope", nil, 404, ""},
		{"unknown API method", "DELETE", "/api/runs", nil, 404, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, h, body := a.do(t, tt.method, tt.path, tt.body)
			if code != tt.want || h.Get("Content-Type") != "application/problem+json" {
				t.Fatalf("got %d %q %s, want %d problem", code, h.Get("Content-Type"), body, tt.want)
			}
			var pr problem
			decode(t, body, &pr)
			if pr.Status != tt.want || !strings.Contains(pr.Detail, tt.detail) {
				t.Errorf("problem = %+v, want detail containing %q", pr, tt.detail)
			}
		})
	}
}

type sseEvent struct {
	id, event string
	data      bench.Event
}

// readSSE parses frames from r until stop returns true or the stream ends.
// It returns an error rather than calling t.Fatal: one test runs it off
// the test goroutine.
func readSSE(r io.Reader, stop func(sseEvent) bool) ([]sseEvent, error) {
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20) // a job event's JSON can pass the 64 KiB default
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // a blank line ends a frame
			out = append(out, cur)
			if stop(cur) {
				return out, nil
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				return out, fmt.Errorf("data line %q: %w", line, err)
			}
		}
	}
	return out, sc.Err()
}

func status(e sseEvent) string {
	m, _ := e.data.Data.(map[string]any)
	s, _ := m["status"].(string)
	return s
}

func TestAPI_EventsStream(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	id := a.startRun(t, smallParams())

	// The timeout turns a stream that never ends into a failure, not a hang.
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get(a.url + "/api/runs/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != 200 || ct != "text/event-stream" {
		t.Fatalf("got %d %q, want 200 text/event-stream", resp.StatusCode, ct)
	}

	type result struct {
		events []sseEvent
		err    error
	}
	done := make(chan result, 1)
	go func() {
		events, err := readSSE(resp.Body, func(e sseEvent) bool {
			return e.event == "run_status" && status(e) != "running"
		})
		done <- result{events, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("no terminal run_status within 15 s")
	}
	if res.err != nil {
		t.Fatal(res.err)
	}
	events := res.events
	if len(events) == 0 {
		t.Fatal("stream ended with no events")
	}

	last := events[len(events)-1]
	if last.event != "run_status" || status(last) != "completed" {
		t.Fatalf("last event = %+v, want run_status completed", last)
	}
	jobs := 0
	var prev int64
	for _, e := range events {
		if e.id != "" && e.id != strconv.FormatInt(e.data.Seq, 10) {
			t.Errorf("id line %q but seq %d", e.id, e.data.Seq)
		}
		if e.data.Seq <= prev {
			t.Errorf("seq %d after %d: not strictly increasing", e.data.Seq, prev)
		}
		prev = e.data.Seq
		if e.event != e.data.Type || e.data.RunID != id {
			t.Errorf("frame %+v doesn't match its data", e)
		}
		if e.event == "job" {
			jobs++
		}
	}
	if jobs == 0 {
		t.Error("no job events")
	}

	if rest, _ := io.ReadAll(resp.Body); len(rest) != 0 {
		t.Errorf("data after the final run_status: %q", rest)
	}
}

func TestAPI_EventsForFinishedRunClosesImmediately(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	ctx := t.Context()
	if err := a.store.CreateRun(ctx, store.Run{ID: "r1", CreatedAt: time.Now(), Status: "running", Params: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.FinishRun(ctx, "r1", "cancelled", "run cancelled"); err != nil {
		t.Fatal(err)
	}

	c := &http.Client{Timeout: 2 * time.Second} // a hang fails here
	resp, err := c.Get(a.url + "/api/runs/r1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events, err := readSSE(resp.Body, func(sseEvent) bool { return false }) // reads to EOF
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].event != "run_status" || status(events[0]) != "cancelled" {
		t.Fatalf("events = %+v, want one run_status cancelled", events)
	}
}

func TestAPI_SPAFallback(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{
		"index.html":    {Data: []byte("<html>spa</html>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	})
	tests := []struct{ path, want string }{
		{"/", "<html>spa</html>"},
		{"/runs/abc", "<html>spa</html>"}, // a client-side route
		{"/assets/app.js", "console.log(1)"},
	}
	for _, tt := range tests {
		code, _, body := a.do(t, "GET", tt.path, nil)
		if code != http.StatusOK || string(body) != tt.want {
			t.Errorf("GET %s = %d %q, want 200 %q", tt.path, code, body, tt.want)
		}
	}
}

func TestAPI_UINotBuilt(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{}) // like web/dist with only .gitkeep
	code, _, body := a.do(t, "GET", "/", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "UI not built") {
		t.Errorf("GET / = %d %q, want 200 and the not-built note", code, body)
	}
}

func TestAPI_HealthzAndDefaults(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	if code, _, body := a.do(t, "GET", "/healthz", nil); code != 200 || string(body) != "ok" {
		t.Errorf("healthz = %d %q", code, body)
	}
	code, _, body := a.do(t, "GET", "/api/defaults", nil)
	var p bench.Params
	decode(t, body, &p)
	if code != 200 || p.Image != "fake/image:latest" || p.MaxWorkers != 3 || !strings.Contains(string(body), `"pools":[]`) {
		t.Errorf("defaults = %d %s", code, body)
	}
	code, _, body = a.do(t, "GET", "/api/gpus", nil)
	var pools []runpod.Pool
	decode(t, body, &pools)
	if code != 200 || len(pools) != 13 || pools[0].ID != "AMPERE_16" {
		t.Errorf("gpus = %d %s", code, body)
	}
}

// A client's budgetCapUsd is accepted and ignored: it can't raise the cap.
func TestAPI_DefaultsCapRoundTrips(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	code, _, body := a.do(t, "GET", "/api/defaults", nil)
	var d map[string]any
	decode(t, body, &d)
	if code != 200 || d["budgetCapUsd"] != 2.0 || d["budgetUsd"] != 2.0 {
		t.Fatalf("defaults = %d %s, want budgetCapUsd 2 next to the Params fields", code, body)
	}
	d["pools"] = []string{"AMPERE_16"}
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", d); code != 200 {
		t.Errorf("estimate of the defaults sent back = %d %s, want 200", code, body)
	}
	d["budgetCapUsd"], d["budgetUsd"] = 100.0, 50.0
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", d); code != 400 || !strings.Contains(string(body), "budgetUsd") {
		t.Errorf("budgetUsd 50 with budgetCapUsd 100 sent = %d %s, want 400 on budgetUsd (the server's cap is 2)", code, body)
	}
}

func TestAPI_ColdStartMode(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	code, _, body := a.do(t, "GET", "/api/defaults", nil)
	var d map[string]any
	decode(t, body, &d)
	if code != 200 || d["coldStartMode"] != "natural" {
		t.Fatalf("defaults = %d %s, want coldStartMode natural", code, body)
	}
	d["pools"] = []string{"AMPERE_16"}
	d["coldStartMode"] = "forced"
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", d); code != 200 {
		t.Errorf("estimate with coldStartMode forced = %d %s, want 200", code, body)
	}
	d["coldStartMode"] = "always"
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", d); code != 400 || !strings.Contains(string(body), "coldStartMode") {
		t.Errorf("estimate with coldStartMode always = %d %s, want 400 naming coldStartMode", code, body)
	}
}

// The POSTs enforce limits' maxWorkers top, and the default maxWorkers is
// clamped below a lower limit so the defaults still estimate.
func TestAPI_DefaultsLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ limit, wantTop, wantDefault int }{
		{0, 60, 3}, // unset: bench.DefaultMaxWorkersLimit
		{5, 5, 3},
		{2, 2, 2},
		{1, 1, 1},
	} {
		c := catalogCfg()
		c.maxWork = tc.limit
		a := newAPIWith(t, c)
		code, _, body := a.do(t, "GET", "/api/defaults", nil)
		var d struct {
			MaxWorkers int          `json:"maxWorkers"`
			Limits     bench.Limits `json:"limits"`
		}
		decode(t, body, &d)
		if code != 200 || !maps.Equal(d.Limits, bench.ParamLimits(tc.wantTop)) || d.MaxWorkers != tc.wantDefault {
			t.Errorf("limit %d: defaults = %d %s, want limits = ParamLimits(%d) and maxWorkers %d", tc.limit, code, body, tc.wantTop, tc.wantDefault)
		}

		var raw map[string]any
		decode(t, body, &raw)
		raw["pools"] = []string{"AMPERE_16"}
		if code, _, body := a.do(t, "POST", "/api/runs/estimate", raw); code != 200 {
			t.Errorf("limit %d: estimate of the defaults sent back (limits included) = %d %s, want 200", tc.limit, code, body)
		}
		raw["maxWorkers"] = tc.wantTop
		if code, _, body := a.do(t, "POST", "/api/runs/estimate", raw); code != 200 {
			t.Errorf("limit %d: maxWorkers %d = %d %s, want 200", tc.limit, tc.wantTop, code, body)
		}
		raw["maxWorkers"] = tc.wantTop + 1
		want := fmt.Sprintf("maxWorkers: %d must be between 1 and %d", tc.wantTop+1, tc.wantTop)
		if code, _, body := a.do(t, "POST", "/api/runs/estimate", raw); code != 400 || !strings.Contains(string(body), want) {
			t.Errorf("limit %d: maxWorkers %d = %d %s, want 400 %q", tc.limit, tc.wantTop+1, code, body, want)
		}
	}
}

// With a cap below the default budget, the served budget is clamped to it.
func TestAPI_DefaultsBudgetClampedToCap(t *testing.T) {
	t.Parallel()
	c := catalogCfg()
	c.capUSD = 1.5
	a := newAPIWith(t, c)
	code, _, body := a.do(t, "GET", "/api/defaults", nil)
	var d map[string]any
	decode(t, body, &d)
	if code != 200 || d["budgetCapUsd"] != 1.5 || d["budgetUsd"] != 1.5 {
		t.Fatalf("defaults = %d %s, want budgetUsd and budgetCapUsd 1.5", code, body)
	}
	d["pools"] = []string{"AMPERE_16"}
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", d); code != 200 {
		t.Errorf("estimate of the clamped defaults sent back = %d %s, want 200", code, body)
	}
}

// Cross-site and DNS-rebinding requests are refused before any handler runs.
func TestAPI_RequestGuards(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	params := mustJSON(t, smallParams())

	tests := []struct {
		name, method, path, host string
		headers                  map[string]string
		want                     int
	}{
		// CSRF: a "simple" text/plain POST needs no CORS preflight.
		{"cross-site POST (Sec-Fetch-Site)", "POST", "/api/runs", "",
			map[string]string{"Content-Type": "text/plain", "Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-origin POST (Origin only, older browser)", "POST", "/api/runs", "",
			map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}, 403},
		{"cross-site cancel", "POST", "/api/runs/x/cancel", "",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"non-JSON body", "POST", "/api/runs", "",
			map[string]string{"Content-Type": "text/plain"}, 415},
		{"non-JSON estimate", "POST", "/api/runs/estimate", "",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 415},
		{"foreign Host", "GET", "/healthz", "evil.example", nil, 403},
		{"foreign Host with port", "POST", "/api/runs", "evil.example:8080",
			map[string]string{"Content-Type": "application/json"}, 403},
		{"0.0.0.0 is not a name to accept", "GET", "/healthz", "0.0.0.0:8080", nil, 403},
		{"localhost", "GET", "/healthz", "localhost:8080", nil, 200},
		{"LOCALHOST, any case", "GET", "/healthz", "LOCALHOST", nil, 200},
		{"IPv6 loopback", "GET", "/healthz", "[::1]:8080", nil, 200},
		{"same-origin estimate, JSON with charset", "POST", "/api/runs/estimate", "",
			map[string]string{"Content-Type": "application/json; charset=utf-8", "Sec-Fetch-Site": "same-origin"}, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, a.url+tt.path, bytes.NewReader(params))
			if err != nil {
				t.Fatal(err)
			}
			if tt.host != "" {
				req.Host = tt.host // the Host header; the connection still goes to a.url
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			code, h, body := send(t, req)
			if code != tt.want {
				t.Fatalf("got %d %s, want %d", code, body, tt.want)
			}
			if code >= 400 && h.Get("Content-Type") != "application/problem+json" {
				t.Errorf("Content-Type %q, want problem JSON", h.Get("Content-Type"))
			}
		})
	}
	if runs, _ := a.store.ListRuns(t.Context()); len(runs) != 0 {
		t.Fatalf("a refused request created a run: %+v", runs)
	}

	req := a.req(t, "POST", "/api/runs", smallParams())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", a.url)
	if code, _, body := send(t, req); code != http.StatusCreated {
		t.Fatalf("same-origin POST = %d %s, want 201", code, body)
	}
}

// COLDPROBE_ADDR's host, when it names one, is accepted too.
func TestAPI_HostAllowlistIncludesAddrHost(t *testing.T) {
	t.Parallel()
	a := newAPIWith(t, apiConfig{static: fstest.MapFS{}, timeouts: testTimeouts,
		fake: fakerunpod.TestOptions(), addr: "10.1.2.3:8080"})
	for host, want := range map[string]int{"10.1.2.3:8080": 200, "10.1.2.4:8080": 403} {
		req := a.req(t, "GET", "/healthz", nil)
		req.Host = host
		if code, _, body := send(t, req); code != want {
			t.Errorf("Host %s = %d %s, want %d", host, code, body, want)
		}
	}
}

// Two POSTs at once: exactly one starts a run, the other gets 409.
func TestAPI_OneRunAtATime(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})

	codes := make(chan int, 2)
	for range 2 {
		go func() {
			// t.Error, not t.Fatal: this is not the test goroutine.
			resp, err := http.DefaultClient.Do(a.req(t, "POST", "/api/runs", smallParams()))
			if err != nil {
				t.Error(err)
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	got := map[int]int{<-codes: 1}
	got[<-codes]++
	if got[http.StatusCreated] != 1 || got[http.StatusConflict] != 1 {
		t.Fatalf("status counts %v, want one 201 and one 409", got)
	}
	if runs, _ := a.store.ListRuns(t.Context()); len(runs) != 1 {
		t.Errorf("%d runs stored, want 1", len(runs))
	}
}

// Card mode: the estimate resolves the card against the catalog, an
// unknown card is a 400 naming it, and a $0 card is refused.
func TestAPI_EstimateCardMode(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.Pools = append(fakerunpod.DefaultPools(),
		fakerunpod.FakePool{ID: "FREE_16", GPUName: "NVIDIA Free", MemoryGB: 16, PricePerHr: 0})
	a := newAPIWith(t, apiConfig{static: fstest.MapFS{}, timeouts: testTimeouts, fake: opts})
	p := smallParams()
	p.Pools, p.Cards = []string{}, []string{"NVIDIA RTX A4500"}

	code, _, body := a.do(t, "POST", "/api/runs/estimate", p)
	if code != http.StatusOK {
		t.Fatalf("estimate = %d %s", code, body)
	}
	var est bench.Estimate
	decode(t, body, &est)
	want := bench.ConfigEstimate{Pool: "AMPERE_16", Card: "NVIDIA RTX A4500", FlashBoot: "OFF", PriceHr: 0.25, Availability: "LOW"}
	if len(est.Configs) != 1 {
		t.Fatalf("configs = %+v, want 1", est.Configs)
	}
	got := est.Configs[0]
	if got.WorstCaseUSD <= 0 {
		t.Errorf("worstCaseUsd = %v, want above 0", got.WorstCaseUSD)
	}
	got.WorstCaseUSD = 0 // checked above
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}

	for card, detail := range map[string]string{
		"NVIDIA H100": `cards: unknown card "NVIDIA H100"`,
		"NVIDIA Free": `cards: card "NVIDIA Free" has no serverless price`,
	} {
		p.Cards = []string{card}
		code, _, body := a.do(t, "POST", "/api/runs/estimate", p)
		var pr problem
		decode(t, body, &pr)
		if code != http.StatusBadRequest || !strings.Contains(pr.Detail, detail) {
			t.Errorf("estimate with %s = %d %s, want 400 containing %q", card, code, body, detail)
		}
	}
}

func TestAPI_RejectsFreePool(t *testing.T) {
	t.Parallel()
	opts := fakerunpod.TestOptions()
	opts.Pools = []fakerunpod.FakePool{{ID: "FREE_16", GPUName: "NVIDIA Free", MemoryGB: 16, PricePerHr: 0}}
	a := newAPIWith(t, apiConfig{static: fstest.MapFS{}, timeouts: testTimeouts, fake: opts})
	p := smallParams()
	p.Pools = []string{"FREE_16"}

	for _, path := range []string{"/api/runs/estimate", "/api/runs"} {
		code, _, body := a.do(t, "POST", path, p)
		var pr problem
		decode(t, body, &pr)
		if code != http.StatusBadRequest || !strings.Contains(pr.Detail, "no serverless price") {
			t.Errorf("POST %s = %d %s, want 400 naming the missing price", path, code, body)
		}
	}
}

// Before a run starts, the janitor deletes our leaked endpoints but spares
// a young one from an unknown run.
func TestAPI_StartRunSweepsLeftovers(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	now := time.Now().UTC()
	finished := now.Format("20060102-150405") + "-aaaa"
	if err := a.store.CreateRun(t.Context(), store.Run{ID: finished, CreatedAt: now, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	leaked := a.fake.AddEndpoint(runpod.NamePrefix + finished + "-0")
	foreign := a.fake.AddEndpoint(runpod.NamePrefix + now.Format("20060102-150405") + "-ffff-0")

	a.startRun(t, smallParams())

	// The sweep finishes before the 201. Check by ID: the new run's endpoint may be live.
	live := a.fake.LiveEndpoints()
	if slices.Contains(live, leaked) {
		t.Errorf("leaked endpoint %s still live after a run started: %v", leaked, live)
	}
	if !slices.Contains(live, foreign) {
		t.Errorf("young endpoint %s of an unknown run was deleted: %v", foreign, live)
	}
}

// A failed pre-run sweep is logged and doesn't block the run. 403, since
// a 5xx would be retried for seconds.
func TestAPI_StartRunSweepFailureDoesNotBlock(t *testing.T) {
	t.Parallel()
	var lists atomic.Int32
	listFails := func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v2/serverless" {
				lists.Add(1)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	a := newAPIWith(t, apiConfig{static: fstest.MapFS{}, timeouts: testTimeouts, fake: fakerunpod.TestOptions(), mgmtURL: listFails})

	a.startRun(t, smallParams()) // fails the test unless 201

	if lists.Load() == 0 {
		t.Error("the sweep never listed endpoints, so its failure path wasn't exercised")
	}
}

// The GPU catalog is read once and reused.
func TestAPI_CatalogIsCached(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK {
		t.Fatalf("gpus = %d %s", code, body)
	}
	for range 3 {
		if code, _, body := a.do(t, "POST", "/api/runs/estimate", smallParams()); code != http.StatusOK {
			t.Fatalf("estimate = %d %s", code, body)
		}
	}
	if n := a.fake.CatalogCalls(); n != 1 {
		t.Errorf("catalog requests = %d, want 1", n)
	}
}

// A failed catalog read falls back to the last good one; with nothing cached it's a 502.
func TestAPI_CatalogFailureUsesStaleCopy(t *testing.T) {
	t.Parallel()
	a := newAPIWith(t, apiConfig{static: fstest.MapFS{}, timeouts: testTimeouts, fake: fakerunpod.TestOptions(), catTTL: time.Millisecond})
	code, _, fresh := a.do(t, "GET", "/api/gpus", nil)
	if code != http.StatusOK {
		t.Fatalf("gpus = %d %s", code, fresh)
	}
	time.Sleep(5 * time.Millisecond) // past the TTL
	a.fake.SetCatalogDown(true)
	before := a.fake.CatalogCalls()

	code, _, stale := a.do(t, "GET", "/api/gpus", nil)
	if code != http.StatusOK || !bytes.Equal(stale, fresh) {
		t.Errorf("gpus with the catalog down = %d %s, want 200 and the cached %s", code, stale, fresh)
	}
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", smallParams()); code != http.StatusOK {
		t.Errorf("estimate with the catalog down = %d %s, want 200 from the cached prices", code, body)
	}
	if a.fake.CatalogCalls() == before {
		t.Error("the expired cache was used without trying the catalog")
	}

	b := newAPI(t, fstest.MapFS{})
	b.fake.SetCatalogDown(true)
	if code, _, body := b.do(t, "GET", "/api/gpus", nil); code != http.StatusBadGateway {
		t.Errorf("gpus, catalog down, no cache = %d %s, want 502", code, body)
	}
}

func catalogCfg() apiConfig {
	return apiConfig{static: fstest.MapFS{}, timeouts: testTimeouts, fake: fakerunpod.TestOptions()}
}

// blockCatalog fronts the fake's management API and, while block is set,
// holds every catalog request until its client gives up (or 3 s pass).
func blockCatalog(t *testing.T, block *atomic.Bool) func(*fakerunpod.Fake) string {
	return func(f *fakerunpod.Fake) string {
		target, _ := url.Parse(f.MgmtURL())
		proxy := httputil.NewSingleHostReverseProxy(target)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/catalog/gpus" && block.Load() {
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
				http.Error(w, "too late", http.StatusServiceUnavailable)
				return
			}
			proxy.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
}

// A hanging catalog read is cut off at CatalogTimeout.
func TestAPI_CatalogReadIsBounded(t *testing.T) {
	t.Parallel()
	var block atomic.Bool
	block.Store(true)
	c := catalogCfg()
	c.mgmtURL, c.catLimit = blockCatalog(t, &block), 50*time.Millisecond
	a := newAPIWith(t, c)

	start := time.Now()
	// Its own client timeout, so a missing bound fails instead of hanging.
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(a.req(t, "GET", "/api/gpus", nil))
	if err != nil {
		t.Fatalf("gpus with the catalog hanging: %v after %v, want a 502 within the 50 ms bound", err, time.Since(start))
	}
	resp.Body.Close()
	if took := time.Since(start); resp.StatusCode != http.StatusBadGateway || took > time.Second {
		t.Errorf("gpus with the catalog hanging = %d after %v, want 502 well within 1 s", resp.StatusCode, took)
	}
}

// After a failed read, the stale copy is served without a Runpod call for
// CatalogRetry; then the next request tries again.
func TestAPI_CatalogFailureBacksOffThenRetries(t *testing.T) {
	t.Parallel()
	c := catalogCfg()
	c.catTTL, c.catRetry = 500*time.Millisecond, 100*time.Millisecond
	a := newAPIWith(t, c)
	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK {
		t.Fatalf("gpus = %d %s", code, body)
	}
	time.Sleep(510 * time.Millisecond) // past the TTL
	a.fake.SetCatalogDown(true)

	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK {
		t.Fatalf("gpus, catalog down = %d %s, want the stale copy", code, body)
	}
	calls := a.fake.CatalogCalls()
	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK || a.fake.CatalogCalls() != calls {
		t.Errorf("gpus within CatalogRetry = %d %s, catalog calls %d -> %d; want 200 from the stale copy and no call",
			code, body, calls, a.fake.CatalogCalls())
	}

	a.fake.SetCatalogDown(false)
	time.Sleep(150 * time.Millisecond) // past CatalogRetry, well within the TTL
	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK || a.fake.CatalogCalls() != calls+1 {
		t.Errorf("gpus after CatalogRetry = %d %s, catalog calls %d -> %d; want a new read",
			code, body, calls, a.fake.CatalogCalls())
	}

	b := newAPIWith(t, catalogCfg())
	b.fake.SetCatalogDown(true)
	if code, _, body := b.do(t, "GET", "/api/gpus", nil); code != http.StatusBadGateway {
		t.Errorf("gpus, catalog down, no cache = %d %s, want 502", code, body)
	}
	b.fake.SetCatalogDown(false)
	calls = b.fake.CatalogCalls() // the transport retries a 503, so more than 1
	var pools []runpod.Pool
	code, _, body := b.do(t, "GET", "/api/gpus", nil)
	if decode(t, body, &pools); code != http.StatusOK || len(pools) == 0 || b.fake.CatalogCalls() != calls+1 {
		t.Errorf("gpus right after a failure = %d %s, catalog calls %d -> %d; want 200 with the pools from a new read",
			code, body, calls, b.fake.CatalogCalls())
	}
}

// A copy older than CatalogMaxStale is never served, even during the backoff.
func TestAPI_CatalogStaleCopyHasAnAgeLimit(t *testing.T) {
	t.Parallel()
	c := catalogCfg()
	c.catTTL, c.catStale = time.Millisecond, time.Second
	a := newAPIWith(t, c)
	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK {
		t.Fatalf("gpus = %d %s", code, body)
	}
	time.Sleep(50 * time.Millisecond)
	a.fake.SetCatalogDown(true)
	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK {
		t.Fatalf("gpus, catalog down, copy 50 ms old = %d %s, want 200", code, body)
	}
	time.Sleep(1100 * time.Millisecond) // past CatalogMaxStale, within the 30 s CatalogRetry
	if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusBadGateway {
		t.Errorf("gpus, catalog down, copy past CatalogMaxStale = %d %s, want 502", code, body)
	}
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", smallParams()); code != http.StatusBadGateway {
		t.Errorf("estimate, catalog down, copy past CatalogMaxStale = %d %s, want 502", code, body)
	}
}

// syncBuf is a bytes.Buffer safe for the handler goroutines' log writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A client aborting mid catalog read isn't a Runpod failure: nothing is
// logged and no backoff starts.
func TestAPI_CatalogClientAbortIsQuiet(t *testing.T) {
	t.Parallel()
	abort := func(a *api) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if _, err := http.DefaultClient.Do(a.req(t, "GET", "/api/gpus", nil).WithContext(ctx)); err == nil {
			t.Fatal("the aborted request succeeded")
		}
		time.Sleep(100 * time.Millisecond) // let the handler finish
	}
	for _, cached := range []bool{true, false} {
		var logs syncBuf
		var block atomic.Bool
		c := catalogCfg()
		c.catTTL, c.mgmtURL, c.log = time.Millisecond, blockCatalog(t, &block), slog.New(slog.NewTextHandler(&logs, nil))
		a := newAPIWith(t, c)
		if cached {
			if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK {
				t.Fatalf("gpus = %d %s", code, body)
			}
			time.Sleep(5 * time.Millisecond)
		}
		block.Store(true)
		abort(a)
		block.Store(false)

		if out := logs.String(); strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") {
			t.Errorf("cached=%v: an aborted request logged:\n%s", cached, out)
		}
		calls := a.fake.CatalogCalls()
		if code, _, body := a.do(t, "GET", "/api/gpus", nil); code != http.StatusOK || a.fake.CatalogCalls() != calls+1 {
			t.Errorf("cached=%v: gpus after an abort = %d %s, catalog calls %d -> %d; want a new read",
				cached, code, body, calls, a.fake.CatalogCalls())
		}
	}
}

// Stock gate.

// runBody is p plus allowLowStock, as the form sends it.
func runBody(t *testing.T, p bench.Params, allowLowStock bool) map[string]any {
	t.Helper()
	var m map[string]any
	decode(t, mustJSON(t, p), &m)
	m["allowLowStock"] = allowLowStock
	return m
}

// postRun posts body to /api/runs and returns the status, the problem and the raw body.
func (a *api) postRun(t *testing.T, body any) (int, problem, []byte) {
	t.Helper()
	code, _, raw := a.do(t, "POST", "/api/runs", body)
	var pr problem
	if code != http.StatusCreated {
		decode(t, raw, &pr)
	}
	return code, pr, raw
}

func cardParams(card string) bench.Params {
	p := smallParams()
	p.Pools, p.Cards = []string{}, []string{card}
	return p
}

func TestGPUs_IncludesCards(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	code, _, body := a.do(t, "GET", "/api/gpus", nil)
	if code != http.StatusOK {
		t.Fatalf("gpus = %d %s", code, body)
	}
	var pools []runpod.Pool
	decode(t, body, &pools)
	i := slices.IndexFunc(pools, func(p runpod.Pool) bool { return p.ID == "AMPERE_16" })
	if i < 0 {
		t.Fatalf("no AMPERE_16 in %s", body)
	}
	got := map[string]string{}
	for _, c := range pools[i].Cards {
		if c.PriceHr <= 0 || c.MemoryGB < 16 || c.Name == "" || c.Name == c.ID {
			t.Errorf("card %+v, want a price, at least 16 GB and a short name", c)
		}
		got[c.ID] = c.Availability
	}
	want := map[string]string{
		"NVIDIA RTX 2000 Ada Generation": "HIGH", "NVIDIA RTX A4000": "MEDIUM",
		"NVIDIA RTX A4500": "LOW", "NVIDIA RTX 4000 Ada Generation": "LOW",
	}
	if !maps.Equal(got, want) {
		t.Errorf("AMPERE_16 cards = %v, want %v", got, want)
	}
}

// Out-of-stock is refused before anything is created, using a fresh read
// even though the preceding estimate cached the catalog while it had stock.
func TestStartRun_RefusesNoStock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, pool, card, detail string
		p                        bench.Params
	}{
		{"pool", "AMPERE_16", "", "AMPERE_16 has no stock right now", smallParams()},
		{"card", "AMPERE_16", "NVIDIA RTX A4500", "NVIDIA RTX A4500 has no stock right now", cardParams("NVIDIA RTX A4500")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAPI(t, fstest.MapFS{})
			if code, _, body := a.do(t, "POST", "/api/runs/estimate", tc.p); code != http.StatusOK {
				t.Fatalf("estimate = %d %s", code, body)
			}
			a.fake.SetAvailability(tc.pool, tc.card, "NONE")

			code, pr, raw := a.postRun(t, tc.p)
			if code != http.StatusBadRequest || pr.Detail != tc.detail {
				t.Errorf("POST /api/runs = %d %s, want 400 %q", code, raw, tc.detail)
			}
			if pr.Code != "no_stock" {
				t.Errorf("no-stock refusal %s has code %q, want no_stock", raw, pr.Code)
			}
			if runs, err := a.store.ListRuns(t.Context()); err != nil || len(runs) != 0 {
				t.Errorf("runs after the refusal = %v (%v), want none", runs, err)
			}
			if live := a.fake.LiveEndpoints(); len(live) != 0 {
				t.Errorf("endpoints after the refusal = %v, want none", live)
			}
		})
	}
}

// LOW is refused with "low_stock" until the body accepts the risk. The
// fake's RTX A4500 is LOW out of the box.
func TestStartRun_LowStockNeedsAllow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, detail string
		setup        func(*fakerunpod.Fake)
		p            bench.Params
	}{
		{"card", "NVIDIA RTX A4500 is low on stock; start anyway to accept the risk",
			func(*fakerunpod.Fake) {}, cardParams("NVIDIA RTX A4500")},
		{"pool", "AMPERE_24 is low on stock; start anyway to accept the risk",
			func(f *fakerunpod.Fake) { f.SetAvailability("AMPERE_24", "", "LOW") },
			func() bench.Params { p := smallParams(); p.Pools = []string{"AMPERE_24"}; return p }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newAPI(t, fstest.MapFS{})
			tc.setup(a.fake)
			for _, body := range []any{tc.p, runBody(t, tc.p, false)} {
				code, pr, raw := a.postRun(t, body)
				if code != http.StatusBadRequest || pr.Detail != tc.detail || pr.Code != "low_stock" {
					t.Errorf("POST /api/runs = %d %s, want 400 %q with code low_stock", code, raw, tc.detail)
				}
			}
			if code, _, raw := a.postRun(t, runBody(t, tc.p, true)); code != http.StatusCreated {
				t.Errorf("POST /api/runs with allowLowStock = %d %s, want 201", code, raw)
			}
		})
	}
}

// Pool mode gates on the pool's best card.
func TestStartRun_PoolStockIsBestCard(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	a.startRun(t, smallParams()) // AMPERE_16: HIGH, MEDIUM and LOW cards
}

// allowLowStock covers LOW only, and a NONE elsewhere wins over a LOW.
func TestStartRun_AllowLowStockDoesNotCoverNone(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	a.fake.SetAvailability("AMPERE_24", "", "NONE")
	p := smallParams()
	p.Pools = []string{"AMPERE_24"}
	code, pr, raw := a.postRun(t, runBody(t, p, true))
	if code != http.StatusBadRequest || pr.Detail != "AMPERE_24 has no stock right now" || pr.Code != "no_stock" {
		t.Errorf("NONE with allowLowStock = %d %s, want 400 %q and code no_stock", code, raw, "AMPERE_24 has no stock right now")
	}

	a.fake.SetAvailability("AMPERE_16", "", "LOW")
	p.Pools = []string{"AMPERE_16", "AMPERE_24"}
	for _, allow := range []bool{false, true} {
		code, pr, raw := a.postRun(t, runBody(t, p, allow))
		if code != http.StatusBadRequest || pr.Detail != "AMPERE_24 has no stock right now" || pr.Code != "no_stock" {
			t.Errorf("LOW + NONE pools, allowLowStock %v = %d %s, want the NONE refusal", allow, code, raw)
		}
	}
}

// The estimate carries each config's stock but never refuses on it.
func TestEstimate_EchoesAvailability(t *testing.T) {
	t.Parallel()
	a := newAPI(t, fstest.MapFS{})
	a.fake.SetAvailability("AMPERE_24", "", "NONE")
	p := smallParams()
	p.Pools = []string{"AMPERE_16", "AMPERE_24"}
	got := estimateStock(t, a, runBody(t, p, false))
	want := map[string]string{"AMPERE_16": "HIGH", "AMPERE_24": "NONE"}
	if !maps.Equal(got, want) {
		t.Errorf("estimate availability = %v, want %v", got, want)
	}

	p = cardParams("NVIDIA RTX A4500")
	got = estimateStock(t, a, p)
	want = map[string]string{"NVIDIA RTX A4500": "LOW"}
	if !maps.Equal(got, want) {
		t.Errorf("card estimate availability = %v, want %v", got, want)
	}
}

// estimateStock maps each estimated config's card (or pool) to its availability.
func estimateStock(t *testing.T, a *api, body any) map[string]string {
	t.Helper()
	code, _, raw := a.do(t, "POST", "/api/runs/estimate", body)
	if code != http.StatusOK {
		t.Fatalf("estimate = %d %s", code, raw)
	}
	var est struct {
		Configs []struct{ Pool, Card, Availability string }
	}
	decode(t, raw, &est)
	out := map[string]string{}
	for _, c := range est.Configs {
		out[cmp.Or(c.Card, c.Pool)] = c.Availability
	}
	return out
}

// The gate's fresh read falls back to the cached copy when the catalog is down.
func TestStartRun_StockReadFallsBackToCache(t *testing.T) {
	t.Parallel()
	var logs syncBuf
	c := catalogCfg()
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	a := newAPIWith(t, c)
	if code, _, body := a.do(t, "POST", "/api/runs/estimate", smallParams()); code != http.StatusOK {
		t.Fatalf("estimate = %d %s", code, body)
	}
	before := a.fake.CatalogCalls()
	a.fake.SetCatalogDown(true)
	a.startRun(t, smallParams()) // fails the test unless 201
	if a.fake.CatalogCalls() == before {
		t.Error("Start used the cached catalog without trying a fresh read")
	}
	// The fallback is the operator's only sign the stock was not re-read.
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "using the cached copy") {
		t.Errorf("no WARN line for the fallback; log:\n%s", out)
	}
}
