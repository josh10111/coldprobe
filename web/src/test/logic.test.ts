import { describe, expect, it } from 'vitest'
import { checkSeq } from '../api'
import { configLabel, configName, duration, latestResults, matmulSuffix, poolIndex, reportTitle, runHeading, runSubject, secs, sig3, statusTone, stockWord, usd, usd3sf, when } from '../format'
import { estimateKey, fieldError, formErrors, formFromDefaults, GPU_IMAGE, levelsError, needsGpuImage, parseNumberList, pickerRows, rangeHelp, stockWarning, switchGpuMode, toggleGpu, toParams } from '../form'
import { lowerIndex, MIN_SAMPLES, THIN_SAMPLES_HELP, thinSamplesHidWinner } from '../finding'
import { coldHeadline, headlineNumbers } from '../headlineToast'
import { applyEvent, phaseWord, withSnapshot } from '../liveRun'
import { sharedExtent } from '../trace'
import type { Card, ConfigDetail, Job, LogEvent, Pool, Run, RunEvent, RunStatus } from '../types'
import { catalog, configDetail, job as fxJob, levelSum, runDetail } from './fixtures'
import { expectedLoadJobs, liveWords } from '../phases'
import { liveSpecRows } from '../specRows'

describe('checkSeq (SSE gap check)', () => {
  it('takes the first seq as a baseline, whatever it is', () => {
    expect(checkSeq(null, 42)).toEqual({ gap: false, last: 42 })
  })
  it('accepts consecutive seqs', () => {
    expect(checkSeq(42, 43)).toEqual({ gap: false, last: 43 })
  })
  it('flags a jump', () => {
    expect(checkSeq(42, 45)).toEqual({ gap: true, last: 45 })
  })
  it('skips seq 0 and keeps the last seq', () => {
    expect(checkSeq(42, 0)).toEqual({ gap: false, last: 42 })
    expect(checkSeq(null, 0)).toEqual({ gap: false, last: null })
  })
})

describe('format', () => {
  it('usd', () => expect(usd(1.421)).toBe('$1.42'))
  it('matmulSuffix: only for a run that set a matmul size', () => {
    const p = runDetail([]).run.params
    expect(matmulSuffix(p)).toBe('')
    expect(matmulSuffix({ ...p, matmulN: 0 })).toBe('')
    expect(matmulSuffix({ ...p, matmulN: 4096 })).toBe(' · matmul 4096')
  })
  it('sig3 and usd3sf: 3 significant figures, never exponent form', () => {
    expect(sig3(163.04)).toBe('163')
    expect(sig3(98.44)).toBe('98.4')
    expect(sig3(0.8452)).toBe('0.845')
    expect(sig3(1234.5)).toBe('1230')
    expect(usd3sf(0.58 / 165)).toBe('$0.00352')
  })
  it('secs: at most 3 significant figures, a space before the unit', () => {
    expect(secs(501.2)).toBe('501 ms')
    expect(secs(999.7)).toBe('1.0 s')
    expect(secs(8000)).toBe('8.0 s')
    expect(secs(1500)).toBe('1.5 s')
    expect(secs(10_740)).toBe('10.7 s')
    expect(secs(28_001)).toBe('28 s')
    expect(secs(117_400)).toBe('117 s')
  })
  it('duration', () => {
    expect(duration(42_900)).toBe('42s')
    expect(duration(662_000)).toBe('11m 02s')
    expect(duration(3_900_000)).toBe('1h 05m')
    expect(duration(-5)).toBe('0s')
  })
  it('when uses local time', () => {
    expect(when(new Date(2026, 10, 8, 17, 5).toISOString())).toBe('Nov 8 · 17:05')
  })
  it('reportTitle', () => {
    const p = runDetail([]).run.params
    expect(reportTitle({ ...p, pools: ['AMPERE_16', 'AMPERE_24'] })).toBe('Benchmark report · AMPERE_16 vs AMPERE_24 · FlashBoot off and on')
    expect(reportTitle({ ...p, flashboot: ['OFF'], cards: ['NVIDIA RTX A4500', 'NVIDIA GONE'] }, catalog())).toBe('Benchmark report · RTX A4500 vs NVIDIA GONE · FlashBoot off')
  })
  it('runHeading: the live header, the report title without "Benchmark report"', () => {
    const p = runDetail([]).run.params
    expect(runHeading({ ...p, pools: ['AMPERE_16', 'AMPERE_24'] })).toBe('AMPERE_16 vs AMPERE_24 · FlashBoot off and on')
    expect(runHeading({ ...p, flashboot: ['FLASHBOOT'] })).toBe('AMPERE_16 · FlashBoot on')
  })
  it('runSubject: cards in card mode, else pools', () => {
    const p = runDetail([]).run.params
    expect(runSubject({ ...p, pools: ['A', 'B'] })).toBe('A + B')
    expect(runSubject({ ...p, cards: [] })).toBe('AMPERE_16')
    expect(runSubject({ ...p, cards: ['NVIDIA RTX A4500'] })).toBe('NVIDIA RTX A4500')
  })
  it('configLabel is the spec row name, and names a card-mode config by its card', () => {
    expect(configLabel({ pool: 'AMPERE_16', flashboot: 'OFF' })).toBe('AMPERE_16 FlashBoot off')
    expect(configLabel({ pool: 'AMPERE_16', card: 'NVIDIA RTX A4500', flashboot: 'FLASHBOOT' })).toBe('NVIDIA RTX A4500 FlashBoot on')
  })
  it('configName: card or pool, then FlashBoot off/on', () => {
    expect(configName({ pool: 'AMPERE_16', flashboot: 'OFF' })).toBe('AMPERE_16 FlashBoot off')
    expect(configName({ pool: 'AMPERE_16', card: 'NVIDIA RTX A4500', flashboot: 'FLASHBOOT' })).toBe('NVIDIA RTX A4500 FlashBoot on')
  })
  it('poolIndex: position in params.pools, or in params.cards in card mode; 0 when absent', () => {
    const p = runDetail([]).run.params
    expect(poolIndex({ ...p, pools: ['A', 'B'] }, { pool: 'B' })).toBe(1)
    expect(poolIndex({ ...p, pools: [], cards: ['X', 'Y'] }, { pool: 'A', card: 'Y' })).toBe(1)
    expect(poolIndex({ ...p, pools: ['A'] }, { pool: 'GONE' })).toBe(0)
  })
  it('stockWord: lower-case word and its class; unknown when empty', () => {
    expect(stockWord('HIGH')).toEqual({ word: 'high', className: 'stock-high' })
    expect(stockWord('NONE')).toEqual({ word: 'none', className: 'stock-none' })
    expect(stockWord('')).toEqual({ word: 'unknown', className: 'stock-unknown' })
  })
  it('statusTone groups run statuses by colour', () => {
    expect(statusTone.completed).toBe('done')
    expect(statusTone.aborted_budget).toBe('warn')
    expect(statusTone.aborted_crash).toBe('bad')
    expect(statusTone.running).toBe('running')
  })
})

