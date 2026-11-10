import { coldNote, SCALE_DOWN_TIMEOUT_MIN, type ColdNote } from './coldNote'
import { flashLabel, isForced, secs } from './format'
import type { FlashBoot, RunDetail } from './types'

// The "run finished" toast's fallback result line, from the first pool only.

export interface ModeNumbers {
  flashboot: FlashBoot
  coldMs: number | null
  coldN: number
  coldNote: ColdNote | null
  warmMs: number | null
  warmN: number
  firstBootMs: number | null
  per1kUsd: number | null
  completed: number
}

export function headlineNumbers(d: RunDetail): { pool: string; modes: ModeNumbers[] } | null {
  // Card-mode runs may store pools as null.
  const pool = d.run.params.pools?.[0] ?? d.configs[0]?.config.pool
  const modes: ModeNumbers[] = []
  for (const fb of ['OFF', 'FLASHBOOT'] as const) {
    const c = d.configs.find((x) => x.config.pool === pool && x.config.flashboot === fb)
    if (!c) continue
    const s = c.summary
    modes.push({
      flashboot: fb,
      coldMs: s.cold.n > 0 ? s.cold.p50 : null,
      coldN: s.cold.n,
      coldNote: coldNote(d.run.params, c),
      warmMs: s.warm.n > 0 ? s.warm.p50 : null,
      warmN: s.warm.n,
      firstBootMs: s.firstBootMs > 0 ? s.firstBootMs : null,
      // The first boot alone isn't a per-request cost.
      per1kUsd: s.completed > 1 && s.costPer1k > 0 ? s.costPer1k : null,
      completed: s.completed,
    })
  }
  return modes.length > 0 ? { pool, modes } : null
}

const KEPT_WARM = 'FlashBoot kept a worker warm (no cold start to measure)'
// No final period: parts are joined with ". ".
const NO_SCALE_DOWN = `Workers stayed up past the ${SCALE_DOWN_TIMEOUT_MIN} min wait, so fewer or no cold starts were measured`

// Each mode is named on its own, never as an "off → on" pair, so it never
// claims a FlashBoot speed-up that wasn't measured.
export function coldHeadline(d: RunDetail): string | null {
  const h = headlineNumbers(d)
  if (!h) return null
  if (isForced(d.run.params)) return forcedHeadline(h.modes)
  const measured = h.modes.filter((m) => m.coldMs !== null)
  const kept = h.modes.filter((m) => m.coldMs === null && m.coldNote)
  if (measured.length === 1 && h.modes.length === 1) return `Cold start (typical) ${secs(measured[0].coldMs!)}`
  const parts = [
    ...measured.map((m) => `Cold start ${secs(m.coldMs!)} ${m.flashboot === 'FLASHBOOT' ? 'with' : 'without'} FlashBoot`),
    ...kept.map((m) => (m.flashboot === 'FLASHBOOT' ? KEPT_WARM : NO_SCALE_DOWN)),
  ]
  return parts.length > 0 ? parts.join('. ') : null
}

// Every forced sample starts a new worker, where FlashBoot can't help.
function forcedHeadline(modes: ModeNumbers[]): string | null {
  const measured = modes.filter((m) => m.coldMs !== null)
  if (measured.length === 0) return null
  if (modes.length === 1) return `New-worker cold start (typical) ${secs(measured[0].coldMs!)}`
  const values = measured.map((m) => `${secs(m.coldMs!)} ${flashLabel(m.flashboot)}`).join(' / ')
  return `New-worker cold start ${values} (FlashBoot can't help a new worker)`
}
