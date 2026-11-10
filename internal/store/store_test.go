package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// openTemp opens a fresh database in a per-test temp directory.
func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coldprobe.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// eqTime checks that *got is the same instant as want, then overwrites *got
// with want so a later reflect.DeepEqual compares only the other fields.
func eqTime(t *testing.T, name string, got *time.Time, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
	*got = want
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	// A non-UTC zone and non-zero nanoseconds, so the test proves the
	// UTC conversion keeps the instant and the full precision.
	zone := time.FixedZone("UTC+2", 2*60*60)
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 123456789, zone)

	run := Run{
		ID:        "run-1",
		CreatedAt: t0,
		Status:    "running",
		Params:    json.RawMessage(`{"pools":["AMPERE_24"],"maxWorkers":2}`),
		BudgetUSD: 2,
		EstMaxUSD: 0.75,
	}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	deleted := t0.Add(5 * time.Minute)
	cfgs := []Config{
		{RunID: "run-1", Pool: "AMPERE_16", FlashBoot: "OFF", PriceHr: 0.58, Status: "pending", CreatedAt: t0.Add(time.Second)},
		{RunID: "run-1", Pool: "AMPERE_24", FlashBoot: "FLASHBOOT", PriceHr: 0.69, Status: "pending", CreatedAt: t0.Add(2 * time.Second)},
	}
	for i := range cfgs {
		id, err := s.CreateConfig(ctx, cfgs[i])
		if err != nil {
			t.Fatalf("CreateConfig %d: %v", i, err)
		}
		cfgs[i].ID = id
	}

	cfgs[1].EndpointID = "ep-abc"
	cfgs[1].Status = "deleted"
	cfgs[1].Error = "placement slow"
	cfgs[1].DeletedAt = &deleted
	cfgs[1].RPMetrics = json.RawMessage(`{"coldStarts":{"p90":1234.5}}`)
	if err := s.UpdateConfig(ctx, cfgs[1]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	jobs := []Job{
		{ConfigID: cfgs[0].ID, Phase: "first_boot", Level: 0, RunpodJobID: "j1", WorkerID: "w1", GPUTypeID: "NVIDIA GeForce RTX 3090", Status: "COMPLETED",
			IntendedAt: t0.Add(3 * time.Second), SentAt: t0.Add(3*time.Second + 7), WallMs: 4123.25, DelayMs: 4000, ExecMs: 101.5},
		{ConfigID: cfgs[1].ID, Phase: "load", Level: 4, RunpodJobID: "j2", WorkerID: "w2", GPUTypeID: "NVIDIA L4", Status: "FAILED",
			IntendedAt: t0.Add(4 * time.Second), SentAt: t0.Add(4 * time.Second), WallMs: 50, Error: "boom"},
		{ConfigID: cfgs[0].ID, Phase: "warm", Level: 0, RunpodJobID: "j3", WorkerID: "w1", GPUTypeID: "NVIDIA GeForce RTX 3090", Status: "COMPLETED",
			IntendedAt: t0.Add(5 * time.Second), SentAt: t0.Add(5 * time.Second), WallMs: 120, DelayMs: 10, ExecMs: 100},
	}
	for i := range jobs {
		id, err := s.InsertJob(ctx, jobs[i])
		if err != nil {
			t.Fatalf("InsertJob %d: %v", i, err)
		}
		jobs[i].ID = id
	}

	got, err := s.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	want := RunDetail{Run: run, Configs: []ConfigDetail{
		{Config: cfgs[0], Jobs: []Job{jobs[0], jobs[2]}}, // jobs ordered by id within a config
		{Config: cfgs[1], Jobs: []Job{jobs[1]}},
	}}

	eqTime(t, "run.CreatedAt", &got.CreatedAt, want.CreatedAt)
	if got.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil", got.FinishedAt)
	}
	if len(got.Configs) != len(want.Configs) {
		t.Fatalf("got %d configs, want %d: %+v", len(got.Configs), len(want.Configs), got.Configs)
	}
	for i := range got.Configs {
		gc, wc := &got.Configs[i], &want.Configs[i]
		eqTime(t, "config.CreatedAt", &gc.CreatedAt, wc.CreatedAt)
		if (gc.DeletedAt == nil) != (wc.DeletedAt == nil) {
			t.Errorf("config %d DeletedAt = %v, want %v", i, gc.DeletedAt, wc.DeletedAt)
		} else if gc.DeletedAt != nil {
			eqTime(t, "config.DeletedAt", gc.DeletedAt, *wc.DeletedAt)
		}
		if len(gc.Jobs) != len(wc.Jobs) {
			t.Fatalf("config %d: got %d jobs, want %d", i, len(gc.Jobs), len(wc.Jobs))
		}
		for j := range gc.Jobs {
			eqTime(t, "job.IntendedAt", &gc.Jobs[j].IntendedAt, wc.Jobs[j].IntendedAt)
			eqTime(t, "job.SentAt", &gc.Jobs[j].SentAt, wc.Jobs[j].SentAt)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetRun mismatch\n got: %+v\nwant: %+v", got, want)
	}

	var typ string
	if err := s.db.QueryRowContext(ctx, "SELECT typeof(params) FROM runs WHERE id = 'run-1'").Scan(&typ); err != nil {
		t.Fatalf("typeof: %v", err)
	}
	if typ != "text" {
		t.Errorf("params stored as %s, want text", typ)
	}
}

