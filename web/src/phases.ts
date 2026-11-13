import { nearestRank } from './chartData'
import { coldNote } from './coldNote'
import { coldName, secs } from './format'
import type { ConfigDetail, Job, Params } from './types'

// Live phase progress, computed from jobs rather than the summary: the
// snapshot is only re-fetched on (re)connect, so its summaries go stale.
// A step is done once a later phase has jobs, since cold samples can be skipped.

// 'short': fewer cold samples than asked because workers stayed up; never green.
export type StepState = 'done' | 'current' | 'pending' | 'failed' | 'cut' | 'unreached' | 'short'

export interface Step {
  label: string
  detail: string
  state: StepState
  help?: string
}

const ok = (j: Job) => j.status === 'COMPLETED'
const typical = (vals: number[]) => {
  const p50 = nearestRank(vals, 50)
  return p50 === null ? '' : ` · typical ${secs(p50)}`
}

// OpenLoop sends at start + i/rps while that's before start + levelSeconds.
export const expectedLoadJobs = (rps: number, levelSeconds: number) => Math.ceil(rps * levelSeconds - 1e-9)

// Turns the runner's latest forced-mode log line into a short hint.
export function forcedHint(activity: string | undefined): string | null {
  if (!activity) return null
  if (activity.includes('pausing the endpoint')) return 'pausing the endpoint…'
  if (activity.includes('endpoint paused, waiting')) return 'endpoint paused, waiting for workers to stop…'
  if (activity.includes('restoring workers.max')) return 'restoring the endpoint…'
  return null
}

export function phaseSteps(p: Params, cd: ConfigDetail, activity?: string): Step[] {
  const jobs = cd.jobs
  const fb = jobs.filter((j) => j.phase === 'first_boot')
  const cold = jobs.filter((j) => j.phase === 'cold')
  const warm = jobs.filter((j) => j.phase === 'warm')
  const coldOk = cold.filter(ok)
  const warmOk = warm.filter(ok)

  const groups = [
    {
      label: 'first boot',
      jobs: fb,
      full: fb.some(ok),
      detail: fb.some(ok) ? secs(fb.filter(ok)[0].wallMs - fb.filter(ok)[0].execMs) : '',
    },
    {
      label: `${coldName(p)}s`,
      jobs: cold,
      full: cold.length >= p.coldStarts,
      detail: cold.length > 0 ? `${coldOk.length}/${p.coldStarts}${typical(coldOk.map((j) => j.wallMs - j.execMs))}` : '',
    },
    {
      label: 'warm',
      jobs: warm,
      full: warm.length >= p.warmRequests,
      detail: warm.length > 0 ? `${warmOk.length}/${p.warmRequests}${typical(warmOk.map((j) => j.wallMs))}` : '',
    },
    ...p.levelsRps.map((rps) => {
      const lv = jobs.filter((j) => j.phase === 'load' && j.level === rps)
      const expected = expectedLoadJobs(rps, p.levelSeconds)
      return {
        label: `load ${rps} req/s`,
        jobs: lv,
        full: lv.length >= expected,
        detail: lv.length > 0 ? `${lv.filter(ok).length}/${expected} completed` : '',
      }
    }),
  ]

  const status = cd.config.status
  const final = status === 'done' || status === 'failed' || status === 'cancelled'
  let last = -1 // last step with any jobs
  groups.forEach((g, i) => {
    if (g.jobs.length > 0) last = i
  })
  const at = last === -1 ? 0 : groups[last].full ? last + 1 : last

  const note = coldNote(p, cd)
  const typ = typical(coldOk.map((j) => j.wallMs - j.execMs))
  const coldShort =
    cold.length < p.coldStarts && status !== 'cancelled' && status !== 'failed' && (status === 'done' || last > 1)
  return groups.map((g, i) => {
    if (i === 1 && note) return { label: g.label, detail: note.text + typ, state: 'short' as const, help: note.help }
    if (i === 1 && coldShort) return { label: g.label, detail: `${coldOk.length}/${p.coldStarts}${typ}`, state: 'short' as const }
    let state: StepState
    if (i < at) state = 'done'
    else if (i > at) state = final ? 'unreached' : 'pending'
    else if (!final) {
      const hint = i === 1 ? forcedHint(activity) : null
      return { label: g.label, detail: hint ? [g.detail, hint].filter(Boolean).join(' · ') : g.detail, state: 'current' as const }
    }
    else if (status === 'failed') state = 'failed'
    // Cancelled before this phase's first job finished still stopped here.
    else if (status === 'cancelled') state = 'cut'
    else state = g.jobs.length > 0 ? 'done' : 'unreached'
    return { label: g.label, detail: g.detail, state }
  })
}

