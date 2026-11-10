import { describe, expect, it } from 'vitest'
import { coldNote } from '../coldNote'
import { coldHeadline, headlineNumbers } from '../headlineToast'
import { phaseSteps } from '../phases'
import { toastFor } from '../toasts'
import type { ConfigDetail, ConfigStatus, FlashBoot } from '../types'
import { configDetail, job, runDetail } from './fixtures'

// params.coldStarts is 3 in the fixtures.

function config(id: number, fb: FlashBoot, coldTaken: number, status: ConfigStatus = 'done', coldP50 = 0): ConfigDetail {
  const c = configDetail(id, 'AMPERE_16', fb, coldP50, coldTaken)
  c.config = { ...c.config, status }
  c.summary = { ...c.summary, completed: 2 + coldTaken, costPer1k: 0.5 }
  c.jobs = [
    job(id * 100, id, 'first_boot', 0, 28_500),
    ...Array.from({ length: coldTaken }, (_, i) => job(id * 100 + 1 + i, id, 'cold', 40 + i, coldP50 + 500)),
    job(id * 100 + 50, id, 'warm', 300, 500),
  ]
  return c
}

describe('coldNote', () => {
  const p = runDetail([]).run.params

  it('FlashBoot with no cold sample: kept warm', () => {
    expect(coldNote(p, config(2, 'FLASHBOOT', 0))).toMatchObject({
      taken: 0,
      requested: 3,
      short: 'kept warm',
      text: 'kept warm: no cold start within 3 min',
    })
  })
  it('FlashBoot with some samples: "2 of 3 (kept warm after that)"', () => {
    expect(coldNote(p, config(2, 'FLASHBOOT', 2))?.text).toBe('2 of 3 (kept warm after that)')
  })
  it('OFF says the workers stayed up past the wait', () => {
    expect(coldNote(p, config(1, 'OFF', 0))).toMatchObject({
      short: 'no scale-down',
      text: 'no cold start: workers stayed up past the 3 min wait',
    })
    expect(coldNote(p, config(1, 'OFF', 1))?.text).toBe('1 of 3 (then workers stayed up past the 3 min wait)')
  })
  it('the "?" explains it, and points at forced mode', () => {
    expect(coldNote(p, config(2, 'FLASHBOOT', 0))?.help).toBe(
      'FlashBoot kept the worker that served the last request running, so there was no cold start to measure. coldprobe only counts a request as cold after every worker has stopped. Forced mode avoids this by pausing the endpoint.',
    )
    expect(coldNote(p, config(1, 'OFF', 0))?.help).toBe(
      "With FlashBoot off, Runpod still kept the worker up past coldprobe's 3-minute wait, so a new request would have hit a warm worker. coldprobe only counts a request as cold after every worker has stopped, so it skipped the remaining samples. Forced mode avoids this by pausing the endpoint.",
    )
  })
  it('no note when every sample was taken', () => {
    expect(coldNote(p, config(2, 'FLASHBOOT', 3))).toBeNull()
  })
  it('no note for a config cut short by a cancel or a failure: its status tells that story', () => {
    expect(coldNote(p, config(2, 'FLASHBOOT', 0, 'cancelled'))).toBeNull()
    expect(coldNote(p, config(2, 'FLASHBOOT', 0, 'failed'))).toBeNull()
  })
  it('while running: only once a later phase has jobs (before that, sampling may still be going on)', () => {
    const c = config(2, 'FLASHBOOT', 1, 'running')
    expect(coldNote(p, c)?.text).toBe('1 of 3 (kept warm after that)')
    expect(coldNote(p, { ...c, jobs: c.jobs.filter((j) => j.phase !== 'warm') })).toBeNull()
  })
  it('a failed cold sample was still taken, so it is not "kept warm"', () => {
    const c = config(2, 'FLASHBOOT', 3)
    c.jobs[1] = { ...c.jobs[1], status: 'TIMED_OUT' }
    expect(coldNote(p, c)).toBeNull()
  })
})

describe('kept warm in the checklist, headline and toast', () => {
  const kept = runDetail([config(1, 'OFF', 3, 'done', 8000), config(2, 'FLASHBOOT', 0)])
  kept.run = { ...kept.run, status: 'completed', finishedAt: '2026-11-08T10:10:00Z' }

  it('the live checklist shows the note, never a blank green done', () => {
    const step = phaseSteps(kept.run.params, kept.configs[1])[1]
    expect(step).toMatchObject({ state: 'short', detail: 'kept warm: no cold start within 3 min' })
  })
  it('tiles carry the note for the mode that has one', () => {
    const h = headlineNumbers(kept)!
    expect(h.modes.map((m) => m.coldMs)).toEqual([8000, null])
    expect(h.modes.map((m) => m.coldNote?.short ?? null)).toEqual([null, 'kept warm'])
  })
  it('the headline claims no FlashBoot speed-up when FlashBoot had no cold start', () => {
    expect(coldHeadline(kept)).toBe('Cold start 8.0 s without FlashBoot. FlashBoot kept a worker warm (no cold start to measure)')
  })
  it('the run-finished toast opens with the finding instead, which never claims a FlashBoot speed-up', () => {
    const t = toastFor({ type: 'run_finished', detail: kept, ts: '' }, { configs: [], shown: new Set() })
    expect(t?.body).toBe('On AMPERE_16 no first boot completed, a cold start typically took 8.0 s and no warm request completed; it did not reach 8 req/s.')
    expect(t?.body).not.toContain('FlashBoot')
  })
  it('both modes without a cold start', () => {
    const d = runDetail([config(1, 'OFF', 0), config(2, 'FLASHBOOT', 0)])
    expect(coldHeadline(d)).toBe(
      'Workers stayed up past the 3 min wait, so fewer or no cold starts were measured. FlashBoot kept a worker warm (no cold start to measure)',
    )
  })
  it('OFF kept up, FlashBoot measured', () => {
    const d = runDetail([config(1, 'OFF', 0), config(2, 'FLASHBOOT', 2, 'done', 1500)])
    expect(coldHeadline(d)).toBe('Cold start 1.5 s with FlashBoot. Workers stayed up past the 3 min wait, so fewer or no cold starts were measured')
  })
  it('one FlashBoot mode, kept warm', () => {
    const d = runDetail([config(2, 'FLASHBOOT', 0)])
    expect(coldHeadline(d)).toBe('FlashBoot kept a worker warm (no cold start to measure)')
  })
  it('$/1k is null when only one request completed (the first boot alone)', () => {
    const c = config(1, 'OFF', 0)
    c.summary = { ...c.summary, completed: 1, costPer1k: 5.94 }
    expect(headlineNumbers(runDetail([c]))!.modes[0].per1kUsd).toBeNull()
  })
})
