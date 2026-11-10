import { capitalize, secs, usd } from './format'
import type { ColdNote } from './coldNote'
import type { SpecRow } from './specRows'

// A lower value wins only when at least 5% lower (502 vs 501 ms is noise) and
// both sides have at least 3 samples (one or two can't call a winner).
export const BETTER_FRACTION = 0.95
export const MIN_SAMPLES = 3

export const THIN_SAMPLES_HELP = ` With fewer than ${MIN_SAMPLES} samples no winner is called.`

export function lowerIndex(values: (number | null)[], ns: number[]): number | null {
  return enoughSamples(ns) ? fivePercentLower(values) : null
}

export function thinSamplesHidWinner(values: (number | null)[], ns: number[]): boolean {
  return !enoughSamples(ns) && fivePercentLower(values) !== null
}

const enoughSamples = (ns: number[]) => ns.length === 2 && ns.every((n) => n >= MIN_SAMPLES)

function fivePercentLower(values: (number | null)[]): number | null {
  const [a, b] = values
  if (values.length !== 2 || a === null || b === null) return null
  if (a <= BETTER_FRACTION * b) return 0
  if (b <= BETTER_FRACTION * a) return 1
  return null
}

// The finding: which pool (or card) suits this job. Each side is represented
// by its FlashBoot-off config so FlashBoot never decides the verdict. Rules in
// priority order: (1) at the top level one served all and the other left some
// queued; (2) typical warm differs by 25%+; (3) otherwise "alike".

// Faster means at most 75% of the slower typical warm time.
export const WARM_DIFF_FRACTION = 0.75
// Two $/1k figures within 15% of the lower one cost "about the same".
export const COST_SAME_FRACTION = 0.15

export interface Finding {
  sentences: string[]
  help: string
}

export function finding(rows: SpecRow[], mode: 'pool' | 'card'): Finding | null {
  const picked = representatives(rows)
  if (picked.length === 0) return null
  if (picked.length === 1) return describe(picked[0])
  const [a, b] = picked
  const noun = mode === 'card' ? 'cards' : 'pools'
  const pair = `${label(a)} and ${label(b)}`

  const queued = servedVsQueued(a, b)
  if (queued) {
    const [w, l] = queued
    const first = `${w.subject} is the better buy for this job: at ${w.topLevel.rps} req/s it ${servedWords(w)}, while ${l.subject} left ${l.topLevel.leftQueued} of ${l.topLevel.sent} queued.`
    const second = betterBuyCost(w, l)
    return {
      sentences: second ? [first, second] : [first],
      help: `Compares ${pair} by the requests each left queued at the top load level, ${w.topLevel.rps} req/s.`,
    }
  }

  const cost = costSentence(a, b)
  const withCost = (s: string) => (cost ? [s, cost] : [s])
  if (a.warm.n < MIN_SAMPLES || b.warm.n < MIN_SAMPLES) {
    return {
      sentences: withCost(`Too few warm requests to compare the ${noun} for this job.`),
      help: `Compares ${pair}; a warm comparison needs at least ${MIN_SAMPLES} warm requests on each side.`,
    }
  }
  const help = `Compares ${pair} on the typical warm request, different when one is at least 25% lower, and on estimated cost per 1k requests, the same within 15%.`
  const [fast, slow] = a.warm.p50! <= b.warm.p50! ? [a, b] : [b, a]
  if (fast.warm.p50! <= WARM_DIFF_FRACTION * slow.warm.p50!) {
    const ranOn = mode === 'pool' && fast.gotCards.length > 0 ? ` (it ran on ${fast.gotCards.join(' and ')})` : ''
    return {
      sentences: withCost(
        `${fast.subject}${ranOn} is faster for this job: a typical warm request takes ${secs(fast.warm.p50!)} against ${secs(slow.warm.p50!)} on ${slow.subject}.`,
      ),
      help,
    }
  }
  return {
    sentences: withCost(
      `The ${noun} performed alike for this job (typical warm ${secs(a.warm.p50!)} on ${a.subject}, ${secs(b.warm.p50!)} on ${b.subject}).`,
    ),
    help,
  }
}

// Keyed by subject so a pool the params don't list can't merge with the first one.
function representatives(rows: SpecRow[]): SpecRow[] {
  const bySubject = new Map<string, SpecRow>()
  for (const r of rows) {
    const have = bySubject.get(r.subject)
    if (!have || (have.flashboot !== 'OFF' && r.flashboot === 'OFF')) bySubject.set(r.subject, r)
  }
  return [...bySubject.values()].sort((a, b) => a.poolIndex - b.poolIndex)
}

const label = (r: SpecRow) => r.name

// A failed request is never counted as served.
function servedWords(r: SpecRow): string {
  const t = r.topLevel
  return t.served === t.sent ? `served all ${t.sent} requests` : `served ${t.served} of ${t.sent} and left none queued`
}

