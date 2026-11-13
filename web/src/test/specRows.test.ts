import { describe, expect, it } from 'vitest'
import { levelRows, loadLevelsNote, specRows, topLevelWords } from '../specRows'
import { cardMode, catalog, levelSum, naturalTwoPool, runDetail, singleConfig } from './fixtures'

describe('topLevelWords', () => {
  const t = { rps: 8, sent: 240, served: 240, leftQueued: 0, failed: 0, reached: true }
  it('all served, left queued, served with failures, not reached', () => {
    expect(topLevelWords(t)).toBe('all 240 served')
    expect(topLevelWords({ ...t, served: 150, leftQueued: 90 })).toBe('90 of 240 left queued')
    expect(topLevelWords({ ...t, served: 220, failed: 20 })).toBe('220 of 240 served, 20 failed')
    expect(topLevelWords({ ...t, reached: false })).toBe('—')
  })
})

describe('specRows', () => {
  it('one row per config, in config order, with n values and the top level from the last levelsRps entry', () => {
    const rows = specRows(naturalTwoPool())
    expect(rows.map((r) => [r.key, r.name, r.poolIndex])).toEqual([
      ['c1', 'AMPERE_16 FlashBoot off', 0],
      ['c2', 'AMPERE_16 FlashBoot on', 0],
      ['c3', 'AMPERE_24 FlashBoot off', 1],
      ['c4', 'AMPERE_24 FlashBoot on', 1],
    ])
    expect(rows[0]).toEqual({
      key: 'c1',
      name: 'AMPERE_16 FlashBoot off',
      subject: 'AMPERE_16',
      poolIndex: 0,
      flashboot: 'OFF',
      firstBootMs: 71_977,
      cold: { p50: 13_189, n: 3 },
      warm: { p50: 1144, n: 10 },
      coldNote: null,
      topLevel: { rps: 8, sent: 240, served: 150, leftQueued: 90, failed: 0, reached: true },
      per1k: 0.1732,
      gotCards: ['NVIDIA RTX A4500'],
    })
    expect(rows[2].topLevel).toEqual({ rps: 8, sent: 240, served: 240, leftQueued: 0, failed: 0, reached: true })
  })
  it('no cold sample: p50 null with n 0', () => {
    expect(specRows(naturalTwoPool())[3].cold).toEqual({ p50: null, n: 0 })
  })
  it('no summary for the top level: reached false, nothing served or queued', () => {
    const d = singleConfig()
    d.configs[0].summary.levels = d.configs[0].summary.levels.slice(0, 2) // 1 and 3 req/s only
    expect(specRows(d)[0].topLevel).toEqual({ rps: 8, sent: 0, served: 0, leftQueued: 0, failed: 0, reached: false })
  })
  it('a config cancelled, failed or budget-stopped mid-level has not reached it, even with a summary for it', () => {
    for (const status of ['cancelled', 'failed', 'running'] as const) {
      const d = singleConfig()
      d.configs[0].config = { ...d.configs[0].config, status }
      expect(specRows(d)[0].topLevel).toEqual({ rps: 8, sent: 0, served: 0, leftQueued: 0, failed: 0, reached: false })
    }
  })
  it('failed requests are not served', () => {
    const d = singleConfig()
    d.configs[0].summary.levels = [levelSum(8, 240, 10, 5)]
    expect(specRows(d)[0].topLevel).toEqual({ rps: 8, sent: 240, served: 225, leftQueued: 10, failed: 5, reached: true })
  })
  it('a genuine $0 per 1k stays 0', () => {
    const d = singleConfig()
    d.configs[0].summary = { ...d.configs[0].summary, costPer1k: 0 }
    expect(specRows(d)[0].per1k).toBe(0)
  })
  it('no first boot, no per-request cost: null', () => {
    const d = singleConfig()
    d.configs[0].summary = { ...d.configs[0].summary, firstBootMs: 0, completed: 1, costPer1k: 9 }
    const [r] = specRows(d)
    expect(r.firstBootMs).toBeNull()
    expect(r.per1k).toBeNull()
  })
  it('card mode: named by card (the catalog name when listed), poolIndex from params.cards', () => {
    expect(specRows(cardMode()).map((r) => [r.name, r.subject, r.poolIndex])).toEqual([
      ['NVIDIA RTX A4500 FlashBoot off', 'NVIDIA RTX A4500', 0],
      ['NVIDIA L4 FlashBoot off', 'NVIDIA L4', 1],
    ])
    expect(specRows(cardMode(), catalog()).map((r) => [r.name, r.subject])).toEqual([
      ['RTX A4500 FlashBoot off', 'RTX A4500'],
      ['L4 FlashBoot off', 'L4'],
    ])
  })
})

describe('levelRows', () => {
  it('one row per config per level, in config order then level order, straight from the summaries', () => {
    const d = naturalTwoPool()
    const rows = levelRows(d)
    expect(rows).toHaveLength(d.configs.length * 3)
    expect(rows.slice(0, 3).map((r) => [r.key, r.name, r.rps])).toEqual([
      ['c1-1', 'AMPERE_16 FlashBoot off', 1],
      ['c1-3', 'AMPERE_16 FlashBoot off', 3],
      ['c1-8', 'AMPERE_16 FlashBoot off', 8],
    ])
    // levelSum(8, 240, 90): 150 completed, 90 left queued, latency p95 1000 ms over the 150.
    expect(rows[2]).toMatchObject({ sent: 240, completed: 150, failed: 0, leftQueued: 90, p95Ms: 1000, latencyN: 150, throughputRps: 8 })
  })
  it('no completed request at a level: no p95; a config with no levels has no rows', () => {
    const d = naturalTwoPool()
    d.configs[0].summary.levels = [levelSum(8, 10, 10)]
    d.configs[1].summary.levels = []
    const rows = levelRows(d).filter((r) => r.key.startsWith('c1-') || r.key.startsWith('c2-'))
    expect(rows).toEqual([expect.objectContaining({ key: 'c1-8', completed: 0, leftQueued: 10, p95Ms: null, latencyN: 0 })])
  })
  it('card mode names rows by the card, with the catalog\'s display name', () => {
    expect(levelRows(cardMode(), catalog())[0].name).toBe('RTX A4500 FlashBoot off')
    expect(levelRows(cardMode())[0].name).toBe('NVIDIA RTX A4500 FlashBoot off')
  })
})

describe('loadLevelsNote', () => {
  const p = { ...runDetail([]).run.params, jobSleepMs: 500, maxWorkers: 3 }
  it('gives the endpoint ceiling and says the sleep job never touches the GPU', () => {
    const t = loadLevelsNote({ ...p, matmulN: 0 })
    expect(t.startsWith('Each job only sleeps 500 ms')).toBe(true)
    expect(t).toContain("only sleeps 500 ms; it doesn't use the GPU")
    expect(t).toContain('about 2 req/s and an endpoint at most about 6 req/s with 3 max workers')
    expect(t).toContain('run with a matmul size above 0')
  })
  it('points at the matmul table when the run measured GPU speed', () => {
    const t = loadLevelsNote({ ...p, jobSleepMs: 300, matmulN: 4096 })
    expect(t).toContain('sleeps 300 ms after its matmul')
    expect(t).toContain('about 3.3 req/s and an endpoint at most about 10 req/s')
    expect(t).toContain('see the GPU matmul table')
  })
})
