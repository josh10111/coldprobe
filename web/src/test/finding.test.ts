import { describe, expect, it } from 'vitest'
import { finding, flashbootReadouts } from '../finding'
import { specRows, type SpecRow } from '../specRows'
import { cardMode, levelSum, naturalTwoPool, singleConfig } from './fixtures'
import type { RunDetail } from '../types'

// The two FlashBoot-off rows of naturalTwoPool, edited per test.
function offRows(edit?: (d: RunDetail) => void): SpecRow[] {
  const d = naturalTwoPool()
  edit?.(d)
  return specRows(d)
}
const COMPARISON = /better|faster|cheaper|alike|against|than|same|less|more|compare/i

describe('finding: served all vs left queued (rule 1)', () => {
  it('tonight: AMPERE_24 served all at 8 req/s, AMPERE_16 left 90 queued; $/1k within 15%', () => {
    expect(finding(offRows(), 'pool')).toEqual({
      sentences: [
        'AMPERE_24 is the better buy for this job: at 8 req/s it served all 240 requests, while AMPERE_16 left 90 of 240 queued.',
        'It costs about the same per request and waits less (typical warm 674 ms against 1.1 s).',
      ],
      help: "Compares AMPERE_16 FlashBoot off and AMPERE_24 FlashBoot off by the requests each left queued at the top load level, 8 req/s.",
    })
  })
  it('$/1k more than 15% apart: says which costs more or less per 1k', () => {
    const dearer = offRows((d) => (d.configs[2].summary.costPer1k = 0.3))
    expect(finding(dearer, 'pool')!.sentences[1]).toBe('It costs more per 1k requests ($0.30 against $0.17); typical warm 674 ms against 1.1 s.')
    const cheaper = offRows((d) => (d.configs[2].summary.costPer1k = 0.1))
    expect(finding(cheaper, 'pool')!.sentences[1]).toBe('It is also cheaper per 1k requests ($0.10 against $0.17); typical warm 674 ms against 1.1 s.')
  })
  it('a row with failed requests at the top level is not the better buy, even with none left queued', () => {
    const f = finding(
      offRows((d) => {
        d.configs[0].summary.levels = [levelSum(8, 240, 10)]
        d.configs[2].summary.levels = [levelSum(8, 240, 0, 120)] // 120 served, 120 failed, 0 queued
      }),
      'pool',
    )!
    expect(f.sentences[0]).not.toMatch(/better buy/)
    expect(f.sentences[0]).toMatch(/^AMPERE_24 \(it ran on .*\) is faster for this job/) // rule 2
  })
  it('a pool cancelled mid-level has not reached the top level: rule 1 does not fire', () => {
    const rows = offRows((d) => (d.configs[2].config = { ...d.configs[2].config, status: 'cancelled' }))
    expect(finding(rows, 'pool')!.sentences[0]).not.toMatch(/better buy/)
  })
  it('a pool that never reached the top level is neither: rule 1 does not fire', () => {
    const rows = offRows((d) => (d.configs[0].summary.levels = d.configs[0].summary.levels.slice(0, 2)))
    expect(finding(rows, 'pool')!.sentences[0]).not.toMatch(/better buy/)
  })
})