describe('types for card mode and warn logs (checked by tsc)', () => {
  it('a pool carries its cards; a log event may be a warning', () => {
    const card: Card = { id: 'NVIDIA RTX A4500', name: 'RTX A4500', memoryGb: 20, priceHr: 0.58, availability: 'HIGH' }
    const pool: Pool = { id: 'AMPERE_24', gpuNames: ['RTX A4500'], memoryGb: 20, pricePerHr: 0.58, availability: 'HIGH', cards: [card] }
    const warn: LogEvent = { runId: 'r1', seq: 1, ts: '', type: 'log', configId: 1, data: { msg: 'no worker', level: 'warn' } }
    expect(pool.cards[0].id).toBe('NVIDIA RTX A4500')
    expect(warn.data.level).toBe('warn')
  })
})

// bench.ParamLimits, with maxWorkers capped to 3 by COLDPROBE_MAX_WORKERS.
const LIMITS: Record<string, [number, number]> = {
  maxWorkers: [1, 3],
  idleTimeoutS: [1, 3600],
  coldStarts: [1, 10],
  warmRequests: [1, 50],
  levelSeconds: [1, 300],
  drainSeconds: [0, 300],
  jobSleepMs: [0, 60000],
  modelLoadS: [0, 300],
  matmulN: [0, 8192],
  pools: [1, 2],
  levelsRps: [1, 3],
  rps: [0, 20],
}

