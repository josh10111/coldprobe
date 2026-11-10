// Package server is coldprobe's HTTP API: JSON endpoints, a Server-Sent
// Events stream per run, and the embedded UI.
package server

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/josh10111/coldprobe/internal/bench"
	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/store"
)

const (
	// pingEvery is the SSE heartbeat; proxies often drop streams idle 30-60 s.
	pingEvery = 15 * time.Second
	// maxBody caps a request body; Params is well under 1 KiB.
	maxBody = 64 << 10
	// sweepTimeout bounds the pre-run janitor so it can't hold startMu long.
	sweepTimeout = 15 * time.Second
	// The GPU catalog cache's defaults; see Deps and catalog.
	defaultCatalogTTL      = 5 * time.Minute
	defaultCatalogTimeout  = 10 * time.Second
	defaultCatalogRetry    = 30 * time.Second
	defaultCatalogMaxStale = time.Hour
)

// Deps is everything the handlers need. main builds it once.
type Deps struct {
	// RootCtx parents every run. It must be the app's lifetime context,
	// never a request's, which ends when the POST returns.
	RootCtx context.Context
	Store   *store.Store
	Runner  *bench.Runner
	Mgmt    *runpod.Mgmt
	Hub     *Hub
	CapUSD  float64 // no run's budget may exceed it
	Image   string  // the default in /api/defaults
	Static  fs.FS   // the built UI, index.html at the root
	// Addr is the listen address. A specific host in it is accepted in the
	// Host header alongside the loopback names.
	Addr string
	Log  *slog.Logger
	// The GPU catalog cache (see catalog). Zero means the default in brackets.
	CatalogTTL      time.Duration // how long a read is reused [5 min]
	CatalogTimeout  time.Duration // bound on one read, retries included [10 s]
	CatalogRetry    time.Duration // after a failed read, serve the stale copy this long before trying again [30 s]
	CatalogMaxStale time.Duration // a copy older than this is never served [1 h]
}

// server is Deps plus the handler methods.
type server struct {
	Deps
	files        http.Handler    // serves files out of Static
	allowedHosts map[string]bool // Host header values checkHost accepts (no port)

	// startMu makes "is a run active?" and Runner.Start one atomic step.
	startMu sync.Mutex

	// catMu guards the cached GPU catalog.
	catMu      sync.Mutex
	catPools   []runpod.Pool
	catAt      time.Time
	catRetryAt time.Time
}

// New returns the HTTP handler with every route registered, behind
// checkHost (Host must be a loopback name or Addr's host: blocks DNS
// rebinding) and http.CrossOriginProtection (refuses cross-origin POSTs:
// blocks CSRF).
func New(d Deps) http.Handler {
	d.CatalogTTL = cmp.Or(d.CatalogTTL, defaultCatalogTTL)
	d.CatalogTimeout = cmp.Or(d.CatalogTimeout, defaultCatalogTimeout)
	d.CatalogRetry = cmp.Or(d.CatalogRetry, defaultCatalogRetry)
	d.CatalogMaxStale = cmp.Or(d.CatalogMaxStale, defaultCatalogMaxStale)
	s := &server{Deps: d, files: http.FileServerFS(d.Static), allowedHosts: allowedHosts(d.Addr)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/gpus", s.gpus)
	mux.HandleFunc("GET /api/defaults", s.defaults)
	mux.HandleFunc("POST /api/runs/estimate", s.estimate)
	mux.HandleFunc("POST /api/runs", s.startRun)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("GET /api/runs/{id}/events", s.events)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancel)
	// "/" has no method so a wrong method on an API path gets our problem
	// JSON rather than the mux's plain-text 405.
	mux.HandleFunc("/", s.static)

	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.problem(w, http.StatusForbidden, "cross-origin request refused")
	}))
	return s.checkHost(csrf.Handler(mux))
}

