package runpod

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Handlers run off the test goroutine: they use t.Errorf, not t.Fatalf.

func TestRunSync_Completed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer k")
		}
		if r.URL.Path != "/ep1/runsync" {
			t.Errorf("path = %q, want /ep1/runsync", r.URL.Path)
		}
		if got := r.URL.Query().Get("wait"); got != "90000" {
			t.Errorf("wait = %q, want 90000", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"input":{"sleep_ms":5}}` {
			t.Errorf("body = %s", body)
		}
		w.Write([]byte(`{"id":"j1","status":"COMPLETED","delayTime":12,"executionTime":5,"workerId":"w1","output":{"slept_ms":5}}`))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	res, err := c.RunSync(context.Background(), "ep1", map[string]int{"sleep_ms": 5}, 90*time.Second)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if res.ID != "j1" || res.Status != "COMPLETED" || res.DelayTime != 12 ||
		res.ExecutionTime != 5 || res.WorkerID != "w1" || res.Error != "" {
		t.Errorf("result = %+v", res)
	}
	if string(res.Output) != `{"slept_ms":5}` {
		t.Errorf("output = %s", res.Output)
	}
}

func TestRunSync_FallsBackToStatusPolling(t *testing.T) {
	var statusHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ep1/runsync":
			w.Write([]byte(`{"id":"j1","status":"IN_PROGRESS"}`))
		case "/ep1/status/j1":
			if statusHits.Add(1) == 1 {
				w.Write([]byte(`{"id":"j1","status":"IN_PROGRESS"}`))
				return
			}
			w.Write([]byte(`{"id":"j1","status":"COMPLETED","executionTime":7}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	res, err := c.RunSync(context.Background(), "ep1", map[string]int{"sleep_ms": 5}, time.Second)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if res.Status != "COMPLETED" || res.ExecutionTime != 7 {
		t.Errorf("result = %+v", res)
	}
	if got := statusHits.Load(); got != 2 {
		t.Errorf("status hits = %d, want 2", got)
	}
}

// Waits are scaled down 10x. Not parallel: it changes package vars.
func TestRunSync_StatusPollBacksOff(t *testing.T) {
	oldStart, oldMax := statusPollStart, statusPollMax
	statusPollStart, statusPollMax = 20*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { statusPollStart, statusPollMax = oldStart, oldMax })

	// arrivals: the runsync, then every /status poll.
	var mu sync.Mutex
	var arrivals []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		n := len(arrivals)
		mu.Unlock()
		// Request 1 is the runsync, 2..6 are unfinished polls, 7 finishes.
		if n < 7 {
			w.Write([]byte(`{"id":"j1","status":"IN_PROGRESS"}`))
			return
		}
		w.Write([]byte(`{"id":"j1","status":"COMPLETED"}`))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	res, err := c.RunSync(context.Background(), "ep1", map[string]int{}, time.Second)
	if err != nil || res.Status != "COMPLETED" {
		t.Fatalf("RunSync = %+v, %v; want COMPLETED", res, err)
	}

	mu.Lock()
	defer mu.Unlock()
	// 160 -> 200, not 320, shows the cap is applied.
	want := []time.Duration{20, 40, 80, 160, 200, 200}
	if len(arrivals) != len(want)+1 {
		t.Fatalf("got %d requests, want %d", len(arrivals), len(want)+1)
	}
	for i, w := range want {
		w *= time.Millisecond
		gap := arrivals[i+1].Sub(arrivals[i])
		// Lower bounds catch a missing doubling; gap 5 would be 320 ms uncapped.
		slack := max(w/2, 20*time.Millisecond)
		if gap < w || gap >= w+slack+20*time.Millisecond {
			t.Errorf("gap %d = %v, want %v (+ under %v)", i+1, gap, w, slack+20*time.Millisecond)
		}
	}
}

func TestRunSync_ContextCancelStopsPolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"j1","status":"IN_QUEUE"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	c := NewJobClient(srv.URL, "k", nil)
	res, err := c.RunSync(ctx, "ep1", map[string]int{"sleep_ms": 5}, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if res.ID != "j1" {
		t.Errorf("res.ID = %q, want j1", res.ID)
	}
}

func TestRunSync_UnfinishedWithoutIDFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"IN_QUEUE"}`))
	}))
	defer srv.Close()

	// The deadline makes a poll-without-ID bug fail fast instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c := NewJobClient(srv.URL, "k", nil)
	_, err := c.RunSync(ctx, "ep1", map[string]int{}, time.Second)
	if err == nil {
		t.Fatal("want an error for an unfinished job without an id")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; RunSync polled until the deadline instead of failing at once", err)
	}
}

func TestAPIError_ParsesProblemJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"title":"Bad","status":400,"detail":"pools"}`))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	_, err := c.RunSync(context.Background(), "ep1", map[string]int{}, time.Second)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Title != "Bad" || apiErr.Detail != "pools" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestAPIError_PlainBodyAndRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("  not allowed \n"))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	err := c.PurgeQueue(context.Background(), "ep1")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 403 || apiErr.Detail != "not allowed" || apiErr.RetryAfter != 7*time.Second {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestParseRetryAfter(t *testing.T) {
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	tests := []struct {
		name   string
		header string
		min    time.Duration
		max    time.Duration
	}{
		{"empty", "", 0, 0},
		{"seconds", "5", 5 * time.Second, 5 * time.Second},
		{"garbage", "soon", 0, 0},
		{"negative", "-3", 0, 0},
		// HTTP-dates have 1 s resolution, so allow the truncation.
		{"http-date", future, 28 * time.Second, 30 * time.Second},
		{"past http-date", "Mon, 02 Jan 2006 15:04:05 GMT", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.header)
			if got < tt.min || got > tt.max {
				t.Errorf("parseRetryAfter(%q) = %v, want in [%v, %v]", tt.header, got, tt.min, tt.max)
			}
		})
	}
}

func TestPurgeQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/ep1/purge-queue" {
			t.Errorf("got %s %s, want POST /ep1/purge-queue", r.Method, r.URL.Path)
		}
		w.Write([]byte(`{"removed":0,"status":"completed"}`))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	if err := c.PurgeQueue(context.Background(), "ep1"); err != nil {
		t.Fatalf("PurgeQueue: %v", err)
	}
}

func TestErrorsNeverContainKey(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("bad auth header: " + r.Header.Get("Authorization")))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "secret-key-123", nil) // gitleaks:allow (test fixture)
	_, err := c.RunSync(context.Background(), "ep1", map[string]int{}, time.Second)
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "secret-key-123") { // gitleaks:allow (test fixture)
		t.Errorf("error leaks the key: %v", err)
	}
	// A caller may log apiErr.Detail directly.
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if strings.Contains(apiErr.Detail, "secret-key-123") { // gitleaks:allow (test fixture)
		t.Errorf("apiErr.Detail leaks the key: %q", apiErr.Detail)
	}
	// A 5xx on runsync may mean the job was already queued (and billed).
	if got := hits.Load(); got != 1 {
		t.Errorf("hits = %d, want 1 (POST must not be retried on 5xx)", got)
	}
}

func TestRunSync_RetriesOn429WithSameBody(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"input":{"sleep_ms":5}}` {
			t.Errorf("attempt %d body = %q", hits.Load()+1, body)
		}
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"id":"j1","status":"COMPLETED"}`))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	res, err := c.RunSync(context.Background(), "ep1", map[string]int{"sleep_ms": 5}, time.Second)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if res.Status != "COMPLETED" || hits.Load() != 2 {
		t.Errorf("status = %s, hits = %d; want COMPLETED after 2 hits", res.Status, hits.Load())
	}
}

// /status is a GET, so the transport retries its 429s.
func TestStatus_RetriesOn429(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"id":"j1","status":"COMPLETED"}`))
	}))
	defer srv.Close()

	c := NewJobClient(srv.URL, "k", nil)
	res, err := c.Status(context.Background(), "ep1", "j1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Status != "COMPLETED" || hits.Load() != 2 {
		t.Errorf("status = %s, hits = %d; want COMPLETED after 2 hits", res.Status, hits.Load())
	}
}

func TestJobResult_ErrorStringOrObject(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"string", `{"id":"j1","status":"FAILED","error":"boom"}`, "boom"},
		{"object kept as raw JSON", `{"id":"j1","status":"FAILED","error":{"type":"OOM"}}`, `{"type":"OOM"}`},
		{"null", `{"id":"j1","status":"COMPLETED","error":null}`, ""},
		{"absent", `{"id":"j1","status":"COMPLETED"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := NewJobClient(srv.URL, "k", nil)
			res, err := c.Status(context.Background(), "ep1", "j1")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if res.Error != tt.want {
				t.Errorf("Error = %q, want %q", res.Error, tt.want)
			}
		})
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []string{"COMPLETED", "FAILED", "CANCELLED", "TIMED_OUT"} {
		if !(JobResult{Status: s}).Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []string{"IN_QUEUE", "IN_PROGRESS", ""} {
		if (JobResult{Status: s}).Terminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}
