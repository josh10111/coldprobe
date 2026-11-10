import type { ConfigDetail, FlashBoot, Job, JobPhase, LevelSummary, Pool, RunDetail, Summary } from '../types'

export const summary = (n: number, p50 = 0): Summary => ({ n, p50, p95: p50, mean: p50, min: p50, max: p50 })

export function configDetail(id: number, pool: string, flashboot: FlashBoot, coldP50 = 0, coldN = 0): ConfigDetail {
  return {
    config: { id, runId: 'r1', pool, flashboot, priceHr: 0.58, status: 'running', createdAt: '2026-11-08T10:00:00Z' },
    summary: {
      configId: id,
      firstBootMs: 0,
      cold: summary(coldN, coldP50),
      coldDelay: summary(0),
      warm: summary(0),
      levels: [],
      completed: 0,
      failed: 0,
      unfinished: 0,
      costUsd: 0,
      costPer1k: 0,
    },
    jobs: [],
  }
}

export function runDetail(configs: ConfigDetail[], pools = ['AMPERE_16']): RunDetail {
  return {
    run: {
      id: 'r1',
      createdAt: '2026-11-08T10:00:00Z',
      status: 'running',
      params: {
        image: 'img',
        pools,
        flashboot: ['OFF', 'FLASHBOOT'],
        maxWorkers: 2,
        idleTimeoutS: 5,
        coldStarts: 3,
        warmRequests: 10,
        levelsRps: [1, 3, 8],
        levelSeconds: 30,
        drainSeconds: 60,
        jobSleepMs: 500,
        modelLoadS: 5,
        budgetUsd: 2,
      },
      budgetUsd: 2,
      estMaxUsd: 2,
    },
    configs,
  }
}

// A job sent `atS` seconds after the run's start (2026-11-08T10:00:00Z).
export function job(id: number, configId: number, phase: JobPhase, atS: number, wallMs: number, extra: Partial<Job> = {}): Job {
  const sent = new Date(Date.parse('2026-11-08T10:00:00Z') + atS * 1000).toISOString()
  return {
    id,
    configId,
    phase,
    level: 0,
    runpodJobId: `rp-${id}`,
    workerId: 'w1',
    gpuTypeId: 'NVIDIA RTX A4000',
    status: 'COMPLETED',
    intendedAt: sent,
    sentAt: sent,
    wallMs,
    delayMs: wallMs - 500,
    execMs: 500,
    ...extra,
  }
}

// The rest of `sent` completed.
export function levelSum(rps: number, sent: number, leftQueued: number, failed = 0): LevelSummary {
  const completed = sent - leftQueued - failed
  return {
    rps,
    sent,
    completed,
    failed,
    unfinished: leftQueued,
    throughputRps: rps,
    latency: summary(completed, 1000),
    delay: summary(0),
  }
}

interface ConfigShape {
  id: number
  pool: string
  card?: string
  flashboot: FlashBoot
  priceHr: number
  firstBootMs: number
  cold: [n: number, p50: number]
  warm: [n: number, p50: number]
  levels: LevelSummary[]
  completed: number
  costPer1k: number
  gpu: string // every job's gpuTypeId
}

// Includes a load job with no gpuTypeId, which `got` must skip.
function shaped(s: ConfigShape): ConfigDetail {
  const cd = configDetail(s.id, s.pool, s.flashboot)
  cd.config = { ...cd.config, priceHr: s.priceHr, status: 'done', ...(s.card ? { card: s.card } : {}) }
  cd.summary = {
    ...cd.summary,
    firstBootMs: s.firstBootMs,
    cold: summary(s.cold[0], s.cold[1]),
    warm: summary(s.warm[0], s.warm[1]),
    levels: s.levels,
    completed: s.completed,
    unfinished: s.levels.reduce((n, l) => n + l.unfinished, 0),
    costPer1k: s.costPer1k,
  }
  const base = s.id * 100
  cd.jobs = [
    job(base + 1, s.id, 'first_boot', 0, s.firstBootMs, { gpuTypeId: s.gpu }),
    ...Array.from({ length: s.cold[0] }, (_, i) => job(base + 10 + i, s.id, 'cold', 40 + 10 * i, s.cold[1] + 500, { gpuTypeId: s.gpu })),
    job(base + 2, s.id, 'warm', 100, s.warm[1], { gpuTypeId: s.gpu }),
    job(base + 3, s.id, 'load', 150, 900, { gpuTypeId: '', level: 1 }),
  ]
  return cd
}