describe('inline field checks (the server\'s ranges, from limits)', () => {
  it('fieldError: a number in range is fine, at both ends', () => {
    expect(fieldError('maxWorkers', '1', LIMITS, 2)).toBeNull()
    expect(fieldError('maxWorkers', '3', LIMITS, 2)).toBeNull()
    expect(fieldError('drainSeconds', '0', LIMITS, 2)).toBeNull()
    expect(fieldError('matmulN', '8192', LIMITS, 2)).toBeNull()
  })
  it('fieldError: outside the range names the range, and a value above it is not clamped', () => {
    expect(fieldError('maxWorkers', '4', LIMITS, 2)).toBe('1 to 3')
    expect(fieldError('maxWorkers', '0', LIMITS, 2)).toBe('1 to 3')
    expect(fieldError('matmulN', '8193', LIMITS, 2)).toBe('0 to 8192')
    expect(fieldError('drainSeconds', '-1', LIMITS, 2)).toBe('0 to 300')
  })
  it('fieldError: blank, not a number, not whole', () => {
    expect(fieldError('coldStarts', '', LIMITS, 2)).toBe('required')
    expect(fieldError('coldStarts', '  ', LIMITS, 2)).toBe('required')
    expect(fieldError('coldStarts', 'abc', LIMITS, 2)).toBe('a number')
    expect(fieldError('coldStarts', '2.5', LIMITS, 2)).toBe('whole number')
  })
  it('fieldError: the budget is above $0 and at most the cap, cents allowed', () => {
    expect(fieldError('budgetUsd', '4', LIMITS, 4)).toBeNull()
    expect(fieldError('budgetUsd', '1.25', LIMITS, 4)).toBeNull()
    expect(fieldError('budgetUsd', '4.01', LIMITS, 4)).toBe('at most $4.00')
    expect(fieldError('budgetUsd', '0', LIMITS, 4)).toBe('above $0.00')
  })
  it('fieldError: with no limits (an older server) only the number checks apply', () => {
    expect(fieldError('maxWorkers', '9', {}, 2)).toBeNull()
    expect(fieldError('maxWorkers', '1.5', {}, 2)).toBe('whole number')
  })
  it('levelsError: count, each rate, repeats', () => {
    expect(levelsError('1, 3, 8', LIMITS)).toBeNull()
    expect(levelsError('20', LIMITS)).toBeNull()
    expect(levelsError('', LIMITS)).toBe('required')
    expect(levelsError('1, x', LIMITS)).toBe('numbers, comma-separated')
    expect(levelsError('1, 2, 3, 4', LIMITS)).toBe('1 to 3 levels')
    expect(levelsError('0, 3', LIMITS)).toBe('each above 0, at most 20')
    expect(levelsError('21', LIMITS)).toBe('each above 0, at most 20')
    expect(levelsError('3, 1, 3', LIMITS)).toBe('no repeats')
  })
  it('formErrors: none for the defaults, one entry per bad field', () => {
    const d = { ...runDetail([]).run.params, maxWorkers: 2, pools: [], coldStartMode: 'natural' as const, budgetCapUsd: 2, limits: LIMITS }
    const f = formFromDefaults(d)
    expect(formErrors(f, LIMITS, 2)).toEqual({})
    const bad = { ...f, levels: '1, 2, 3, 4', nums: { ...f.nums, maxWorkers: '4', budgetUsd: '3', warmRequests: '' } }
    expect(formErrors(bad, LIMITS, 2)).toEqual({ maxWorkers: '1 to 3', budgetUsd: 'at most $2.00', warmRequests: 'required', levels: '1 to 3 levels' })
  })
  it('rangeHelp: the sentence each field\'s help ends with, from limits', () => {
    expect(rangeHelp('maxWorkers', LIMITS)).toBe('1 to 3.')
    expect(rangeHelp('maxWorkers', { ...LIMITS, maxWorkers: [1, 60] })).toBe('1 to 60.')
    expect(rangeHelp('matmulN', LIMITS)).toBe('Up to 8192.')
    expect(rangeHelp('budgetUsd', LIMITS)).toBe('')
    expect(rangeHelp('coldStarts', {})).toBe('')
  })
})

describe('form', () => {
  const defaults = { ...runDetail([]).run.params, pools: [], coldStartMode: 'natural' as const, budgetCapUsd: 2, limits: LIMITS }

  it('parseNumberList', () => {
    expect(parseNumberList('1, 3,8')).toEqual([1, 3, 8])
    expect(parseNumberList('1, 3,')).toEqual([1, 3])
    expect(parseNumberList('1, x')).toBeNull()
    expect(parseNumberList(' ')).toBeNull()
  })
  it('round-trips the defaults without budgetCapUsd and limits (the body is exactly Params)', () => {
    const { budgetCapUsd: _, limits: _l, ...params } = defaults
    expect(toParams(formFromDefaults(defaults))).toEqual(params)
  })
  it('is null while a number is blank', () => {
    const f = formFromDefaults(defaults)
    expect(toParams({ ...f, nums: { ...f.nums, maxWorkers: '' } })).toBeNull()
    expect(toParams({ ...f, levels: '1, two' })).toBeNull()
  })
  it('starts matmulN at 0 when the defaults leave it out, and leaves it out of the body at 0', () => {
    const f = formFromDefaults(defaults)
    expect(f.nums.matmulN).toBe('0')
    expect(toParams(f)).not.toHaveProperty('matmulN')
  })
  it('sends matmulN when it is above 0', () => {
    const f = formFromDefaults(defaults)
    expect(toParams({ ...f, nums: { ...f.nums, matmulN: '512' } })?.matmulN).toBe(512)
  })
  it('is null while matmulN is blank, like any other number', () => {
    const f = formFromDefaults(defaults)
    expect(toParams({ ...f, nums: { ...f.nums, matmulN: '' } })).toBeNull()
  })
})