describe('finding: warm medians 25% apart (rule 2)', () => {
  // Both pools served everything at 8 req/s.
  const allServed = (d: RunDetail) => {
    for (const c of d.configs) c.summary.levels = [levelSum(1, 30, 0), levelSum(3, 91, 0), levelSum(8, 240, 0)]
  }
  it('pool mode names the faster pool, the card it ran on and the $/1k relation', () => {
    expect(finding(offRows(allServed), 'pool')).toEqual({
      sentences: [
        'AMPERE_24 (it ran on NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb) is faster for this job: a typical warm request takes 674 ms against 1.1 s on AMPERE_16.',
        'They cost about the same per 1k requests ($0.17 and $0.18).',
      ],
      help: 'Compares AMPERE_16 FlashBoot off and AMPERE_24 FlashBoot off on the typical warm request, different when one is at least 25% lower, and on estimated cost per 1k requests, the same within 15%.',
    })
  })
  it('card mode says card and uses the card names', () => {
    expect(finding(specRows(cardMode()), 'card')).toEqual({
      sentences: [
        'NVIDIA L4 is faster for this job: a typical warm request takes 600 ms against 1.0 s on NVIDIA RTX A4500.',
        'NVIDIA RTX A4500 is cheaper per 1k requests ($0.20 against $0.30).',
      ],
      help: 'Compares NVIDIA RTX A4500 FlashBoot off and NVIDIA L4 FlashBoot off on the typical warm request, different when one is at least 25% lower, and on estimated cost per 1k requests, the same within 15%.',
    })
  })
})

describe('finding: alike (rule 3)', () => {
  const close = (d: RunDetail) => {
    for (const c of d.configs) c.summary.levels = [levelSum(8, 240, 0)]
    d.configs[2].summary.warm = { ...d.configs[2].summary.warm, p50: 1000 }
  }
  it('pool mode', () => {
    expect(finding(offRows(close), 'pool')!.sentences).toEqual([
      'The pools performed alike for this job (typical warm 1.1 s on AMPERE_16, 1.0 s on AMPERE_24).',
      'They cost about the same per 1k requests ($0.17 and $0.18).',
    ])
  })
  it('card mode says cards', () => {
    const d = cardMode()
    d.configs[1].summary.warm = { ...d.configs[1].summary.warm, p50: 900 }
    expect(finding(specRows(d), 'card')!.sentences[0]).toBe('The cards performed alike for this job (typical warm 1.0 s on NVIDIA RTX A4500, 900 ms on NVIDIA L4).')
  })
  it('fewer than 3 warm requests on a side: no warm verdict, not "alike"', () => {
    const thin = offRows((d) => {
      close(d)
      d.configs[2].summary.warm = { ...d.configs[2].summary.warm, n: 2, p50: 300 }
    })
    const f = finding(thin, 'pool')!
    expect(f.sentences[0]).toBe('Too few warm requests to compare the pools for this job.')
    expect(f.help).toBe('Compares AMPERE_16 FlashBoot off and AMPERE_24 FlashBoot off; a warm comparison needs at least 3 warm requests on each side.')
  })
})

describe('finding: which configuration stands for a pool', () => {
  it('a pool with only a FlashBoot-on configuration uses that one, and the help names it', () => {
    const d = naturalTwoPool()
    d.configs = d.configs.filter((c) => c.config.id !== 3) // AMPERE_24 on only
    expect(finding(specRows(d), 'pool')!.help).toMatch(/^Compares AMPERE_16 FlashBoot off and AMPERE_24 FlashBoot on /)
  })
})

describe('finding: pools are told apart by name, not position', () => {
  it('a pool the params do not list is not merged with the first pool', () => {
    const d = naturalTwoPool()
    d.run.params = { ...d.run.params, pools: ['AMPERE_16'] } // AMPERE_24 gets poolIndex 0 too
    expect(finding(specRows(d), 'pool')!.sentences[0]).toMatch(/^AMPERE_24 is the better buy/)
  })
})

