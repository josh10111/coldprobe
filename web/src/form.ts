import { usd } from './format'
import type { Availability, ColdStartMode, Defaults, FlashBoot, Limits, Params, Pool } from './types'

// New run form state. Numbers are kept as input text so a half-typed "1."
// doesn't snap to 0; toParams builds the request body.

export const NUM_KEYS = [
  'maxWorkers',
  'coldStarts',
  'warmRequests',
  'levelSeconds',
  'jobSleepMs',
  'matmulN',
  'idleTimeoutS',
  'modelLoadS',
  'drainSeconds',
  'budgetUsd',
] as const

export type NumKey = (typeof NUM_KEYS)[number]

export type GpuMode = 'pool' | 'card'

export const MAX_GPUS = 2

export interface FormState {
  image: string
  gpuMode: GpuMode
  pools: string[] // empty in card mode
  cards: string[] // catalog GPU ids; empty in pool mode
  flashboot: FlashBoot[]
  coldStartMode: ColdStartMode
  levels: string // "1, 3, 8"
  nums: Record<NumKey, string>
}

export function formFromDefaults(d: Defaults): FormState {
  const nums = {} as Record<NumKey, string>
  for (const k of NUM_KEYS) nums[k] = String(k === 'matmulN' ? (d.matmulN ?? 0) : d[k])
  return {
    image: d.image,
    gpuMode: d.cards?.length ? 'card' : 'pool',
    pools: d.pools ?? [],
    cards: d.cards ?? [],
    flashboot: d.flashboot,
    coldStartMode: d.coldStartMode ?? 'natural',
    levels: d.levelsRps.join(', '),
    nums,
  }
}

// Returns null on any non-number: NaN would serialise to null in JSON.
export function parseNumberList(text: string): number[] | null {
  const parts = text.split(',').map((s) => s.trim()).filter((s) => s !== '')
  const nums = parts.map(Number)
  if (nums.length === 0 || nums.some((n) => !Number.isFinite(n))) return null
  return nums
}

const toNumber = (s: string) => (s.trim() === '' ? NaN : Number(s))

// The body of POST /api/runs/estimate and /api/runs. The server takes pools
// or cards, never both, so card mode omits `pools`.
export type RunBody = Omit<Params, 'pools'> & { pools?: string[] }

export function toParams(f: FormState): RunBody | null {
  const levelsRps = parseNumberList(f.levels)
  const n = {} as Record<NumKey, number>
  for (const k of NUM_KEYS) {
    n[k] = toNumber(f.nums[k])
    if (!Number.isFinite(n[k])) return null
  }
  if (!levelsRps) return null
  // matmulN is omitted at 0, matching how the server stores it.
  const { matmulN, ...rest } = n
  return {
    image: f.image,
    ...(f.gpuMode === 'card' ? { cards: f.cards } : { pools: f.pools }),
    flashboot: f.flashboot,
    coldStartMode: f.coldStartMode,
    levelsRps,
    ...rest,
    ...(matmulN > 0 ? { matmulN } : {}),
  }
}

// Inline field checks use the server's own ranges (GET /api/defaults); the
// server re-checks everything on Estimate and Start.

export function rangeText(k: string, limits: Limits): string | null {
  const r = limits[k]
  return r ? `${r[0]} to ${r[1]}` : null
}

// matmulN reads "Up to N." because its 0 means off.
export function rangeHelp(k: NumKey, limits: Limits): string {
  const r = limits[k]
  if (!r) return ''
  return k === 'matmulN' ? `Up to ${r[1]}.` : `${r[0]} to ${r[1]}.`
}

// Out-of-range values are reported, never clamped.
export function fieldError(k: NumKey, text: string, limits: Limits, capUsd: number): string | null {
  if (text.trim() === '') return 'required'
  const v = Number(text)
  if (!Number.isFinite(v)) return 'a number'
  if (k === 'budgetUsd') {
    if (!(v > 0)) return `above ${usd(0)}`
    if (v > capUsd) return `at most ${usd(capUsd)}`
    return null
  }
  if (!Number.isInteger(v)) return 'whole number'
  const r = limits[k]
  if (r && (v < r[0] || v > r[1])) return rangeText(k, limits)
  return null
}

// The open-request rule (rate x seconds) is left to the server.
export function levelsError(text: string, limits: Limits): string | null {
  const levels = parseNumberList(text)
  if (!levels) return text.trim() === '' ? 'required' : 'numbers, comma-separated'
  const count = limits.levelsRps
  if (count && (levels.length < count[0] || levels.length > count[1])) return `${count[0]} to ${count[1]} levels`
  const rps = limits.rps
  if (rps && levels.some((x) => !(x > rps[0] && x <= rps[1]))) return `each above ${rps[0]}, at most ${rps[1]}`
  if (new Set(levels).size !== levels.length) return 'no repeats'
  return null
}

