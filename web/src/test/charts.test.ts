import { describe, expect, it } from 'vitest'
import {
  buildSeries,
  coldBreakdown,
  gpuRows,
  nearestRank,
  LABEL_GAP_FRACTION,
  phaseMarks,
  placeMarkLabels,
  rpColdCount,
  totalCost,
  unlabelledCaption,
} from '../chartData'
import { expectedLoadJobs, phaseSteps } from '../phases'
import type { ConfigDetail, GPUSummary, Summary } from '../types'
import { catalog, configDetail, job, runDetail, summary } from './fixtures'

describe('nearestRank (the server percentile)', () => {
  it('matches sorted[ceil(p*n/100)-1]', () => {
    expect(nearestRank([5, 1, 3, 2, 4], 50)).toBe(3)
    expect(nearestRank([1, 2, 3, 4], 50)).toBe(2)
    expect(nearestRank([1, 2, 3, 4], 95)).toBe(4)
    expect(nearestRank([], 50)).toBeNull()
  })
})

describe('buildSeries', () => {
  it('colour = pool (first --text-soft, second --measured), dash = FlashBoot on; names are the spec rows\' names', () => {
    const d = runDetail(
      [configDetail(1, 'A', 'OFF'), configDetail(2, 'A', 'FLASHBOOT'), configDetail(3, 'B', 'OFF'), configDetail(4, 'B', 'FLASHBOOT')],
      ['A', 'B'],
    )
    const s = buildSeries([{ detail: d }])
    expect(s.map((x) => [x.key, x.label, x.color, x.dashed])).toEqual([
      ['s0', 'A FlashBoot off', '#c9c6bd', false],
      ['s1', 'A FlashBoot on', '#c9c6bd', true],
      ['s2', 'B FlashBoot off', '#7fd1a4', false],
      ['s3', 'B FlashBoot on', '#7fd1a4', true],
    ])
  })
  it('in card mode the colour follows the card, even when both cards share a pool', () => {
    const a = configDetail(1, 'AMPERE_16', 'OFF')
    a.config = { ...a.config, card: 'NVIDIA RTX A4000' }
    const b = configDetail(2, 'AMPERE_16', 'OFF')
    b.config = { ...b.config, card: 'NVIDIA RTX A4500' }
    const d = runDetail([a, b], [])
    d.run.params = { ...d.run.params, cards: ['NVIDIA RTX A4000', 'NVIDIA RTX A4500'] }
    expect(buildSeries([{ detail: d }]).map((x) => x.color)).toEqual(['#c9c6bd', '#7fd1a4'])
    // With the catalog, cards are named as the spec rows name them.
    expect(buildSeries([{ detail: d, catalog: catalog() }]).map((x) => x.name)).toEqual(['RTX A4000 FlashBoot off', 'RTX A4500 FlashBoot off'])
  })
  it('in compare, names carry the run prefix and mode; dash is still FlashBoot, not the run', () => {
    const a = runDetail([configDetail(1, 'A', 'OFF')])
    const b = runDetail([configDetail(9, 'A', 'OFF')])
    const s = buildSeries([{ detail: a, prefix: 'Nov 8 · 10:00' }, { detail: b, prefix: 'Nov 8 · 11:00' }])
    expect(s.map((x) => [x.key, x.label, x.dashed])).toEqual([
      ['s0', 'Nov 8 · 10:00 · natural · A FlashBoot off', false],
      ['s1', 'Nov 8 · 11:00 · natural · A FlashBoot off', false],
    ])
  })
})

describe('phase separators', () => {
  const cd = configDetail(1, 'A', 'OFF')
  cd.jobs = [
    job(1, 1, 'first_boot', 0, 28_500),
    job(2, 1, 'cold', 40, 8_500),
    job(3, 1, 'load', 90, 60_000, { status: 'CANCELLED', level: 3 }),
    job(4, 1, 'load', 91, 600, { level: 3 }),
  ]
  it('marks the earliest send of each phase and level', () => {
    const other = configDetail(2, 'A', 'FLASHBOOT')
    other.jobs = [job(5, 2, 'cold', 35, 1500)]
    expect(phaseMarks(runDetail([cd, other]))).toEqual([
      { x: 0, label: 'first boot' },
      { x: 35, label: 'cold starts' },
      { x: 90, label: '3 req/s' },
    ])
  })
  it('a run with no jobs has no marks', () => {
    const empty = runDetail([configDetail(1, 'A', 'OFF')])
    expect(phaseMarks(empty)).toEqual([])
  })
})