describe('GPU picker: pool or card mode', () => {
  const defaults = { ...runDetail([]).run.params, pools: [], coldStartMode: 'natural' as const, budgetCapUsd: 2, limits: LIMITS }
  const card = (id: string, name: string, priceHr: number, availability: Card['availability']): Card => ({
    id,
    name,
    memoryGb: 16,
    priceHr,
    availability,
  })
  const catalog: Pool[] = [
    {
      id: 'AMPERE_16',
      gpuNames: ['RTX A4000', 'RTX A4500', 'RTX 2000 Ada'],
      memoryGb: 16,
      pricePerHr: 0.58,
      availability: 'HIGH',
      cards: [
        card('NVIDIA RTX A4000', 'RTX A4000', 0.5, 'HIGH'),
        card('NVIDIA RTX A4500', 'RTX A4500', 0.58, 'MEDIUM'),
        card('NVIDIA RTX 2000 Ada Generation', 'RTX 2000 Ada', 0, 'LOW'),
      ],
    },
    { id: 'AMPERE_24', gpuNames: ['L4'], memoryGb: 24, pricePerHr: 0.75, availability: 'HIGH', cards: [card('NVIDIA L4', 'L4', 0.75, 'HIGH')] },
    { id: 'ADA_80', gpuNames: ['H100'], memoryGb: 80, pricePerHr: 0, availability: '', cards: [card('NVIDIA H100', 'H100', 0, '')] },
  ]

  it('pool rows: one per priced pool, with the cards it may hand out', () => {
    const rows = pickerRows(catalog, 'pool')
    expect(rows.map((r) => r.id)).toEqual(['AMPERE_16', 'AMPERE_24'])
    expect(rows[0]).toEqual({
      id: 'AMPERE_16',
      name: 'AMPERE_16',
      other: 'RTX A4000 · RTX A4500 · RTX 2000 Ada',
      memoryGb: 16,
      priceHr: 0.58,
      availability: 'HIGH',
    })
  })
  it('card rows: one per priced card, with its pool, price and stock', () => {
    const rows = pickerRows(catalog, 'card')
    expect(rows.map((r) => r.id)).toEqual(['NVIDIA RTX A4000', 'NVIDIA RTX A4500', 'NVIDIA L4'])
    expect(rows[1]).toEqual({
      id: 'NVIDIA RTX A4500',
      name: 'RTX A4500',
      other: 'AMPERE_16',
      memoryGb: 16,
      priceHr: 0.58,
      availability: 'MEDIUM',
    })
  })
  it('pool mode sends pools and omits cards', () => {
    const f = { ...formFromDefaults(defaults), pools: ['AMPERE_16'] }
    const body = toParams(f)!
    expect(body.pools).toEqual(['AMPERE_16'])
    expect(body).not.toHaveProperty('cards')
  })
  it('card mode sends cards and omits pools', () => {
    const f = switchGpuMode({ ...formFromDefaults(defaults), pools: ['AMPERE_16'] }, 'card')
    const body = toParams({ ...f, cards: ['NVIDIA RTX A4500'] })!
    expect(body.cards).toEqual(['NVIDIA RTX A4500'])
    expect(body).not.toHaveProperty('pools')
  })
  it('switching mode clears the other selection', () => {
    const f = { ...formFromDefaults(defaults), pools: ['AMPERE_16'] }
    const c = switchGpuMode(f, 'card')
    expect(c.gpuMode).toBe('card')
    expect(c.pools).toEqual([])
    const withCard = { ...c, cards: ['NVIDIA L4'] }
    const p = switchGpuMode(withCard, 'pool')
    expect(p.gpuMode).toBe('pool')
    expect(p.cards).toEqual([])
    expect(switchGpuMode(withCard, 'card')).toBe(withCard) // same mode: no change
  })
  it('toggleGpu: keeps row order, allows at most two', () => {
    const order = ['NVIDIA RTX A4000', 'NVIDIA RTX A4500', 'NVIDIA L4']
    expect(toggleGpu(['NVIDIA L4'], 'NVIDIA RTX A4000', order)).toEqual(['NVIDIA RTX A4000', 'NVIDIA L4'])
    expect(toggleGpu(['NVIDIA RTX A4000', 'NVIDIA L4'], 'NVIDIA RTX A4500', order)).toEqual(['NVIDIA RTX A4000', 'NVIDIA L4'])
    expect(toggleGpu(['NVIDIA RTX A4000', 'NVIDIA L4'], 'NVIDIA L4', order)).toEqual(['NVIDIA RTX A4000'])
  })
  it('estimate equality ignores allowLowStock', () => {
    const body = toParams({ ...formFromDefaults(defaults), pools: ['AMPERE_16'] })!
    expect(estimateKey({ ...body, allowLowStock: true })).toBe(estimateKey(body))
    expect(estimateKey({ ...body, pools: ['AMPERE_24'] })).not.toBe(estimateKey(body))
  })
  it('stockWarning: one line per level, NONE red and LOW amber, each selection once', () => {
    const cfg = (pool: string, availability: Card['availability'], card?: string) => ({ pool, card, availability })
    expect(stockWarning([cfg('AMPERE_16', 'HIGH'), cfg('AMPERE_16', 'HIGH')])).toEqual([])
    expect(stockWarning([cfg('AMPERE_24', 'NONE'), cfg('AMPERE_24', 'NONE')])).toEqual([
      { level: 'NONE', text: 'AMPERE_24 has no stock right now, so Start will be refused.' },
    ])
    expect(stockWarning([cfg('AMPERE_16', 'LOW', 'NVIDIA RTX A4500')])).toEqual([
      {
        level: 'LOW',
        text: 'NVIDIA RTX A4500 is low on stock: the first boot may wait or fail, and Start will ask you to confirm.',
      },
    ])
  })
  it('stockWarning: plural verbs for two selections', () => {
    const cfg = (pool: string, availability: Card['availability']) => ({ pool, availability })
    expect(stockWarning([cfg('AMPERE_16', 'NONE'), cfg('AMPERE_24', 'NONE')])).toEqual([
      { level: 'NONE', text: 'AMPERE_16 and AMPERE_24 have no stock right now, so Start will be refused.' },
    ])
    expect(stockWarning([cfg('AMPERE_16', 'LOW'), cfg('AMPERE_24', 'LOW')])).toEqual([
      {
        level: 'LOW',
        text: 'AMPERE_16 and AMPERE_24 are low on stock: the first boot may wait or fail, and Start will ask you to confirm.',
      },
    ])
  })
})

