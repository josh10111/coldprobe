package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runpod/go-sdk/v2/transport"
)

const testKey = "fake-mgmt-key-123" // gitleaks:allow (test fixture)

// newTestMgmt points a Mgmt at an httptest server running handler.
// Handlers write plain vars that the test reads only after the call
// returns, which the response orders for the race detector.
func newTestMgmt(t *testing.T, handler http.HandlerFunc) *Mgmt {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m, err := NewMgmt(srv.URL, testKey)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// writeJSON sends a JSON body; the SDK decodes only "json" Content-Types.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestCreateEndpoint_SendsRequiredFields(t *testing.T) {
	var got map[string]any
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/serverless" {
			t.Errorf("got %s %s, want POST /v2/serverless", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(w, http.StatusCreated, `{"id":"ep_123","name":"bench-coldprobe-x"}`)
	})

	id, err := m.CreateEndpoint(context.Background(), EndpointSpec{
		Name:         NamePrefix + "x",
		Image:        "ghcr.io/example/worker:1",
		Pool:         "P1",
		FlashBoot:    "FLASHBOOT",
		MaxWorkers:   2,
		IdleTimeoutS: 5,
		Env:          map[string]string{"MODEL_LOAD_SECONDS": "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "ep_123" {
		t.Errorf("id = %q, want ep_123", id)
	}

	scaling, _ := got["scaling"].(map[string]any)
	gpu, _ := got["gpu"].(map[string]any)
	workers, _ := got["workers"].(map[string]any)
	env, _ := got["env"].(map[string]any)
	checks := []struct {
		name      string
		got, want any
	}{
		{"type", got["type"], "QUEUE"},
		{"scaling.type", scaling["type"], "QUEUE_DELAY"},
		{"scaling.queueDelay", scaling["queueDelay"], 4.0},
		{"gpu.pools", gpu["pools"], []any{"P1"}},
		{"workers.max", workers["max"], 2.0},
		{"workers.min", workers["min"], 0.0},
		{"workers.idleTimeout", workers["idleTimeout"], 5.0},
		{"timeout", got["timeout"], 300000.0},
		{"flashboot", got["flashboot"], "FLASHBOOT"},
		{"image", got["image"], "ghcr.io/example/worker:1"},
		{"env.MODEL_LOAD_SECONDS", env["MODEL_LOAD_SECONDS"], "5"},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.name, c.got, c.want)
		}
	}
	if name, _ := got["name"].(string); !strings.HasPrefix(name, NamePrefix) {
		t.Errorf("name = %q, want prefix %q", name, NamePrefix)
	}

	// The create schema rejects unknown fields.
	keys := slices.Sorted(maps.Keys(got))
	wantKeys := []string{"env", "flashboot", "gpu", "image", "name", "scaling", "timeout", "type", "workers"}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("top-level keys = %q, want %q", keys, wantKeys)
	}
}

func TestCreateEndpoint_RejectsNameWithoutPrefix(t *testing.T) {
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP call: %s %s", r.Method, r.URL.Path)
	})
	_, err := m.CreateEndpoint(context.Background(), EndpointSpec{Name: "prod-endpoint", Pool: "P1"})
	if err == nil {
		t.Fatal("want an error for a name without the prefix")
	}
}

func TestDeleteEndpoint_404IsNil(t *testing.T) {
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v2/serverless/ep_gone" {
			t.Errorf("got %s %s, want DELETE /v2/serverless/ep_gone", r.Method, r.URL.Path)
		}
		// With an unparseable JSON body the SDK returns a decode error, not the status.
		writeJSON(w, http.StatusNotFound, `{"title":"Not Found","status":404}`)
	})
	if err := m.DeleteEndpoint(context.Background(), "ep_gone"); err != nil {
		t.Fatalf("want nil for 404, got %v", err)
	}
}

// SetMaxWorkers(0) must really send "max": 0.
func TestSetMaxWorkers_SendsZero(t *testing.T) {
	var got map[string]any
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v2/serverless/ep_1" {
			t.Errorf("got %s %s, want PATCH /v2/serverless/ep_1", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, `{"id":"ep_1"}`)
	})
	if err := m.SetMaxWorkers(context.Background(), "ep_1", 0); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"workers": map[string]any{"max": 0.0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("body = %v, want %v", got, want)
	}
}

