import { coldNote, type ColdNote } from './coldNote'
import { cardName, coldName, configName, isForced, modeWord, poolIndex, type ModeWord } from './format'
import type { ConfigDetail, Job, Params, Pool, RpMetrics, RunDetail } from './types'

// Snapshot to chart data. "No data" is null, never NaN or 0, so a chart draws
// nothing instead of a wrong bar. Chart times are seconds.

// Hex, not var(), because charts append an alpha ("#7fd1a440") for fills.
export const POOL_COLORS = ['#c9c6bd', '#7fd1a4'] as const

export interface Series {
  key: string // s0, s1, ...: safe as a Recharts dataKey
  label: string // for tooltips; includes the mode word in compare
  name: string // label without the mode word, which legends show as a badge
  mode: ModeWord | null // compare only
  color: string // by pool
  dashed: boolean // FlashBoot on
  runCreatedAt: string
  cd: ConfigDetail
  coldNote: ColdNote | null
}

export interface RunInput {
  detail: RunDetail
  prefix?: string // compare: the run's short date
  catalog?: Pool[] // card mode: for card names
}

export function buildSeries(runs: RunInput[]): Series[] {
  const compare = runs.length > 1
  const out: Series[] = []
  runs.forEach((r) => {
    // In compare, natural and forced cold numbers must never mix silently.
    const mode = compare ? modeWord(r.detail.run.params) : null
    const prefix = r.prefix ? `${r.prefix} · ` : ''
    for (const cd of r.detail.configs) {
      const c = cd.config
      const name = configName({ pool: c.pool, card: c.card ? cardName(c.card, r.catalog) : undefined, flashboot: c.flashboot })
      out.push({
        key: `s${out.length}`,
        label: prefix + (mode ? `${mode} · ` : '') + name,
        name: prefix + name,
        mode,
        color: POOL_COLORS[poolIndex(r.detail.run.params, c)] ?? POOL_COLORS[0],
        dashed: c.flashboot === 'FLASHBOOT',
        runCreatedAt: r.detail.run.createdAt,
        cd,
        coldNote: coldNote(r.detail.run.params, cd),
      })
    }
  })
  return out
}

// The server's percentile: nearest rank, sorted[ceil(p*n/100) - 1].
export function nearestRank(values: number[], p: number): number | null {
  if (values.length === 0) return null
  const sorted = [...values].sort((a, b) => a - b)
  const rank = Math.min(sorted.length, Math.max(1, Math.ceil((p * sorted.length) / 100)))
  return sorted[rank - 1]
}

const completed = (j: Job) => j.status === 'COMPLETED'

// The window grows from 1 at the start so the line begins at the first point.
export function rollingMedian(values: number[], window = 10): number[] {
  return values.map((_, i) => median(values.slice(Math.max(0, i - window + 1), i + 1)))
}

function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b)
  const mid = Math.floor(sorted.length / 2)
  return sorted.length % 2 === 1 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2
}

const ms = (iso: string) => new Date(iso).getTime()

export interface Point {
  x: number // seconds from the run's start to when the job was sent
  y: number // wallMs, in seconds
}

export interface PhaseMark {
  x: number
  label: string
}

// Configs run side by side, so a separator marks each phase's earliest send
// across the run's configs.
export function phaseMarks(d: RunDetail): PhaseMark[] {
  const start = ms(d.run.createdAt)
  const cold = `${coldName(d.run.params)}s`
  const first = new Map<string, number>()
  for (const c of d.configs) {
    for (const j of c.jobs) {
      const label =
        j.phase === 'first_boot' ? 'first boot' : j.phase === 'cold' ? cold : j.phase === 'warm' ? 'warm' : `${j.level} req/s`
      const x = (ms(j.sentAt) - start) / 1000
      const prev = first.get(label)
      if (prev === undefined || x < prev) first.set(label, x)
    }
  }
  return [...first].map(([label, x]) => ({ x, label })).sort((a, b) => a.x - b.x)
}

// Labels alternate between two rows; one within 4% of the axis span of the
// previous label on its row is dropped (the caption lists it instead).
export const LABEL_GAP_FRACTION = 0.04

