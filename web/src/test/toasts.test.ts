import { describe, expect, it } from 'vitest'
import {
  cancelRequestedToast,
  dismissKeysFor,
  errorToast,
  isLowStock,
  loadShownKeys,
  lostToast,
  missedConfigToasts,
  reconnectedToast,
  saveShownKeys,
  startedToast,
  startFailedToast,
  toastFor,
  type ToastState,
} from '../toasts'
import { ApiError } from '../api'
import type { Job, RunEvent } from '../types'
import { configDetail, runDetail } from './fixtures'

const detail = runDetail([configDetail(1, 'AMPERE_16', 'OFF', 10700, 3), configDetail(2, 'AMPERE_16', 'FLASHBOOT', 1500, 3)])
const state: ToastState = { configs: detail.configs.map((c) => c.config), shown: new Set() }
const base = { runId: 'r1', seq: 5, ts: '2026-11-08T10:05:00Z' }

describe('rule 1: run started / cancel requested', () => {
  it('run started is info, auto-dismissed, and says what it runs', () => {
    const t = startedToast(detail.run.params, 1.42)
    expect(t).toMatchObject({ tone: 'info', sticky: false, title: 'Run started' })
    expect(t.body).toBe('2 configs · AMPERE_16 · may spend up to $1.42')
  })
  it('cancel requested is info, auto-dismissed', () => {
    expect(cancelRequestedToast()).toMatchObject({ tone: 'info', sticky: false, title: 'Cancel requested' })
  })
})

describe('rule 2: each config finished', () => {
  const ev = (status: string, detail?: string): RunEvent =>
    ({ ...base, type: 'config_status', configId: 1, data: { status, detail } }) as RunEvent

  it('done is green and auto-dismissed', () => {
    expect(toastFor(ev('done'), state)).toMatchObject({ tone: 'done', sticky: false, title: 'AMPERE_16 FlashBoot off done' })
  })
  it('cancelled is amber and auto-dismissed', () => {
    expect(toastFor(ev('cancelled', 'run cancelled'), state)).toMatchObject({ tone: 'warn', sticky: false, body: 'run cancelled' })
  })
  it('failed is red, sticky, and shows the reason', () => {
    expect(toastFor(ev('failed', 'no stock'), state)).toMatchObject({ tone: 'bad', sticky: true, body: 'no stock' })
  })
  it('non-final statuses (phase progress) are not toasted', () => {
    for (const s of ['pending', 'creating', 'running', 'tearing_down']) expect(toastFor(ev(s), state)).toBeNull()
  })
  it('an unknown config is named by its id', () => {
    const e = { ...base, type: 'config_status', configId: 9, data: { status: 'done' } } as RunEvent
    expect(toastFor(e, state)?.title).toBe('config 9 done')
  })
  it('the same config result is not toasted twice', () => {
    const t = toastFor(ev('done'), state)!
    expect(toastFor(ev('done'), { ...state, shown: new Set([t.key!]) })).toBeNull()
  })
})

describe('rule 3: run finished', () => {
  const finished = (status: string, error?: string) =>
    toastFor(
      {
        type: 'run_finished',
        detail: { ...detail, run: { ...detail.run, status: status as never, error, finishedAt: '2026-11-08T10:11:02Z' } },
        ts: '2026-11-08T10:12:00Z',
      },
      state,
    )

  it('completed is sticky, green, with the finding\'s first sentence and a report link', () => {
    expect(finished('completed')).toEqual({
      tone: 'done',
      title: 'Run completed · 11m 02s',
      // The finding's first sentence.
      body: 'On AMPERE_16 no first boot completed, a cold start typically took 10.7 s and no warm request completed; it did not reach 8 req/s.',
      sticky: true,
      link: { to: '/runs/r1', label: 'View report' },
      key: 'final',
    })
  })
  it('completed with no configs to describe falls back to the cold-start line', () => {
    const t = toastFor({ type: 'run_finished', detail: { ...detail, configs: [], run: { ...detail.run, status: 'completed' } }, ts: '' }, state)
    expect(t?.body).toBe('No cold start completed.')
  })
  it('no toast body has an arrow', () => {
    expect(finished('completed')?.body).not.toContain('→')
  })
  it('cancelled and budget-stopped are amber and say why', () => {
    expect(finished('cancelled', 'run cancelled')).toMatchObject({ tone: 'warn', sticky: true, body: 'run cancelled' })
    expect(finished('aborted_budget', 'budget would be exceeded')).toMatchObject({
      tone: 'warn',
      title: 'Run budget-stopped · 11m 02s',
    })
  })
  it('failed and crashed are red', () => {
    expect(finished('failed', 'every config failed')).toMatchObject({ tone: 'bad', body: 'every config failed' })
    expect(finished('aborted_crash')).toMatchObject({ tone: 'bad', title: 'Run crashed · 11m 02s', body: 'coldprobe stopped mid-run.' })
  })
  it('the raw run_status event itself is not toasted (it waits for the snapshot)', () => {
    expect(toastFor({ ...base, type: 'run_status', data: { status: 'completed' } }, state)).toBeNull()
  })
})

