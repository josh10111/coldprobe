package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	runpodsdk "github.com/runpod/go-sdk/v2"
	"github.com/runpod/go-sdk/v2/lib/catalog"
	"github.com/runpod/go-sdk/v2/lib/serverless"
	"github.com/runpod/go-sdk/v2/lib/types"
)

// NamePrefix marks every endpoint coldprobe creates; the API has no tags,
// so the name is the only ownership mark.
const NamePrefix = "bench-coldprobe-"

// Fixed parts of the create body.
const (
	queueDelaySeconds = 4      // scale up when a job has waited this long
	jobTimeoutMs      = 300000 // the API's default, stated so it can't drift
	metricsInterval   = "1m"   // smallest bucket; a benchmark run is minutes long
	mgmtHTTPTimeout   = 60 * time.Second
)

// Mgmt calls Runpod's management API. It is safe for concurrent use.
type Mgmt struct {
	c      *runpodsdk.Client
	apiKey string // sent by doJSON, and redacted from errors
	// hc and baseURL serve metrics, which the SDK lacks; baseURL has no /v2.
	hc      *http.Client
	baseURL string
}

// NewMgmt returns a management client. baseURL "" means the production host.
func NewMgmt(baseURL, apiKey string) (*Mgmt, error) {
	if baseURL == "" {
		baseURL = runpodsdk.DefaultBaseURL
	}
	// One client for the SDK and our raw GETs: a shared pool and retry policy.
	hc := &http.Client{Timeout: mgmtHTTPTimeout, Transport: newTransport()}
	c, err := runpodsdk.New(
		runpodsdk.WithAPIKey(apiKey),
		runpodsdk.WithBaseURL(baseURL),
		runpodsdk.WithHTTPClient(hc),
	)
	if err != nil {
		return nil, wrapRedacted("new mgmt client", err, apiKey)
	}
	return &Mgmt{c: c, apiKey: apiKey, hc: hc, baseURL: baseURL}, nil
}

// EndpointSpec is the part of an endpoint that varies between runs.
type EndpointSpec struct {
	Name, Image, Pool, FlashBoot string
	MaxWorkers, IdleTimeoutS     int
	Env                          map[string]string
	// MinCudaVersion is the lowest host CUDA version, "major.minor"; "" omits it.
	MinCudaVersion string
	// ExcludedTypes are catalog GPU ids subtracted from Pool, to pin one card.
	ExcludedTypes []string
}

// CreateEndpoint creates a queue-based Serverless endpoint and returns its
// ID. A name without NamePrefix is refused: we could never find it to delete.
func (m *Mgmt) CreateEndpoint(ctx context.Context, s EndpointSpec) (string, error) {
	if !strings.HasPrefix(s.Name, NamePrefix) {
		err := fmt.Errorf("name %q must start with %q", s.Name, NamePrefix)
		return "", wrapRedacted("create endpoint", err, m.apiKey)
	}

	// The oneOf union can only be filled via its From... helper.
	var scaling types.EndpointScaling
	err := scaling.FromQueueDelayScaling(types.QueueDelayScaling{
		Type:       types.QueueDelayScalingTypeQueueDelay,
		QueueDelay: queueDelaySeconds,
	})
	if err != nil {
		return "", wrapRedacted("create endpoint", err, m.apiKey)
	}

	gpu := &types.CreateEndpointGpuConfig{Pools: []string{s.Pool}}
	if s.MinCudaVersion != "" {
		gpu.MinCudaVersion = &s.MinCudaVersion
	}
	if len(s.ExcludedTypes) > 0 {
		gpu.ExcludedTypes = &s.ExcludedTypes
	}

	flashboot := types.FlashBoot(s.FlashBoot)
	body := serverless.CreateEndpointJSONRequestBody{
		Name:      s.Name,
		Type:      types.EndpointTypeQueue,
		Scaling:   scaling,
		Image:     &s.Image,
		Gpu:       gpu,
		Flashboot: &flashboot,
		Timeout:   runpodsdk.Ptr(jobTimeoutMs),
		// Min is a pointer to 0, not nil, so "min" is sent.
		Workers: &struct {
			IdleTimeout *int `json:"idleTimeout,omitempty"`
			Max         *int `json:"max,omitempty"`
			Min         *int `json:"min,omitempty"`
		}{
			IdleTimeout: &s.IdleTimeoutS,
			Max:         &s.MaxWorkers,
			Min:         runpodsdk.Ptr(0),
		},
	}
	if len(s.Env) > 0 {
		body.Env = &s.Env
	}

	res, err := m.c.Serverless.CreateEndpointWithResponse(ctx, body)
	if err != nil {
		return "", wrapRedacted("create endpoint", err, m.apiKey)
	}
	if res.JSON201 == nil {
		return "", m.resultError("create endpoint", res.HTTPResponse, res.Body)
	}
	return res.JSON201.Id, nil
}

