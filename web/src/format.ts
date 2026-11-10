import type { Availability, FlashBoot, Params, Pool, Run, RunStatus } from './types'

export const usd = (x: number) => `$${x.toFixed(2)}`

export const usdPer1k = (x: number) => `$${x.toFixed(3)}`

// At most 3 significant figures: 501 ms, 8.0 s, 10.7 s, 28 s, 117 s.
export function secs(ms: number): string {
  if (ms < 999.5) return `${Math.round(ms)} ms` // not "1000 ms"
  const s = ms / 1000
  if (s < 10) return `${s.toFixed(1)} s`
  if (s < 100) return `${s.toFixed(1).replace(/\.0$/, '')} s`
  return `${Math.round(s)} s`
}

// 3 significant figures, never in exponent form.
export const sig3 = (x: number) => String(Number(x.toPrecision(3)))

export const usd3sf = (x: number) => `$${sig3(x)}`

const pad = (n: number) => String(n).padStart(2, '0')

// 42s, 4m 12s, 1h 05m.
export function duration(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${pad(s % 60)}s`
  return `${Math.floor(m / 60)}h ${pad(m % 60)}m`
}

export function when(iso: string): string {
  const d = new Date(iso)
  const day = d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' })
  return `${day} · ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export const flashLabel = (f: FlashBoot) => (f === 'OFF' ? 'off' : 'on')

export const configLabel = (c: { pool: string; card?: string; flashboot: FlashBoot }) => configName(c)

export const configName = (c: { pool: string; card?: string; flashboot: FlashBoot }) =>
  `${c.card ?? c.pool} FlashBoot ${flashLabel(c.flashboot)}`

// Falls back to the id when the catalog is missing or no longer lists the card.
export function cardName(id: string, catalog?: Pool[]): string {
  for (const p of catalog ?? []) {
    const card = (p.cards ?? []).find((x) => x.id === id) // `cards` is absent from older servers
    if (card) return card.name
  }
  return id
}

// 0 or 1. In card mode it is the card's position, since two cards may share a pool.
export function poolIndex(p: Params, c: { pool: string; card?: string }): number {
  const i = c.card && p.cards?.length ? p.cards.indexOf(c.card) : (p.pools ?? []).indexOf(c.pool)
  return Math.max(0, i)
}

export function runHeading(p: Params, catalog?: Pool[]): string {
  const subjects = p.cards?.length ? p.cards.map((id) => cardName(id, catalog)) : (p.pools ?? [])
  const fb = p.flashboot.length === 2 ? 'FlashBoot off and on' : `FlashBoot ${flashLabel(p.flashboot[0])}`
  return `${subjects.join(' vs ')} · ${fb}`
}

export const reportTitle = (p: Params, catalog?: Pool[]) => `Benchmark report · ${runHeading(p, catalog)}`

// Card-mode runs may store pools as null or omit them.
export const runSubject = (p: { pools?: string[] | null; cards?: string[] }) =>
  (p.cards?.length ? p.cards : (p.pools ?? [])).join(' + ')

export const latestResults = (runs: Run[], n: number) => runs.filter((r) => r.status !== 'running').slice(0, n)

export function stockWord(level: Availability): { word: string; className: string } {
  const word = level ? level.toLowerCase() : 'unknown'
  return { word, className: `stock-${word}` }
}

// Forced runs pause the endpoint before each cold sample, so each starts a new
// worker. Absent (older runs) means natural.
export const isForced = (p: Params) => p.coldStartMode === 'forced'

export type ModeWord = 'natural' | 'forced'
export const modeWord = (p: Params): ModeWord => (isForced(p) ? 'forced' : 'natural')

export const coldName = (p: Params) => (isForced(p) ? 'new-worker cold start' : 'cold start')

export const matmulSuffix = (p: Params) => ((p.matmulN ?? 0) > 0 ? ` · matmul ${p.matmulN}` : '')

export const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

export const statusLabel: Record<RunStatus, string> = {
  running: 'running',
  completed: 'completed',
  failed: 'failed',
  cancelled: 'cancelled',
  aborted_budget: 'budget-stopped',
  aborted_crash: 'crashed',
}

export type StatusTone = 'running' | 'done' | 'warn' | 'bad'
export const statusTone: Record<RunStatus, StatusTone> = {
  running: 'running',
  completed: 'done',
  cancelled: 'warn',
  aborted_budget: 'warn',
  failed: 'bad',
  aborted_crash: 'bad',
}