describe('rule 5: budget warning', () => {
  const cost = (boundUsd: number): RunEvent => ({ ...base, type: 'cost', data: { boundUsd, capUsd: 2 } })

  it('does not fire at or below 80% of the cap', () => {
    expect(toastFor(cost(1.0), state)).toBeNull()
    expect(toastFor(cost(1.6), state)).toBeNull()
  })
  it('fires amber once the bound passes 80%', () => {
    expect(toastFor(cost(1.61), state)).toMatchObject({ tone: 'warn', key: 'budget' })
  })
  it('fires only once per run', () => {
    const shown = new Set<string>()
    let count = 0
    for (const b of [1.61, 1.7, 1.9]) {
      const t = toastFor(cost(b), { ...state, shown })
      if (t) {
        count++
        shown.add(t.key!)
      }
    }
    expect(count).toBe(1)
  })
})

describe('phase changes and jobs are not toasted', () => {
  it('log events (phase changes) return null', () => {
    expect(toastFor({ ...base, type: 'log', configId: 1, data: { msg: 'warm: 10 sequential requests' } }, state)).toBeNull()
    expect(toastFor({ ...base, type: 'log', data: { msg: 'ENDPOINT NOT DELETED', level: 'error' } }, state)).toBeNull()
  })
  it('job events return null', () => {
    const job = { id: 1, configId: 1, phase: 'cold', status: 'COMPLETED' } as Job
    expect(toastFor({ ...base, type: 'job', configId: 1, data: job }, state)).toBeNull()
  })
})

describe('rule 7: no worker placed during the first boot', () => {
  const msg = 'No worker placed on AMPERE_16 after 90 s. The pool may be out of stock; Cancel stops the run.'
  const warn: RunEvent = { ...base, type: 'log', configId: 1, data: { msg, level: 'warn' } }

  it('warn log event makes a sticky amber toast keyed by config', () => {
    expect(toastFor(warn, state)).toEqual({
      tone: 'warn',
      title: 'AMPERE_16 FlashBoot off has no worker yet',
      body: msg,
      sticky: true,
      key: 'warn:1',
    })
  })
  it('is shown once per config', () => {
    expect(toastFor(warn, { ...state, shown: new Set(['warn:1']) })).toBeNull()
  })
  it('a warn log without a config is not toasted', () => {
    expect(toastFor({ ...base, type: 'log', data: { msg, level: 'warn' } }, state)).toBeNull()
  })

  const open = ['warn:1', 'warn:2', 'budget']
  it('config leaving first boot dismisses it', () => {
    // A first-boot job finishing with any status means a worker was placed.
    const firstBoot = { id: 1, configId: 1, phase: 'first_boot', status: 'COMPLETED' } as Job
    expect(dismissKeysFor({ ...base, type: 'job', configId: 1, data: firstBoot }, open)).toEqual(['warn:1'])
    const failed = { ...firstBoot, status: 'TIMED_OUT' } as Job
    expect(dismissKeysFor({ ...base, type: 'job', configId: 1, data: failed }, open)).toEqual(['warn:1'])
    // So does the config ending before its first boot did.
    for (const status of ['tearing_down', 'done', 'failed', 'cancelled'] as const) {
      expect(dismissKeysFor({ ...base, type: 'config_status', configId: 1, data: { status } }, open)).toEqual(['warn:1'])
    }
  })
  it('the run ending dismisses every config\'s warning', () => {
    expect(dismissKeysFor({ ...base, type: 'run_status', data: { status: 'cancelled' } }, open)).toEqual(['warn:1', 'warn:2'])
  })
  it('anything else dismisses nothing', () => {
    const cold = { id: 2, configId: 1, phase: 'cold', status: 'COMPLETED' } as Job
    expect(dismissKeysFor({ ...base, type: 'job', configId: 1, data: cold }, open)).toEqual([])
    expect(dismissKeysFor({ ...base, type: 'config_status', configId: 1, data: { status: 'running' } }, open)).toEqual([])
    expect(dismissKeysFor({ ...base, type: 'run_status', data: { status: 'running' } }, open)).toEqual([])
    expect(dismissKeysFor({ ...base, type: 'config_status', configId: 3, data: { status: 'done' } }, open)).toEqual([])
  })
})