describe('finding: a single configuration', () => {
  it('describes first boot, cold, warm and the top level, with no comparison words', () => {
    const f = finding(specRows(singleConfig()), 'pool')!
    expect(f).toEqual({
      sentences: ['On AMPERE_16 the first boot took 72 s, a cold start typically took 13.2 s and a warm request typically took 1.1 s; at 8 req/s it left 90 of 240 requests queued.'],
      help: 'Describes AMPERE_16 FlashBoot off: the first boot is one sample, the other times are typical (median) values.',
    })
    expect(f.sentences.join(' ')).not.toMatch(COMPARISON)
  })
  it('one pool with FlashBoot off and on is still one pool: describes the off row', () => {
    const d = naturalTwoPool()
    d.configs = d.configs.slice(0, 2)
    d.run.params = { ...d.run.params, pools: ['AMPERE_16'] }
    const f = finding(specRows(d), 'pool')!
    expect(f.sentences).toHaveLength(1)
    expect(f.sentences[0]).toMatch(/^On AMPERE_16 /)
    expect(f.sentences[0]).not.toMatch(COMPARISON)
  })
  it('served with failures: the true count', () => {
    const d = singleConfig()
    d.configs[0].summary.levels = [levelSum(8, 240, 0, 3)]
    expect(finding(specRows(d), 'pool')!.sentences[0]).toMatch(/; at 8 req\/s it served 237 of 240 and left none queued\.$/)
  })
  it('missing values and a level not reached are said plainly', () => {
    const d = singleConfig()
    d.configs[0].summary = { ...d.configs[0].summary, cold: { ...d.configs[0].summary.cold, n: 0 }, levels: [] }
    expect(finding(specRows(d), 'pool')!.sentences[0]).toBe(
      'On AMPERE_16 the first boot took 72 s, no cold start was measured and a warm request typically took 1.1 s; it did not reach 8 req/s.',
    )
  })
  it('no rows: null', () => {
    expect(finding([], 'pool')).toBeNull()
  })
})

describe('finding sentences never mention FlashBoot', () => {
  it('in any branch (the "?" may, to name the rows used)', () => {
    const outputs = [
      finding(offRows(), 'pool'),
      finding(specRows(cardMode()), 'card'),
      finding(specRows(singleConfig()), 'pool'),
      finding(offRows((d) => { for (const c of d.configs) c.summary.levels = [levelSum(8, 240, 0)] }), 'pool'),
    ]
    for (const f of outputs) {
      expect(f).not.toBeNull()
      expect(f!.sentences.join(' ')).not.toMatch(/flashboot/i)
    }
  })
})

describe('flashbootReadouts (the FlashBoot frame)', () => {
  it('natural: cold medians side by side where both sides have samples; a kept-alive readout where FlashBoot kept the worker', () => {
    const r = flashbootReadouts(specRows(naturalTwoPool()), false)
    expect(r).toHaveLength(2)
    expect(r[0]).toEqual({ kind: 'cold', subject: 'AMPERE_16', off: 13_189, on: 13_111, lower: null, note: 'typical of 3 and 3 samples; within 5% of each other' })
    expect(r[1]).toMatchObject({ kind: 'kept', subject: 'AMPERE_24' })
  })
  it('a 5% lower side with 3 samples each is coloured; with fewer it is named as too few', () => {
    const lower = naturalTwoPool()
    lower.configs[1].summary.cold = { ...lower.configs[1].summary.cold, p50: 9000 }
    expect(flashbootReadouts(specRows(lower), false)[0]).toMatchObject({ lower: 1, note: 'typical of 3 and 3 samples; FlashBoot on is at least 5% lower' })
    lower.configs[1].summary.cold = { ...lower.configs[1].summary.cold, n: 2 }
    expect(flashbootReadouts(specRows(lower), false)[0]).toMatchObject({ lower: null, note: 'typical of 3 and 2 samples; too few to call a difference' })
  })
  it('forced: never a winner, and the note says every sample started a new worker', () => {
    const d = naturalTwoPool()
    d.run.params = { ...d.run.params, coldStartMode: 'forced' }
    d.configs[1].summary.cold = { ...d.configs[1].summary.cold, p50: 9000 }
    const r = flashbootReadouts(specRows(d), true)
    expect(r[0]).toMatchObject({ kind: 'cold', lower: null, note: 'typical of 3 and 3 samples; every one started a new worker' })
    expect(r.some((x) => x.kind === 'kept')).toBe(false)
  })
  it('a pool run with one FlashBoot setting has no readout', () => {
    expect(flashbootReadouts(specRows(singleConfig()), false)).toEqual([])
  })
})
