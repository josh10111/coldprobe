-- Applied on every Open; IF NOT EXISTS makes that a no-op once the tables
-- exist. There is no migration tool: a new nullable column (or NOT NULL with
-- a constant default) goes here and in store.go's addedJobColumns or
-- addedConfigColumn, which add it to older databases. Any other table change
-- means deleting the database file.
--
-- Times are TEXT in a fixed-width RFC 3339 UTC form (timeLayout in store.go)
-- so they sort as text. Strings that are "empty when unset" are NOT NULL
-- DEFAULT '' so Go can scan them straight into a string.

CREATE TABLE IF NOT EXISTS runs (
    id          TEXT PRIMARY KEY,
    created_at  TEXT NOT NULL,
    finished_at TEXT,
    status      TEXT NOT NULL CHECK (status IN
                    ('running', 'completed', 'failed', 'cancelled', 'aborted_budget', 'aborted_crash')),
    params      TEXT,  -- JSON: the bench parameters the run was started with
    budget_usd  REAL NOT NULL DEFAULT 0,
    est_max_usd REAL NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);

-- One row per GPU pool (or card) x FlashBoot setting, i.e. one temporary endpoint.
CREATE TABLE IF NOT EXISTS configs (
    id          INTEGER PRIMARY KEY,  -- alias for SQLite's rowid, auto-assigned
    run_id      TEXT NOT NULL REFERENCES runs (id),
    pool        TEXT NOT NULL,
    -- Card mode: the GPU id the endpoint was pinned to; '' in pool mode.
    -- Added later: store.go adds it to older databases.
    card        TEXT NOT NULL DEFAULT '',
    flashboot   TEXT NOT NULL,
    endpoint_id TEXT NOT NULL DEFAULT '',
    price_hr    REAL NOT NULL DEFAULT 0,
    status      TEXT NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    deleted_at  TEXT,
    rp_metrics  TEXT  -- JSON: Runpod's own /metrics numbers for this endpoint
);
CREATE INDEX IF NOT EXISTS configs_run_id ON configs (run_id);

-- One row per request sent to an endpoint.
CREATE TABLE IF NOT EXISTS jobs (
    id            INTEGER PRIMARY KEY,
    config_id     INTEGER NOT NULL REFERENCES configs (id),
    phase         TEXT NOT NULL CHECK (phase IN ('first_boot', 'cold', 'warm', 'load')),
    level         REAL NOT NULL DEFAULT 0,  -- load level (req/s); 0 outside the load phase
    runpod_job_id TEXT NOT NULL DEFAULT '',
    worker_id     TEXT NOT NULL DEFAULT '',
    gpu_type_id   TEXT NOT NULL DEFAULT '',  -- pools mix GPU models; this is the one the worker had
    status        TEXT NOT NULL,
    intended_at   TEXT NOT NULL,
    sent_at       TEXT NOT NULL,
    wall_ms       REAL NOT NULL DEFAULT 0,
    delay_ms      REAL NOT NULL DEFAULT 0,
    exec_ms       REAL NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    -- From the GPU worker image's output; NULL when the job didn't report
    -- them. Added later: store.go adds them to older databases.
    matmul_ms     REAL,  -- median ms per fp16 matmul
    tflops        REAL,  -- that matmul's TFLOP/s
    cuda_init_ms  REAL   -- the worker process's CUDA init, measured once at its start
);
CREATE INDEX IF NOT EXISTS jobs_config_id ON jobs (config_id);
