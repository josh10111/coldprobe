// Package store keeps benchmark runs, their configs and their jobs in SQLite.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// schema is applied on every Open.
//
//go:embed schema.sql
var schema string

// timeLayout is RFC 3339 with a fixed nine fractional digits, so text order
// is time order. RFC3339Nano trims zeros and "...:00Z" would sort after "...:00.1Z".
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Run is one benchmark run: a set of configs measured together.
// Status is one of running|completed|failed|cancelled|aborted_budget|aborted_crash.
type Run struct {
	ID         string          `json:"id"`
	CreatedAt  time.Time       `json:"createdAt"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"` // nil while running
	Status     string          `json:"status"`
	Params     json.RawMessage `json:"params"`
	BudgetUSD  float64         `json:"budgetUsd"`
	EstMaxUSD  float64         `json:"estMaxUsd"`
	Error      string          `json:"error,omitempty"`
}

// Config is one GPU pool (or card) x FlashBoot setting within a run, i.e.
// one temporary endpoint. Status is one of pending|creating|running|
// tearing_down|done|failed|cancelled.
type Config struct {
	ID    int64  `json:"id"`
	RunID string `json:"runId"`
	Pool  string `json:"pool"`
	// Card is the catalog GPU id a card-mode config was pinned to; "" in pool mode.
	Card       string          `json:"card,omitempty"`
	FlashBoot  string          `json:"flashboot"`
	EndpointID string          `json:"endpointId,omitempty"`
	PriceHr    float64         `json:"priceHr"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	DeletedAt  *time.Time      `json:"deletedAt,omitempty"` // nil until the endpoint is deleted
	RPMetrics  json.RawMessage `json:"rpMetrics,omitempty"`
}

// Job is one request sent to an endpoint.
// Phase is one of first_boot|cold|warm|load.
type Job struct {
	ID          int64     `json:"id"`
	ConfigID    int64     `json:"configId"`
	Phase       string    `json:"phase"`
	Level       float64   `json:"level"`
	RunpodJobID string    `json:"runpodJobId"`
	WorkerID    string    `json:"workerId"`
	GPUTypeID   string    `json:"gpuTypeId"` // pools mix GPU models; this is the one the worker ran on
	Status      string    `json:"status"`
	IntendedAt  time.Time `json:"intendedAt"`
	SentAt      time.Time `json:"sentAt"`
	WallMs      float64   `json:"wallMs"`
	DelayMs     float64   `json:"delayMs"`
	ExecMs      float64   `json:"execMs"`
	Error       string    `json:"error,omitempty"`
	// From the GPU worker image's output; nil (NULL) otherwise.
	MatmulMs   *float64 `json:"matmulMs,omitempty"`   // median ms per matmul, when the job ran one
	TFLOPS     *float64 `json:"tflops,omitempty"`     // fp16 TFLOP/s of that matmul
	CudaInitMs *float64 `json:"cudaInitMs,omitempty"` // per worker process, repeated on every job it serves
}

// ConfigDetail is a Config with its jobs, as GetRun returns it.
type ConfigDetail struct {
	Config
	Jobs []Job `json:"jobs"`
}

// RunDetail is a Run with its configs and their jobs.
type RunDetail struct {
	Run
	Configs []ConfigDetail `json:"configs"`
}

// Store wraps the database handle. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and applies the schema.
// The PRAGMAs go in the DSN so every connection database/sql opens gets
// them, including replacements. The path must not contain '?'.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// One connection serializes every query; SQLite allows one writer anyway,
	// and this turns "database is locked" into a wait. A slow read blocks writes.
	db.SetMaxOpenConns(1)

	// sql.Open doesn't connect; this Exec surfaces a bad path or pragma.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if err := addJobColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: add job columns: %w", err)
	}
	if err := addColumn(db, "configs", addedConfigColumn, addedConfigType); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: add config columns: %w", err)
	}
	return &Store{db: db}, nil
}

// addedJobColumns postdate the first release; CREATE TABLE IF NOT EXISTS
// won't add them to an existing database, so addJobColumns does.
var addedJobColumns = []string{"matmul_ms", "tflops", "cuda_init_ms"}

// addedConfigColumn is card, added after the first release; existing rows
// were all pool mode, hence the empty-string default.
const addedConfigColumn, addedConfigType = "card", "TEXT NOT NULL DEFAULT ''"

// addJobColumns adds each missing column in addedJobColumns as a nullable REAL.
func addJobColumns(db *sql.DB) error {
	for _, col := range addedJobColumns {
		if err := addColumn(db, "jobs", col, "REAL"); err != nil {
			return err
		}
	}
	return nil
}

// addColumn adds column name, of type typ, to table unless it exists
// (SQLite has no ADD COLUMN IF NOT EXISTS).
func addColumn(db *sql.DB, table, name, typ string) error {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, name).Scan(&n)
	if err != nil || n > 0 {
		return err
	}
	// Identifiers can't be parameters; every caller passes fixed strings.
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + typ)
	return err
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateRun inserts a new run. The caller chooses the ID and CreatedAt.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (id, created_at, finished_at, status, params, budget_usd, est_max_usd, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, fmtTime(r.CreatedAt), nullTime(r.FinishedAt), r.Status, nullJSON(r.Params),
		r.BudgetUSD, r.EstMaxUSD, r.Error)
	if err != nil {
		return fmt.Errorf("store: create run %s: %w", r.ID, err)
	}
	return nil
}

// FinishRun sets a run's final status and error and stamps finished_at.
func (s *Store) FinishRun(ctx context.Context, id, status, errMsg string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errMsg, fmtTime(time.Now()), id)
	return oneRow(res, err, "finish run "+id)
}

// MarkCrashed runs once at startup: runs still "running" belong to a dead
// process and become aborted_crash, and their unfinished configs become
// failed. It returns how many runs changed. The configs step matches every
// aborted_crash run, so a failure is repaired on the next start.
func (s *Store) MarkCrashed(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = 'aborted_crash', finished_at = ? WHERE status = 'running'`,
		fmtTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: mark crashed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark crashed: rows affected: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE configs SET status = 'failed', error = 'interrupted: coldprobe stopped mid-run'
		 WHERE status NOT IN ('done', 'failed', 'cancelled')
		   AND run_id IN (SELECT id FROM runs WHERE status = 'aborted_crash')`)
	if err != nil {
		return 0, fmt.Errorf("store: mark crashed configs: %w", err)
	}
	return n, nil
}

// CreateConfig inserts a config and returns its new ID. c.ID is ignored.
func (s *Store) CreateConfig(ctx context.Context, c Config) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO configs (run_id, pool, card, flashboot, endpoint_id, price_hr, status, error, created_at, deleted_at, rp_metrics)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.RunID, c.Pool, c.Card, c.FlashBoot, c.EndpointID, c.PriceHr, c.Status, c.Error,
		fmtTime(c.CreatedAt), nullTime(c.DeletedAt), nullJSON(c.RPMetrics))
	if err != nil {
		return 0, fmt.Errorf("store: create config: %w", err)
	}
	return res.LastInsertId()
}

// UpdateConfig writes the fields that change after creation: endpoint_id,
// status, error, deleted_at and rp_metrics.
func (s *Store) UpdateConfig(ctx context.Context, c Config) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE configs SET endpoint_id = ?, status = ?, error = ?, deleted_at = ?, rp_metrics = ? WHERE id = ?`,
		c.EndpointID, c.Status, c.Error, nullTime(c.DeletedAt), nullJSON(c.RPMetrics), c.ID)
	return oneRow(res, err, fmt.Sprintf("update config %d", c.ID))
}