// DeleteEndpoint deletes an endpoint, stopping its workers and queued jobs.
// A 404 counts as success, since teardown may run twice.
func (m *Mgmt) DeleteEndpoint(ctx context.Context, id string) error {
	res, err := m.c.Serverless.DeleteEndpointWithResponse(ctx, id)
	if err != nil {
		return wrapRedacted("delete endpoint "+id, err, m.apiKey)
	}
	status := res.HTTPResponse.StatusCode
	if status == http.StatusNotFound || (status >= 200 && status <= 299) {
		return nil
	}
	return m.resultError("delete endpoint "+id, res.HTTPResponse, res.Body)
}

// SetMaxWorkers PATCHes an endpoint's workers.max to n; n = 0 pauses it and
// stops its workers. Any non-2xx is an error, 404 included, and a 5xx isn't
// retried, so the caller decides whether to try again.
func (m *Mgmt) SetMaxWorkers(ctx context.Context, id string, n int) error {
	op := fmt.Sprintf("set workers.max=%d on endpoint %s", n, id)
	body := types.UpdateEndpointRequest{Workers: &types.EndpointWorkers{Max: &n}}
	res, err := m.c.Serverless.UpdateEndpointWithResponse(ctx, id, body)
	if err != nil {
		return wrapRedacted(op, err, m.apiKey)
	}
	if status := res.HTTPResponse.StatusCode; status < 200 || status > 299 {
		return m.resultError(op, res.HTTPResponse, res.Body)
	}
	return nil
}

// Endpoint is the part of a listed endpoint we use.
type Endpoint struct{ ID, Name string }

// ListEndpoints returns every endpoint on the account, following pagination.
func (m *Mgmt) ListEndpoints(ctx context.Context) ([]Endpoint, error) {
	var all []Endpoint
	params := &serverless.ListEndpointsParams{}
	for {
		res, err := m.c.Serverless.ListEndpointsWithResponse(ctx, params)
		if err != nil {
			return nil, wrapRedacted("list endpoints", err, m.apiKey)
		}
		if res.JSON200 == nil {
			return nil, m.resultError("list endpoints", res.HTTPResponse, res.Body)
		}
		for _, e := range res.JSON200.Endpoints {
			all = append(all, Endpoint{ID: e.Id, Name: e.Name})
		}
		next, err := nextCursor(res.JSON200.Pagination)
		if err != nil {
			return nil, wrapRedacted("list endpoints", err, m.apiKey)
		}
		if next == nil {
			return all, nil
		}
		params.Cursor = next
	}
}

// nextCursor returns the next page's cursor, or nil on the last page.
// hasNextPage without a cursor is an error, not an endless loop on page 1.
func nextCursor(p types.Pagination) (*string, error) {
	if !p.HasNextPage {
		return nil, nil
	}
	if p.NextCursor == nil || *p.NextCursor == "" {
		return nil, errors.New("response says hasNextPage but has no nextCursor")
	}
	return p.NextCursor, nil
}

// WorkerSummary is an endpoint's workers counted by status, from
// GET /v2/serverless/{id}/workers.
type WorkerSummary struct {
	Running, Idle, Initializing, Throttled, Unhealthy int
	// Total counts listed workers, not workers up: a scaled-down worker
	// stays listed. See Active.
	Total int
	// GPUTypes maps worker ID to GPU type ID, for workers already placed.
	GPUTypes map[string]string

	active int
}

// Active is the number of workers whose container is up, i.e. billing.
// After the idle timeout a worker stays listed as IDLE with a null
// uptimeSeconds, so IDLE counts only with a non-null uptime.
func (w WorkerSummary) Active() int { return w.active }

// isActive applies Active's rule. A null uptime is the signal, not 0:
// a warm worker can report 0 in its first second.
func isActive(w types.Worker) bool {
	switch w.Status {
	case types.WorkerStatusRunning, types.WorkerStatusInitializing:
		return true
	case types.WorkerStatusIdle:
		return w.UptimeSeconds != nil
	}
	// THROTTLED and UNHEALTHY: inferred from the spec, never observed.
	return false
}

