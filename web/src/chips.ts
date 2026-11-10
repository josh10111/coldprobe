import { cardName } from './format'
import type { Availability, Pool, RunDetail } from './types'

// id, priceHr and got come from the stored run; the optional fields come
// from the live catalog and are absent when it doesn't list the pool or card.
export interface Chip {
  id: string // pool id, or catalog card id in card mode
  name: string
  kind: 'pool' | 'card'
  pool?: string // card mode
  memoryGb?: number
  cardsOffered?: string[] // pool mode
  priceHr: number
  stock?: Availability
  got: string[] // cards the jobs reported, by display name
}

export function chipsFor(d: RunDetail, catalog?: Pool[]): Chip[] {
  const cards = d.run.params.cards ?? []
  if (cards.length > 0) return cards.map((id) => cardChip(d, id, catalog))
  // Card-mode runs may store pools as null.
  return (d.run.params.pools ?? []).map((id) => poolChip(d, id, catalog))
}

function poolChip(d: RunDetail, id: string, catalog?: Pool[]): Chip {
  const configs = d.configs.filter((c) => c.config.pool === id)
  const chip: Chip = { id, name: id, kind: 'pool', priceHr: configs[0]?.config.priceHr ?? 0, got: got(configs, catalog) }
  const p = catalog?.find((x) => x.id === id)
  if (p) {
    chip.memoryGb = p.memoryGb
    // Older servers send no `cards`; gpuNames are the same display names.
    chip.cardsOffered = p.cards ? p.cards.map((c) => c.name) : p.gpuNames
    chip.stock = p.availability
  }
  return chip
}

function cardChip(d: RunDetail, id: string, catalog?: Pool[]): Chip {
  const configs = d.configs.filter((c) => c.config.card === id)
  const chip: Chip = { id, name: cardName(id, catalog), kind: 'card', priceHr: configs[0]?.config.priceHr ?? 0, got: got(configs, catalog) }
  if (configs.length > 0) chip.pool = configs[0].config.pool
  for (const p of catalog ?? []) {
    const card = (p.cards ?? []).find((c) => c.id === id)
    if (!card) continue
    chip.memoryGb = card.memoryGb
    chip.stock = card.availability
    break
  }
  return chip
}

const got = (configs: RunDetail['configs'], catalog?: Pool[]) =>
  [...new Set(configs.flatMap((c) => c.jobs.map((j) => j.gpuTypeId)).filter((g) => g !== ''))].map((id) => cardName(id, catalog)).sort()