describe('needsGpuImage', () => {
  const synthetic = 'ghcr.io/josh10111/coldprobe-worker:latest'
  it('is true for a matmul on the synthetic image, under any tag or digest', () => {
    expect(needsGpuImage(512, synthetic)).toBe(true)
    expect(needsGpuImage(512, 'ghcr.io/josh10111/coldprobe-worker')).toBe(true)
    expect(needsGpuImage(512, 'ghcr.io/josh10111/coldprobe-worker:sha-abc1234')).toBe(true)
    expect(needsGpuImage(512, ' ghcr.io/josh10111/coldprobe-worker@sha256:00 ')).toBe(true)
  })
  it('is false with no matmul, on the GPU image, or on any other image', () => {
    expect(needsGpuImage(0, synthetic)).toBe(false)
    expect(needsGpuImage(NaN, synthetic)).toBe(false)
    expect(needsGpuImage(512, GPU_IMAGE)).toBe(false)
    expect(needsGpuImage(512, 'example.com/my-worker:1')).toBe(false)
  })
})

describe('headline numbers', () => {
  it('compares FlashBoot on the first pool; no samples is null', () => {
    const d = runDetail([configDetail(1, 'A', 'OFF', 10_700, 3), configDetail(2, 'A', 'FLASHBOOT'), configDetail(3, 'B', 'OFF', 1, 1)], ['A', 'B'])
    const h = headlineNumbers(d)!
    expect(h.pool).toBe('A')
    expect(h.modes.map((m) => m.coldMs)).toEqual([10_700, null])
  })
  it('lowerIndex: green only when at least 5% lower', () => {
    const n = [5, 5]
    expect(lowerIndex([10_700, 1500], n)).toBe(1)
    expect(lowerIndex([1500, 10_700], n)).toBe(0)
    expect(lowerIndex([502, 501], n)).toBeNull() // noise
    expect(lowerIndex([100, 95], n)).toBe(1) // exactly 5% lower
    expect(lowerIndex([100, 95.1], n)).toBeNull() // just under 5%
    expect(lowerIndex([10_700, null], n)).toBeNull()
    expect(lowerIndex([10_700], [5])).toBeNull()
  })
  it('lowerIndex: no green unless both sides have at least 3 samples', () => {
    expect(MIN_SAMPLES).toBe(3)
    expect(lowerIndex([10_700, 1500], [2, 5])).toBeNull()
    expect(lowerIndex([1500, 10_700], [5, 2])).toBeNull()
    expect(lowerIndex([100, 90], [3, 3])).toBe(1) // a 10% lead on 3 vs 3
    expect(lowerIndex([28_000, 20_000], [1, 1])).toBeNull() // first boot: always one sample
  })
  it('thinSamplesHidWinner: only when the 5% rule would have called it', () => {
    expect(thinSamplesHidWinner([10_700, 1500], [2, 5])).toBe(true)
    expect(thinSamplesHidWinner([502, 501], [2, 5])).toBe(false) // neutral by the 5% rule anyway
    expect(thinSamplesHidWinner([10_700, 1500], [3, 3])).toBe(false) // green
    expect(thinSamplesHidWinner([10_700, null], [2, 0])).toBe(false)
    expect(thinSamplesHidWinner([10_700], [1])).toBe(false)
    expect(THIN_SAMPLES_HELP).toBe(' With fewer than 3 samples no winner is called.')
  })
  it('headline numbers carry each value\'s sample count', () => {
    const off = configDetail(1, 'AMPERE_16', 'OFF', 10_700, 2)
    off.summary = { ...off.summary, warm: { ...off.summary.warm, n: 10 }, completed: 14 }
    const m = headlineNumbers(runDetail([off]))!.modes[0]
    expect([m.coldN, m.warmN, m.completed]).toEqual([2, 10, 14])
  })
  it('coldHeadline', () => {
    expect(coldHeadline(runDetail([configDetail(1, 'AMPERE_16', 'OFF', 10_700, 3)]))).toBe('Cold start (typical) 10.7 s')
    expect(coldHeadline(runDetail([configDetail(1, 'AMPERE_16', 'OFF')]))).toBeNull()
    // Both modes measured: each named on its own, never "X → Y".
    const both = coldHeadline(runDetail([configDetail(1, 'AMPERE_16', 'OFF', 10_700, 3), configDetail(2, 'AMPERE_16', 'FLASHBOOT', 1500, 3)]))
    expect(both).toBe('Cold start 10.7 s without FlashBoot. Cold start 1.5 s with FlashBoot')
    expect(both).not.toContain('→')
  })
})

