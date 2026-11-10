// Package runpod talks to Runpod Serverless. The job API isn't in the
// management OpenAPI spec, so its client is hand-written.
package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/runpod/go-sdk/v2/transport"
)

// DefaultJobBaseURL is the job API host, distinct from the management API's.
const DefaultJobBaseURL = "https://api.runpod.ai/v2"

// maxStatusPoll caps the wait between /status polls. Runpod allows 2000
// /status requests per 10 s per endpoint; 400 in-flight jobs polling every
// 2 s sit at that limit, and the transport retries any 429 overflow.
const maxStatusPoll = 2 * time.Second

// RunSync's poll backoff; vars so tests can shrink them.
var (
	statusPollStart = 200 * time.Millisecond
	statusPollMax   = maxStatusPoll
)

// JobClient sends jobs to Serverless endpoints. It is safe for concurrent use.
type JobClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewJobClient returns a client for baseURL. A nil hc uses newTransport with
// no client timeout: runsync blocks for up to wait, so callers bound each
// call with a context deadline.
func NewJobClient(baseURL, apiKey string, hc *http.Client) *JobClient {
	if hc == nil {
		hc = &http.Client{Transport: newTransport()}
	}
	return &JobClient{baseURL: baseURL, apiKey: apiKey, http: hc}
}

// newTransport is the SDK's retry transport over an HTTP/2 transport that
// pings idle connections, so a silently dead connection is dropped in ~20 s
// instead of hanging every request until TCP keepalive gives up.
func newTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 10 * time.Second, PingTimeout: 10 * time.Second}
	return &transport.Retry{Base: t}
}

// JobResult is a job's state as returned by /runsync and /status.
type JobResult struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	DelayTime     int64           `json:"delayTime"`     // ms in the queue (includes cold start)
	ExecutionTime int64           `json:"executionTime"` // ms the handler ran
	WorkerID      string          `json:"workerId"`
	Output        json.RawMessage `json:"output"` // left as raw JSON; only the caller knows its shape
	// Error is a string or, when Runpod returns an object, its raw JSON.
	Error string `json:"error"`
}

// Terminal reports whether the job has finished. A FAILED job is a result, not a Go error.
func (r JobResult) Terminal() bool {
	switch r.Status {
	case "COMPLETED", "FAILED", "CANCELLED", "TIMED_OUT":
		return true
	}
	return false
}

// jobResponse is the wire form of JobResult. The shallower Error field wins
// the "error" key, so it gets the raw value whether string or object.
type jobResponse struct {
	JobResult
	Error json.RawMessage `json:"error"`
}

// result converts the wire form, turning the raw error into text.
func (w jobResponse) result() JobResult {
	r := w.JobResult
	var s string
	switch {
	case len(w.Error) == 0 || string(w.Error) == "null":
	case json.Unmarshal(w.Error, &s) == nil:
		r.Error = s
	default:
		r.Error = string(w.Error)
	}
	return r
}

// RunSync submits input to /runsync, which holds the connection for up to
// wait, then polls /status with backoff until the job finishes or ctx is
// done. On a ctx error the last result is returned so the caller keeps the
// job ID. Keep wait at 60 s or less: a longer wait with no worker placed got
// a dropped response instead of IN_QUEUE.
func (c *JobClient) RunSync(ctx context.Context, endpointID string, input any, wait time.Duration) (JobResult, error) {
	path := "/" + url.PathEscape(endpointID) + "/runsync?wait=" + strconv.FormatInt(wait.Milliseconds(), 10)
	body := map[string]any{"input": input}

	var w jobResponse
	if err := c.do(ctx, http.MethodPost, path, body, &w); err != nil {
		return JobResult{}, wrapRedacted("runsync", err, c.apiKey)
	}
	res := w.result()

	delay := statusPollStart
	for !res.Terminal() {
		if res.ID == "" {
			err := fmt.Errorf("unfinished job (status %q) has no id", res.Status)
			return res, wrapRedacted("runsync", err, c.apiKey)
		}
		select {
		case <-ctx.Done():
			return res, wrapRedacted("poll job "+res.ID, ctx.Err(), c.apiKey)
		case <-time.After(delay):
		}
		next, err := c.Status(ctx, endpointID, res.ID)
		if err != nil {
			return res, err // already wrapped and redacted by Status
		}
		res = next
		delay = min(delay*2, statusPollMax)
	}
	return res, nil
}

// Status fetches a job's current state from /status/{jobID}.
func (c *JobClient) Status(ctx context.Context, endpointID, jobID string) (JobResult, error) {
	path := "/" + url.PathEscape(endpointID) + "/status/" + url.PathEscape(jobID)
	var w jobResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &w); err != nil {
		return JobResult{}, wrapRedacted("status "+jobID, err, c.apiKey)
	}
	return w.result(), nil
}

// PurgeQueue drops every queued (not yet running) job on endpointID.
func (c *JobClient) PurgeQueue(ctx context.Context, endpointID string) error {
	path := "/" + url.PathEscape(endpointID) + "/purge-queue"
	if err := c.do(ctx, http.MethodPost, path, nil, nil); err != nil {
		return wrapRedacted("purge-queue", err, c.apiKey)
	}
	return nil
}

// do sends one job-API request.
func (c *JobClient) do(ctx context.Context, method, path string, in, out any) error {
	return doJSON(ctx, c.http, c.baseURL, c.apiKey, method, path, in, out)
}

// doJSON sends one authenticated JSON request to baseURL+path and decodes a
// 2xx response into out (nil skips decoding); non-2xx becomes an *APIError.
// Errors are unredacted. Mgmt uses it too, for metrics, with a base URL
// that lacks the /v2 the job base carries.
func doJSON(ctx context.Context, hc *http.Client, baseURL, apiKey, method, path string, in, out any) error {
	// Only a *bytes.Reader body can be rewound, and the retry transport
	// won't replay (retry a 429) what it can't rewind.
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return err // a *url.Error; ctx errors unwrap through it
	}
	// Drain up to 4 KiB so the HTTP/1.1 connection can be reused.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp, apiKey)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