describe('timeline separator labels', () => {
  // Domain [0, 100]: 4% of the span is 4 s.
  const marks = [
    { x: 0, label: 'first boot' },
    { x: 70, label: 'cold starts' },
    { x: 72, label: 'warm' }, // row 0, 72 s after first boot: drawn
    { x: 73, label: '1 req/s' }, // row 1, 3 s after cold starts: hidden
    { x: 80, label: '3 req/s' }, // row 0, 8 s after warm: drawn
    { x: 77, label: '8 req/s' }, // row 1, 7 s after cold starts (the last DRAWN label on row 1): drawn
  ]

  it('rows alternate; a label within 4% of the span of the previous drawn label on its row is hidden', () => {
    expect(LABEL_GAP_FRACTION).toBe(0.04)
    expect(placeMarkLabels(marks, 100)).toEqual([
      { x: 0, label: 'first boot', row: 0, showLabel: true },
      { x: 70, label: 'cold starts', row: 1, showLabel: true },
      { x: 72, label: 'warm', row: 0, showLabel: true },
      { x: 73, label: '1 req/s', row: 1, showLabel: false },
      { x: 80, label: '3 req/s', row: 0, showLabel: true },
      { x: 77, label: '8 req/s', row: 1, showLabel: true },
    ])
  })
  it('exactly 4% apart is within 4%, so hidden; just over is drawn', () => {
    const two = (x: number) => placeMarkLabels([{ x: 0, label: 'a' }, { x: 50, label: 'b' }, { x, label: 'c' }], 100)[2].showLabel
    expect(two(4)).toBe(false)
    expect(two(4.5)).toBe(true)
  })
  it('the span scales the gap: the same marks on a 1000 s axis are 0.4% apart', () => {
    const placed = placeMarkLabels(
      [
        { x: 0, label: 'a' },
        { x: 0, label: 'b' },
        { x: 30, label: 'c' },
      ],
      1000,
    )
    expect(placed.map((m) => m.showLabel)).toEqual([true, true, false])
  })
  it('two separators at the same x on the same row: the second label is hidden and listed', () => {
    const placed = placeMarkLabels(
      [
        { x: 0, label: 'first boot' }, // row 0
        { x: 70, label: 'cold starts' }, // row 1
        { x: 70, label: 'warm' }, // row 0, 70 s after first boot: drawn
        { x: 70, label: '1 req/s' }, // row 1, same x as cold starts: hidden
      ],
      100,
    )
    expect(placed.map((m) => [m.label, m.row, m.showLabel])).toEqual([
      ['first boot', 0, true],
      ['cold starts', 1, true],
      ['warm', 0, true],
      ['1 req/s', 1, false],
    ])
    expect(unlabelledCaption(placed)).toBe('Unlabelled dashed lines: 1 req/s at 70 s.')
  })
  it('the caption lists hidden labels, and is null when every label is drawn', () => {
    expect(unlabelledCaption(placeMarkLabels(marks, 100))).toBe('Unlabelled dashed lines: 1 req/s at 73 s.')
    const crowded = placeMarkLabels(
      [
        { x: 0, label: 'first boot' },
        { x: 240.4, label: 'cold starts' },
        { x: 241.2, label: 'warm' },
        { x: 246.6, label: '1 req/s' },
      ],
      300,
    )
    expect(unlabelledCaption(crowded)).toBe('Unlabelled dashed lines: 1 req/s at 247 s.')
    expect(unlabelledCaption(placeMarkLabels([{ x: 0, label: 'first boot' }, { x: 40, label: 'cold starts' }], 100))).toBeNull()
    expect(unlabelledCaption([])).toBeNull()
  })
})

describe('cold breakdown', () => {
  it('waiting = delay p50, running = exec p50 of completed cold jobs; null without samples', () => {
    const off = configDetail(1, 'A', 'OFF')
    off.summary.coldDelay = summary(2, 10_500)
    off.jobs = [
      job(1, 1, 'cold', 1, 11_000, { execMs: 600 }),
      job(2, 1, 'cold', 2, 11_000, { execMs: 700 }),
      job(3, 1, 'cold', 3, 11_000, { execMs: 9999, status: 'FAILED' }),
    ]
    const failedEarly: ConfigDetail = { ...configDetail(2, 'A', 'FLASHBOOT'), jobs: [] }
    failedEarly.config = { ...failedEarly.config, status: 'failed', error: 'no stock' }
    const rows = coldBreakdown(buildSeries([{ detail: runDetail([off, failedEarly]) }]))
    expect(rows[0]).toMatchObject({ n: 2, waiting: 10.5, running: 0.6 })
    expect(rows[1]).toMatchObject({ n: 0, waiting: null, running: null })
  })
})

