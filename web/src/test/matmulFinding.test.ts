import { describe, expect, it } from 'vitest'
import { matmulFinding } from '../matmulFinding'
import type { ConfigDetail, Job } from '../types'
import { configDetail, job, runDetail } from './fixtures'

const A5000 = 'NVIDIA RTX A5000'
const R4090 = 'NVIDIA GeForce RTX 4090'
const catalog = [
  { id: 'AMPERE_24', cards: [{ id: A5000, name: 'RTX A5000' }] },
  { id: 'ADA_24', cards: [{ id: R4090, name: 'RTX 4090' }] },
] as never

// n completed matmul jobs on one GPU model, each measuring `tflops`.
const jobs = (configId: number, gpu: string, tflops: number[], workerId = 'w1'): Job[] =>
  tflops.map((t, i) => job(configId * 1000 + i, configId, 'load', i, 600, { gpuTypeId: gpu, workerId, tflops: t }))
const cfg = (id: number, pool: string, priceHr: number, js: Job[]): ConfigDetail => {
  const c = configDetail(id, pool, 'OFF')
  return { ...c, config: { ...c.config, priceHr }, jobs: js }
}

describe('matmulFinding', () => {
  it('names the faster model by how much, and the cheaper per TFLOP-hour', () => {
    const d = runDetail([cfg(1, 'AMPERE_24', 0.69, jobs(1, A5000, [100, 102, 104])), cfg(2, 'ADA_24', 0.89, jobs(2, R4090, [158, 160, 162]))])
    const f = matmulFinding(d, catalog)!
    expect(f.sentences).toEqual([
      'RTX 4090 is 57% faster at fp16 matmul than RTX A5000 (160 against 102 TFLOPS typical).',
      'RTX 4090 is also cheaper per TFLOP-hour ($0.00556 against $0.00676).',
    ])
    expect(f.help).toContain('at least 10% higher')
  })
  it('pools both FlashBoot settings and every config a model ran on', () => {
    const d = runDetail([
      cfg(1, 'AMPERE_24', 0.69, jobs(1, A5000, [100, 101])),
      cfg(2, 'AMPERE_24', 0.69, jobs(2, A5000, [103, 104])),
      cfg(3, 'ADA_24', 0.89, jobs(3, R4090, [150, 151, 152])),
    ])
    expect(matmulFinding(d, catalog)!.sentences[0]).toBe('RTX 4090 is 50% faster at fp16 matmul than RTX A5000 (151 against 101 TFLOPS typical).')
  })
  it('calls models within 10% alike, and costs within 15% the same', () => {
    const d = runDetail([cfg(1, 'AMPERE_24', 0.7, jobs(1, A5000, [100, 100, 100])), cfg(2, 'ADA_24', 0.75, jobs(2, R4090, [105, 105, 105]))])
    expect(matmulFinding(d, catalog)!.sentences).toEqual([
      'RTX 4090 and RTX A5000 performed alike at fp16 matmul (105 against 100 TFLOPS typical).',
      'They cost about the same per TFLOP-hour ($0.00714 and $0.007).',
    ])
  })
  it('says the slower one is cheaper when it is', () => {
    const d = runDetail([cfg(1, 'AMPERE_24', 0.3, jobs(1, A5000, [100, 100, 100])), cfg(2, 'ADA_24', 0.89, jobs(2, R4090, [160, 160, 160]))])
    expect(matmulFinding(d, catalog)!.sentences[1]).toBe('RTX A5000 is cheaper per TFLOP-hour ($0.003 against $0.00556).')
  })
  it('names the GPU via the worker when a job lacks gpuTypeId, and calls no winner on thin samples', () => {
    const late = jobs(1, '', [100, 100], 'w9')
    const named = jobs(1, A5000, [100], 'w9')
    const d = runDetail([cfg(1, 'AMPERE_24', 0.69, [...named, ...late]), cfg(2, 'ADA_24', 0.89, jobs(2, R4090, [160, 160]))])
    const f = matmulFinding(d, catalog)!
    expect(f.sentences).toEqual(['Too few matmul samples to compare RTX 4090 and RTX A5000 (at least 3 each needed).'])
  })
  it('ranks three or more models, fastest first', () => {
    const d = runDetail([
      cfg(1, 'AMPERE_24', 0.69, [...jobs(1, A5000, [100, 100, 100]), ...jobs(1, 'NVIDIA L4', [60, 60, 60], 'w2')]),
      cfg(2, 'ADA_24', 0.89, jobs(2, R4090, [160, 160, 160])),
    ])
    expect(matmulFinding(d, catalog)!.sentences[0]).toBe(
      'Fastest at fp16 matmul: RTX 4090 (160 TFLOPS typical), then RTX A5000 (100), then NVIDIA L4 (60).',
    )
  })
  it('leaves thin models out of a ranking, falling back to a pair', () => {
    const d = runDetail([
      cfg(1, 'AMPERE_24', 0.69, [...jobs(1, A5000, [100, 100, 100]), ...jobs(1, 'NVIDIA L4', [60], 'w2')]),
      cfg(2, 'ADA_24', 0.89, jobs(2, R4090, [160, 160, 160])),
    ])
    expect(matmulFinding(d, catalog)!.sentences[0]).toBe('RTX 4090 is 60% faster at fp16 matmul than RTX A5000 (160 against 100 TFLOPS typical).')
  })
  it('is null with fewer than two GPU models or no matmul at all', () => {
    expect(matmulFinding(runDetail([cfg(1, 'AMPERE_24', 0.69, jobs(1, A5000, [100, 100, 100]))]), catalog)).toBeNull()
    expect(matmulFinding(runDetail([cfg(1, 'AMPERE_24', 0.69, [job(1, 1, 'warm', 0, 500)])]), catalog)).toBeNull()
  })
})
