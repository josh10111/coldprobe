// Command coldprobe benchmarks Runpod Serverless endpoints.
//
//	coldprobe [serve]                    the web app: API, live events and UI (default)
//	coldprobe janitor [-delete] [-force] list (or delete) leftover bench-coldprobe-* endpoints
//	coldprobe healthcheck                exit 0 if the local server answers /healthz
//
// Settings come from environment variables only; no .env file is read.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/josh10111/coldprobe/internal/bench"
	"github.com/josh10111/coldprobe/internal/fakerunpod"
	"github.com/josh10111/coldprobe/internal/runpod"
	"github.com/josh10111/coldprobe/internal/server"
	"github.com/josh10111/coldprobe/internal/store"
	"github.com/josh10111/coldprobe/web"
)

// shutdownTimeout bounds srv.Shutdown only; run teardown comes after it and
// is bounded by the container's stop grace period.
const shutdownTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches to a subcommand and returns the process exit code.
func run(args []string) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve()
	case "janitor":
		return janitor(args)
	case "healthcheck":
		return healthcheck()
	default:
		fmt.Fprintf(os.Stderr, "coldprobe: unknown command %q (want serve, janitor or healthcheck)\n", cmd)
		return 2
	}
}

// config is the environment, read once.
type config struct {
	fake   bool    // COLDPROBE_FAKE=1: talk to an in-process fake Runpod
	key    string  // RUNPOD_API_KEY; may be empty in fake mode
	addr   string  // COLDPROBE_ADDR
	db     string  // COLDPROBE_DB
	capUSD float64 // COLDPROBE_BUDGET_CAP_USD
	image  string  // COLDPROBE_WORKER_IMAGE
	// noStock is COLDPROBE_FAKE_NOSTOCK; see applyNoStock.
	noStock string
	// noPlace is COLDPROBE_FAKE_NOPLACE; see applyNoPlace.
	noPlace string
	// noWorkerWarn is COLDPROBE_NO_WORKER_WARN_S; 0 keeps the runner's default.
	noWorkerWarn time.Duration
	// maxWorkers is COLDPROBE_MAX_WORKERS, a lower ceiling on workers.max.
	maxWorkers int
}

// loadConfig reads the environment. Errors never echo a value: it could be
// the API key pasted into the wrong variable.
func loadConfig() (config, error) {
	c := config{
		fake: os.Getenv("COLDPROBE_FAKE") == "1",
		key:  os.Getenv("RUNPOD_API_KEY"),
		// Loopback by default: the API can start billable runs and has no login.
		addr:       envOr("COLDPROBE_ADDR", "127.0.0.1:8080"),
		db:         envOr("COLDPROBE_DB", "coldprobe.db"),
		image:      envOr("COLDPROBE_WORKER_IMAGE", bench.SyntheticImage),
		noStock:    os.Getenv("COLDPROBE_FAKE_NOSTOCK"),
		noPlace:    os.Getenv("COLDPROBE_FAKE_NOPLACE"),
		maxWorkers: bench.DefaultMaxWorkersLimit,
	}
	if !c.fake && c.key == "" {
		return c, errors.New("RUNPOD_API_KEY is not set (set COLDPROBE_FAKE=1 to run against the built-in fake instead)")
	}
	// Refused rather than ignored, so nobody silently gets the real Runpod.
	if !c.fake && c.noStock != "" {
		return c, errors.New("COLDPROBE_FAKE_NOSTOCK only works with COLDPROBE_FAKE=1")
	}
	if !c.fake && c.noPlace != "" {
		return c, errors.New("COLDPROBE_FAKE_NOPLACE only works with COLDPROBE_FAKE=1")
	}
	if v := os.Getenv("COLDPROBE_NO_WORKER_WARN_S"); v != "" {
		// A warning later than the first-boot timeout could never fire.
		maxS := int(bench.DefaultTimeouts().FirstBoot / time.Second)
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > maxS {
			return c, fmt.Errorf("COLDPROBE_NO_WORKER_WARN_S must be a whole number of seconds from 1 to %d (the first-boot timeout)", maxS)
		}
		c.noWorkerWarn = time.Duration(n) * time.Second
	}
	if v := os.Getenv("COLDPROBE_MAX_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > bench.MaxWorkersCeiling {
			return c, fmt.Errorf("COLDPROBE_MAX_WORKERS must be a whole number from 1 to %d", bench.MaxWorkersCeiling)
		}
		c.maxWorkers = n
	}
	capUSD, err := strconv.ParseFloat(envOr("COLDPROBE_BUDGET_CAP_USD", "2.00"), 64)
	// !(> 0) also refuses NaN.
	if err != nil || !(capUSD > 0) {
		return c, errors.New("COLDPROBE_BUDGET_CAP_USD must be a number above 0")
	}
	c.capUSD = capUSD
	return c, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// clients returns the Runpod clients. In fake mode they point at a fresh
