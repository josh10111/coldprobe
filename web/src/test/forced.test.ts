import { describe, expect, it } from 'vitest'
import { buildSeries, coldGroupName, modesDiffer, phaseMarks } from '../chartData'
import { coldNote } from '../coldNote'
import { coldName, isForced, modeWord } from '../format'
import { formFromDefaults, toParams } from '../form'
import { coldHeadline } from '../headlineToast'
import { forcedHint, phaseSteps } from '../phases'
import { startedToast, toastFor } from '../toasts'
import type { ConfigDetail, ConfigStatus, FlashBoot, Params, RunDetail } from '../types'
import { configDetail, job, runDetail } from './fixtures'

const forced = (d: RunDetail): RunDetail => ({ ...d, run: { ...d.run, params: { ...d.run.params, coldStartMode: 'forced' } } })

function config(id: number, fb: FlashBoot, coldP50: number, coldTaken: number, status: ConfigStatus = 'done'): ConfigDetail {
  const c = configDetail(id, 'AMPERE_16', fb, coldP50, coldTaken)
  c.config = { ...c.config, status }
  c.summary = { ...c.summary, completed: 5 }
  c.jobs = [
    job(id * 100, id, 'first_boot', 0, 28_500),
    ...Array.from({ length: coldTaken }, (_, i) => job(id * 100 + 1 + i, id, 'cold', 100 * (i + 1), coldP50 + 500)),
    job(id * 100 + 50, id, 'warm', 400, 500),
  ]
  return c
}

describe('recognising a forced run', () => {
  const p = runDetail([]).run.params
  it('only "forced" is forced; absent (older runs) and "natural" are natural', () => {
    expect(isForced({ ...p, coldStartMode: 'forced' })).toBe(true)
    expect(isForced({ ...p, coldStartMode: 'natural' })).toBe(false)
    expect(isForced(p)).toBe(false)
  })
  it('forced samples are called new-worker cold starts', () => {
    expect(coldName({ ...p, coldStartMode: 'forced' })).toBe('new-worker cold start')
    expect(coldName(p)).toBe('cold start')
  })
})

describe('the New run form', () => {
  const defaults = { ...runDetail([]).run.params, pools: ['AMPERE_16'], coldStartMode: 'natural' as const, budgetCapUsd: 2, limits: {} }
  it('starts natural and sends the mode', () => {
    const f = formFromDefaults(defaults)
    expect(f.coldStartMode).toBe('natural')
    expect(toParams({ ...f, coldStartMode: 'forced' })?.coldStartMode).toBe('forced')
  })
  it('defaults without the field (an older server) mean natural', () => {
    const { coldStartMode: _, ...old } = defaults
    expect(formFromDefaults(old).coldStartMode).toBe('natural')
  })
  it('changing the mode changes the body, so it clears the estimate gate', () => {
    const f = formFromDefaults(defaults)
    expect(JSON.stringify(toParams({ ...f, coldStartMode: 'forced' }))).not.toBe(JSON.stringify(toParams(f)))
  })
})