describe('report helpers', () => {
  it('rpColdCount: the sum of Runpod cold-start counts; 0 for a null list, null without metrics', () => {
    const pct = (p95: number) => ({ min: 0, max: 0, total: 0, p70: 0, p90: 0, p95, p98: 0 })
    expect(
      rpColdCount({ requests: null, coldStarts: [{ time: 't', count: 2, durationMs: pct(28_000) }, { time: 't', count: 1, durationMs: pct(8000) }] }),
    ).toBe(3)
    expect(rpColdCount({ requests: null, coldStarts: null })).toBe(0)
    expect(rpColdCount(undefined)).toBeNull()
  })
  it('totalCost', () => {
    const a = configDetail(1, 'A', 'OFF')
    a.summary.costUsd = 0.25
    const b = configDetail(2, 'A', 'FLASHBOOT')
    b.summary.costUsd = 0.5
    const d = runDetail([a, b])
    expect(totalCost(d)).toBe(0.75)
  })
})

describe('phase checklist', () => {
  const p = { ...runDetail([]).run.params, coldStarts: 3, warmRequests: 2, levelsRps: [1, 3], levelSeconds: 10 }
  const cd = (jobs: ReturnType<typeof job>[], status: ConfigDetail['config']['status'] = 'running'): ConfigDetail => {
    const c = configDetail(1, 'A', 'OFF')
    return { ...c, config: { ...c.config, status }, jobs }
  }
  const states = (c: ConfigDetail) => phaseSteps(p, c).map((s) => s.state)

  it('expected load jobs follow the open-loop schedule', () => {
    expect(expectedLoadJobs(3, 10)).toBe(30)
    expect(expectedLoadJobs(0.3, 10)).toBe(3)
  })
  it('before any job, first boot is current', () => {
    expect(states(cd([]))).toEqual(['current', 'pending', 'pending', 'pending', 'pending'])
  })
  it('fills in counts and typical values as jobs complete', () => {
    const steps = phaseSteps(p, cd([job(1, 1, 'first_boot', 0, 94_500), job(2, 1, 'cold', 100, 11_200)]))
    expect(steps.map((s) => s.state)).toEqual(['done', 'current', 'pending', 'pending', 'pending'])
    expect(steps[0].detail).toBe('94 s')
    expect(steps[1].detail).toBe('1/3 · typical 10.7 s')
  })
  it('skipped cold samples (workers stayed up) are "short", never a green done', () => {
    const steps = phaseSteps(p, cd([job(1, 1, 'first_boot', 0, 9000), job(2, 1, 'cold', 10, 2000), job(3, 1, 'warm', 20, 600)]))
    expect(steps.map((s) => s.state)).toEqual(['done', 'short', 'current', 'pending', 'pending'])
    expect(steps[1].detail).toBe('1 of 3 (then workers stayed up past the 3 min wait) · typical 1.5 s')
    expect(steps[1].help).toMatch(/skipped the remaining samples/)
  })
  it('a load step counts completed jobs, not every recorded one', () => {
    const lv = [job(10, 1, 'load', 30, 600, { level: 1 }), job(11, 1, 'load', 31, 600, { level: 1, status: 'CANCELLED' })]
    expect(phaseSteps(p, cd([job(1, 1, 'first_boot', 0, 9000), ...lv]))[3].detail).toBe('1/10 completed')
  })
  it('a full level moves on to the next', () => {
    const lv = Array.from({ length: 10 }, (_, i) => job(10 + i, 1, 'load', 30 + i, 600, { level: 1 }))
    const jobs = [job(1, 1, 'first_boot', 0, 9000), ...[2, 3, 4].map((i) => job(i, 1, 'cold', i, 2000)), job(5, 1, 'warm', 20, 600), job(6, 1, 'warm', 21, 600), ...lv]
    expect(states(cd(jobs))).toEqual(['done', 'done', 'done', 'done', 'current'])
    expect(phaseSteps(p, cd(jobs))[3].detail).toBe('10/10 completed')
  })
  it('a config that failed before its first boot', () => {
    expect(states(cd([], 'failed'))).toEqual(['failed', 'unreached', 'unreached', 'unreached', 'unreached'])
  })
  it('a cancel mid-level: that level is cut short, the rest not reached', () => {
    const jobs = [job(1, 1, 'first_boot', 0, 9000), ...[2, 3, 4].map((i) => job(i, 1, 'cold', i, 2000)), job(5, 1, 'warm', 20, 600), job(6, 1, 'warm', 21, 600), job(7, 1, 'load', 30, 600, { level: 1 })]
    expect(states(cd(jobs, 'cancelled'))).toEqual(['done', 'done', 'done', 'cut', 'unreached'])
  })
  it('a done config with every step complete is all done', () => {
    const jobs = [
      job(1, 1, 'first_boot', 0, 9000),
      ...[2, 3, 4].map((i) => job(i, 1, 'cold', i, 2000)),
      job(5, 1, 'warm', 20, 600),
      job(6, 1, 'warm', 21, 600),
      ...Array.from({ length: 10 }, (_, i) => job(10 + i, 1, 'load', 30 + i, 600, { level: 1 })),
      ...Array.from({ length: 30 }, (_, i) => job(30 + i, 1, 'load', 50 + i, 600, { level: 3 })),
    ]
    expect(states(cd(jobs, 'done'))).toEqual(['done', 'done', 'done', 'done', 'done'])
  })
})

