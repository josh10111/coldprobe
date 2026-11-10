# Runpod API

Every Runpod endpoint coldprobe calls, the fields we rely on, quirks we found and what's still unverified.

We use **REST v2 only**. REST v1 and GraphQL are being retired.
- Management API: `https://api.runpod.io/v2`, Bearer auth. The public OpenAPI spec at `https://api.runpod.io/v2/openapi.json` is the source of truth.
- Job API (a different host): `https://api.runpod.ai/v2/{endpointId}/...`.

## Management API (checked against the public v2 OpenAPI, 2026-11-07)

### Create endpoint: `POST /v2/serverless`

- Requires `name`, `type` (`QUEUE` | `LOAD_BALANCER`) and `scaling`.
- `image` is required unless `templateId` is given.
- `scaling` is a union: `{type: "QUEUE_DELAY", queueDelay: number >= 0.5}` or `{type: "REQUEST_COUNT", ...}`.
- The body has `unevaluatedProperties: false`, so unknown fields are rejected.
- `gpu: {pools: [poolId], excludedTypes?: [gpuTypeId], count?: 1}`. GPUs are chosen by **pool**, not by GPU type.
- `gpu.excludedTypes` narrows a pool to specific cards: workers run on every card in `pools` except these. The ids are the catalog's `gpus[].id` (`NVIDIA RTX A4000`), the same strings `workers[].gpuTypeId` reports, not the short `gpus[].name`. Listing every other card in the pool pins one card; that's coldprobe's card mode. Checked 2026-11-10: create with `pools: ["AMPERE_16"]` and three excluded ids returned 201 and echoed `excludedTypes` unchanged, a GET read it back the same, and DELETE answered 204. In that body `flashboot` must be the string (`"OFF"`/`"FLASHBOOT"`), not a boolean. coldprobe leaves the key out when the list is empty: per the spec a PATCH that sends `pools` without it clears the exclusions, so the key only goes out when it means something. That workers then land only on the remaining cards wasn't checked (no jobs were sent).
- `workers: {min, max, idleTimeout}`. `idleTimeout` is in **seconds**, 1–3600, default 10.
- `timeout` is in **milliseconds**, default 300000.
- `flashboot`: `OFF | FLASHBOOT | PRIORITY_FLASHBOOT`, default `OFF`.
- `env` (`map[string]string`) is accepted.
- There is no `tags` field on create or PATCH, so our endpoints are marked with the name prefix `bench-coldprobe-`.

### Update endpoint: `PATCH /v2/serverless/{id}`