// Failing fast empties a queue too, so a row with failures can't win on queued counts.
function servedVsQueued(a: SpecRow, b: SpecRow): [SpecRow, SpecRow] | null {
  const all = (r: SpecRow) => r.topLevel.reached && r.topLevel.leftQueued === 0 && r.topLevel.failed === 0
  const some = (r: SpecRow) => r.topLevel.reached && r.topLevel.leftQueued > 0
  if (all(a) && some(b)) return [a, b]
  if (all(b) && some(a)) return [b, a]
  return null
}

const sameCost = (x: number, y: number) => Math.abs(x - y) <= COST_SAME_FRACTION * Math.min(x, y)
const enoughWarm = (r: SpecRow) => r.warm.n >= MIN_SAMPLES

function betterBuyCost(w: SpecRow, l: SpecRow): string | null {
  const warm = w.warm.p50 !== null && l.warm.p50 !== null ? `typical warm ${secs(w.warm.p50)} against ${secs(l.warm.p50)}` : null
  if (w.per1k === null || l.per1k === null) return warm ? `${capitalize(warm)}.` : null
  const tail = warm ? `; ${warm}` : ''
  if (sameCost(w.per1k, l.per1k)) {
    const waitsLess = warm && enoughWarm(w) && enoughWarm(l) && w.warm.p50! < l.warm.p50! ? ' and waits less' : ''
    return `It costs about the same per request${waitsLess}${warm ? ` (${warm})` : ''}.`
  }
  const prices = `(${usd(w.per1k)} against ${usd(l.per1k)})`
  return w.per1k < l.per1k ? `It is also cheaper per 1k requests ${prices}${tail}.` : `It costs more per 1k requests ${prices}${tail}.`
}

function costSentence(a: SpecRow, b: SpecRow): string | null {
  if (a.per1k === null || b.per1k === null) return null
  if (sameCost(a.per1k, b.per1k)) return `They cost about the same per 1k requests (${usd(a.per1k)} and ${usd(b.per1k)}).`
  const [cheap, dear] = a.per1k < b.per1k ? [a, b] : [b, a]
  return `${cheap.subject} is cheaper per 1k requests (${usd(cheap.per1k!)} against ${usd(dear.per1k!)}).`
}

function describe(r: SpecRow): Finding {
  const first = r.firstBootMs !== null ? `the first boot took ${secs(r.firstBootMs)}` : 'no first boot completed'
  const cold = r.cold.p50 !== null ? `a cold start typically took ${secs(r.cold.p50)}` : 'no cold start was measured'
  const warm = r.warm.p50 !== null ? `a warm request typically took ${secs(r.warm.p50)}` : 'no warm request completed'
  const t = r.topLevel
  let top = ''
  if (t.rps > 0) {
    if (!t.reached) top = `; it did not reach ${t.rps} req/s`
    else if (t.leftQueued === 0) top = `; at ${t.rps} req/s it ${servedWords(r)}`
    else top = `; at ${t.rps} req/s it left ${t.leftQueued} of ${t.sent} requests queued`
  }
  return {
    sentences: [`On ${r.subject} ${first}, ${cold} and ${warm}${top}.`],
    help: `Describes ${label(r)}: the first boot is one sample, the other times are typical (median) values.`,
  }
}

// One readout per subject that ran with FlashBoot both off and on. No winner
// in forced mode, where FlashBoot can't help a new worker. `kept`: the
// FlashBoot-on worker stayed up, so it took fewer cold samples than asked.
export type FlashBootReadout =
  | { kind: 'cold'; subject: string; off: number; on: number; lower: number | null; note: string }
  | { kind: 'kept'; subject: string; note: ColdNote }

export function flashbootReadouts(rows: SpecRow[], forced: boolean): FlashBootReadout[] {
  const out: FlashBootReadout[] = []
  const subjects = [...new Set([...rows].sort((a, b) => a.poolIndex - b.poolIndex).map((r) => r.subject))]
  for (const subject of subjects) {
    const off = rows.find((r) => r.subject === subject && r.flashboot === 'OFF')
    const on = rows.find((r) => r.subject === subject && r.flashboot === 'FLASHBOOT')
    if (!off || !on) continue
    if (off.cold.p50 !== null && on.cold.p50 !== null) {
      const values = [off.cold.p50, on.cold.p50]
      const ns = [off.cold.n, on.cold.n]
      const lower = forced ? null : lowerIndex(values, ns)
      out.push({ kind: 'cold', subject, off: off.cold.p50, on: on.cold.p50, lower, note: readoutNote(values, ns, lower, forced) })
    }
    if (on.coldNote) out.push({ kind: 'kept', subject, note: on.coldNote })
  }
  return out
}

function readoutNote(values: number[], ns: number[], lower: number | null, forced: boolean): string {
  const samples = `typical of ${ns[0]} and ${ns[1]} samples`
  if (forced) return `${samples}; every one started a new worker`
  if (lower !== null) return `${samples}; FlashBoot ${lower === 1 ? 'on' : 'off'} is at least 5% lower`
  if (thinSamplesHidWinner(values, ns)) return `${samples}; too few to call a difference`
  return `${samples}; within 5% of each other`
}
