import { rollingMedian, type Point } from './chartData'
import { secs } from './format'
import type { ConfigDetail, Job, RunDetail } from './types'

// One config's latency through the run: dots for the first boot and cold
// starts, then a rolling median of warm and load wall times in send order.
export function tracePoints(cd: ConfigDetail, runStartIso: string): { dots: Point[]; line: Point[] } {
  const start = Date.parse(runStartIso)
  const x = (j: Job) => (Date.parse(j.sentAt) - start) / 1000
  const done = cd.jobs.filter((j) => j.status === 'COMPLETED').sort((a, b) => Date.parse(a.sentAt) - Date.parse(b.sentAt))

  const dots = done.filter((j) => j.phase === 'first_boot' || j.phase === 'cold').map((j) => ({ x: x(j), y: j.wallMs / 1000 }))
  const steady = done.filter((j) => j.phase === 'warm' || j.phase === 'load')
  const medians = rollingMedian(steady.map((j) => j.wallMs / 1000))
  const line = steady.map((j, i) => ({ x: x(j), y: medians[i] }))
  return { dots, line }
}

// key matches the spec row's (c<config id>).
export interface Trace {
  key: string
  dots: Point[]
  line: Point[]
}

export function runTraces(d: RunDetail): Trace[] {
  return d.configs.map((cd) => ({ key: `c${cd.config.id}`, ...tracePoints(cd, d.run.createdAt) }))
}

// Shared scales so trace cells and the big chart read against each other.
// x is at least 10 s so a first dot near 0 doesn't squash the axis.
export function traceExtent(traces: Trace[], nowS = 0): { xMax: number; yMax: number } {
  const points = traces.flatMap((t) => [...t.dots, ...t.line])
  return {
    xMax: points.reduce((m, p) => Math.max(m, p.x), Math.max(nowS, 10)),
    yMax: niceCeil(points.reduce((m, p) => Math.max(m, p.y), 1)),
  }
}

// Compare uses identical axes across runs. A max of niceCeil values is itself one.
export function sharedExtent(extents: { xMax: number; yMax: number }[]): { xMax: number; yMax: number } {
  return extents.reduce((m, e) => ({ xMax: Math.max(m.xMax, e.xMax), yMax: Math.max(m.yMax, e.yMax) }), { xMax: 10, yMax: 1 })
}

// 28 -> 40, 7.3 -> 8, 0.9 -> 1. A quarter of each is a round tick.
const NICE = [1, 2, 4, 5, 6, 8, 10]

export function niceCeil(y: number): number {
  if (y <= 0) return 1
  const p = 10 ** Math.floor(Math.log10(y))
  // Round the ratio first, or float noise (0.6000000000000001) bumps 0.6 up to 0.8.
  const m = NICE.find((n) => n >= Number((y / p).toFixed(9)))!
  return Number((m * p).toPrecision(12)) // 6 * 0.1 is 0.6000000000000001
}

// toPrecision(12) so 0.6 / 4 * 3 reads 0.45, not 0.44999999999999996.
export function yTicks(yMax: number): number[] {
  return [0, 1, 2, 3, 4].map((i) => Number(((i * yMax) / 4).toPrecision(12)))
}

// "0 s", not "0 ms", to match the seconds above it.
export const yTickLabel = (v: number) => (v === 0 ? '0 s' : secs(v * 1000))

// Requests with no latency to draw: anything not COMPLETED.
export const notDrawn = (d: RunDetail) =>
  d.configs.reduce((n, cd) => n + cd.jobs.filter((j) => j.status !== 'COMPLETED').length, 0)