// Spec table columns a live phase word can sit in; every load level uses topLevel.
export type LiveColumn = 'firstBoot' | 'cold' | 'warm' | 'topLevel'
export type LiveWords = Partial<Record<LiveColumn, string>>

const columnOf = (stepIndex: number): LiveColumn =>
  stepIndex === 0 ? 'firstBoot' : stepIndex === 1 ? 'cold' : stepIndex === 2 ? 'warm' : 'topLevel'

// The current phase's progress word for the live spec row. First boot counts
// from the config's creation, as the runner's 90 s no-worker warning does.
export function liveWords(p: Params, cd: ConfigDetail, nowMs: number, activity?: string): LiveWords {
  const status = cd.config.status
  if (status === 'tearing_down') return { topLevel: 'tearing down' }
  if (status === 'done') return {}
  const steps = phaseSteps(p, cd, activity)
  const i = steps.findIndex((s) => s.state === 'current' || s.state === 'failed' || s.state === 'cut')
  if (i === -1) return {}
  const step = steps[i]
  if (step.state === 'failed') return { [columnOf(i)]: `${step.label} · failed` }
  if (step.state === 'cut') return { [columnOf(i)]: `${step.label} · cut short` }
  const done = (phase: Job['phase'], rps?: number) =>
    cd.jobs.filter((j) => ok(j) && j.phase === phase && (rps === undefined || j.level === rps)).length
  let detail: string
  if (i === 0) {
    const so = Math.floor(Math.max(0, nowMs - Date.parse(cd.config.createdAt)) / 1000)
    detail = `${secs(so * 1000)} so far`
  } else if (i === 1) {
    detail = [`${done('cold')}/${p.coldStarts}`, forcedHint(activity)].filter(Boolean).join(' · ')
  } else if (i === 2) {
    detail = `${done('warm')}/${p.warmRequests}`
  } else {
    const rps = p.levelsRps[i - 3]
    detail = `${done('load', rps)}/${expectedLoadJobs(rps, p.levelSeconds)} completed`
  }
  return { [columnOf(i)]: `${step.label} · ${detail}` }
}

export interface FirstBootWait {
  text: string
  tone?: 'ok' | 'warn' | 'bad'
}

export function firstBootWait(cd: ConfigDetail, nowMs: number): FirstBootWait {
  const fb = cd.jobs.find((j) => j.phase === 'first_boot' && ok(j))
  if (fb) return { text: `worker connected · first request in ${secs(fb.wallMs)}`, tone: 'ok' }
  switch (cd.config.status) {
    case 'pending':
      return { text: 'queued' }
    case 'creating':
      return { text: 'creating the endpoint', tone: 'warn' }
    case 'running': {
      const so = Math.floor(Math.max(0, nowMs - Date.parse(cd.config.createdAt)) / 1000)
      return { text: `waiting for a GPU worker · ${secs(so * 1000)}`, tone: 'warn' }
    }
    case 'failed':
      return { text: 'failed', tone: 'bad' }
    default:
      return { text: cd.config.status.replace('_', ' ') }
  }
}

export interface RunProgress {
  fraction: number // 0 to 1
  configsDone: number
  configs: number
}

// Every phase of every config weighs the same, so this is how far through the
// run is, not a time estimate: phases take very different times.
export function runProgress(p: Params, configs: ConfigDetail[]): RunProgress {
  const phases = 3 + p.levelsRps.length
  let sum = 0
  let configsDone = 0
  for (const cd of configs) {
    const s = cd.config.status
    if (s === 'done' || s === 'failed' || s === 'cancelled') {
      sum += 1
      configsDone++
      continue
    }
    const count = (phase: Job['phase'], rps?: number) =>
      cd.jobs.filter((j) => j.phase === phase && (rps === undefined || j.level === rps)).length
    const part = (n: number, of: number) => (of > 0 ? Math.min(1, n / of) : 1)
    const fb = cd.jobs.some((j) => j.phase === 'first_boot' && ok(j)) ? 1 : 0
    const steps = phaseSteps(p, cd)
    let f = fb
    // Skipped cold samples ('short') and phases already passed count as done.
    f += steps[1].state === 'done' || steps[1].state === 'short' ? 1 : part(count('cold'), p.coldStarts)
    f += steps[2].state === 'done' ? 1 : part(count('warm'), p.warmRequests)
    p.levelsRps.forEach((rps, i) => {
      f += steps[3 + i].state === 'done' ? 1 : part(count('load', rps), expectedLoadJobs(rps, p.levelSeconds))
    })
    sum += f / phases
  }
  return { fraction: configs.length ? sum / configs.length : 0, configsDone, configs: configs.length }
}