describe('applyEvent', () => {
  const base = { runId: 'r1', seq: 1, ts: '2026-11-08T10:00:00Z' }
  const job = (id: number): Job => ({
    id, configId: 1, phase: 'cold', level: 0, runpodJobId: '', workerId: '', gpuTypeId: '', status: 'COMPLETED',
    intendedAt: base.ts, sentAt: base.ts, wallMs: 1, delayMs: 1, execMs: 1,
  })
  const live = () => withSnapshot(null, runDetail([configDetail(1, 'AMPERE_16', 'OFF')]))

  it('appends a job once, even if the snapshot already had it', () => {
    const e: RunEvent = { ...base, type: 'job', configId: 1, data: job(7) }
    const once = applyEvent(live(), e)
    expect(applyEvent(once, e).detail.configs[0].jobs).toHaveLength(1)
  })
  it('ignores a config status older than the one it has', () => {
    const done = applyEvent(live(), { ...base, type: 'config_status', configId: 1, data: { status: 'done' } })
    const stale = applyEvent(done, { ...base, type: 'config_status', configId: 1, data: { status: 'tearing_down' } })
    expect(stale.detail.configs[0].config.status).toBe('done')
  })
  it('never moves a finished run back to running', () => {
    const done = applyEvent(live(), { ...base, type: 'run_status', data: { status: 'completed' } })
    expect(applyEvent(done, { ...base, type: 'run_status', data: { status: 'running' } }).detail.run.status).toBe('completed')
  })
  it('keeps the latest log per config as its activity, and the bound from cost', () => {
    let l = applyEvent(live(), { ...base, type: 'log', configId: 1, data: { msg: 'warm: 10 sequential requests' } })
    l = applyEvent(l, { ...base, type: 'cost', data: { boundUsd: 0.4, capUsd: 2 } })
    expect(l.activity[1]).toBe('warm: 10 sequential requests')
    expect(l.bound).toEqual({ boundUsd: 0.4, capUsd: 2 })
    expect(withSnapshot(l, l.detail).activity[1]).toBe('warm: 10 sequential requests')
  })
  it("a new run's snapshot doesn't inherit the previous run's logs or bound", () => {
    let l = applyEvent(live(), { ...base, type: 'log', configId: 1, data: { msg: 'tearing down' } })
    l = applyEvent(l, { ...base, type: 'cost', data: { boundUsd: 0.4, capUsd: 2 } })
    const other = runDetail([])
    other.run = { ...other.run, id: 'r2' }
    const next = withSnapshot(l, other)
    expect(next.logs).toEqual([])
    expect(next.activity).toEqual({})
    expect(next.bound).toBeNull()
  })
})