export type FormErrors = Partial<Record<NumKey | 'levels' | 'flashboot', string>>

export function formErrors(f: FormState, limits: Limits, capUsd: number): FormErrors {
  const out: FormErrors = {}
  for (const k of NUM_KEYS) {
    const e = fieldError(k, f.nums[k], limits, capUsd)
    if (e) out[k] = e
  }
  const e = levelsError(f.levels, limits)
  if (e) out.levels = e
  if (f.flashboot.length === 0) out.flashboot = 'pick at least one'
  return out
}

export function flashbootSummary(flashboot: FlashBoot[], gpus: number): string {
  if (flashboot.length === 0) return 'Pick at least one setting to measure.'
  const each = gpus === 1 ? 'The GPU' : gpus > 1 ? `Each of the ${gpus} GPUs` : 'Each GPU'
  const total = gpus > 0 ? ` (${gpus * flashboot.length} config${gpus * flashboot.length === 1 ? '' : 's'})` : ''
  if (flashboot.length === 2)
    return `${each} is measured twice, on separate endpoints: once without FlashBoot and once with it, so you can compare them${total}.`
  return `${each} is measured once, ${flashboot[0] === 'FLASHBOOT' ? 'with' : 'without'} FlashBoot${total}. Tick both to compare.`
}

// Mirrors internal/bench/params.go. Only the GPU image has torch for matmul.
export const SYNTHETIC_IMAGE_REPO = 'ghcr.io/josh10111/coldprobe-worker'
export const GPU_IMAGE = 'ghcr.io/josh10111/coldprobe-worker-gpu:latest'

// Mirrors the server's isSyntheticImage. The GPU repo shares the prefix, so
// what follows it must start a tag or digest.
export function needsGpuImage(matmulN: number, image: string): boolean {
  if (!(matmulN > 0)) return false
  const img = image.trim()
  if (!img.startsWith(SYNTHETIC_IMAGE_REPO)) return false
  const rest = img.slice(SYNTHETIC_IMAGE_REPO.length)
  return rest === '' || rest[0] === ':' || rest[0] === '@'
}

// allowLowStock is excluded so a "Start anyway" resend still matches the estimate.
export function estimateKey(body: RunBody & { allowLowStock?: boolean }): string {
  const { allowLowStock: _, ...rest } = body
  return JSON.stringify(rest)
}

export function switchGpuMode(f: FormState, mode: GpuMode): FormState {
  if (f.gpuMode === mode) return f
  return { ...f, gpuMode: mode, pools: [], cards: [] }
}

// Keeps table row order so series colours don't depend on click order.
export function toggleGpu(selected: string[], id: string, rowOrder: string[]): string[] {
  const on = new Set(selected)
  if (on.has(id)) on.delete(id)
  else if (on.size < MAX_GPUS) on.add(id)
  return rowOrder.filter((x) => on.has(x))
}

export interface PickerRow {
  id: string // a pool id or a catalog GPU id
  name: string
  other: string // a pool's cards, or a card's pool
  memoryGb: number
  priceHr: number
  availability: Availability
}

// Rows without a serverless price can't be run, so they're hidden. A pool's
// price is its dearest card's, the figure the estimate uses.
export function pickerRows(catalog: Pool[], mode: GpuMode): PickerRow[] {
  if (mode === 'pool') {
    return catalog
      .filter((p) => p.pricePerHr > 0)
      .map((p) => ({
        id: p.id,
        name: p.id,
        other: p.gpuNames.join(' · '),
        memoryGb: p.memoryGb,
        priceHr: p.pricePerHr,
        availability: p.availability,
      }))
  }
  return catalog.flatMap((p) =>
    p.cards
      .filter((c) => c.priceHr > 0)
      .map((c) => ({
        id: c.id,
        name: c.name,
        other: p.id,
        memoryGb: c.memoryGb,
        priceHr: c.priceHr,
        availability: c.availability,
      })),
  )
}

export interface StockLine {
  level: 'NONE' | 'LOW'
  text: string
}

export function stockWarning(configs: { pool: string; card?: string; availability: Availability }[]): StockLine[] {
  const seen = new Map<string, Availability>()
  for (const c of configs) seen.set(c.card ?? c.pool, c.availability)
  const ids = (level: Availability) => [...seen].filter(([, a]) => a === level).map(([id]) => id)
  const none = ids('NONE')
  const low = ids('LOW')
  const lines: StockLine[] = []
  if (none.length) {
    const verb = none.length > 1 ? 'have' : 'has'
    lines.push({ level: 'NONE', text: `${none.join(' and ')} ${verb} no stock right now, so Start will be refused.` })
  }
  if (low.length) {
    const verb = low.length > 1 ? 'are' : 'is'
    lines.push({
      level: 'LOW',
      text: `${low.join(' and ')} ${verb} low on stock: the first boot may wait or fail, and Start will ask you to confirm.`,
    })
  }
  return lines
}
