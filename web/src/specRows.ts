import { nearestRank } from './chartData'
import { coldNote, type ColdNote } from './coldNote'
import { cardName, configName, poolIndex } from './format'
import type { ConfigDetail, FlashBoot, Job, Pool, RunDetail } from './types'

// One config's numbers for the spec table. "No data" is null (shown as "—").
export interface SpecRow {
  key: string // c<config id>: stable across live updates
  name: string // "AMPERE_16 FlashBoot off"
  subject: string // the pool or card alone
  poolIndex: number
  flashboot: FlashBoot
  firstBootMs: number | null
  cold: { p50: number | null; n: number }
  warm: { p50: number | null; n: number }
  coldNote: ColdNote | null
  // The last of params.levelsRps. reached only when the config is done and
  // has a summary for it; a level cut short mid-way isn't a full level.
  topLevel: { rps: number; sent: number; served: number; leftQueued: number; failed: number; reached: boolean }
  per1k: number | null // est. $ per 1k completed requests
  gotCards: string[] // gpuTypeIds its jobs ran on
}

export function specRows(d: RunDetail, catalog?: Pool[]): SpecRow[] {
  const levels = d.run.params.levelsRps
  const topRps = levels.length > 0 ? levels[levels.length - 1] : 0
  return d.configs.map((cd) => row(d, cd, topRps, catalog))
}

function row(d: RunDetail, cd: ConfigDetail, topRps: number, catalog?: Pool[]): SpecRow {
  const c = cd.config
  const s = cd.summary
  const card = c.card ? cardName(c.card, catalog) : undefined
  const level = c.status === 'done' ? s.levels.find((l) => l.rps === topRps) : undefined
  return {
    key: `c${c.id}`,
    name: configName({ pool: c.pool, card, flashboot: c.flashboot }),
    subject: card ?? c.pool,
    poolIndex: poolIndex(d.run.params, c),
    flashboot: c.flashboot,
    firstBootMs: s.firstBootMs > 0 ? s.firstBootMs : null,
    cold: { p50: s.cold.n > 0 ? s.cold.p50 : null, n: s.cold.n },
    warm: { p50: s.warm.n > 0 ? s.warm.p50 : null, n: s.warm.n },
    coldNote: coldNote(d.run.params, cd),
    topLevel: level
      ? { rps: topRps, sent: level.sent, served: level.completed, leftQueued: level.unfinished, failed: level.failed, reached: true }
      : { rps: topRps, sent: 0, served: 0, leftQueued: 0, failed: 0, reached: false },
    // The first boot alone isn't a per-request cost.
    per1k: s.completed > 1 ? s.costPer1k : null,
    gotCards: [...new Set(cd.jobs.map((j) => j.gpuTypeId).filter((g) => g !== ''))].sort(),
  }
}

// One config at one load level, unsmoothed (the latency chart is a rolling median).
export interface LevelRow {
  key: string // c<config id>-<rps>
  name: string
  rps: number // offered
  sent: number
  completed: number
  failed: number
  leftQueued: number // the API's `unfinished`
  p95Ms: number | null
  latencyN: number
  throughputRps: number
}

export function levelRows(d: RunDetail, catalog?: Pool[]): LevelRow[] {
  return d.configs.flatMap((cd) => {
    const c = cd.config
    const name = configName({ pool: c.pool, card: c.card ? cardName(c.card, catalog) : undefined, flashboot: c.flashboot })
    return cd.summary.levels.map((l) => ({
      key: `c${c.id}-${l.rps}`,
      name,
      rps: l.rps,
      sent: l.sent,
      completed: l.completed,
      failed: l.failed,
      leftQueued: l.unfinished,
      p95Ms: l.latency.n > 0 ? l.latency.p95 : null,
      latencyN: l.latency.n,
      throughputRps: l.throughputRps,
    }))
  })
}

// Live rows come from jobs, since the snapshot's summaries go stale between
// re-fetches. Same definitions as the server's Summarize (internal/bench);
// per1k needs the server's cost model, so it waits for the report.
export function liveSpecRows(d: RunDetail, catalog?: Pool[]): SpecRow[] {
  const rows = specRows(d, catalog)
  return rows.map((r, i) => {
    const cd = d.configs[i]
    const done = cd.jobs.filter((j) => j.status === 'COMPLETED')
    const of = (phase: Job['phase']) => done.filter((j) => j.phase === phase)
    const boot = of('first_boot')[0]
    const cold = of('cold').map((j) => j.wallMs - j.execMs)
    const warm = of('warm').map((j) => j.wallMs)
    const rps = r.topLevel.rps
    const level = cd.jobs.filter((j) => j.phase === 'load' && j.level === rps)
    const count = (status: string) => level.filter((j) => j.status === status).length
    const reached = cd.config.status === 'done' && level.length > 0
    const leftQueued = count('CANCELLED')
    const served = count('COMPLETED')
    return {
      ...r,
      firstBootMs: boot ? boot.wallMs - boot.execMs : null,
      cold: { p50: nearestRank(cold, 50), n: cold.length },
      warm: { p50: nearestRank(warm, 50), n: warm.length },
      topLevel: reached
        ? { rps, sent: level.length, served, leftQueued, failed: level.length - served - leftQueued, reached }
        : { rps, sent: 0, served: 0, leftQueued: 0, failed: 0, reached },
      per1k: null,
    }
  })
}

export function topLevelWords(t: SpecRow['topLevel']): string {
  if (!t.reached) return '—'
  if (t.leftQueued > 0) return `${t.leftQueued} of ${t.sent} left queued`
  if (t.failed > 0) return `${t.served} of ${t.sent} served, ${t.failed} failed`
  return `all ${t.sent} served`
}