// InsertJob inserts a job and returns its new ID. j.ID is ignored.
func (s *Store) InsertJob(ctx context.Context, j Job) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (config_id, phase, level, runpod_job_id, worker_id, gpu_type_id, status,
		                   intended_at, sent_at, wall_ms, delay_ms, exec_ms, error,
		                   matmul_ms, tflops, cuda_init_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ConfigID, j.Phase, j.Level, j.RunpodJobID, j.WorkerID, j.GPUTypeID, j.Status,
		fmtTime(j.IntendedAt), fmtTime(j.SentAt), j.WallMs, j.DelayMs, j.ExecMs, j.Error,
		j.MatmulMs, j.TFLOPS, j.CudaInitMs)
	if err != nil {
		return 0, fmt.Errorf("store: insert job: %w", err)
	}
	return res.LastInsertId()
}

// SELECT column lists live next to the scan functions that read them.
const runCols = `id, created_at, finished_at, status, params, budget_usd, est_max_usd, error`

// ListRuns returns every run, newest first, without configs or jobs.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	runs := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list runs: %w", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	return runs, nil
}

// GetRun returns one run with its configs and their jobs, ordered by id. A
// missing run returns an error wrapping sql.ErrNoRows. Each query's rows
// must close before the next starts: with one connection it would deadlock.
func (s *Store) GetRun(ctx context.Context, id string) (RunDetail, error) {
	run, err := s.GetRunRow(ctx, id)
	if err != nil {
		return RunDetail{}, err
	}
	configs, err := s.listConfigs(ctx, id)
	if err != nil {
		return RunDetail{}, fmt.Errorf("store: get run %s: %w", id, err)
	}
	if err := s.attachJobs(ctx, id, configs); err != nil {
		return RunDetail{}, fmt.Errorf("store: get run %s: %w", id, err)
	}
	return RunDetail{Run: run, Configs: configs}, nil
}

