import { describe, expect, it } from 'vitest'
import { rollingMedian } from '../chartData'
import { niceCeil, notDrawn, runTraces, traceExtent, tracePoints, yTickLabel, yTicks } from '../trace'
import { configDetail, job, runDetail } from './fixtures'

describe('rollingMedian', () => {
  it('the window grows from 1 at the start: odd counts take the middle, even the mean of the two middles', () => {
    expect(rollingMedian([5, 1, 3])).toEqual([5, 3, 3])
    expect(rollingMedian([1, 2, 3, 4, 100], 3)).toEqual([1, 1.5, 2, 3, 4])
  })
  it('defaults to the last 10 values', () => {
    // An unbounded window would give 6.5 at the end.
    const out = rollingMedian([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12])
    expect(out).toHaveLength(12)
    expect(out[9]).toBe(5.5) // 1..10
    expect(out[10]).toBe(6.5) // 2..11
    expect(out[11]).toBe(7.5) // 3..12
  })
  it('an empty list gives an empty list', () => {
    expect(rollingMedian([])).toEqual([])
  })
})

describe('tracePoints', () => {
  const cd = configDetail(1, 'A', 'OFF')
  cd.jobs = [
    job(1, 1, 'first_boot', 0, 72_000),
    job(2, 1, 'cold', 40, 8_500),
    job(3, 1, 'cold', 45, 9_000, { status: 'FAILED' }),
    job(5, 1, 'warm', 61, 3_000), // saved after job 6, sent later
    job(4, 1, 'warm', 60, 1_000),
    job(6, 1, 'load', 90, 60_000, { status: 'CANCELLED', level: 3 }),
    job(7, 1, 'load', 91, 2_000, { level: 3 }),
  ]
  const t = tracePoints(cd, '2026-11-08T10:00:00Z')

  it('dots: completed first-boot and cold jobs, at (seconds since start, wall seconds)', () => {
    expect(t.dots).toEqual([
      { x: 0, y: 72 },
      { x: 40, y: 8.5 },
    ])
  })
  it('line: the rolling median of completed warm and load jobs, in send order', () => {
    expect(t.line).toEqual([
      { x: 60, y: 1 },
      { x: 61, y: 2 }, // median of 1 s and 3 s
      { x: 91, y: 2 }, // median of 1, 3, 2
    ])
  })
  it('a config with no jobs has no dots and no line', () => {
    expect(tracePoints(configDetail(2, 'A', 'OFF'), '2026-11-08T10:00:00Z')).toEqual({ dots: [], line: [] })
  })
})

describe('runTraces, traceExtent, notDrawn', () => {
  const a = configDetail(1, 'A', 'OFF')
  a.jobs = [job(1, 1, 'first_boot', 0, 28_000), job(2, 1, 'warm', 50, 600), job(3, 1, 'load', 120, 9_000, { status: 'CANCELLED', level: 8 })]
  const b = configDetail(2, 'A', 'FLASHBOOT')
  b.jobs = [job(4, 2, 'cold', 40, 8_000), job(5, 2, 'load', 90, 1_500, { level: 3 })]
  const d = runDetail([a, b])

  it('one trace per config, keyed like its spec row', () => {
    const t = runTraces(d)
    expect(t.map((x) => x.key)).toEqual(['c1', 'c2'])
    expect(t[0].dots).toEqual([{ x: 0, y: 28 }])
    expect(t[1].line).toEqual([{ x: 90, y: 1.5 }])
  })
  it('the shared scale: x to the latest point drawn or now (at least 10 s), y to the slowest point rounded up nicely (at least 1 s)', () => {
    expect(traceExtent(runTraces(d))).toEqual({ xMax: 90, yMax: 40 }) // slowest 28 s
    expect(traceExtent(runTraces(d), 200)).toEqual({ xMax: 200, yMax: 40 })
    expect(traceExtent([])).toEqual({ xMax: 10, yMax: 1 })
  })
  it('yTicks: quarters of the y max, labelled with secs so fractions read correctly, and 0 as "0 s"', () => {
    const label = (yMax: number) => yTicks(yMax).map(yTickLabel)
    expect(yTicks(1)).toEqual([0, 0.25, 0.5, 0.75, 1])
    expect(label(1)).toEqual(['0 s', '250 ms', '500 ms', '750 ms', '1.0 s'])
    expect(yTicks(5)).toEqual([0, 1.25, 2.5, 3.75, 5])
    expect(label(5)).toEqual(['0 s', '1.3 s', '2.5 s', '3.8 s', '5.0 s'])
    expect(label(40)).toEqual(['0 s', '10 s', '20 s', '30 s', '40 s'])
    expect(yTicks(0.6)).toEqual([0, 0.15, 0.3, 0.45, 0.6]) // no 0.44999999999999996
  })
  it('niceCeil: 1, 2, 4, 5, 6, 8 or 10 times a power of ten', () => {
    expect([28, 30, 7.3, 0.9, 1, 41, 100, 101, 0].map(niceCeil)).toEqual([40, 40, 8, 1, 1, 50, 100, 200, 1])
    expect(niceCeil(0.30000000000000004 * 2)).toBe(0.6) // float noise doesn't push it to 0.8
  })
  it('counts the requests with no latency to draw', () => {
    expect(notDrawn(d)).toBe(1)
  })
})