describe('rule 6: problems', () => {
  it('API errors are red, show the problem detail, and go after 8 s', () => {
    const t = errorToast('Start failed', new ApiError(409, 'a run is already in progress; wait for it to finish or cancel it'))
    expect(t).toMatchObject({
      tone: 'bad',
      title: 'Start failed',
      body: 'a run is already in progress; wait for it to finish or cancel it',
      sticky: false,
      ms: 8000,
    })
  })
  it('server unreachable is red and sticky; reconnected is green and auto-dismissed after the default 5 s', () => {
    expect(lostToast()).toMatchObject({ tone: 'bad', sticky: true })
    const r = reconnectedToast()
    expect(r).toMatchObject({ tone: 'done', sticky: false })
    expect(r.ms).toBeUndefined()
  })
})

describe('catching up after a reconnect or gap', () => {
  const ended = (status: string, error?: string) => ({ ...detail.configs[0].config, status, error }) as never
  it('toasts configs that finished while events were lost', () => {
    const d = { ...detail, configs: [{ ...detail.configs[0], config: ended('failed', 'no stock') }, detail.configs[1]] }
    expect(missedConfigToasts(d, new Set())).toEqual([
      { tone: 'bad', title: 'AMPERE_16 FlashBoot off failed', body: 'no stock', sticky: true, key: 'config:1:failed' },
    ])
  })
  it('skips the ones already toasted', () => {
    const d = { ...detail, configs: [{ ...detail.configs[0], config: ended('done') }] }
    expect(missedConfigToasts(d, new Set(['config:1:done']))).toEqual([])
  })
})

describe('toast keys survive a reload (sessionStorage)', () => {
  const fakeStore = () => {
    const m = new Map<string, string>()
    return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) }
  }
  it('round-trips per run', () => {
    const store = fakeStore()
    saveShownKeys(store, 'r1', new Set(['budget']))
    expect(loadShownKeys(store, 'r1').has('budget')).toBe(true)
    expect(loadShownKeys(store, 'r2').size).toBe(0)
  })
  it('so the budget warning stays once per run across a reload', () => {
    const store = fakeStore()
    saveShownKeys(store, 'r1', new Set(['budget']))
    const cost: RunEvent = { ...base, type: 'cost', data: { boundUsd: 1.9, capUsd: 2 } }
    expect(toastFor(cost, { ...state, shown: loadShownKeys(store, 'r1') })).toBeNull()
  })
  it('works without storage, or with storage that throws', () => {
    expect(loadShownKeys(undefined, 'r1').size).toBe(0)
    const broken = { getItem: () => { throw new Error('blocked') }, setItem: () => { throw new Error('blocked') } }
    expect(loadShownKeys(broken, 'r1').size).toBe(0)
    expect(() => saveShownKeys(broken, 'r1', new Set(['x']))).not.toThrow()
  })
})

describe('the stock gate on Start', () => {
  it('low stock: sticky red with the detail and no action ("Start anyway" is beside Start in the form)', () => {
    const err = new ApiError(400, 'AMPERE_24 is low on stock; start anyway to accept the risk', 'low_stock')
    const t = startFailedToast(err)
    expect(t).toMatchObject({ tone: 'bad', sticky: true, title: 'Start refused', body: err.message, key: 'start-refused' })
    expect(t.action).toBeUndefined()
    expect(isLowStock(err)).toBe(true)
  })
  it('isLowStock is only the low_stock 400', () => {
    expect(isLowStock(new ApiError(400, 'x', 'no_stock'))).toBe(false)
    expect(isLowStock(new ApiError(409, 'x', 'low_stock'))).toBe(false)
    expect(isLowStock(new Error('low_stock'))).toBe(false)
  })
  it('no stock (code no_stock): sticky red with the detail, no action', () => {
    const t = startFailedToast(new ApiError(400, 'NVIDIA L4 is out, try later', 'no_stock'))
    expect(t).toMatchObject({ tone: 'bad', sticky: true, title: 'Start refused', body: 'NVIDIA L4 is out, try later', key: 'start-refused' })
    expect(t.action).toBeUndefined()
  })
  it('no stock without a code (older server): matched on the detail', () => {
    const t = startFailedToast(new ApiError(400, 'AMPERE_24 has no stock right now'))
    expect(t).toMatchObject({ tone: 'bad', sticky: true, title: 'Start refused', body: 'AMPERE_24 has no stock right now', key: 'start-refused' })
    expect(t.action).toBeUndefined()
  })
  it('any other failure is the ordinary 8 s error toast', () => {
    const t = startFailedToast(new ApiError(409, 'a run is already in progress'))
    expect(t).toEqual(errorToast('Start failed', new ApiError(409, 'a run is already in progress')))
  })
  it('startedToast names the cards in card mode', () => {
    const { pools: _, ...rest } = detail.run.params
    const t = startedToast({ ...rest, cards: ['NVIDIA RTX A4500'] }, 1)
    expect(t.body).toBe('2 configs · NVIDIA RTX A4500 · may spend up to $1.00')
  })
})