describe('live view: spec rows filling in', () => {
  const at = (s: number) => Date.parse('2026-11-08T10:00:00Z') + s * 1000
  // The summary stays empty: live rows must come from the jobs.
  const firstBootOnly = () => {
    const cd = configDetail(1, 'AMPERE_16', 'OFF')
    cd.jobs = [fxJob(1, 1, 'first_boot', 0, 9000)]
    return runDetail([cd])
  }

  it('a config with only a first boot: first boot from its job, cold and warm "—", top level not reached', () => {
    const [r] = liveSpecRows(firstBootOnly())
    expect(r.firstBootMs).toBe(8500)
    expect(r.cold).toEqual({ p50: null, n: 0 })
    expect(r.warm).toEqual({ p50: null, n: 0 })
    expect(r.topLevel.reached).toBe(false)
    expect(r.per1k).toBeNull()
  })
  it('cold and warm typical values are nearest-rank medians of completed jobs', () => {
    const d = firstBootOnly()
    d.configs[0].jobs.push(
      fxJob(2, 1, 'cold', 20, 4500), // cold 4000
      fxJob(3, 1, 'cold', 40, 2500), // cold 2000
      fxJob(4, 1, 'cold', 60, 9000, { status: 'FAILED' }),
      fxJob(5, 1, 'warm', 80, 600),
    )
    const [r] = liveSpecRows(d)
    expect(r.cold).toEqual({ p50: 2000, n: 2 })
    expect(r.warm).toEqual({ p50: 600, n: 1 })
  })
  it('a config that is done has its top level, counted from the jobs', () => {
    const d = firstBootOnly()
    d.configs[0].config.status = 'done'
    d.configs[0].summary.levels = [levelSum(8, 1, 0)] // stale on purpose
    const load = (id: number, status: string) => fxJob(id, 1, 'load', 100 + id, 700, { level: 8, status })
    d.configs[0].jobs.push(load(10, 'COMPLETED'), load(11, 'COMPLETED'), load(12, 'CANCELLED'), load(13, 'FAILED'))
    expect(liveSpecRows(d)[0].topLevel).toEqual({ rps: 8, sent: 4, served: 2, leftQueued: 1, failed: 1, reached: true })
  })

  it('liveWords: during the first boot, the time so far in the first boot column', () => {
    const d = runDetail([configDetail(1, 'AMPERE_16', 'OFF')])
    expect(liveWords(d.run.params, d.configs[0], at(52))).toEqual({ firstBoot: 'first boot · 52 s so far' })
  })
  it('liveWords: after the first boot, the cold column says how far the cold phase is', () => {
    const d = firstBootOnly()
    expect(liveWords(d.run.params, d.configs[0], at(60))).toEqual({ cold: 'cold starts · 0/3' })
    d.configs[0].jobs.push(fxJob(2, 1, 'cold', 20, 4500), fxJob(3, 1, 'cold', 40, 2500))
    expect(liveWords(d.run.params, d.configs[0], at(60))).toEqual({ cold: 'cold starts · 2/3' })
  })
  it('liveWords: a forced run adds the step of getting a new worker', () => {
    const d = firstBootOnly()
    d.run.params.coldStartMode = 'forced'
    const w = liveWords(d.run.params, d.configs[0], at(60), 'cold sample 1/3: pausing the endpoint to force a new worker (workers.max 0)')
    expect(w).toEqual({ cold: 'new-worker cold starts · 0/3 · pausing the endpoint…' })
  })
  it('liveWords: every load level reads in the top-level column', () => {
    const d = firstBootOnly()
    const p = d.run.params
    const cd = d.configs[0]
    for (let i = 0; i < p.coldStarts; i++) cd.jobs.push(fxJob(10 + i, 1, 'cold', 20 + i, 4500))
    for (let i = 0; i < p.warmRequests; i++) cd.jobs.push(fxJob(20 + i, 1, 'warm', 40 + i, 600))
    cd.jobs.push(fxJob(40, 1, 'load', 60, 700, { level: 1 }))
    expect(liveWords(p, cd, at(70))).toEqual({ topLevel: 'load 1 req/s · 1/30 completed' })
  })
  it('liveWords: a config tearing down says so in the top-level column', () => {
    const d = firstBootOnly()
    d.configs[0].config.status = 'tearing_down'
    expect(liveWords(d.run.params, d.configs[0], at(70)).topLevel).toBe('tearing down')
  })
  it('liveWords: a config that failed mid-phase names the phase', () => {
    const d = firstBootOnly()
    d.configs[0].config.status = 'failed'
    d.configs[0].jobs.push(fxJob(2, 1, 'cold', 20, 4500))
    expect(liveWords(d.run.params, d.configs[0], at(70))).toEqual({ cold: 'cold starts · failed' })
  })
  it('liveWords: during the warm phase, the warm column counts completed warm requests', () => {
    const d = firstBootOnly()
    const p = d.run.params
    const cd = d.configs[0]
    for (let i = 0; i < p.coldStarts; i++) cd.jobs.push(fxJob(10 + i, 1, 'cold', 20 + i, 4500))
    cd.jobs.push(fxJob(20, 1, 'warm', 40, 600), fxJob(21, 1, 'warm', 41, 600, { status: 'FAILED' }))
    expect(liveWords(p, cd, at(60))).toEqual({ warm: `warm · 1/${p.warmRequests}` })
  })
  it('liveWords: a config that is done has no word', () => {
    const d = firstBootOnly()
    d.configs[0].config.status = 'done'
    expect(liveWords(d.run.params, d.configs[0], at(70))).toEqual({})
  })
  it('liveWords: a cancelled config says its phase was cut short, even before that phase had a job', () => {
    const d = firstBootOnly()
    d.configs[0].config.status = 'cancelled'
    expect(liveWords(d.run.params, d.configs[0], at(70))).toEqual({ cold: 'cold starts · cut short' })
    d.configs[0].jobs.push(fxJob(2, 1, 'cold', 20, 4500))
    expect(liveWords(d.run.params, d.configs[0], at(70))).toEqual({ cold: 'cold starts · cut short' })
  })
  it('liveWords: a later load level reads in the top-level column too', () => {
    const d = firstBootOnly()
    const p = { ...d.run.params, levelsRps: [1, 4] }
    const cd = d.configs[0]
    for (let i = 0; i < p.coldStarts; i++) cd.jobs.push(fxJob(10 + i, 1, 'cold', 20 + i, 4500))
    for (let i = 0; i < p.warmRequests; i++) cd.jobs.push(fxJob(20 + i, 1, 'warm', 40 + i, 600))
    const first = expectedLoadJobs(1, p.levelSeconds)
    for (let i = 0; i < first; i++) cd.jobs.push(fxJob(100 + i, 1, 'load', 60 + i, 700, { level: 1 }))
    cd.jobs.push(fxJob(500, 1, 'load', 200, 700, { level: 4 }), fxJob(501, 1, 'load', 201, 700, { level: 4 }))
    expect(liveWords(p, cd, at(210))).toEqual({ topLevel: `load 4 req/s · 2/${expectedLoadJobs(4, p.levelSeconds)} completed` })
  })
})