// GetRunRow returns one run without its configs or jobs. A missing run
// returns an error wrapping sql.ErrNoRows.
func (s *Store) GetRunRow(ctx context.Context, id string) (Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
	if err != nil {
		return Run{}, fmt.Errorf("store: get run %s: %w", id, err)
	}
	return run, nil
}

// MarkEndpointDeleted stamps deleted_at on the config that owned
// endpointID, if any and not yet stamped.
func (s *Store) MarkEndpointDeleted(ctx context.Context, endpointID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE configs SET deleted_at = ? WHERE endpoint_id = ? AND deleted_at IS NULL`,
		fmtTime(at), endpointID)
	if err != nil {
		return fmt.Errorf("store: mark endpoint %s deleted: %w", endpointID, err)
	}
	return nil
}

const configCols = `id, run_id, pool, card, flashboot, endpoint_id, price_hr, status, error, created_at, deleted_at, rp_metrics`

// listConfigs returns a run's configs, each with an empty (non-nil) Jobs slice.
func (s *Store) listConfigs(ctx context.Context, runID string) ([]ConfigDetail, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+configCols+` FROM configs WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	configs := []ConfigDetail{}
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, ConfigDetail{Config: c, Jobs: []Job{}})
	}
	return configs, rows.Err()
}

const jobCols = `id, config_id, phase, level, runpod_job_id, worker_id, gpu_type_id, status,
                 intended_at, sent_at, wall_ms, delay_ms, exec_ms, error,
                 matmul_ms, tflops, cuda_init_ms`

// attachJobs loads every job of the run in one query and appends each to
// its config's Jobs.
func (s *Store) attachJobs(ctx context.Context, runID string, configs []ConfigDetail) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobCols+` FROM jobs
		 WHERE config_id IN (SELECT id FROM configs WHERE run_id = ?)
		 ORDER BY id`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()

	index := make(map[int64]int, len(configs))
	for i, c := range configs {
		index[c.ID] = i
	}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return err
		}
		i, ok := index[j.ConfigID]
		if !ok {
			// The config was created after listConfigs ran; the next read shows it.
			continue
		}
		configs[i].Jobs = append(configs[i].Jobs, j)
	}
	return rows.Err()
}

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface {
	Scan(dest ...any) error
}

func scanRun(sc scanner) (Run, error) {
	var r Run
	var created string
	var finished, params sql.NullString
	err := sc.Scan(&r.ID, &created, &finished, &r.Status, &params, &r.BudgetUSD, &r.EstMaxUSD, &r.Error)
	if err != nil {
		return Run{}, err
	}
	if r.CreatedAt, err = parseTime(created); err != nil {
		return Run{}, err
	}
	if r.FinishedAt, err = parseNullTime(finished); err != nil {
		return Run{}, err
	}
	r.Params = rawJSON(params)
	return r, nil
}

func scanConfig(sc scanner) (Config, error) {
	var c Config
	var created string
	var deleted, metrics sql.NullString
	err := sc.Scan(&c.ID, &c.RunID, &c.Pool, &c.Card, &c.FlashBoot, &c.EndpointID, &c.PriceHr, &c.Status, &c.Error,
		&created, &deleted, &metrics)
	if err != nil {
		return Config{}, err
	}
	if c.CreatedAt, err = parseTime(created); err != nil {
		return Config{}, err
	}
	if c.DeletedAt, err = parseNullTime(deleted); err != nil {
		return Config{}, err
	}
	c.RPMetrics = rawJSON(metrics)
	return c, nil
}

func scanJob(sc scanner) (Job, error) {
	var j Job
	var intended, sent string
	err := sc.Scan(&j.ID, &j.ConfigID, &j.Phase, &j.Level, &j.RunpodJobID, &j.WorkerID, &j.GPUTypeID, &j.Status,
		&intended, &sent, &j.WallMs, &j.DelayMs, &j.ExecMs, &j.Error,
		&j.MatmulMs, &j.TFLOPS, &j.CudaInitMs)
	if err != nil {
		return Job{}, err
	}
	if j.IntendedAt, err = parseTime(intended); err != nil {
		return Job{}, err
	}
	if j.SentAt, err = parseTime(sent); err != nil {
		return Job{}, err
	}
	return j, nil
}

// oneRow turns an UPDATE by id that changed nothing into a wrapped sql.ErrNoRows.
func oneRow(res sql.Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s: %w", what, sql.ErrNoRows)
	}
	return nil
}

func fmtTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// nullTime maps a nil *time.Time to SQL NULL.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

// parseTime reads a stored time.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad stored time %q: %w", s, err)
	}
	return t, nil
}

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// nullJSON stores empty JSON as NULL and anything else as TEXT, not a BLOB.
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// rawJSON is the reverse of nullJSON: NULL comes back as nil.
func rawJSON(ns sql.NullString) json.RawMessage {
	if !ns.Valid {
		return nil
	}
	return json.RawMessage(ns.String)
}