// allowedHosts is the set of Host values (no port) the server answers.
func allowedHosts(addr string) map[string]bool {
	hosts := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if h, _, err := net.SplitHostPort(addr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
		hosts[strings.ToLower(h)] = true
	}
	return hosts
}

// checkHost refuses a request whose Host header isn't in allowedHosts. A
// rebound DNS name looks same-origin to the browser, so only Host gives it away.
func (s *server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		if !s.allowedHosts[host] {
			s.problem(w, http.StatusForbidden, fmt.Sprintf("host %q not allowed; use localhost or 127.0.0.1", r.Host))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "ok")
}

// gpus lists the Serverless GPU pools with their prices, for the form.
func (s *server) gpus(w http.ResponseWriter, r *http.Request) {
	pools, err := s.catalog(r.Context())
	if err != nil {
		s.serverError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

// catalog returns the GPU pools, reusing the last read for CatalogTTL: the
// form estimates on every change, and Runpod calls share the account's
// 180 requests/min with the run's polls.
//
// A failed read falls back to a copy younger than CatalogMaxStale, then
// serves it without retrying for CatalogRetry so an outage doesn't make
// every request wait out its timeout. A client-cancelled read is returned
// as is. The lock isn't held during the read; concurrent reads are harmless.
func (s *server) catalog(ctx context.Context) ([]runpod.Pool, error) {
	return s.readCatalog(ctx, false)
}

// freshCatalog reads the catalog now, for startRun's stock gate: stock moves
// in minutes. A failed read still falls back under catalog's rules.
func (s *server) freshCatalog(ctx context.Context) ([]runpod.Pool, error) {
	return s.readCatalog(ctx, true)
}

// readCatalog backs catalog and freshCatalog; with skipCache set, the cached
// copy is only a fallback for a failed read.
func (s *server) readCatalog(ctx context.Context, skipCache bool) ([]runpod.Pool, error) {
	s.catMu.Lock()
	pools, at, retryAt := s.catPools, s.catAt, s.catRetryAt
	s.catMu.Unlock()
	age := time.Since(at)
	if !skipCache && pools != nil && age < s.CatalogTTL {
		return pools, nil
	}
	usable := pools != nil && age < s.CatalogMaxStale
	if !skipCache && usable && time.Now().Before(retryAt) {
		return pools, nil // a read failed moments ago
	}

	readCtx, cancel := context.WithTimeout(ctx, s.CatalogTimeout)
	defer cancel()
	fresh, err := s.Mgmt.Pools(readCtx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !usable {
			if pools != nil {
				err = fmt.Errorf("%w (the cached copy is %v old, past the %v limit)", err, age.Round(time.Second), s.CatalogMaxStale)
			}
			return nil, err
		}
		s.Log.Warn("GPU catalog read failed; using the cached copy", "age", age.Round(time.Second), "retry_in", s.CatalogRetry, "err", err)
		s.catMu.Lock()
		s.catRetryAt = time.Now().Add(s.CatalogRetry)
		s.catMu.Unlock()
		return pools, nil
	}
	s.catMu.Lock()
	s.catPools, s.catAt = fresh, time.Now()
	s.catMu.Unlock()
	return fresh, nil
}

// paramsBody is what GET /api/defaults sends and the POSTs read: Params
// plus the server's cap and limits. In a request, those extras are ignored.
type paramsBody struct {
	bench.Params
	BudgetCapUSD float64 `json:"budgetCapUsd"`
	// Limits is every range Validate checks, so the form can check them too.
	Limits bench.Limits `json:"limits,omitempty"`
}

// runBody is what the POSTs read: paramsBody plus allowLowStock, the user's
// "start anyway" for this one Start. It isn't validated, stored or estimated.
type runBody struct {
	paramsBody
	AllowLowStock bool `json:"allowLowStock"`
}

// defaults is the form's starting values, clamped to the server's budget cap
// and maxWorkers limit so they always pass Validate.
func (s *server) defaults(w http.ResponseWriter, r *http.Request) {
	p := bench.DefaultParams(s.Image)
	// [] rather than null for the UI.
	p.Pools = []string{}
	p.BudgetUSD = min(p.BudgetUSD, s.CapUSD)
	limits := s.Runner.Limits()
	p.MaxWorkers = min(p.MaxWorkers, limits["maxWorkers"][1])
	writeJSON(w, http.StatusOK, paramsBody{Params: p, BudgetCapUSD: s.CapUSD, Limits: limits})
}

// estimate answers "what could this run cost at most?" with the same checks
// as startRun. It reports stock but never refuses on it.
func (s *server) estimate(w http.ResponseWriter, r *http.Request) {
	p, catalog, _, ok := s.readParams(w, r, false)
	if !ok {
		return
	}
	// Runner.Estimate, not s.Runner.Timeouts directly: the Runner fills in
	// default timeouts on first use, so reading the field would race.
	est, err := s.Runner.Estimate(p, s.CapUSD, catalog)
	if err != nil {
		s.serverError(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, est)
}

// startRun starts a run in the background and returns its ID at once. Only
// one run may be active, so spending is bounded by one budget cap. The
// catalog is read fresh for the stock gate.
func (s *server) startRun(w http.ResponseWriter, r *http.Request) {
	p, catalog, allowLowStock, ok := s.readParams(w, r, true)
	if !ok {
		return
	}
	if detail, code := stockRefusal(p, catalog, allowLowStock); detail != "" {
		s.problemCode(w, http.StatusBadRequest, detail, code)
		return
	}

	// Held until Start has written the new run's row, so the next check sees it.
	s.startMu.Lock()
	defer s.startMu.Unlock()
	active, err := s.anyRunActive(r.Context())
	if err != nil {
		s.serverError(w, r, http.StatusInternalServerError, err)
		return
	}
	if active {
		s.problem(w, http.StatusConflict, "a run is already in progress; wait for it to finish or cancel it")
		return
	}
	// No run is active, so any endpoint of one of our runs is a leftover.
	// A sweep failure doesn't block the run.
	sweepCtx, cancel := context.WithTimeout(r.Context(), sweepTimeout)
	if err := bench.Sweep(sweepCtx, s.Mgmt, s.Store, s.Log); err != nil {
		s.Log.Error("pre-run janitor failed; starting the run anyway", "err", err)
	}
	cancel()
	// s.RootCtx, not r.Context(): see Deps.RootCtx.
	id, err := s.Runner.Start(s.RootCtx, p, s.CapUSD, catalog)
	if err != nil {
		// readParams already ran Start's checks, so this is a store error.
		s.serverError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("run started", "run", id, "pools", p.Pools, "cards", p.Cards, "flashboot", p.FlashBoot)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// anyRunActive reports whether the store has a run with status "running".
func (s *server) anyRunActive(ctx context.Context) (bool, error) {
	runs, err := s.Store.ListRuns(ctx)
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.Status == "running" {
			return true, nil
		}
	}
	return false, nil
}

// stockRefusal is the stock gate before a run starts. It returns the detail
// and code for the first pool or card that refuses the run, or "": NONE
// always refuses ("no_stock"), LOW refuses unless allowLowStock
// ("low_stock"). Any NONE wins over a LOW, so the form never offers "Start
// anyway" for a run that would be refused again. An unknown level passes.
func stockRefusal(p bench.Params, catalog []runpod.Pool, allowLowStock bool) (detail, code string) {
	type stock struct{ id, level string }
	var all []stock
	// readParams has checked every pool and card is in the catalog.
	for _, id := range p.Pools {
		i := slices.IndexFunc(catalog, func(pool runpod.Pool) bool { return pool.ID == id })
		all = append(all, stock{id, catalog[i].Availability})
	}
	for _, id := range p.Cards {
		_, card, _ := bench.CardPool(catalog, id)
		all = append(all, stock{id, card.Availability})
	}
	for _, st := range all {
		if st.level == "NONE" {
			return st.id + " has no stock right now", "no_stock"
		}
	}
	if !allowLowStock {
		for _, st := range all {
			if st.level == "LOW" {
				return st.id + " is low on stock; start anyway to accept the risk", "low_stock"
			}
		}
	}
	return "", ""
}

// readParams decodes and checks the request's Params and returns them with
// the catalog they were priced from and allowLowStock. On a problem it
// writes the response and returns ok=false. fresh skips the catalog cache.
func (s *server) readParams(w http.ResponseWriter, r *http.Request, fresh bool) (p bench.Params, catalog []runpod.Pool, allowLowStock, ok bool) {
	// Only JSON bodies: CSRF defense in depth, since a browser can send
	// text/plain or a form cross-site without a CORS preflight.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		s.problem(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return p, nil, false, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	// A misspelt field would otherwise silently fall back to the default.
	dec.DisallowUnknownFields()
	var body runBody
	if err := dec.Decode(&body); err != nil {
		s.problem(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return p, nil, false, false
	}
	p = body.Params

	read := s.catalog
	if fresh {
		read = s.freshCatalog
	}
	pools, err := read(r.Context())
	if err != nil {
		s.serverError(w, r, http.StatusBadGateway, err)
		return p, nil, false, false
	}
	if err := s.Runner.Validate(p, s.CapUSD, pools); err != nil {
		// Validate's errors name each field by its JSON name.
		s.problem(w, http.StatusBadRequest, err.Error())
		return p, nil, false, false
	}

	prices := make(map[string]float64, len(pools))
	for _, pool := range pools {
		prices[pool.ID] = pool.PricePerHr
	}
	for _, id := range p.Pools {
		price, ok := prices[id]
		if !ok {
			s.problem(w, http.StatusBadRequest, fmt.Sprintf("pools: unknown pool %q (see GET /api/gpus)", id))
			return p, nil, false, false
		}
		// A $0 price would make the budget guard's bound $0: it could never fire.
		if !(price > 0) {
			s.problem(w, http.StatusBadRequest, fmt.Sprintf("pools: pool %q has no serverless price", id))
			return p, nil, false, false
		}
	}
	// Validate has checked each card is in the catalog; the same $0 rule.
	for _, id := range p.Cards {
		if _, card, _ := bench.CardPool(pools, id); !(card.PriceHr > 0) {
			s.problem(w, http.StatusBadRequest, fmt.Sprintf("cards: card %q has no serverless price", id))
			return p, nil, false, false
		}
	}
	return p, pools, body.AllowLowStock, true
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.ListRuns(r.Context())
	if err != nil {
		s.serverError(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// configView is one config in GET /api/runs/{id}.
type configView struct {
	Config  store.Config        `json:"config"`
	Summary bench.ConfigSummary `json:"summary"`
	Jobs    []store.Job         `json:"jobs"`
}

type runView struct {
	Run     store.Run    `json:"run"`
	Configs []configView `json:"configs"`
}

// getRun is a run's full snapshot. Summaries are computed per request, so
// they always match the jobs.
func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, err := s.Store.GetRun(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.problem(w, http.StatusNotFound, "no run "+id)
		return
	}
	if err != nil {
		s.serverError(w, r, http.StatusInternalServerError, err)
		return
	}
	var p bench.Params
	if err := json.Unmarshal(d.Params, &p); err != nil {
		s.serverError(w, r, http.StatusInternalServerError, fmt.Errorf("run %s params: %w", id, err))
		return
	}
	configs := make([]configView, 0, len(d.Configs))
	for _, c := range d.Configs {
		configs = append(configs, configView{Config: c.Config, Summary: bench.Summarize(c, p), Jobs: c.Jobs})
	}
	writeJSON(w, http.StatusOK, runView{Run: d.Run, Configs: configs})
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.Runner.Cancel(id) {
		s.problem(w, http.StatusNotFound, "no active run "+id)
		return
	}
	s.Log.Info("run cancel requested", "run", id)
	// The final run_status event says when teardown is done.
	w.WriteHeader(http.StatusAccepted)
}

// events streams a run's events as Server-Sent Events, ending after the
// final run_status or when the client goes. A finished run gets one stored
// run_status. EventSource reconnects on end, so the UI must close() itself.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Subscribe before reading the status, or the final event could fall
	// between the two and the stream would never end.
	ch, unsubscribe := s.Hub.Subscribe(id)
	defer unsubscribe()

	run, err := s.Store.GetRun(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.problem(w, http.StatusNotFound, "no run "+id)
		return
	}
	if err != nil {
		s.serverError(w, r, http.StatusInternalServerError, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Turn off nginx's response buffering.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// NewResponseController finds the Flusher even through wrapping middleware.
	rc := http.NewResponseController(w)

	if run.Status != "running" {
		s.sendStored(w, rc, run.Run)
		return
	}
	rc.Flush() // send the headers now, so the browser sees the stream open

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			// Includes server shutdown: BaseContext cancels every request.
			return
		case e := <-ch:
			if writeEvent(w, e) != nil || rc.Flush() != nil {
				return
			}
			if isFinal(e) {
				return
			}
		case <-ping.C:
			// The hub may have dropped the final run_status for a slow
			// reader, so each heartbeat re-checks the stored status.
			if cur, err := s.Store.GetRunRow(r.Context(), id); err == nil && cur.Status != "running" {
				s.sendStored(w, rc, cur)
				return
			}
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// sendStored sends one run_status event built from the stored run, with Seq 0.
func (s *server) sendStored(w io.Writer, rc *http.ResponseController, run store.Run) {
	data := map[string]any{"status": run.Status}
	if run.Error != "" {
		data["error"] = run.Error
	}
	e := bench.Event{RunID: run.ID, TS: time.Now().UTC(), Type: "run_status", Data: data}
	if writeEvent(w, e) == nil {
		rc.Flush()
	}
}

// writeEvent writes one SSE frame. json.Marshal escapes newlines, which
// would otherwise break the data: line.
func writeEvent(w io.Writer, e bench.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, data)
	return err
}

// static serves the embedded UI: a file in Static as itself, anything else
// as index.html so client-side routes survive a reload.
func (s *server) static(w http.ResponseWriter, r *http.Request) {
	// An unmatched /api path gets a JSON 404, not the app's HTML.
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		s.problem(w, http.StatusNotFound, "no API route "+r.Method+" "+r.URL.Path)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if info, err := fs.Stat(s.Static, name); err == nil && !info.IsDir() {
		s.files.ServeHTTP(w, r)
		return
	}

	if _, err := fs.Stat(s.Static, "index.html"); err != nil {
		// No UI build yet: say so instead of a bare 404.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "UI not built — run `npm run build` in web/\n")
		return
	}
	http.ServeFileFS(w, r, s.Static, "index.html")
}

// problem writes an RFC 9457 problem details response.
func (s *server) problem(w http.ResponseWriter, status int, detail string) {
	s.problemCode(w, status, detail, "")
}

// problemCode is problem with a "code" extension member, so the UI can tell
// the stock refusals apart; "" is omitted.
func (s *server) problemCode(w http.ResponseWriter, status int, detail, code string) {
	body := map[string]any{
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	}
	if code != "" {
		body["code"] = code
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// serverError logs err and answers with a generic 5xx, keeping internals out
// of the response. A cancel from a client that has gone writes nothing.
func (s *server) serverError(w http.ResponseWriter, r *http.Request, status int, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}
	s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	s.problem(w, status, "internal error")
}

// writeJSON writes v as a JSON response. An Encode error means the client has gone.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