describe('compare: one extent for both runs (small multiples)', () => {
  it('takes the larger x and the larger y of the runs', () => {
    expect(sharedExtent([{ xMax: 120, yMax: 8 }, { xMax: 300, yMax: 4 }])).toEqual({ xMax: 300, yMax: 8 })
    expect(sharedExtent([{ xMax: 300, yMax: 4 }, { xMax: 120, yMax: 80 }])).toEqual({ xMax: 300, yMax: 80 })
  })
  it("keeps traceExtent's floors with nothing to draw", () => {
    expect(sharedExtent([])).toEqual({ xMax: 10, yMax: 1 })
  })
})

describe('home: latest results', () => {
  const run = (id: string, status: RunStatus): Run => ({ ...runDetail([]).run, id, status })
  it('picks the newest finished runs, skipping the running one', () => {
    const runs = [run('r5', 'running'), run('r4', 'completed'), run('r3', 'cancelled'), run('r2', 'failed'), run('r1', 'completed')]
    expect(latestResults(runs, 3).map((r) => r.id)).toEqual(['r4', 'r3', 'r2'])
  })
  it('returns fewer when there are fewer', () => {
    expect(latestResults([run('r1', 'completed')], 3).map((r) => r.id)).toEqual(['r1'])
    expect(latestResults([run('r1', 'running')], 3)).toEqual([])
  })
})

describe("home: a running config's phase word", () => {
  const p = runDetail([]).run.params
  const at = (status: ConfigDetail['config']['status'], jobs: Job[] = []) => {
    const cd = configDetail(1, 'AMPERE_16', 'OFF')
    return { ...cd, config: { ...cd.config, status }, jobs }
  }
  it('names the step a running config is on', () => {
    expect(phaseWord(p, at('running'))).toBe('first boot')
    expect(phaseWord(p, at('running', [fxJob(1, 1, 'first_boot', 0, 9000), fxJob(2, 1, 'cold', 30, 8000)]))).toBe('cold starts')
  })
  it('says what happens around the steps, and the final status', () => {
    expect(phaseWord(p, at('pending'))).toBe('waiting')
    expect(phaseWord(p, at('creating'))).toBe('creating the endpoint')
    expect(phaseWord(p, at('tearing_down'))).toBe('deleting the endpoint')
    expect(phaseWord(p, at('done'))).toBe('done')
    expect(phaseWord(p, at('failed'))).toBe('failed')
    expect(phaseWord(p, at('cancelled'))).toBe('cancelled')
  })
})
