// The server's JSON. Names must match exactly: the server rejects unknown
// fields in POST bodies. Optional fields are omitted by the server, never null.

export type FlashBoot = 'OFF' | 'FLASHBOOT'

// natural waits for Runpod to scale to zero; forced pauses the endpoint first
// so each cold sample needs a brand-new worker.
export type ColdStartMode = 'natural' | 'forced'

export type Availability = 'HIGH' | 'MEDIUM' | 'LOW' | 'NONE' | ''

export type RunStatus =
  | 'running'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'aborted_budget'
  | 'aborted_crash'

export type ConfigStatus =
  | 'pending'
  | 'creating'
  | 'running'
  | 'tearing_down'
  | 'done'
  | 'failed'
  | 'cancelled'

export type JobPhase = 'first_boot' | 'cold' | 'warm' | 'load'

// Anything else the server sends counts as failed, so this stays a string.
export type JobStatus = 'COMPLETED' | 'CANCELLED' | 'FAILED' | 'TIMED_OUT' | 'ERROR' | string

// id is the catalog GPU id ("NVIDIA RTX A4500"), the same string as Job.gpuTypeId.
export interface Card {
  id: string
  name: string
  memoryGb: number
  priceHr: number
  availability: Availability
}

// pricePerHr is the dearest card's (the conservative estimate); availability the best card's.
export interface Pool {
  id: string
  gpuNames: string[]
  memoryGb: number
  pricePerHr: number
  availability: Availability
  cards: Card[]
}

export interface Params {
  image: string
  pools: string[]
  flashboot: FlashBoot[]
  maxWorkers: number
  idleTimeoutS: number
  coldStarts: number
  warmRequests: number
  levelsRps: number[]
  levelSeconds: number
  drainSeconds: number
  jobSleepMs: number
  modelLoadS: number
  budgetUsd: number
  coldStartMode?: ColdStartMode // absent means natural
  // Side of the fp16 square matmul each job runs (GPU image only); absent means off.
  matmulN?: number
  cards?: string[] // card mode only: catalog GPU ids
}

// Inclusive [lo, hi] ranges keyed by Params JSON name. pools and levelsRps
// limit counts; rps limits each level's rate (above lo, at most hi).
export type Limits = Record<string, [number, number]>

export interface Defaults extends Params {
  budgetCapUsd: number
  limits: Limits
}

export interface EstimateConfig {
  pool: string
  card?: string // card mode only
  flashboot: FlashBoot
  priceHr: number
  worstCaseUsd: number
  availability: Availability
}

export interface Estimate {
  configs: EstimateConfig[]
  worstCaseUsd: number
  capUsd: number // this run's budget, not the server's cap
  maxSpendUsd: number
  guardStopsAfterS: number
}

export interface Run {
  id: string
  createdAt: string
  finishedAt?: string
  status: RunStatus
  params: Params
  budgetUsd: number
  estMaxUsd: number
  error?: string
}

export interface Percentiles {
  min: number
  max: number
  total: number
  p70: number
  p90: number
  p95: number
  p98: number
}

export interface RpRequestBucket {
  time: string
  requests: number
  completed: number
  failed: number
  retries: number
  executionTimeMs: Percentiles
  delayTimeMs: Percentiles
}

export interface RpColdStartBucket {
  time: string
  count: number
  durationMs: Percentiles
}

export interface RpMetrics {
  // The one exception to "lists are never null".
  requests: RpRequestBucket[] | null
  coldStarts: RpColdStartBucket[] | null
}

export interface Config {
  id: number
  runId: string
  pool: string
  card?: string // card mode only
  flashboot: FlashBoot
  endpointId?: string
  priceHr: number
  status: ConfigStatus
  error?: string
  createdAt: string
  deletedAt?: string
  rpMetrics?: RpMetrics
}

export interface Job {
  id: number
  configId: number
  phase: JobPhase
  level: number
  runpodJobId: string
  workerId: string
  gpuTypeId: string
  status: JobStatus
  intendedAt: string
  sentAt: string
  wallMs: number
  delayMs: number
  execMs: number
  error?: string
  // GPU image only. matmulMs and tflops need matmulN > 0.
  matmulMs?: number
  tflops?: number
  cudaInitMs?: number
}

export interface Summary {
  n: number
  p50: number
  p95: number
  mean: number
  min: number
  max: number
}

export interface LevelSummary {
  rps: number
  sent: number
  completed: number
  failed: number
  unfinished: number
  throughputRps: number
  latency: Summary
  delay: Summary
}

export interface ConfigSummary {
  configId: number
  firstBootMs: number
  cold: Summary
  coldDelay: Summary
  warm: Summary
  levels: LevelSummary[]
  completed: number
  failed: number
  unfinished: number
  costUsd: number
  costPer1k: number
  gpus?: GPUSummary[]
}

// Pools mix GPU models, so matmul numbers are per gpuTypeId, never per pool.
export interface GPUSummary {
  gpuTypeId: string // "" when Runpod never named the worker's GPU
  matmulMs: Summary // one sample per job
  tflops: Summary // fp16
  usdPerTflopHr: number // 0 without TFLOPS samples
  coldCudaInitMs: Summary // first_boot and cold jobs only
}

export interface ConfigDetail {
  config: Config
  summary: ConfigSummary
  jobs: Job[]
}

export interface RunDetail {
  run: Run
  configs: ConfigDetail[]
}

// SSE envelope. seq is 0 on a run_status synthesised from the stored run.
interface EventBase {
  runId: string
  seq: number
  ts: string
  configId?: number
}

export interface RunStatusEvent extends EventBase {
  type: 'run_status'
  data: { status: RunStatus; error?: string }
}

export interface ConfigStatusEvent extends EventBase {
  type: 'config_status'
  data: { status: ConfigStatus; detail?: string }
}

export interface JobEvent extends EventBase {
  type: 'job'
  data: Job
}

// boundUsd is the budget guard's upper bound on spend, not the spend.
export interface CostEvent extends EventBase {
  type: 'cost'
  data: { boundUsd: number; capUsd: number }
}

export interface LogEvent extends EventBase {
  type: 'log'
  data: { msg: string; level?: 'warn' | 'error' }
}

export type RunEvent = RunStatusEvent | ConfigStatusEvent | JobEvent | CostEvent | LogEvent

export const EVENT_TYPES: RunEvent['type'][] = ['run_status', 'config_status', 'job', 'cost', 'log']