describe('forced labelling', () => {
  const d = forced(runDetail([config(1, 'OFF', 72_000, 3), config(2, 'FLASHBOOT', 71_000, 3)]))
  d.run = { ...d.run, status: 'completed', finishedAt: '2026-11-08T10:20:00Z' }

  it('the headline lists both values and says FlashBoot can\'t help, never "off → on"', () => {
    expect(coldHeadline(d)).toBe("New-worker cold start 72 s off / 71 s on (FlashBoot can't help a new worker)")
  })
  it('the run-finished toast opens with the finding, which never mentions FlashBoot', () => {
    const body = toastFor({ type: 'run_finished', detail: d, ts: '' }, { configs: [], shown: new Set() })?.body
    expect(body).toBe('On AMPERE_16 no first boot completed, a cold start typically took 72 s and no warm request completed; it did not reach 8 req/s.')
    expect(body).not.toContain('FlashBoot')
  })
  it('one FlashBoot mode', () => {
    const one = forced(runDetail([config(2, 'FLASHBOOT', 75_000, 3)]))
    expect(coldHeadline(one)).toBe('New-worker cold start (typical) 75 s')
  })
  it('one mode without a sample is left out; none at all is null', () => {
    const part = forced(runDetail([config(1, 'OFF', 72_000, 3), config(2, 'FLASHBOOT', 0, 0)]))
    expect(coldHeadline(part)).toBe("New-worker cold start 72 s off (FlashBoot can't help a new worker)")
    expect(coldHeadline(forced(runDetail([config(1, 'OFF', 0, 0)])))).toBeNull()
  })
  it('the started toast says forced', () => {
    const p: Params = { ...d.run.params, pools: ['AMPERE_16'] }
    expect(startedToast(p, 1.5).body).toBe('2 configs · AMPERE_16 · forced cold starts · may spend up to $1.50')
  })
  it('timeline separators say new-worker cold starts', () => {
    expect(phaseMarks(d).map((m) => m.label)).toContain('new-worker cold starts')
  })
  it('chart groups: new-worker when all runs are forced, both words when a comparison mixes modes', () => {
    const natural = runDetail([config(9, 'OFF', 8000, 3)])
    expect(coldGroupName([{ detail: d }])).toBe('new-worker cold start')
    expect(coldGroupName([{ detail: natural }])).toBe('cold start')
    expect(coldGroupName([{ detail: natural }, { detail: d }])).toBe('cold start / new-worker')
  })
  it('in compare every series names its run\'s mode', () => {
    const natural = runDetail([config(9, 'OFF', 8000, 3)])
    const s = buildSeries([
      { detail: natural, prefix: 'Nov 8 · 10:00' },
      { detail: d, prefix: 'Nov 8 · 11:00' },
    ])
    expect(s.map((x) => x.label)).toEqual([
      'Nov 8 · 10:00 · natural · AMPERE_16 FlashBoot off',
      'Nov 8 · 11:00 · forced · AMPERE_16 FlashBoot off',
      'Nov 8 · 11:00 · forced · AMPERE_16 FlashBoot on',
    ])
    // The legend and table show the name plus the mode as a badge.
    expect(s.map((x) => [x.name, x.mode])).toEqual([
      ['Nov 8 · 10:00 · AMPERE_16 FlashBoot off', 'natural'],
      ['Nov 8 · 11:00 · AMPERE_16 FlashBoot off', 'forced'],
      ['Nov 8 · 11:00 · AMPERE_16 FlashBoot on', 'forced'],
    ])
  })
  it('a single run\'s series carry no mode (the header badge says it)', () => {
    expect(buildSeries([{ detail: d }])[0]).toMatchObject({ label: 'AMPERE_16 FlashBoot off', name: 'AMPERE_16 FlashBoot off', mode: null })
  })
  it('badge text: "forced" only for forced, "natural" otherwise (including older runs without the field)', () => {
    const p = runDetail([]).run.params
    expect(modeWord({ ...p, coldStartMode: 'forced' })).toBe('forced')
    expect(modeWord({ ...p, coldStartMode: 'natural' })).toBe('natural')
    expect(modeWord(p)).toBe('natural')
  })
  it('modesDiffer: true for natural vs forced, false when the modes match', () => {
    const natural = runDetail([]).run.params
    const f = forced(runDetail([])).run.params
    expect(modesDiffer([natural, f])).toBe(true)
    expect(modesDiffer([f, natural])).toBe(true)
    expect(modesDiffer([natural, natural])).toBe(false)
    expect(modesDiffer([f, f])).toBe(false)
    expect(modesDiffer([{ ...natural, coldStartMode: 'natural' }, natural])).toBe(false) // absent = natural
  })
})

describe('kept warm never applies to forced runs', () => {
  it('no cold note, even with fewer samples than asked', () => {
    const d = forced(runDetail([config(2, 'FLASHBOOT', 0, 0)]))
    expect(coldNote(d.run.params, d.configs[0])).toBeNull()
  })
  it('the checklist still shows a skipped forced phase as short, with its count, not green', () => {
    const d = forced(runDetail([config(2, 'FLASHBOOT', 71_000, 1)]))
    const step = phaseSteps(d.run.params, d.configs[0])[1]
    expect(step).toEqual({ label: 'new-worker cold starts', detail: '1/3 · typical 71 s', state: 'short' })
  })
  it('a config that failed on a restore keeps its failed state and error', () => {
    const d = forced(runDetail([config(2, 'FLASHBOOT', 0, 0, 'failed')]))
    d.configs[0].config.error = 'cold sample 1/3: could not restore workers.max to 2 after pausing the endpoint'
    d.configs[0].jobs = d.configs[0].jobs.filter((j) => j.phase === 'first_boot')
    expect(phaseSteps(d.run.params, d.configs[0]).map((s) => s.state)).toEqual(['done', 'failed', 'unreached', 'unreached', 'unreached', 'unreached'])
  })
})

describe('live checklist hints while getting a new worker', () => {
  it('turns the forced log lines into short hints', () => {
    expect(forcedHint('cold sample 2/3: pausing the endpoint to force a new worker (workers.max 0)')).toBe('pausing the endpoint…')
    expect(forcedHint('cold sample 2/3: endpoint paused, waiting for workers to scale to zero')).toBe(
      'endpoint paused, waiting for workers to stop…',
    )
    expect(forcedHint('cold sample 2/3: restoring workers.max to 2')).toBe('restoring the endpoint…')
    expect(forcedHint('cold sample 2/3: sending')).toBeNull()
    expect(forcedHint(undefined)).toBeNull()
  })
  it('the current cold step shows the hint', () => {
    const d = forced(runDetail([config(2, 'FLASHBOOT', 71_000, 1, 'running')]))
    d.configs[0].jobs = d.configs[0].jobs.filter((j) => j.phase !== 'warm')
    const step = phaseSteps(d.run.params, d.configs[0], 'cold sample 2/3: pausing the endpoint to force a new worker (workers.max 0)')[1]
    expect(step).toEqual({ label: 'new-worker cold starts', detail: '1/3 · typical 71 s · pausing the endpoint…', state: 'current' })
  })
})