// A 403, because 5xx is retried with backoff.
func TestMgmt_NonSuccessIsRedactedAPIError(t *testing.T) {
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"title":"Forbidden","status":403,"detail":"key ` + testKey + ` lacks serverless:write"}`))
	})
	ctx := context.Background()

	calls := []struct {
		name string
		call func() error
	}{
		{"delete", func() error { return m.DeleteEndpoint(ctx, "ep_1") }},
		{"set max workers", func() error { return m.SetMaxWorkers(ctx, "ep_1", 0) }},
		{"create", func() error {
			_, err := m.CreateEndpoint(ctx, EndpointSpec{Name: NamePrefix + "x", Image: "img", Pool: "P1", MaxWorkers: 1, IdleTimeoutS: 5})
			return err
		}},
		{"metrics", func() error {
			_, err := m.Metrics(ctx, "ep_1", time.Now())
			return err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want *APIError, got %T: %v", err, err)
			}
			if apiErr.StatusCode != http.StatusForbidden || apiErr.Title != "Forbidden" {
				t.Errorf("got status %d title %q", apiErr.StatusCode, apiErr.Title)
			}
			if !strings.Contains(apiErr.Detail, "lacks serverless:write") {
				t.Errorf("detail lost: %q", apiErr.Detail)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("key leaked in Error(): %v", err)
			}
			if strings.Contains(apiErr.Detail, testKey) {
				t.Errorf("key leaked in apiErr.Detail: %q", apiErr.Detail)
			}
		})
	}
}

func TestListEndpoints_FollowsPages(t *testing.T) {
	var cursors []string
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		if cursor == "" {
			writeJSON(w, 200, `{"endpoints":[{"id":"ep_1","name":"bench-coldprobe-a"}],
				"pagination":{"nextCursor":"c2","hasNextPage":true}}`)
			return
		}
		writeJSON(w, 200, `{"endpoints":[{"id":"ep_2","name":"other"}],
			"pagination":{"nextCursor":null,"hasNextPage":false}}`)
	})

	got, err := m.ListEndpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Endpoint{{ID: "ep_1", Name: "bench-coldprobe-a"}, {ID: "ep_2", Name: "other"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(cursors, []string{"", "c2"}) {
		t.Errorf("cursors sent = %q, want [\"\" \"c2\"]", cursors)
	}
}

func TestListEndpoints_NextPageWithoutCursorFails(t *testing.T) {
	var calls atomic.Int32
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, `{"endpoints":[],"pagination":{"nextCursor":null,"hasNextPage":true}}`)
	})
	if _, err := m.ListEndpoints(context.Background()); err == nil {
		t.Fatal("want an error for hasNextPage without a cursor")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestWorkers_SummaryAndGPUTypes(t *testing.T) {
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/serverless/ep_1/workers" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, 200, `{
			"workers":[
				{"id":"w1","status":"RUNNING","gpuTypeId":"NVIDIA GeForce RTX 3090","gpuCount":1,"uptimeSeconds":40},
				{"id":"w2","status":"IDLE","gpuTypeId":"NVIDIA L4","gpuCount":1,"uptimeSeconds":6},
				{"id":"w3","status":"THROTTLED","gpuTypeId":null,"gpuCount":1,"uptimeSeconds":null}
			],
			"summary":{"running":1,"idle":1,"initializing":0,"throttled":1,"unhealthy":0,"total":3},
			"endpointVersion":1}`)
	})

	got, err := m.Workers(context.Background(), "ep_1")
	if err != nil {
		t.Fatal(err)
	}
	want := WorkerSummary{
		Running: 1, Idle: 1, Throttled: 1, Total: 3,
		GPUTypes: map[string]string{"w1": "NVIDIA GeForce RTX 3090", "w2": "NVIDIA L4"},
		active:   2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// summary.total is 1 in every row, so a regression to "Active is Total"
// fails the inactive row.
func TestWorkers_ActiveRule(t *testing.T) {
	tests := []struct {
		name   string
		worker string
		want   int
	}{
		{"idle, container stopped", `{"id":"w1","status":"IDLE","uptimeSeconds":null}`, 0},
		{"idle, warm", `{"id":"w1","status":"IDLE","uptimeSeconds":6}`, 1},
		// 0 is a real uptime: the check is != nil, not > 0.
		{"idle, warm, first second", `{"id":"w1","status":"IDLE","uptimeSeconds":0}`, 1},
		{"initializing", `{"id":"w1","status":"INITIALIZING","uptimeSeconds":null}`, 1},
		{"running", `{"id":"w1","status":"RUNNING","uptimeSeconds":40}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 200, `{"workers":[`+tt.worker+`],
					"summary":{"running":0,"idle":0,"initializing":0,"throttled":0,"unhealthy":0,"total":1},
					"endpointVersion":1}`)
			})
			got, err := m.Workers(context.Background(), "ep_1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Active() != tt.want {
				t.Errorf("Active() = %d, want %d", got.Active(), tt.want)
			}
		})
	}
}