// The frontend expects [] rather than null.
func TestEmptySlicesEncodeAsArrays(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	runs, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if b, _ := json.Marshal(runs); string(b) != "[]" {
		t.Errorf("ListRuns on empty db = %s, want []", b)
	}

	if err := s.CreateRun(ctx, Run{ID: "r", CreatedAt: time.Now(), Status: "running"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := s.CreateConfig(ctx, Config{RunID: "r", Pool: "P", FlashBoot: "OFF", Status: "pending", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	rd, err := s.GetRun(ctx, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	b, _ := json.Marshal(rd)
	if !strings.Contains(string(b), `"jobs":[]`) {
		t.Errorf("config with no jobs should encode jobs as []: %s", b)
	}
	// An empty Params is stored as NULL and comes back nil, which encodes as null.
	if rd.Params != nil {
		t.Errorf("Params = %q, want nil", rd.Params)
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	// With RFC3339Nano these would format as "...:00Z" and "...:00.1Z", and
	// '.' sorts before 'Z'.
	base := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	for _, r := range []Run{
		{ID: "a", CreatedAt: base, Status: "completed"},
		{ID: "b", CreatedAt: base.Add(100 * time.Millisecond), Status: "running"},
		{ID: "c", CreatedAt: base.Add(-time.Hour), Status: "failed"},
	} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun %s: %v", r.ID, err)
		}
	}

	runs, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "b,a,c" {
		t.Errorf("ListRuns order = %v, want [b a c]", ids)
	}
}

func TestFinishRun(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	if err := s.CreateRun(ctx, Run{ID: "r", CreatedAt: time.Now(), Status: "running"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.FinishRun(ctx, "r", "failed", "endpoint create: HTTP 400"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	rd, err := s.GetRun(ctx, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if rd.Status != "failed" || rd.Error != "endpoint create: HTTP 400" || rd.FinishedAt == nil {
		t.Errorf("after FinishRun: status=%q error=%q finishedAt=%v", rd.Status, rd.Error, rd.FinishedAt)
	}

	if err := s.FinishRun(ctx, "nope", "completed", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("FinishRun on missing id: err = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateConfig(ctx, Config{ID: 999, Status: "deleted"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UpdateConfig on missing id: err = %v, want sql.ErrNoRows", err)
	}
}

func TestMarkCrashed(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	if err := s.CreateRun(ctx, Run{ID: "live", CreatedAt: time.Now(), Status: "running"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.CreateRun(ctx, Run{ID: "done", CreatedAt: time.Now(), Status: "running"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.FinishRun(ctx, "done", "completed", ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	// The dead run's configs: one in each unfinished status and one in each
	// finished one. The finished run has a done config too.
	cfgs := map[string]int64{}
	for _, c := range []struct{ name, run, status string }{
		{"pending", "live", "pending"}, {"creating", "live", "creating"},
		{"running", "live", "running"}, {"tearing_down", "live", "tearing_down"},
		{"finished", "live", "done"}, {"failed", "live", "failed"}, {"cancelled", "live", "cancelled"},
		{"other", "done", "done"},
	} {
		id, err := s.CreateConfig(ctx, Config{RunID: c.run, Pool: "P", FlashBoot: "OFF", Status: c.status, Error: c.status, CreatedAt: time.Now()})
		if err != nil {
			t.Fatalf("CreateConfig: %v", err)
		}
		cfgs[c.name] = id
	}
	doneBefore, err := s.GetRun(ctx, "done")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	n, err := s.MarkCrashed(ctx)
	if err != nil {
		t.Fatalf("MarkCrashed: %v", err)
	}
	if n != 1 {
		t.Errorf("MarkCrashed = %d, want 1", n)
	}

	live, err := s.GetRun(ctx, "live")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if live.Status != "aborted_crash" || live.FinishedAt == nil {
		t.Errorf("live run: status=%q finishedAt=%v, want aborted_crash with a finish time", live.Status, live.FinishedAt)
	}
	got := map[int64]Config{}
	for _, c := range live.Configs {
		got[c.ID] = c.Config
	}
	for _, name := range []string{"pending", "creating", "running", "tearing_down"} {
		if c := got[cfgs[name]]; c.Status != "failed" || c.Error != "interrupted: coldprobe stopped mid-run" {
			t.Errorf("%s config: status=%q error=%q, want failed, interrupted", name, c.Status, c.Error)
		}
	}
	for _, name := range []string{"finished", "failed", "cancelled"} {
		if c := got[cfgs[name]]; c.Error != c.Status {
			t.Errorf("%s config: status=%q error=%q, want both left as they were", name, c.Status, c.Error)
		}
	}

	done, err := s.GetRun(ctx, "done")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if done.FinishedAt == nil || doneBefore.FinishedAt == nil {
		t.Fatalf("completed run has no finish time: before %v, after %v", doneBefore.FinishedAt, done.FinishedAt)
	}
	if done.Status != "completed" || !done.FinishedAt.Equal(*doneBefore.FinishedAt) {
		t.Errorf("completed run changed: status=%q finishedAt=%v (was %v)", done.Status, done.FinishedAt, doneBefore.FinishedAt)
	}
	if c := done.Configs[0]; c.Status != "done" {
		t.Errorf("completed run's config: status=%q, want done untouched", c.Status)
	}
}

func TestGetRun_Missing(t *testing.T) {
	s, _ := openTemp(t)
	_, err := s.GetRun(context.Background(), "nope")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want one wrapping sql.ErrNoRows", err)
	}
}

// GetRunRow reads the run alone (the SSE heartbeat's status check).
func TestGetRunRow(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if err := s.CreateRun(ctx, Run{ID: "r", CreatedAt: time.Now(), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.GetRunRow(ctx, "r"); err != nil || r.ID != "r" || r.Status != "running" {
		t.Errorf("GetRunRow = %+v, %v, want run r, running", r, err)
	}
	if _, err := s.GetRunRow(ctx, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want one wrapping sql.ErrNoRows", err)
	}
}

func TestConcurrentInsertJob(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	if err := s.CreateRun(ctx, Run{ID: "r", CreatedAt: time.Now(), Status: "running"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cfgID, err := s.CreateConfig(ctx, Config{RunID: "r", Pool: "P", FlashBoot: "OFF", Status: "running", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	const goroutines, perGoroutine = 50, 20
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				_, err := s.InsertJob(ctx, Job{ConfigID: cfgID, Phase: "load", Level: 16, Status: "COMPLETED",
					IntendedAt: time.Now(), SentAt: time.Now()})
				if err != nil {
					t.Errorf("goroutine %d insert %d: %v", g, i, err)
					return
				}
			}
		})
	}
	wg.Wait()

	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != goroutines*perGoroutine {
		t.Errorf("count = %d, want %d", count, goroutines*perGoroutine)
	}
}

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "coldprobe.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.CreateRun(ctx, Run{ID: "r", CreatedAt: time.Now(), Status: "completed", BudgetUSD: 2}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	rd, err := s.GetRun(ctx, "r")
	if err != nil {
		t.Fatalf("GetRun after reopen: %v", err)
	}
	if rd.Status != "completed" || rd.BudgetUSD != 2 {
		t.Errorf("after reopen: %+v", rd.Run)
	}
}

// TestOneConnection pins SetMaxOpenConns(1). TestConcurrentInsertJob
// can't: WAL plus busy_timeout absorb its load without the limit.
func TestOneConnection(t *testing.T) {
	s, _ := openTemp(t)
	if n := s.db.Stats().MaxOpenConnections; n != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", n)
	}
}

// foreign_keys is off by default in SQLite. This pins that Open turns it on.
func TestForeignKeysOn(t *testing.T) {
	s, _ := openTemp(t)
	_, err := s.InsertJob(context.Background(), Job{ConfigID: 42, Phase: "warm", Status: "COMPLETED"})
	if err == nil {
		t.Fatal("InsertJob with a missing config succeeded; foreign_keys is off")
	}
}

// oldJobsSchema is the jobs table before the matmul columns.
const oldJobsSchema = `
CREATE TABLE runs (
    id TEXT PRIMARY KEY, created_at TEXT NOT NULL, finished_at TEXT,
    status TEXT NOT NULL, params TEXT, budget_usd REAL NOT NULL DEFAULT 0,
    est_max_usd REAL NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE configs (
    id INTEGER PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs (id),
    pool TEXT NOT NULL, flashboot TEXT NOT NULL, endpoint_id TEXT NOT NULL DEFAULT '',
    price_hr REAL NOT NULL DEFAULT 0, status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL, deleted_at TEXT, rp_metrics TEXT
);
CREATE TABLE jobs (
    id            INTEGER PRIMARY KEY,
    config_id     INTEGER NOT NULL REFERENCES configs (id),
    phase         TEXT NOT NULL CHECK (phase IN ('first_boot', 'cold', 'warm', 'load')),
    level         REAL NOT NULL DEFAULT 0,
    runpod_job_id TEXT NOT NULL DEFAULT '',
    worker_id     TEXT NOT NULL DEFAULT '',
    gpu_type_id   TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,
    intended_at   TEXT NOT NULL,
    sent_at       TEXT NOT NULL,
    wall_ms       REAL NOT NULL DEFAULT 0,
    delay_ms      REAL NOT NULL DEFAULT 0,
    exec_ms       REAL NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT ''
);
INSERT INTO runs (id, created_at, status) VALUES ('old', '2026-11-07T10:00:00.000000000Z', 'completed');
INSERT INTO configs (run_id, pool, flashboot, status, created_at)
    VALUES ('old', 'AMPERE_24', 'OFF', 'done', '2026-11-07T10:00:00.000000000Z');
INSERT INTO jobs (config_id, phase, status, intended_at, sent_at, wall_ms)
    VALUES (1, 'warm', 'COMPLETED', '2026-11-07T10:00:01.000000000Z', '2026-11-07T10:00:01.000000000Z', 600);
`

// Open adds the matmul columns to an old database; a second Open adds nothing.
func TestOpen_AddsMatmulColumnsToOldDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldJobsSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	db.Close()

	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open old db: %v", err)
		}
		s.Close()
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	matmul, tflops, cudaInit := 1.25, 110.0, 230.5
	_, err = s.InsertJob(ctx, Job{ConfigID: 1, Phase: "warm", Status: "COMPLETED",
		MatmulMs: &matmul, TFLOPS: &tflops, CudaInitMs: &cudaInit})
	if err != nil {
		t.Fatalf("InsertJob after migration: %v", err)
	}
	rd, err := s.GetRun(ctx, "old")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	jobs := rd.Configs[0].Jobs
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if old := jobs[0]; old.WallMs != 600 || old.MatmulMs != nil || old.TFLOPS != nil || old.CudaInitMs != nil {
		t.Errorf("old job = %+v, want wall 600 and nil matmul fields", old)
	}
	if j := jobs[1]; j.MatmulMs == nil || *j.MatmulMs != matmul || j.TFLOPS == nil || *j.TFLOPS != tflops ||
		j.CudaInitMs == nil || *j.CudaInitMs != cudaInit {
		t.Errorf("new job = %+v, want matmul 1.25, tflops 110, cudaInit 230.5", j)
	}
}

// The matmul fields are omitempty.
func TestJob_MatmulFieldsOmittedWhenNil(t *testing.T) {
	b, _ := json.Marshal(Job{})
	for _, k := range []string{"matmulMs", "tflops", "cudaInitMs"} {
		if strings.Contains(string(b), k) {
			t.Errorf("json = %s, want no %s", b, k)
		}
	}
}

// Open adds the card column to an old database; a second Open adds nothing.
func TestOpen_AddsCardColumnToOldSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldJobsSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	db.Close()

	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open old db: %v", err)
		}
		s.Close()
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = s.CreateConfig(ctx, Config{RunID: "old", Pool: "AMPERE_16", Card: "NVIDIA RTX A4500",
		FlashBoot: "OFF", Status: "pending", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("CreateConfig after migration: %v", err)
	}
	rd, err := s.GetRun(ctx, "old")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(rd.Configs) != 2 {
		t.Fatalf("got %d configs, want 2", len(rd.Configs))
	}
	if old := rd.Configs[0]; old.Card != "" || old.Pool != "AMPERE_24" {
		t.Errorf("old config = %+v, want AMPERE_24 and no card", old.Config)
	}
	if c := rd.Configs[1]; c.Card != "NVIDIA RTX A4500" {
		t.Errorf("new config card = %q, want NVIDIA RTX A4500", c.Card)
	}
	if b, _ := json.Marshal(rd.Configs[0].Config); strings.Contains(string(b), `"card"`) {
		t.Errorf("json = %s, want no card", b)
	}
}