// in-process fake, also returned so the caller can close it.
func clients(c config) (*runpod.Mgmt, *runpod.JobClient, *fakerunpod.Fake, error) {
	mgmtURL, jobURL, key := "", runpod.DefaultJobBaseURL, c.key
	var fake *fakerunpod.Fake
	if c.fake {
		fake = fakerunpod.New(fakerunpod.DemoOptions())
		mgmtURL, jobURL, key = fake.MgmtURL(), fake.JobURL(), "fake"
		if err := applyNoStock(fake, fakerunpod.DefaultPools(), c.noStock); err != nil {
			fake.Close()
			return nil, nil, nil, err
		}
		if err := applyNoPlace(fake, fakerunpod.DefaultPools(), c.noPlace); err != nil {
			fake.Close()
			return nil, nil, nil, err
		}
	}
	mgmt, err := runpod.NewMgmt(mgmtURL, key)
	if err != nil {
		if fake != nil {
			fake.Close()
		}
		return nil, nil, nil, err
	}
	return mgmt, runpod.NewJobClient(jobURL, key, nil), fake, nil
}

// applyNoStock applies COLDPROBE_FAKE_NOSTOCK: a pool or card id, optionally
// with ":LOW" (default NONE), whose catalog stock the fake lowers, so the
// stock gate can be shown without a real outage. A pool at NONE also gets
// no workers; placement in the fake is per pool, so a card only changes
// the catalog.
func applyNoStock(fake *fakerunpod.Fake, pools []fakerunpod.FakePool, value string) error {
	if value == "" {
		return nil
	}
	// Card ids have spaces but no colons, so the last ":" is the level's.
	id, level := value, "NONE"
	if i := strings.LastIndex(value, ":"); i >= 0 {
		id, level = value[:i], value[i+1:]
	}
	if level != "NONE" && level != "LOW" {
		return fmt.Errorf("COLDPROBE_FAKE_NOSTOCK: level %q after the colon must be LOW or NONE", level)
	}
	for _, p := range pools {
		if p.ID == id {
			fake.SetAvailability(id, "", level)
			if level == "NONE" {
				fake.BlockPlacement(id)
			}
			return nil
		}
	}
	for _, p := range pools {
		if p.GPUName == id {
			fake.SetAvailability(p.ID, id, level)
			return nil
		}
	}
	return fmt.Errorf("COLDPROBE_FAKE_NOSTOCK: %q is not a pool or card id in the fake catalog", id)
}

// applyNoPlace applies COLDPROBE_FAKE_NOPLACE: a pool where no worker is
// ever placed while its stock looks normal, to demonstrate the no-worker warning.
func applyNoPlace(fake *fakerunpod.Fake, pools []fakerunpod.FakePool, value string) error {
	if value == "" {
		return nil
	}
	for _, p := range pools {
		if p.ID == value {
			fake.BlockPlacement(value)
			return nil
		}
	}
	return fmt.Errorf("COLDPROBE_FAKE_NOPLACE: %q is not a pool id in the fake catalog", value)
}