// Workers reads an endpoint's current workers.
func (m *Mgmt) Workers(ctx context.Context, id string) (WorkerSummary, error) {
	res, err := m.c.Serverless.ListEndpointWorkersWithResponse(ctx, id)
	if err != nil {
		return WorkerSummary{}, wrapRedacted("workers "+id, err, m.apiKey)
	}
	if res.JSON200 == nil {
		return WorkerSummary{}, m.resultError("workers "+id, res.HTTPResponse, res.Body)
	}
	s := res.JSON200.Summary
	out := WorkerSummary{
		Running:      s.Running,
		Idle:         s.Idle,
		Initializing: s.Initializing,
		Throttled:    s.Throttled,
		Unhealthy:    s.Unhealthy,
		Total:        s.Total,
		GPUTypes:     map[string]string{},
	}
	for _, w := range res.JSON200.Workers {
		if w.GpuTypeId != nil {
			out.GPUTypes[w.Id] = *w.GpuTypeId
		}
		if isActive(w) {
			out.active++
		}
	}
	return out, nil
}

// Card is one GPU model in a pool, as the catalog lists it.
type Card struct {
	// ID is the catalog's gpus[].id, as excludedTypes and gpuTypeId use it.
	ID       string `json:"id"`
	Name     string `json:"name"`
	MemoryGB int    `json:"memoryGb"`
	// PriceHr is the serverless price in USD per GPU-hour.
	PriceHr float64 `json:"priceHr"`
	// Availability is HIGH, MEDIUM, LOW, NONE, or "" if unknown.
	Availability string `json:"availability"`
}

// Pool is a Serverless GPU pool: the unit an endpoint selects GPUs by.
type Pool struct {
	ID string `json:"id"`
	// GPUNames is Cards' names in order, for older clients.
	GPUNames []string `json:"gpuNames"`
	// Cards are the pool's GPUs, cheapest first, then by id.
	Cards []Card `json:"cards"`
	// MemoryGB is the smallest VRAM in the pool, all a job can rely on.
	MemoryGB int `json:"memoryGb"`
	// PricePerHr is the pool's highest serverless price: a worker can land
	// on any GPU, so the budget guard assumes the dearest.
	PricePerHr float64 `json:"pricePerHr"`
	// Availability is the pool's best card's level, or "" if unknown.
	Availability string `json:"availability"`
}

// availabilityRank orders the API's levels, worst first.
var availabilityRank = []types.AvailabilityLevel{
	types.AvailabilityLevelNone, types.AvailabilityLevelLow,
	types.AvailabilityLevelMedium, types.AvailabilityLevelHigh,
}

// Pools reads the GPU catalog and groups Serverless GPUs by pool, cheapest
// first. GPUs with no pool or no serverless price are left out.
func (m *Mgmt) Pools(ctx context.Context) ([]Pool, error) {
	// Same query as the console: availability scoped to Serverless.
	params := &catalog.ListGpuTypesParams{
		Include: &types.CatalogIncludeParam{types.CatalogIncludeAvailability},
		Product: &types.GpuProductFilter{types.ProductServerless},
	}
	res, err := m.c.Catalog.ListGpuTypesWithResponse(ctx, params)
	if err != nil {
		return nil, wrapRedacted("gpu catalog", err, m.apiKey)
	}
	if res.JSON200 == nil {
		return nil, m.resultError("gpu catalog", res.HTTPResponse, res.Body)
	}

	byID := map[string]*Pool{}
	for _, g := range res.JSON200.Gpus {
		if g.Pool == nil || g.Price.Serverless == nil {
			continue
		}
		price := float32To64(*g.Price.Serverless)
		p, ok := byID[*g.Pool]
		if !ok {
			p = &Pool{ID: *g.Pool, MemoryGB: g.Memory, PricePerHr: price}
			byID[*g.Pool] = p
		}
		card := Card{ID: g.Id, Name: g.Name, MemoryGB: g.Memory, PriceHr: price}
		if a := g.Availability; a != nil && a.Valid() {
			card.Availability = string(*a)
		}
		p.Cards = append(p.Cards, card)
		p.MemoryGB = min(p.MemoryGB, g.Memory)
		p.PricePerHr = max(p.PricePerHr, price)
		if betterAvailability(card.Availability, p.Availability) {
			p.Availability = card.Availability
		}
	}

	// By price, then ID.
	pools := make([]Pool, 0, len(byID))
	for _, p := range byID {
		sortCards(p.Cards)
		for _, c := range p.Cards {
			p.GPUNames = append(p.GPUNames, c.Name)
		}
		pools = append(pools, *p)
	}
	sort.Slice(pools, func(i, j int) bool {
		if pools[i].PricePerHr != pools[j].PricePerHr {
			return pools[i].PricePerHr < pools[j].PricePerHr
		}
		return pools[i].ID < pools[j].ID
	})
	return pools, nil
}

// betterAvailability reports whether level a beats b; "" loses to any level.
func betterAvailability(a, b string) bool {
	if a == "" {
		return false
	}
	return b == "" || slices.Index(availabilityRank, types.AvailabilityLevel(a)) >
		slices.Index(availabilityRank, types.AvailabilityLevel(b))
}