export interface PlacedMark extends PhaseMark {
  row: 0 | 1 // 0 = upper
  showLabel: boolean
}

export function placeMarkLabels(marks: PhaseMark[], domainMax: number): PlacedMark[] {
  const minGap = LABEL_GAP_FRACTION * domainMax
  const lastDrawn: (number | null)[] = [null, null]
  return marks.map((m, i) => {
    const row = i % 2 === 0 ? 0 : 1
    const prev = lastDrawn[row]
    const showLabel = prev === null || m.x - prev > minGap
    if (showLabel) lastDrawn[row] = m.x
    return { ...m, row, showLabel }
  })
}

export function unlabelledCaption(placed: PlacedMark[]): string | null {
  const hidden = placed.filter((m) => !m.showLabel)
  if (hidden.length === 0) return null
  return `Unlabelled dashed lines: ${hidden.map((m) => `${m.label} at ${Math.round(m.x)} s`).join(', ')}.`
}

export function coldGroupName(runs: RunInput[]): string {
  const forced = runs.filter((r) => isForced(r.detail.run.params)).length
  if (forced === 0) return 'cold start'
  return forced === runs.length ? 'new-worker cold start' : 'cold start / new-worker'
}

export function modesDiffer(params: Params[]): boolean {
  return new Set(params.map(modeWord)).size > 1
}

export interface ColdBreakdown {
  key: string
  label: string
  n: number
  waiting: number | null // Runpod delayTime p50 (s), includes the worker's start
  running: number | null // executionTime p50 (s)
  note: ColdNote | null
}

// The summary has no cold execMs, so it comes from the cold jobs themselves.
export function coldBreakdown(series: Series[]): ColdBreakdown[] {
  return series.map((s) => {
    const cold = s.cd.jobs.filter((j) => j.phase === 'cold' && completed(j))
    const d = s.cd.summary.coldDelay
    const exec = nearestRank(cold.map((j) => j.execMs), 50)
    return {
      key: s.key,
      label: s.label,
      n: d.n,
      waiting: d.n > 0 ? d.p50 / 1000 : null,
      running: exec === null ? null : exec / 1000,
      note: s.coldNote,
    }
  })
}

// Runpod's own boot count, as a cross-check. Its durations aren't shown:
// they include the first boot, so they can't be set beside ours.
export function rpColdCount(m: RpMetrics | undefined): number | null {
  if (!m) return null
  return (m.coldStarts ?? []).reduce((n, b) => n + b.count, 0)
}

export interface GpuRow {
  key: string
  name: string
  mode: ModeWord | null
  color: string
  dashed: boolean
  gpuTypeId: string
  tflopsP50: number | null
  tflopsN: number
  matmulMsP50: number | null
  matmulMsP95: number | null
  usdPerTflopHr: number | null
  coldCudaInitP50: number | null
  coldCudaInitN: number
}

// Spread is shown on matmul time: for TFLOPS (higher is better) p95 would be the fastest.
export function gpuRows(series: Series[]): GpuRow[] {
  const rows: GpuRow[] = []
  for (const s of series) {
    for (const g of s.cd.summary.gpus ?? []) {
      rows.push({
        key: `${s.key}-${g.gpuTypeId}`,
        name: s.name,
        mode: s.mode,
        color: s.color,
        dashed: s.dashed,
        gpuTypeId: g.gpuTypeId,
        tflopsP50: g.tflops.n > 0 ? g.tflops.p50 : null,
        tflopsN: g.tflops.n,
        matmulMsP50: g.matmulMs.n > 0 ? g.matmulMs.p50 : null,
        matmulMsP95: g.matmulMs.n > 0 ? g.matmulMs.p95 : null,
        usdPerTflopHr: g.usdPerTflopHr > 0 ? g.usdPerTflopHr : null,
        coldCudaInitP50: g.coldCudaInitMs.n > 0 ? g.coldCudaInitMs.p50 : null,
        coldCudaInitN: g.coldCudaInitMs.n,
      })
    }
  }
  return rows
}

export const totalCost = (d: RunDetail) => d.configs.reduce((sum, c) => sum + c.summary.costUsd, 0)