// newLogger returns a JSON logger that redacts the API key from string and
// error attributes. Other kinds (structs, slices) bypass it: never log one
// that could hold the key.
func newLogger(w io.Writer, key string) *slog.Logger {
	redact := func(groups []string, a slog.Attr) slog.Attr {
		switch a.Value.Kind() {
		case slog.KindString:
			a.Value = slog.StringValue(runpod.Redact(a.Value.String(), key))
		case slog.KindAny:
			// The JSON handler would call err.Error() after this hook.
			if err, ok := a.Value.Any().(error); ok {
				a.Value = slog.StringValue(runpod.Redact(err.Error(), key))
			}
		}
		return a
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: redact}))
}

// startup marks runs left "running" by a dead process as aborted_crash, then
// runs the janitor. A janitor failure is logged, not returned: a brief Runpod
// outage shouldn't keep the app down.
func startup(ctx context.Context, st *store.Store, m *runpod.Mgmt, log *slog.Logger) error {
	n, err := st.MarkCrashed(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Warn("marked runs from a previous process as aborted_crash", "count", n)
	}
	if err := bench.Sweep(ctx, m, st, log); err != nil {
		log.Error("janitor failed; leftover endpoints may still be billing", "err", err)
	}
	return nil
}

// lockDB takes an exclusive, non-blocking flock on db+".lock", so only one
// serve uses a database at a time. The kernel drops it even on kill -9.
// flock is advisory on some network filesystems.
func lockDB(db string) (*os.File, error) {
	f, err := os.OpenFile(db+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another coldprobe is already using %s", db)
		}
		return nil, fmt.Errorf("lock %s.lock: %w", db, err)
	}
	return f, nil
}

// serve runs the web app until SIGINT or SIGTERM. Shutdown order matters:
// the signal cancels every run, srv.Shutdown stops new runs from starting,
// runner.Wait waits for every teardown, then the fake and database close.
func serve() int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "coldprobe:", err)
		return 1
	}
	// The real key even in fake mode, never "fake", which would redact the word.
	log := newLogger(os.Stdout, cfg.key)
	if cfg.fake {
		log.Warn("FAKE MODE: COLDPROBE_FAKE=1, talking to an in-process fake Runpod; nothing real is created or billed")
		if cfg.noStock != "" {
			log.Warn("fake catalog stock overridden", "COLDPROBE_FAKE_NOSTOCK", cfg.noStock)
		}
		if cfg.noPlace != "" {
			log.Warn("fake places no workers in this pool", "COLDPROBE_FAKE_NOPLACE", cfg.noPlace)
		}
	}

	// The handler stays registered until serve returns, so a second signal
	// can't kill the process mid-teardown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Listen before touching the database or Runpod: if the port is taken,
	// another coldprobe may be live, and startup would crash its runs.
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		log.Error("listen", "addr", cfg.addr, "err", err)
		return 1
	}
	// Covers early returns before Serve takes ownership of ln.
	defer ln.Close()

	// A second serve on another port must not share the database either.
	// The deferred Close also keeps the file from being finalized early.
	lock, err := lockDB(cfg.db)
	if err != nil {
		log.Error("lock database", "err", err)
		return 1
	}
	defer lock.Close()

	st, err := store.Open(cfg.db)
	if err != nil {
		log.Error("open database", "err", err)
		return 1
	}
	defer st.Close()

	mgmt, jobs, fake, err := clients(cfg)
	if err != nil {
		log.Error("runpod client", "err", err)
		return 1
	}
	if fake != nil {
		defer fake.Close()
	}

	if err := startup(ctx, st, mgmt, log); err != nil {
		log.Error("startup", "err", err)
		return 1
	}

	hub := server.NewHub()
	runner := &bench.Runner{Mgmt: mgmt, Jobs: jobs, Store: st, Publish: hub.Publish, Log: log, MaxWorkersLimit: cfg.maxWorkers}
	if cfg.fake {
		// The fake's workers idle out in seconds; a demo run takes ~2-3 min.
		runner.WorkerPoll = time.Second
		runner.MetricsPoll = 2 * time.Second
		runner.MetricsWait = 6 * time.Second
	}
	if cfg.noWorkerWarn > 0 {
		// The Runner fills in defaults only when Timeouts is entirely zero.
		runner.Timeouts = bench.DefaultTimeouts()
		runner.Timeouts.NoWorkerWarn = cfg.noWorkerWarn
	}

	static, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		log.Error("embedded UI", "err", err)
		return 1
	}

	// runsCtx parents every run and request. Cancelling it rather than
	// calling stop keeps further signals absorbed during runner.Wait.
	runsCtx, cancelRuns := context.WithCancel(ctx)
	defer cancelRuns()

	srv := &http.Server{
		Handler: server.New(server.Deps{
			RootCtx: runsCtx, Store: st, Runner: runner, Mgmt: mgmt, Hub: hub,
			CapUSD: cfg.capUSD, Image: cfg.image, Static: static, Addr: cfg.addr, Log: log,
		}),
		// SSE streams end at once on shutdown instead of holding Shutdown for 10 s.
		BaseContext: func(net.Listener) context.Context { return runsCtx },
		// net/http's own messages go through our logger, so they're redacted too.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
		// No WriteTimeout: it would cut off every SSE stream.
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "db", cfg.db, "capUsd", cfg.capUSD, "maxWorkers", cfg.maxWorkers)

	exit := 0
	select {
	case <-runsCtx.Done():
		log.Info("shutdown 1/3: signal received; runs cancelled, stopping the HTTP server")
	case err := <-serveErr:
		log.Error("http server failed", "err", err)
		exit = 1
		cancelRuns()
	}

	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		// Carry on: the runs must still be waited for.
		log.Error("http shutdown", "err", err)
	}
	log.Info("shutdown 2/3: HTTP server stopped; waiting for runs to delete their endpoints")
	runner.Wait()
	if fake != nil {
		log.Info("fake runpod: endpoints still live", "count", len(fake.LiveEndpoints()))
	}
	log.Info("shutdown 3/3: every run finished; exiting")
	return exit
}