// sortCards orders cards by price, then id, so the list is stable.
func sortCards(cards []Card) {
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].PriceHr != cards[j].PriceHr {
			return cards[i].PriceHr < cards[j].PriceHr
		}
		return cards[i].ID < cards[j].ID
	})
}

// float32To64 converts an SDK float32 price to the float64 with the same
// shortest decimal, so 0.69 doesn't become 0.6899999976158142.
func float32To64(f float32) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	return v
}

// DurationStats is one bucket's statistics, in ms. The API has no p50.
type DurationStats struct {
	Min   int     `json:"min"`
	Max   int     `json:"max"`
	Total float64 `json:"total"` // sum over every request in the bucket
	P70   int     `json:"p70"`
	P90   int     `json:"p90"`
	P95   int     `json:"p95"`
	P98   int     `json:"p98"`
}

// RequestBucket is one record of /metrics/requests.
type RequestBucket struct {
	Time            time.Time     `json:"time"` // bucket start, UTC
	Requests        int           `json:"requests"`
	Completed       int           `json:"completed"`
	Failed          int           `json:"failed"`
	Retries         int           `json:"retries"`
	ExecutionTimeMs DurationStats `json:"executionTimeMs"`
	DelayTimeMs     DurationStats `json:"delayTimeMs"`
}

// ColdStartBucket is one record of /metrics/cold-starts.
type ColdStartBucket struct {
	Time       time.Time     `json:"time"` // bucket start, UTC
	Count      int           `json:"count"`
	DurationMs DurationStats `json:"durationMs"`
}

// Metrics is Runpod's own view of a run, to compare with client-side numbers.
type Metrics struct {
	Requests   []RequestBucket   `json:"requests"`
	ColdStarts []ColdStartBucket `json:"coldStarts"`
}

// Metrics reads an endpoint's request and cold-start series in 1-minute
// buckets from start until now. Empty buckets are absent, not zero. No
// endTime is sent: it is exclusive and could cut off the filling bucket.
func (m *Mgmt) Metrics(ctx context.Context, id string, start time.Time) (Metrics, error) {
	var out Metrics
	err := m.metricPages(ctx, id, "requests", start, func(records json.RawMessage) error {
		var page []RequestBucket
		if err := json.Unmarshal(records, &page); err != nil {
			return err
		}
		out.Requests = append(out.Requests, page...)
		return nil
	})
	if err != nil {
		return Metrics{}, err
	}
	err = m.metricPages(ctx, id, "cold-starts", start, func(records json.RawMessage) error {
		var page []ColdStartBucket
		if err := json.Unmarshal(records, &page); err != nil {
			return err
		}
		out.ColdStarts = append(out.ColdStarts, page...)
		return nil
	})
	if err != nil {
		return Metrics{}, err
	}
	return out, nil
}

// metricPages GETs every page of one metrics series and hands each page's
// records to add.
func (m *Mgmt) metricPages(ctx context.Context, id, series string, start time.Time, add func(json.RawMessage) error) error {
	op := "metrics " + series + " " + id
	q := url.Values{}
	// Floor to the minute so the bucket holding the first cold start is in.
	// Later pages must resend the same startTime/interval as the cursor's.
	q.Set("startTime", start.UTC().Truncate(time.Minute).Format(time.RFC3339))
	q.Set("interval", metricsInterval)
	path := "/v2/serverless/" + url.PathEscape(id) + "/metrics/" + series

	for {
		var page struct {
			Records    json.RawMessage  `json:"records"`
			Pagination types.Pagination `json:"pagination"`
		}
		if err := doJSON(ctx, m.hc, m.baseURL, m.apiKey, http.MethodGet, path+"?"+q.Encode(), nil, &page); err != nil {
			return wrapRedacted(op, err, m.apiKey)
		}
		// A page without "records" would fail to decode; treat it as empty.
		if len(page.Records) > 0 {
			if err := add(page.Records); err != nil {
				return wrapRedacted(op, err, m.apiKey)
			}
		}
		next, err := nextCursor(page.Pagination)
		if err != nil {
			return wrapRedacted(op, err, m.apiKey)
		}
		if next == nil {
			return nil
		}
		q.Set("cursor", *next)
	}
}

// resultError explains why a generated ...WithResponse result has no decoded
// body: a non-2xx status, or a 2xx body that wasn't JSON.
func (m *Mgmt) resultError(op string, resp *http.Response, body []byte) error {
	var err error
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		err = fmt.Errorf("HTTP %d with unexpected Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	} else {
		err = apiErrorFromBody(resp.StatusCode, resp.Header, body, m.apiKey)
	}
	return wrapRedacted(op, err, m.apiKey)
}
