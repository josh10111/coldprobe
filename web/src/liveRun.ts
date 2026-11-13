import { statusLabel } from './format'
import { phaseSteps } from './phases'
import type { ConfigDetail, ConfigStatus, LogLevel, Params, RunDetail, RunEvent, RunStatus } from './types'

// The active run: the snapshot (source of truth) with SSE events applied on top.
export interface LogLine {
  seq: number
  ts: string
  configId?: number
  msg: string
  tone?: LogTone
}

export type LogTone = 'ok' | 'warn' | 'bad'

const LEVEL_TONE: Record<LogLevel, LogTone> = { ok: 'ok', wait: 'warn', note: 'warn', warn: 'warn', error: 'bad' }
const RUN_END_TONE: Partial<Record<RunStatus, LogTone>> = {
  completed: 'ok',
  cancelled: 'warn',
  aborted_budget: 'warn',
  failed: 'bad',
  aborted_crash: 'bad',
}

export interface LiveRun {
  detail: RunDetail
  bound: { boundUsd: number; capUsd: number } | null
  activity: Record<number, string> // configId -> latest log line
  logs: LogLine[] // newest last
}

const MAX_LOGS = 500

// Statuses only move forward, so an event a fresher snapshot overtook is ignored.
const configRank: Record<ConfigStatus, number> = {
  pending: 0,
  creating: 1,
  running: 2,
  tearing_down: 3,
  done: 4,
  failed: 4,
  cancelled: 4,
}

// Event-only state carries over only within the same run.
export function withSnapshot(prev: LiveRun | null, detail: RunDetail): LiveRun {
  const same = prev?.detail.run.id === detail.run.id ? prev : null
  return { detail, bound: same?.bound ?? null, activity: same?.activity ?? {}, logs: same?.logs ?? [] }
}

const withLog = (live: LiveRun, line: LogLine): LiveRun => ({ ...live, logs: [...live.logs, line].slice(-MAX_LOGS) })

export function applyEvent(live: LiveRun, e: RunEvent): LiveRun {
  const d = live.detail
  switch (e.type) {
    case 'job': {
      const job = e.data
      const configs = d.configs.map((c) => {
        if (c.config.id !== job.configId) return c
        // Id 0 means unsaved, so it can't already be in the snapshot.
        if (job.id !== 0 && c.jobs.some((j) => j.id === job.id)) return c
        return { ...c, jobs: [...c.jobs, job] }
      })
      return { ...live, detail: { ...d, configs } }
    }
    case 'config_status': {
      const configs = d.configs.map((c) => {
        if (c.config.id !== e.configId) return c
        if (configRank[e.data.status] < configRank[c.config.status]) return c
        return { ...c, config: { ...c.config, status: e.data.status, error: e.data.detail } }
      })
      const next = { ...live, detail: { ...d, configs } }
      const was = d.configs.find((c) => c.config.id === e.configId)?.config.status
      if (e.data.status !== 'failed' || was === 'failed') return next
      return withLog(next, { seq: e.seq, ts: e.ts, configId: e.configId, msg: `failed${e.data.detail ? `: ${e.data.detail}` : ''}`, tone: 'bad' })
    }
    case 'run_status': {
      // Every status but running is final.
      if (e.data.status === 'running' && d.run.status !== 'running') return live
      const next = { ...live, detail: { ...d, run: { ...d.run, status: e.data.status, error: e.data.error } } }
      if (e.data.status === 'running' || d.run.status === e.data.status) return next
      const msg = `Run ${statusLabel[e.data.status]}${e.data.error ? `: ${e.data.error}` : ''}`
      return withLog(next, { seq: e.seq, ts: e.ts, msg, tone: RUN_END_TONE[e.data.status] })
    }
    case 'cost':
      return { ...live, bound: e.data }
    case 'log': {
      const line: LogLine = {
        seq: e.seq,
        ts: e.ts,
        configId: e.configId,
        msg: e.data.msg,
        tone: e.data.level && LEVEL_TONE[e.data.level],
      }
      const activity = e.configId === undefined ? live.activity : { ...live.activity, [e.configId]: e.data.msg }
      return withLog({ ...live, activity }, line)
    }
  }
}

// Home's "Running now" phase word for one config.
const AROUND: Partial<Record<ConfigStatus, string>> = {
  pending: 'waiting',
  creating: 'creating the endpoint',
  tearing_down: 'deleting the endpoint',
}
export function phaseWord(p: Params, cd: ConfigDetail): string {
  const status = cd.config.status
  if (status !== 'running') return AROUND[status] ?? status
  return phaseSteps(p, cd).find((s) => s.state === 'current')?.label ?? status
}
