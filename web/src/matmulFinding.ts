import { nearestRank } from './chartData'
import { COST_SAME_FRACTION, MIN_SAMPLES, type Finding } from './finding'
import { cardName, sig3, usd3sf } from './format'
import type { Pool, RunDetail } from './types'

export const MATMUL_FASTER_RATIO = 1.1

interface Model {
  name: string
  tflops: number
  n: number
  usdPerTflopHr: number | null // null when its configs were priced differently
}

// Pools can hand out several GPU models, so this compares models, not configs.
// FlashBoot doesn't change matmul speed, so both settings' jobs count.
function models(d: RunDetail, catalog?: Pool[]): Model[] {
  const gpuOf = new Map<string, string>()
  for (const cd of d.configs) for (const j of cd.jobs) if (j.gpuTypeId) gpuOf.set(j.workerId, j.gpuTypeId)

  const byGpu = new Map<string, { tflops: number[]; prices: Set<number> }>()
  for (const cd of d.configs) {
    for (const j of cd.jobs) {
      if (j.status !== 'COMPLETED' || j.tflops == null) continue
      const gpu = j.gpuTypeId || gpuOf.get(j.workerId)
      if (!gpu) continue
      const g = byGpu.get(gpu) ?? { tflops: [], prices: new Set<number>() }
      g.tflops.push(j.tflops)
      g.prices.add(cd.config.priceHr)
      byGpu.set(gpu, g)
    }
  }
  return [...byGpu].map(([gpu, g]) => {
    const tflops = nearestRank(g.tflops, 50)!
    const price = g.prices.size === 1 ? [...g.prices][0] : null
    return { name: cardName(gpu, catalog), tflops, n: g.tflops.length, usdPerTflopHr: price && tflops > 0 ? price / tflops : null }
  })
}

const HELP =
  `Compares GPU models by typical (median) fp16 TFLOPS across every completed request that ran on each, with both FlashBoot settings counted since FlashBoot doesn't change matmul speed. ` +
  `One is faster when at least ${Math.round((MATMUL_FASTER_RATIO - 1) * 100)}% higher, and needs at least ${MIN_SAMPLES} samples. ` +
  `$ per TFLOP-hour is the hourly price divided by typical TFLOPS; within ${COST_SAME_FRACTION * 100}% counts as the same.`

export function matmulFinding(d: RunDetail, catalog?: Pool[]): Finding | null {
  const ms = models(d, catalog).sort((a, b) => b.tflops - a.tflops)
  if (ms.length < 2) return null

  if (ms.length > 2) {
    const enough = ms.filter((m) => m.n >= MIN_SAMPLES)
    if (enough.length < 2) {
      return { sentences: [`Too few matmul samples to rank the GPU models (at least ${MIN_SAMPLES} each needed).`], help: HELP }
    }
    if (enough.length === 2) return compareTwo(enough[0], enough[1])
    const rank = enough.map((m, i) => (i === 0 ? `${m.name} (${sig3(m.tflops)} TFLOPS typical)` : `${m.name} (${sig3(m.tflops)})`))
    const priced = enough.filter((m) => m.usdPerTflopHr !== null).sort((a, b) => a.usdPerTflopHr! - b.usdPerTflopHr!)
    const sentences = [`Fastest at fp16 matmul: ${rank.join(', then ')}.`]
    if (priced.length > 1) sentences.push(`Cheapest per TFLOP-hour: ${priced[0].name} (${usd3sf(priced[0].usdPerTflopHr!)}).`)
    return { sentences, help: HELP }
  }

  return compareTwo(ms[0], ms[1])
}

function compareTwo(fast: Model, slow: Model): Finding {
  if (fast.n < MIN_SAMPLES || slow.n < MIN_SAMPLES) {
    return { sentences: [`Too few matmul samples to compare ${fast.name} and ${slow.name} (at least ${MIN_SAMPLES} each needed).`], help: HELP }
  }
  const pair = `(${sig3(fast.tflops)} against ${sig3(slow.tflops)} TFLOPS typical)`
  const faster = fast.tflops >= MATMUL_FASTER_RATIO * slow.tflops
  const sentences = [
    faster
      ? `${fast.name} is ${Math.round((fast.tflops / slow.tflops - 1) * 100)}% faster at fp16 matmul than ${slow.name} ${pair}.`
      : `${fast.name} and ${slow.name} performed alike at fp16 matmul ${pair}.`,
  ]
  const [fc, sc] = [fast.usdPerTflopHr, slow.usdPerTflopHr]
  if (fc !== null && sc !== null) {
    if (Math.abs(fc - sc) <= COST_SAME_FRACTION * Math.min(fc, sc)) {
      sentences.push(`They cost about the same per TFLOP-hour (${usd3sf(fc)} and ${usd3sf(sc)}).`)
    } else if (fc < sc) {
      sentences.push(`${fast.name} is ${faster ? 'also ' : ''}cheaper per TFLOP-hour (${usd3sf(fc)} against ${usd3sf(sc)}).`)
    } else {
      sentences.push(`${slow.name} is cheaper per TFLOP-hour (${usd3sf(sc)} against ${usd3sf(fc)}).`)
    }
  }
  return { sentences, help: HELP }
}