- The body is an `UpdateEndpointRequest`. Every field is optional, and a field left out stays unchanged. coldprobe sends only `{"workers": {"max": n}}` (`Mgmt.SetMaxWorkers`).
- `workers.max: 0` pauses the endpoint. See [FlashBoot keep-alive and forced cold starts](#flashboot-keep-alive-and-forced-cold-starts-verified-2026-11-08) for what that does.

### Delete endpoint: `DELETE /v2/serverless/{id}`

Terminates workers and cancels queued jobs. No scale-down is needed first.

### GPU catalog: `GET /v2/catalog/gpus`

- Pool ids come from `gpus[].pool`. The value is null if the GPU isn't available on serverless.
- Prices come from `gpus[].price.serverless`: the USD **per hour** list price per GPU. There is no flex/active split, and account discounts aren't reflected.
- Stock comes from `gpus[].availability`, present only with `include=AVAILABILITY` (which also needs `product`, here `SERVERLESS`). It is one of `HIGH`, `MEDIUM`, `LOW`, `NONE` (the go-sdk's `types.AvailabilityLevel`), per GPU type, overall across data centers; `gpus[].dataCenters` breaks it down per data center. How well `NONE` predicts a first boot that never gets a worker is unverified.

### Metrics: `GET /v2/serverless/{id}/metrics/requests | cold-starts | workers`

- Query: `startTime`, `endTime`, `interval` ∈ `1m, 5m, 15m, 30m, 1h, 6h, 12h, 1d`.
- Stats are `EndpointDurationStats{min, max, total, p70, p90, p95, p98}` in ms. **There is no p50.**
- Empty buckets are omitted, and the response is paginated (`pagination`).
- Request records: `time, requests, completed, failed, retries, executionTimeMs, delayTimeMs`.
- Cold-start records: `time, count, durationMs`.

### Errors and rate limits

- Errors follow RFC 9457 (`title, status, detail`).
- Management rate limits aren't published. Read the `RateLimit-Policy`, `RateLimit` and `Retry-After` headers.

### Go SDK: `github.com/runpod/go-sdk/v2` v2.1.0 (go ≥ 1.25)

- `runpod.New(runpod.WithAPIKey(k), runpod.WithBaseURL(u))`, with clients `.Serverless`, `.Catalog`, ...
- The scaling union is set with `(*EndpointScaling).FromQueueDelayScaling(...)`.
- `transport.Retry` retries POST and PATCH only on 429, and idempotent methods on 429, 5xx and transport errors.
- Update is `Serverless.UpdateEndpointWithResponse(ctx, id, types.UpdateEndpointRequest{Workers: &types.EndpointWorkers{Max: &n}})`. `Max` is `*int` with `omitempty`, so a pointer to 0 is sent.

### Fields the management wrapper relies on

Every field `internal/runpod/mgmt.go` sends or reads.

| call | we send | we read |
|---|---|---|
| `POST /v2/serverless` | `name`, `type`, `scaling.type`, `scaling.queueDelay`, `image`, `gpu.pools`, `gpu.excludedTypes` (only when non-empty), `workers.min`, `workers.max`, `workers.idleTimeout` (s), `timeout` (ms), `flashboot`, `env` | 201: `id` |
| `PATCH /v2/serverless/{id}` | `workers.max` | status only: 2xx = success, anything else (404 too) = error |
| `DELETE /v2/serverless/{id}` | — | status only: 2xx or 404 = success |
| `GET /v2/serverless` | `cursor` | `endpoints[].id`, `endpoints[].name`, `pagination.nextCursor`, `pagination.hasNextPage` |
| `GET /v2/serverless/{id}/workers` | — | `summary.{running, idle, initializing, throttled, unhealthy, total}`, `workers[].id`, `workers[].gpuTypeId` (null until placed) |
| `GET /v2/catalog/gpus` | `include=AVAILABILITY`, `product=SERVERLESS` | `gpus[].id`, `gpus[].name`, `gpus[].memory` (GB), `gpus[].pool` (null = not on Serverless), `gpus[].price.serverless` (USD/GPU-hour, absent = not offered), `gpus[].availability` (`HIGH`/`MEDIUM`/`LOW`/`NONE`) |
| `GET /v2/serverless/{id}/metrics/requests` | `startTime` (UTC RFC 3339, floored to the minute), `interval=1m`, `cursor`; **not** `endTime` | `records[].{time, requests, completed, failed, retries, executionTimeMs, delayTimeMs}`, `pagination.*` |
| `GET /v2/serverless/{id}/metrics/cold-starts` | same | `records[].{time, count, durationMs}`, `pagination.*` |
| duration stats (all metrics) | — | `min`, `max`, `total` (float), `p70`, `p90`, `p95`, `p98` (ms) |
| any non-2xx | — | status, `Retry-After` header, body `title` and `detail` (RFC 9457) |

The SDK only decodes a body whose `Content-Type` contains `json`; we rely on Runpod sending `application/json` / `application/problem+json`.

## Job API: `https://api.runpod.ai/v2/{endpointId}/...`

- Routes: `runsync`, `run`, `status/{jobId}`, `cancel/{jobId}`, `health`, `purge-queue`.
- It is **not** in the management OpenAPI spec. Response fields come from the docs: `id, status, delayTime, executionTime, workerId?, output, error`.
- Rate limits are per endpoint per 10 s. `/run` allows 1000.

## How to run the day-1 checks

`scripts/day1-checks.sh` checks API behaviour coldprobe depends on (see [Still unverified](#still-unverified)) against the real API using only curl and jq. You need bash, curl ≥ 7.55, jq ≥ 1.6 and `column`.

**The key.** The script reads `RUNPOD_API_KEY` from your shell. If it isn't set there, it loads the repo's gitignored `.env` without printing anything from it. It writes the key into a temp header file with mode 600 and passes that file to `curl -H @file`, so the key never shows up in `ps`. It deletes the file on exit. Every JSON it prints goes through a jq filter that drops keys matching `(account|user|org).*id` or `email`.

### Read-only mode (free)

```sh
./scripts/day1-checks.sh
```

This mode makes three GETs and creates nothing:
- `GET /v2/catalog/gpus?include=AVAILABILITY&product=SERVERLESS` prints a table of pool, GPU id, VRAM, `price.serverless` and availability per data center. If the server rejects those params, the script says so and retries with no params.
- It prints the `RateLimit*` and `Retry-After` headers from that call.
- `GET /v2/serverless` prints the id and name of each existing endpoint.

Use it to pick pool ids for the probe.

### Probe mode (billable)

```sh
./scripts/day1-checks.sh --probe <poolId> [OFF|FLASHBOOT]          # prints the plan, then asks you to type yes
./scripts/day1-checks.sh --probe <poolId> [OFF|FLASHBOOT] --yes    # approved non-interactively
```

**Before it creates anything**, the script prints the plan and an upper-bound cost: (1500 s hard timeout + 150 s teardown slack) × 1 worker × the highest `price.serverless` among the pool's GPUs. It refuses to run if the pool is unknown or if any GPU in it has no price.

- **On a terminal**, you must type exactly `yes`.
- **Without a terminal** (for example from another script), it refuses unless you pass `--yes`. A run without `--yes` is therefore a free dry run that shows the cost; re-run with `--yes` to go ahead.

The probe then creates `bench-coldprobe-<UTC YYYYMMDD-HHMMSS>-<4 hex>-probe`. That is a run ID's shape, so coldprobe's janitor (at startup, before every run, or by hand) reads it as a young endpoint of a run it doesn't know and leaves it alone for an hour; any other name would be deleted as unparseable. It has:
- `QUEUE`, `QUEUE_DELAY 4`
- workers min 0, max 1, `idleTimeout` 5 s
- `timeout` 60000 ms
- image `$COLDPROBE_WORKER_IMAGE` (default `ghcr.io/josh10111/coldprobe-worker:latest`, which must be public on GHCR)
- `MODEL_LOAD_SECONDS=5`

It then measures:

| step | what it does | answers |
|------|--------------|---------|
| (a) | `runsync?wait=60000` with `{"input":{"sleep_ms":1000}}`. Prints the wall time and the full JSON, and polls `/status/{jobId}` if the job comes back unfinished (`IN_QUEUE` after 60 s on a cold endpoint). Not a longer wait: a `wait=300000` response got dropped (see [Jobs](#jobs-apirunpodaiv2id)). Gives up after 10 min, telling you to check that the image is public. | runsync response fields |
| (b) | Polls `/v2/serverless/{id}/workers` every 2 s, printing the active count and `summary` each time, until no worker is active (cap 300 s). Active is coldprobe's rule: `RUNNING`, `INITIALIZING`, or `IDLE` with a non-null `uptimeSeconds`. `summary.total` is no use here: it never reaches 0. Prints the full response at zero. | what scaled-to-zero looks like, and how long it takes |
| (c) | A second runsync, which should be a cold start. | cold-start wall time |
| (d) | Polls `metrics/cold-starts` and `metrics/requests` every 30 s until each counts **both** jobs (cap 15 min). Uses `interval=1m` and `startTime` = probe start floored to the minute, with no `endTime`, so it defaults to now. Job 2's lag is logged as a bracket: "between A and B s", from the poll before to the poll that saw it. Job 1's figure is only an upper bound, because the first poll comes after (b) and (c). | how soon `/metrics` reflects a run |

If create fails, the script prints the HTTP status and the RFC 9457 body. On an out-of-stock pool, that body is the answer to "create with no stock". A worker stuck in `summary.throttled` during (b) is the other way stock shortage can show up.

**Teardown.**
- The script deletes the endpoint, retrying up to 3 times, then lists endpoints to confirm that no `bench-coldprobe-` names remain.
- An `EXIT` trap does the same delete if the script fails or is interrupted at any point. `INT`, `TERM`, `HUP` and `PIPE` all route to it.
  - Once teardown starts, it ignores further signals. A second Ctrl-C, a closed terminal, or piping through `| tee` and pressing Ctrl-C can't cut the delete short. **Piping the output (e.g. `| tee probe.log`) is safe.** Teardown messages go to stderr.
  - A 4xx other than 408/429 means nothing was created. A 408 or 429 can come from middleware while the create continues behind it. If create got a 408 or 429, timed out (90 s), or got a 5xx, the trap looks for the endpoint by its unique name, 3 times, 5 s apart. If it still can't find the endpoint, or the list call fails, it prints a loud warning naming the endpoint. Check the console for it.
- If the delete still fails, the script prints a loud warning with the endpoint id. **Delete that endpoint in the console right away.**
- The whole probe stops after 25 minutes: each request's `--max-time` is clipped to the time left.
- Polls in (b) and (d) stop early with a warning when the deadline is near, so the run still reaches the delete and the check.

`RUNPOD_MGMT_URL` and `RUNPOD_JOB_URL` override the API base URLs. Use them only to point the script at a local mock for testing.

## Day-1 findings (verified 2026-11-07 against the real API)

Measured with `scripts/day1-checks.sh` plus four instrumented one-off probes. Each probe created one endpoint on AMPERE_24 (`workers.max` 1, `idleTimeout` 5 s, FlashBoot OFF, `MODEL_LOAD_SECONDS=5`, 1000 ms jobs) and deleted it afterwards. Afterwards, the janitor-style listing found 0 `bench-coldprobe-*` endpoints left.

### Catalog and rate limits
- Serverless pools and list prices (USD per GPU-hour, from `price.serverless`):
  - AMPERE_16: **$0.58/h** ($0.000161/s). RTX 2000 Ada, RTX 4000 Ada, RTX A4000, RTX A4500.
  - AMPERE_24: **$0.75/h** ($0.000208/s). RTX 3090, L4, RTX A5000, RTX PRO 6000 Blackwell MIG 1g.24gb.
  - ADA_24: **$1.34/h** (RTX 4090).
  - Pricier pools run up to B300 at $11.99/h.
- **Pool names don't match GPU generations.** "AMPERE_16" contains Ada cards. Our AMPERE_24 jobs actually ran on an **RTX PRO 6000 Blackwell MIG 1g.24gb** slice. So coldprobe records `gpuTypeId` per job, from the worker list.
- Management API rate limits, from the headers: `ratelimit-policy: "minute";q=180;w=60, "hour";q=7200;w=3600, "day";q=86400;w=86400`. The remaining count is in `ratelimit`.

### Create, delete, metrics
- `POST /v2/serverless` returns **201 in about 1 s**. It echoes the body plus `requestUrls`, `gpu.minCudaVersion: "12.0"` and `disk: 10`.
- `DELETE /v2/serverless/{id}` returns **204**.
- After delete, `/metrics/*` returns **404** `{"detail":"endpoint not found"}`. **Snapshot metrics before teardown.**
- **After delete, the job API doesn't reliably say so.** Two runs on 2026-11-08, each deleting an endpoint by hand mid-load (DELETE → 204), saw two different behaviours:
  - **One run:** every later `POST /runsync` returned **HTTP 409** with detail `Endpoint is paused (max_workers=0). Set max_workers > 0 to accept work.`, in about 60 ms. Never 404.
  - **The other (run 20261108-215004-a254, AMPERE_24):** 12 more jobs **COMPLETED** after the delete (the old worker kept serving for a while), then the next 42 runsync POSTs **hung**, with no answer within the 60 s wait, until the run was cancelled 61 s later. Probably a race between the worker draining and the endpoint record going away.
  - **The management API is the reliable signal:** `GET /v2/serverless/{id}` and `GET /v2/serverless/{id}/workers` return **404** `{"detail":"endpoint not found"}` once the endpoint is deleted (both runs). The job API's `GET /{id}/health` still returns 200 with stale counts.
  - coldprobe therefore polls `/workers` every 10 s per config for the whole measurement (`watchGone`), and treats a runsync 409 (or 404) as a hint that triggers the same check at once (`confirmGone`). Not checked: what `/status` and `/purge-queue` answer after a delete (the fake answers 409 like runsync).
- **Metrics lag:** both requests and the cold starts appeared **within 40 s** of the last job finishing (the first poll; the true lag may be lower).
- **Runpod's cold-start metric is not what a caller sees.** In the same run:
  - Our first request's `delayTime` was **53.1 s**.
  - `/metrics/cold-starts` reported a `durationMs` of **about 11.2 s**.
  - So Runpod's metric covers container start → ready and leaves out the ~40 s spent getting a container scheduled on a host.
  - coldprobe reports both and explains the gap.
- `cold-starts` counted **3** while we caused 2. The extra one is a second worker Runpod started even though `workers.max` was 1 (see below).

### Jobs (`api.runpod.ai/v2/{id}/...`)
- A COMPLETED `runsync` returns `{delayTime, executionTime, id, output, status, workerId}`. The times are integers in ms. **`runsync` job ids start with `sync-`**; `/run` ids look like `<uuid>-u2`.
- `runsync?wait=60000` on a cold endpoint returned `{"id":"sync-…","status":"IN_QUEUE"}` after **60.2 s**, as documented.
- **Gotcha:** with `wait=300000` and no worker placed within 300 s, the request returned **0 bytes** and was cut at the client's 310 s timeout, instead of returning IN_QUEUE. coldprobe therefore uses `wait=60s` and then polls `/status`.
- **Timings on AMPERE_24 for a 1000 ms sleep:**

  | Situation | delayTime | executionTime |
  |---|---|---|
  | first boot on a fresh endpoint | 51.6–54.9 s (about 44 s waiting for a container on a host, + 5 s simulated model load, + about 0.4 s of fitness checks) | |
  | **cold start** after scale-to-zero, same worker slot, image cached | **17.3 s** | |
  | warm | **17 ms** | 1091–1102 ms |
- The runpod Python SDK 2.1.0 runs **fitness checks** at worker start (GPU binary test, memory, disk, CUDA ≥ 11.8, network to api.runpod.ai). Any failure makes the worker exit with `os._exit(1)`. Our slim, non-CUDA image passes them, because the GPU test binary ships with the SDK.
- `/health` returns `jobs{completed,failed,inProgress,inQueue,retried}` and `workers{idle,initializing,ready,running,throttled,unhealthy}`.

### Workers and scale-to-zero
- `GET /v2/serverless/{id}/workers` lists `{id, status, gpuTypeId, dataCenterId, uptimeSeconds, isStale}`. We saw the statuses `IDLE`, `INITIALIZING` and `RUNNING`.
- **`summary.total` never reaches 0.** After the idle timeout, the container stops (the system log says "stop container"), but the worker stays listed as `IDLE` with **`uptimeSeconds: null`**. We watched it stay that way for 16 min.
- A warm idle worker is `IDLE` with a non-null `uptimeSeconds`.
- **coldprobe's rule:** a worker is *active* if it is `RUNNING`, `INITIALIZING`, or `IDLE` with non-null uptime. "Scaled to zero" means active == 0.
- **Time to scale to zero** after the last job: **39–53 s** with `idleTimeout` 5 s, polled every 2 s.
- With `workers.max: 1`, Runpod briefly started a **second** worker (`total` read 2). Expect `total` to exceed `max` for a while.

### First end-to-end coldprobe run (2026-11-07)
Settings: AMPERE_24 × FlashBoot OFF/ON, `maxWorkers` 1, `idleTimeoutS` 5, 2 cold starts, 5 warm requests, open-loop levels of 1 and 4 req/s for 20 s each (30 s drain), 500 ms jobs, `MODEL_LOAD_SECONDS=5`, budget $1.00. The run took 7 min 16 s and ended `completed`. Both endpoints were deleted, and the janitor dry run found 0 left afterwards.

| | FlashBoot OFF | FlashBoot ON |
|---|---|---|
| first boot (wall − exec) | 132.0 s | 73.7 s |
| **cold start p50 / max (n=2)** | **27.3 s / 92.5 s** | **0.76 s / 13.6 s** |
| warm p50 / p95 (500 ms job) | 721 / 729 ms | 716 / 737 ms |
| 1 req/s: throughput, p50 latency | 1.02 rps, 672 ms | 1.02 rps, 682 ms |
| 4 req/s: throughput, p50 / p95 latency | 1.38 rps, 16.8 s / 31.5 s | 1.36 rps, 16.7 s / 31.7 s |
| estimated $/1k requests | $0.69 | $0.34 |

What this shows:
- **FlashBoot cut the median cold start from 27 s to 0.76 s.** One FlashBoot sample still took 13.6 s, so FlashBoot is not guaranteed.
- **About 220 ms of platform overhead per warm request.** The job sleeps for 500 ms, and the warm p50 is about 720 ms.
- **One worker saturates at about 1.4 req/s.** Each request costs 500 ms of sleep plus that overhead. At 4 req/s the queue grows, and latency climbs to about 17 s p50 because coldprobe measures from the *intended* send time (open loop). The 11 and 12 "failed" jobs per config are those still queued at the drain deadline. They were cancelled and purged, so they never reached a worker, and that's why 23 jobs have no `gpuTypeId`.
- Every job that ran landed on an **RTX PRO 6000 Blackwell MIG 1g.24gb** slice inside the "AMPERE_24" pool.
- Runpod's `cold-starts` metric reported 30.1 s and 91.8 s for the OFF config. Those are close to our numbers. The FlashBoot cold starts that Runpod counted came in around 11–13 s.

### FlashBoot keep-alive and forced cold starts (verified 2026-11-08)

Found when a GPU run got 0 cold samples on both FlashBoot configs (each `waitScaledToZero` timed out at 180 s), while the OFF configs got 3 of 3.

- **On FlashBoot endpoints, a worker that served a job often never scales to zero.**
  - A diagnostic endpoint on AMPERE_24 had FlashBoot on, `idleTimeout` 5 s and a synthetic image.
  - Its first job's delay was 75 s.
  - That worker then stayed `IDLE` with a growing `uptimeSeconds` (20 s → 718 s) for 12 minutes, so `Active()` never dropped to 0.
  - A job sent after those 12 minutes ran on the **same** worker, with the same `booted_at` (the same process), and a `delayTime` of 585 ms. That's a warm start.
  - In another run on AMPERE_24 with FlashBoot, 5 workers were listed while `max` was 2. The one that ran the first boot stayed `IDLE` with uptime growing from 7 s to 166 s, and 4 standby workers were `IDLE` with `uptimeSeconds: null`.
- **It varies.** On 2026-11-07 (the first end-to-end run) FlashBoot got 3 of 3 cold samples (p50 0.76 s). On 2026-11-08 (16:21) it got 2 of 3 (1.5 s and 12 s).
- **Pausing forces a new worker.** On AMPERE_24, synthetic image, `idleTimeout` 5 s, `max` 2:
  - `PATCH workers.max 0` returned **HTTP 200 in about 0.8 s**.
  - `Active()` reached **0 within 1–4 s**.
  - While paused, `/workers` kept answering 200.
  - Restoring `workers.max` to 2 returned **HTTP 200**.
  - **The next job always ran on a NEW worker**, with a new worker ID and a new `booted_at` (a fresh process). Its `delayTime` was **70–81 s**, about the same as a first boot: FlashBoot 71.7 s and 70.1 s, OFF 72.6 s and 80.6 s.
- **FlashBoot gives no benefit after a pause.** Compare natural cold starts, where the same worker comes back: OFF 10–27 s, FlashBoot 0.76–1.5 s (when they happen at all).
- **What coldprobe does with it:** natural mode stays the default and reports skipped samples honestly. `coldStartMode: "forced"` pauses and restores before each cold sample, and the UI labels those samples "new-worker cold start".
- **A kept-alive `IDLE` worker isn't billed while idle.** Checked 2026-11-10 against the billing API: a FlashBoot worker kept alive after its job is not metered while idle; the UI's FlashBoot frame says "idle time not billed". Figures stay out of this doc.
- **Not checked:**
  - What runsync answers while paused. No job was sent then. We expect 409 `Endpoint is paused (max_workers=0)...`, the post-delete text, which names `max_workers=0`.
  - Which workers a paused endpoint's `/workers` lists.
  - What a pause does to jobs still queued or running.

### Recorded runs (2026-11-10)
Two runs on AMPERE_16 + AMPERE_24 × FlashBoot OFF/ON with the defaults: `maxWorkers` 2, `idleTimeoutS` 5, 3 cold starts, 10 warm requests, open-loop levels of 1, 3 and 8 req/s for 30 s each, 500 ms jobs, `MODEL_LOAD_SECONDS=5`, budget $4. Both ended `completed`, every endpoint was deleted, and the janitor dry run found 0 left after each. Cold = wall − exec of a cold job; $/1k is coldprobe's estimate, not billing.

**Natural mode** (run `20261110-052424-9862`, 9 min 13 s). Attributed cost estimate $0.22 in total; the guard's upper bound is ≤ $1.23.

| config | GPU the jobs ran on | first boot | cold p50 (n) | warm p50 | est. $/1k | completed | unfinished |
|---|---|---|---|---|---|---|---|
| AMPERE_16 OFF | NVIDIA RTX A4500 | 72.0 s | 13.2 s (3) | 1.14 s | $0.173 | 285 | 90 |
| AMPERE_16 FLASHBOOT | NVIDIA RTX A4500 | 74.0 s | 13.1 s (3) | 1.15 s | $0.168 | 286 | 89 |
| AMPERE_24 OFF | RTX PRO 6000 Blackwell MIG 1g.24gb | 72.0 s | 9.3 s (3) | 674 ms | $0.181 | 375 | 0 |
| AMPERE_24 FLASHBOOT | RTX PRO 6000 Blackwell MIG 1g.24gb | 76.1 s | kept warm (0) | 598 ms | $0.156 | 372 | 0 |

**Forced mode** (run `20261110-053408-e49a`, same settings with `coldStartMode: "forced"`). Every cold sample is a new-worker cold start. The jobs ran on the same GPUs as above. Attributed cost estimate $0.36 in total; the guard's upper bound is ≤ $1.25.

| config | first boot | new-worker cold p50 (n) | warm p50 | est. $/1k | completed | unfinished |
|---|---|---|---|---|---|---|
| AMPERE_16 OFF | 52.1 s | 61.5 s (3) | 1.17 s | $0.249 | 284 | 90 |
| AMPERE_16 FLASHBOOT | 84.3 s | 65.6 s (3) | 1.16 s | $0.279 | 285 | 90 |
| AMPERE_24 OFF | 76.0 s | 63.6 s (3) | 736 ms | $0.283 | 375 | 0 |
| AMPERE_24 FLASHBOOT | 65.7 s | 63.7 s (3) | 701 ms | $0.270 | 374 | 0 |

What this shows:
- **Natural mode: FlashBoot gave the same median on AMPERE_16** (13.2 s vs 13.1 s); one of the three FlashBoot cold starts took 1.1 s. On AMPERE_24 the FlashBoot worker never scaled to zero, so no cold sample was possible ("kept warm"; see [above](#flashboot-keep-alive-and-forced-cold-starts-verified-2026-11-08)).
- **Forced mode: every cold start landed on a brand-new worker,** on both pools, FlashBoot on or off: typical (p50) values 61–66 s, individual samples 56–88 s.
- **AMPERE_16 (RTX A4500) left about 90 requests unfinished** at 8 req/s with 2 workers, while the Blackwell MIG slice on AMPERE_24 drained everything. Three cold samples per config is a small sample.

### Estimates vs billing
- **Diagnostic probes:** the actual billed amount (`/v2/billing/serverless`) was far below the upper-bound estimates, roughly an order of magnitude, because the guard's bound assumes `maxWorkers+1` workers billing for the whole run.

### Still unverified
- [ ] Whether create fails, or just waits, when a pool has no stock. The first probe sat for over 300 s with no worker placed, and billing shows $0, which fits "waits".
- [ ] Billing lag: `/v2/billing/serverless` still showed $0 a few minutes after the probes. Note that it requires `startTime` and `endTime` together, or it returns 400.