// A real natural run, rounded: at 8 req/s AMPERE_16 left ~90 of 240 queued and
// AMPERE_24 served all; AMPERE_24 with FlashBoot kept warm (cold n 0).
export function naturalTwoPool(): RunDetail {
  const blackwell = 'NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb'
  const lv = (u8: number) => [levelSum(1, 30, 0), levelSum(3, 91, 0), levelSum(8, 240, u8)]
  const d = runDetail(
    [
      shaped({ id: 1, pool: 'AMPERE_16', flashboot: 'OFF', priceHr: 0.58, firstBootMs: 71_977, cold: [3, 13_189], warm: [10, 1144], levels: lv(90), completed: 285, costPer1k: 0.1732, gpu: 'NVIDIA RTX A4500' }),
      shaped({ id: 2, pool: 'AMPERE_16', flashboot: 'FLASHBOOT', priceHr: 0.58, firstBootMs: 73_981, cold: [3, 13_111], warm: [10, 1149], levels: lv(89), completed: 286, costPer1k: 0.168, gpu: 'NVIDIA RTX A4500' }),
      shaped({ id: 3, pool: 'AMPERE_24', flashboot: 'OFF', priceHr: 0.75, firstBootMs: 71_978, cold: [3, 9315], warm: [10, 674], levels: lv(0), completed: 375, costPer1k: 0.1806, gpu: blackwell }),
      shaped({ id: 4, pool: 'AMPERE_24', flashboot: 'FLASHBOOT', priceHr: 0.75, firstBootMs: 76_050, cold: [0, 0], warm: [10, 598], levels: lv(0), completed: 372, costPer1k: 0.1565, gpu: blackwell }),
    ],
    ['AMPERE_16', 'AMPERE_24'],
  )
  d.run.status = 'completed'
  return d
}

export function singleConfig(): RunDetail {
  const d = runDetail([
    shaped({ id: 1, pool: 'AMPERE_16', flashboot: 'OFF', priceHr: 0.58, firstBootMs: 71_977, cold: [3, 13_189], warm: [10, 1144], levels: [levelSum(1, 30, 0), levelSum(3, 91, 0), levelSum(8, 240, 90)], completed: 285, costPer1k: 0.1732, gpu: 'NVIDIA RTX A4500' }),
  ])
  d.run.params = { ...d.run.params, flashboot: ['OFF'] }
  d.run.status = 'completed'
  return d
}

// The L4 is 40% faster warm and both serve everything: the warm-latency branch.
export function cardMode(): RunDetail {
  const lv = [levelSum(1, 30, 0), levelSum(3, 90, 0)]
  const d = runDetail(
    [
      shaped({ id: 1, pool: 'AMPERE_16', card: 'NVIDIA RTX A4500', flashboot: 'OFF', priceHr: 0.58, firstBootMs: 60_000, cold: [3, 12_000], warm: [10, 1000], levels: lv, completed: 124, costPer1k: 0.2, gpu: 'NVIDIA RTX A4500' }),
      shaped({ id: 2, pool: 'AMPERE_24', card: 'NVIDIA L4', flashboot: 'OFF', priceHr: 0.75, firstBootMs: 65_000, cold: [3, 9000], warm: [10, 600], levels: lv, completed: 124, costPer1k: 0.3, gpu: 'NVIDIA L4' }),
    ],
    [],
  )
  d.run.params = { ...d.run.params, pools: [], cards: ['NVIDIA RTX A4500', 'NVIDIA L4'], flashboot: ['OFF'], levelsRps: [1, 3] }
  d.run.status = 'completed'
  return d
}

// A stored run on a pool the catalog no longer lists.
export function retiredPool(): RunDetail {
  const d = runDetail([
    shaped({ id: 1, pool: 'ADA_RETIRED', flashboot: 'OFF', priceHr: 0.69, firstBootMs: 50_000, cold: [3, 10_000], warm: [10, 700], levels: [levelSum(1, 30, 0)], completed: 44, costPer1k: 0.25, gpu: 'NVIDIA RTX 4000 Ada Generation' }),
  ], ['ADA_RETIRED'])
  d.run.params = { ...d.run.params, flashboot: ['OFF'], levelsRps: [1] }
  return d
}

// The fake's catalog (internal/fakerunpod): cards sorted by price then id.
export function catalog(): Pool[] {
  return [
    {
      id: 'AMPERE_16', gpuNames: ['RTX 2000 Ada', 'RTX A4000', 'RTX A4500'], memoryGb: 16, pricePerHr: 0.58, availability: 'HIGH',
      cards: [
        { id: 'NVIDIA RTX 2000 Ada Generation', name: 'RTX 2000 Ada', memoryGb: 16, priceHr: 0.58, availability: 'LOW' },
        { id: 'NVIDIA RTX A4000', name: 'RTX A4000', memoryGb: 16, priceHr: 0.58, availability: 'HIGH' },
        { id: 'NVIDIA RTX A4500', name: 'RTX A4500', memoryGb: 16, priceHr: 0.58, availability: 'MEDIUM' },
      ],
    },
    {
      id: 'AMPERE_24', gpuNames: ['L4', 'RTX A5000'], memoryGb: 24, pricePerHr: 0.75, availability: 'HIGH',
      cards: [
        { id: 'NVIDIA L4', name: 'L4', memoryGb: 24, priceHr: 0.75, availability: 'HIGH' },
        { id: 'NVIDIA RTX A5000', name: 'RTX A5000', memoryGb: 24, priceHr: 0.75, availability: 'MEDIUM' },
      ],
    },
  ]
}