// janitor is the "janitor [-delete] [-force]" subcommand. It applies the
// startup janitor's rule against COLDPROBE_DB; -force drops the rule.
func janitor(args []string) int {
	flags := flag.NewFlagSet("janitor", flag.ContinueOnError)
	del := flags.Bool("delete", false, "delete the endpoints (default: only list them)")
	force := flags.Bool("force", false, "ignore the skip rule: every bench-coldprobe-* endpoint, even one a live run may be using")
	if err := flags.Parse(args); err != nil {
		return 2 // flag has already printed the error
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "coldprobe:", err)
		return 1
	}
	mgmt, _, fake, err := clients(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coldprobe:", err)
		return 1
	}
	if fake != nil {
		defer fake.Close()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var skip func(runpod.Endpoint) string // nil deletes everything
	if !*force {
		// The server's database, so its runs are recognised; WAL allows sharing.
		st, err := store.Open(cfg.db)
		if err != nil {
			fmt.Fprintln(os.Stderr, "coldprobe:", err)
			return 1
		}
		defer st.Close()
		if skip, err = bench.SkipRule(ctx, st, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "coldprobe:", err)
			return 1
		}
	}
	eps, err := bench.Janitor(ctx, mgmt, *del, skip)
	n, skipped := 0, 0
	for _, e := range eps {
		switch {
		case e.Skip != "":
			skipped++
			fmt.Println(e.ID, e.Name, "SKIPPED:", e.Skip)
		case e.Err != nil:
			fmt.Println(e.ID, e.Name, "DELETE FAILED:", e.Err)
		default:
			n++
			fmt.Println(e.ID, e.Name)
		}
	}
	verb := "found (dry run; -delete to delete them)"
	if *del {
		verb = "deleted"
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "coldprobe:", err)
		return 1
	}
	fmt.Printf("%d endpoint(s) %s, %d skipped\n", n, verb, skipped)
	return 0
}

// healthcheck GETs the local /healthz: the distroless image has no curl.
func healthcheck() int {
	// Only the port of the listen address is reused, against 127.0.0.1.
	_, port, err := net.SplitHostPort(envOr("COLDPROBE_ADDR", "127.0.0.1:8080"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "coldprobe: COLDPROBE_ADDR is not host:port")
		return 1
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "coldprobe: healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "coldprobe: healthcheck: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}