describe('GPU matmul rows', () => {
  const sum = (n: number, p50: number, p95 = p50): Summary => ({ n, p50, p95, mean: p50, min: p50, max: p95 })
  const gpu = (gpuTypeId: string, tflops: number, slowMs: number): GPUSummary => {
    // At matmulN 4096 a matmul is 2 * 4096^3 = 1.374e11 FLOPs.
    const ms = (2 * 4096 ** 3) / (tflops * 1e12) * 1000
    return { gpuTypeId, matmulMs: sum(6, ms, slowMs), tflops: sum(6, tflops), usdPerTflopHr: 0.58 / tflops, coldCudaInitMs: sum(2, 220) }
  }
  const withGpus = (cd: ConfigDetail, gpus?: GPUSummary[]): ConfigDetail => ({ ...cd, summary: { ...cd.summary, gpus } })
  const d = runDetail([
    withGpus(configDetail(1, 'AMPERE_24', 'OFF'), [gpu('NVIDIA GeForce RTX 4090', 160, 1.0), gpu('NVIDIA RTX A5000', 55, 3.0)]),
    withGpus(configDetail(2, 'AMPERE_24', 'FLASHBOOT')),
  ])

  it('one row per GPU per config; a config without gpus adds none', () => {
    const rows = gpuRows(buildSeries([{ detail: d }]))
    expect(rows.map((r) => [r.name, r.gpuTypeId])).toEqual([
      ['AMPERE_24 FlashBoot off', 'NVIDIA GeForce RTX 4090'],
      ['AMPERE_24 FlashBoot off', 'NVIDIA RTX A5000'],
    ])
    const r = rows[0]
    expect(r.tflopsP50).toBe(160)
    expect(r.tflopsN).toBe(6)
    expect(r.matmulMsP50).toBeCloseTo(0.859, 3)
    expect(r.matmulMsP95).toBe(1.0)
    expect(r.usdPerTflopHr).toBeCloseTo(0.003625, 6)
    expect(r.coldCudaInitP50).toBe(220)
    expect(r.coldCudaInitN).toBe(2)
    expect(new Set(rows.map((x) => x.key)).size).toBe(2)
  })
  it('a GPU with CUDA init only (matmulN 0) has no matmul numbers', () => {
    const cudaOnly: GPUSummary = { gpuTypeId: '', matmulMs: sum(0, 0), tflops: sum(0, 0), usdPerTflopHr: 0, coldCudaInitMs: sum(1, 240) }
    const [r] = gpuRows(buildSeries([{ detail: runDetail([withGpus(configDetail(1, 'A', 'OFF'), [cudaOnly])]) }]))
    expect([r.tflopsP50, r.matmulMsP50, r.matmulMsP95, r.usdPerTflopHr, r.coldCudaInitP50]).toEqual([null, null, null, null, 240])
  })
  it('compare: only the run with gpus has rows, with its mode', () => {
    const plain = runDetail([configDetail(1, 'AMPERE_16', 'OFF')])
    const rows = gpuRows(buildSeries([{ detail: plain, prefix: 'A' }, { detail: d, prefix: 'B' }]))
    expect(rows.map((r) => r.name)).toEqual(['B · AMPERE_24 FlashBoot off', 'B · AMPERE_24 FlashBoot off'])
    expect(rows.every((r) => r.mode === 'natural' && !r.dashed)).toBe(true)
  })
  it('no gpus anywhere: no rows (the table is left out)', () => {
    expect(gpuRows(buildSeries([{ detail: runDetail([configDetail(1, 'A', 'OFF')]) }]))).toEqual([])
  })
})