func TestPools_GroupsAndUsesMaxPrice(t *testing.T) {
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("include") != "AVAILABILITY" || q.Get("product") != "SERVERLESS" {
			t.Errorf("query = %q, want include=AVAILABILITY&product=SERVERLESS", r.URL.RawQuery)
		}
		// Real pool ids and GPU names. The RTX A4000 is raised to 0.69 so
		// the test can tell max from first or min. PLENTY is undocumented
		// and maps to "".
		writeJSON(w, 200, `{"gpus":[
			{"id":"NVIDIA RTX A4000","name":"RTX A4000","memory":16,"pool":"AMPERE_16","price":{"serverless":0.69},"availability":"LOW"},
			{"id":"NVIDIA RTX 2000 Ada Generation","name":"RTX 2000 Ada","memory":16,"pool":"AMPERE_16","price":{"serverless":0.58},"availability":"MEDIUM"},
			{"id":"NVIDIA RTX 4000 Ada Generation","name":"RTX 4000 Ada","memory":20,"pool":"AMPERE_16","price":{"serverless":0.58},"availability":"NONE"},
			{"id":"NVIDIA H200","name":"H200","memory":141,"pool":null,"price":{"secure":3.59},"availability":"HIGH"},
			{"id":"NVIDIA L4","name":"L4","memory":24,"pool":"AMPERE_24","price":{"serverless":0.75}},
			{"id":"NVIDIA L40","name":"L40","memory":48,"pool":"AMPERE_24","price":{"serverless":0.75},"availability":"PLENTY"},
			{"id":"NVIDIA B300","name":"B300","memory":288,"pool":"BLACKWELL_288","price":{}}
		]}`)
	})

	got, err := m.Pools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Pool{
		{ID: "AMPERE_16", GPUNames: []string{"RTX 2000 Ada", "RTX 4000 Ada", "RTX A4000"}, MemoryGB: 16, PricePerHr: 0.69, Availability: "MEDIUM",
			Cards: []Card{
				{ID: "NVIDIA RTX 2000 Ada Generation", Name: "RTX 2000 Ada", MemoryGB: 16, PriceHr: 0.58, Availability: "MEDIUM"},
				{ID: "NVIDIA RTX 4000 Ada Generation", Name: "RTX 4000 Ada", MemoryGB: 20, PriceHr: 0.58, Availability: "NONE"},
				{ID: "NVIDIA RTX A4000", Name: "RTX A4000", MemoryGB: 16, PriceHr: 0.69, Availability: "LOW"},
			}},
		{ID: "AMPERE_24", GPUNames: []string{"L4", "L40"}, MemoryGB: 24, PricePerHr: 0.75,
			Cards: []Card{
				{ID: "NVIDIA L4", Name: "L4", MemoryGB: 24, PriceHr: 0.75},
				{ID: "NVIDIA L40", Name: "L40", MemoryGB: 48, PriceHr: 0.75},
			}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestMetrics_QueryPagesAndMapping(t *testing.T) {
	// Mid-minute on purpose, to check the floor.
	start := time.Date(2026, 11, 7, 20, 3, 27, 0, time.FixedZone("CET", 3600))
	var reqCursors []string
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("interval") != "1m" {
			t.Errorf("interval = %q, want 1m", q.Get("interval"))
		}
		if q.Get("startTime") != "2026-11-07T19:03:00Z" {
			t.Errorf("startTime = %q, want UTC RFC 3339 floored to the minute", q.Get("startTime"))
		}
		if q.Has("endTime") {
			t.Errorf("endTime sent (%q); it must be omitted", q.Get("endTime"))
		}
		switch r.URL.Path {
		case "/v2/serverless/ep_1/metrics/requests":
			reqCursors = append(reqCursors, q.Get("cursor"))
			if q.Get("cursor") == "" {
				writeJSON(w, 200, `{"records":[{"time":"2026-11-07T19:03:00Z","requests":1,"completed":1,"failed":0,"retries":0,
					"executionTimeMs":{"min":1000,"max":1000,"total":1000,"p70":1000,"p90":1000,"p95":1000,"p98":1000},
					"delayTimeMs":{"min":9000,"max":9000,"total":9000,"p70":9000,"p90":9000,"p95":9000,"p98":9000}}],
					"pagination":{"nextCursor":"r2","hasNextPage":true}}`)
				return
			}
			writeJSON(w, 200, `{"records":[{"time":"2026-11-07T19:05:00Z","requests":2,"completed":1,"failed":1,"retries":1,
				"executionTimeMs":{"min":900,"max":1100,"total":2000,"p70":1100,"p90":1100,"p95":1100,"p98":1100},
				"delayTimeMs":{"min":5,"max":7,"total":12,"p70":7,"p90":7,"p95":7,"p98":7}}],
				"pagination":{"nextCursor":null,"hasNextPage":false}}`)
		case "/v2/serverless/ep_1/metrics/cold-starts":
			writeJSON(w, 200, `{"records":[{"time":"2026-11-07T19:03:00Z","count":1,
				"durationMs":{"min":8200,"max":8200,"total":8200.5,"p70":8200,"p90":8200,"p95":8200,"p98":8200}}],
				"pagination":{"nextCursor":null,"hasNextPage":false}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	got, err := m.Metrics(context.Background(), "ep_1", start)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reqCursors, []string{"", "r2"}) {
		t.Errorf("request cursors = %q", reqCursors)
	}
	if len(got.Requests) != 2 || len(got.ColdStarts) != 1 {
		t.Fatalf("got %d request and %d cold-start buckets, want 2 and 1", len(got.Requests), len(got.ColdStarts))
	}
	r1 := got.Requests[1]
	if !r1.Time.Equal(time.Date(2026, 11, 7, 19, 5, 0, 0, time.UTC)) || r1.Requests != 2 || r1.Failed != 1 || r1.Retries != 1 {
		t.Errorf("request bucket = %+v", r1)
	}
	if r1.ExecutionTimeMs.P70 != 1100 || r1.DelayTimeMs.Max != 7 {
		t.Errorf("duration stats = %+v / %+v", r1.ExecutionTimeMs, r1.DelayTimeMs)
	}
	cs := got.ColdStarts[0]
	if cs.Count != 1 || cs.DurationMs.P98 != 8200 || cs.DurationMs.Total != 8200.5 {
		t.Errorf("cold-start bucket = %+v", cs)
	}
}

func TestMetrics_MissingRecordsIsEmpty(t *testing.T) {
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"pagination":{"nextCursor":null,"hasNextPage":false}}`)
	})
	got, err := m.Metrics(context.Background(), "ep_1", time.Now())
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(got.Requests) != 0 || len(got.ColdStarts) != 0 {
		t.Errorf("got %+v, want no buckets", got)
	}
}

// Both clients must use the transport with the HTTP/2 ping. A black-holed
// h2 connection can't be faked cheaply, so this checks the configuration.
func TestClientsUsePingTransport(t *testing.T) {
	m, err := NewMgmt("", testKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, rt := range map[string]http.RoundTripper{
		"job":  NewJobClient(DefaultJobBaseURL, testKey, nil).http.Transport,
		"mgmt": m.hc.Transport,
	} {
		retry, ok := rt.(*transport.Retry)
		if !ok {
			t.Errorf("%s transport = %T, want *transport.Retry", name, rt)
			continue
		}
		base, ok := retry.Base.(*http.Transport)
		if !ok {
			t.Errorf("%s base transport = %T, want *http.Transport", name, retry.Base)
			continue
		}
		if h2 := base.HTTP2; h2 == nil || h2.SendPingTimeout != 10*time.Second || h2.PingTimeout != 10*time.Second {
			t.Errorf("%s HTTP2 = %+v, want ping after 10 s, close after 10 s more", name, h2)
		}
		// The clone must still negotiate h2, or the ping settings do nothing.
		if !base.ForceAttemptHTTP2 || base.Proxy == nil {
			t.Errorf("%s base transport isn't a clone of http.DefaultTransport", name)
		}
	}
}

func TestCreateEndpoint_MinCudaVersion(t *testing.T) {
	for _, minCuda := range []string{"", "12.8"} {
		var got map[string]any
		m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body: %v", err)
			}
			writeJSON(w, http.StatusCreated, `{"id":"ep_123","name":"bench-coldprobe-x"}`)
		})
		_, err := m.CreateEndpoint(context.Background(), EndpointSpec{
			Name: NamePrefix + "x", Image: "img", Pool: "P1", FlashBoot: "OFF",
			MaxWorkers: 1, IdleTimeoutS: 5, MinCudaVersion: minCuda,
		})
		if err != nil {
			t.Fatal(err)
		}
		gpu, _ := got["gpu"].(map[string]any)
		keys := slices.Sorted(maps.Keys(gpu))
		want := []string{"pools"}
		if minCuda != "" {
			want = []string{"minCudaVersion", "pools"}
			if gpu["minCudaVersion"] != minCuda {
				t.Errorf("gpu.minCudaVersion = %#v, want %q", gpu["minCudaVersion"], minCuda)
			}
		}
		if !slices.Equal(keys, want) {
			t.Errorf("MinCudaVersion %q: gpu keys = %q, want %q", minCuda, keys, want)
		}
	}
}

// The pool's own fields stay the conservative roll-up (max price, best availability).
func TestPools_CardsPerPool(t *testing.T) {
	m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"gpus":[
			{"id":"NVIDIA RTX A4000","name":"RTX A4000","memory":16,"pool":"AMPERE_16","price":{"serverless":0.69},"availability":"LOW"},
			{"id":"NVIDIA RTX A4500","name":"RTX A4500","memory":20,"pool":"AMPERE_16","price":{"serverless":0.58},"availability":"HIGH"}
		]}`)
	})
	got, err := m.Pools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d pools, want 1: %+v", len(got), got)
	}
	p := got[0]
	wantCards := []Card{
		{ID: "NVIDIA RTX A4500", Name: "RTX A4500", MemoryGB: 20, PriceHr: 0.58, Availability: "HIGH"},
		{ID: "NVIDIA RTX A4000", Name: "RTX A4000", MemoryGB: 16, PriceHr: 0.69, Availability: "LOW"},
	}
	if !reflect.DeepEqual(p.Cards, wantCards) {
		t.Errorf("cards = %+v\nwant    %+v", p.Cards, wantCards)
	}
	if p.PricePerHr != 0.69 || p.Availability != "HIGH" || p.MemoryGB != 16 {
		t.Errorf("pool = $%v %q %d GB, want $0.69 (max) HIGH (best) 16 GB (min)", p.PricePerHr, p.Availability, p.MemoryGB)
	}
	if want := []string{"RTX A4500", "RTX A4000"}; !slices.Equal(p.GPUNames, want) {
		t.Errorf("gpuNames = %q, want %q (from Cards)", p.GPUNames, want)
	}
}

// excludedTypes appears only when non-empty: on PATCH an absent key clears them.
func TestCreateEndpoint_SendsExcludedTypes(t *testing.T) {
	for _, excluded := range [][]string{nil, {"NVIDIA RTX A4000", "NVIDIA RTX 2000 Ada Generation"}} {
		var got map[string]any
		m := newTestMgmt(t, func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body: %v", err)
			}
			writeJSON(w, http.StatusCreated, `{"id":"ep_123","name":"bench-coldprobe-x"}`)
		})
		_, err := m.CreateEndpoint(context.Background(), EndpointSpec{
			Name: NamePrefix + "x", Image: "img", Pool: "AMPERE_16", FlashBoot: "OFF",
			MaxWorkers: 1, IdleTimeoutS: 5, ExcludedTypes: excluded,
		})
		if err != nil {
			t.Fatal(err)
		}
		gpu, _ := got["gpu"].(map[string]any)
		v, present := gpu["excludedTypes"]
		if len(excluded) == 0 {
			if present {
				t.Errorf("no exclusions: gpu.excludedTypes = %#v, want key absent", v)
			}
			continue
		}
		want := []any{"NVIDIA RTX A4000", "NVIDIA RTX 2000 Ada Generation"}
		if !reflect.DeepEqual(v, want) {
			t.Errorf("gpu.excludedTypes = %#v, want %#v", v, want)
		}
	}
}
