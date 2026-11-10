import { describe, expect, it } from 'vitest'
import { chipsFor } from '../chips'
import type { Pool } from '../types'
import { cardMode, catalog, naturalTwoPool, retiredPool } from './fixtures'

const BLACKWELL = 'NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb'

describe('chipsFor', () => {
  it('pool mode with the catalog: VRAM, the cards on offer and stock now; got from the jobs', () => {
    expect(chipsFor(naturalTwoPool(), catalog())).toStrictEqual([
      {
        id: 'AMPERE_16',
        name: 'AMPERE_16',
        kind: 'pool',
        memoryGb: 16,
        cardsOffered: ['RTX 2000 Ada', 'RTX A4000', 'RTX A4500'],
        priceHr: 0.58,
        stock: 'HIGH',
        got: ['RTX A4500'],
      },
      {
        id: 'AMPERE_24',
        name: 'AMPERE_24',
        kind: 'pool',
        memoryGb: 24,
        cardsOffered: ['L4', 'RTX A5000'],
        priceHr: 0.75,
        stock: 'HIGH',
        got: [BLACKWELL],
      },
    ])
  })
  it('no catalog: pool, price from the config row and got only', () => {
    expect(chipsFor(naturalTwoPool())).toStrictEqual([
      { id: 'AMPERE_16', name: 'AMPERE_16', kind: 'pool', priceHr: 0.58, got: ['NVIDIA RTX A4500'] },
      { id: 'AMPERE_24', name: 'AMPERE_24', kind: 'pool', priceHr: 0.75, got: [BLACKWELL] },
    ])
  })
  it('a pool the catalog no longer lists: no stock, no cards offered, no crash (Review Focus 2)', () => {
    expect(chipsFor(retiredPool(), catalog())).toStrictEqual([
      { id: 'ADA_RETIRED', name: 'ADA_RETIRED', kind: 'pool', priceHr: 0.69, got: ['NVIDIA RTX 4000 Ada Generation'] },
    ])
  })
  it('card mode: one chip per card with its pool, the card price and stock, named like the spec rows (catalog name, else the id)', () => {
    expect(chipsFor(cardMode(), catalog())).toStrictEqual([
      { id: 'NVIDIA RTX A4500', name: 'RTX A4500', kind: 'card', pool: 'AMPERE_16', memoryGb: 16, priceHr: 0.58, stock: 'MEDIUM', got: ['RTX A4500'] },
      { id: 'NVIDIA L4', name: 'L4', kind: 'card', pool: 'AMPERE_24', memoryGb: 24, priceHr: 0.75, stock: 'HIGH', got: ['L4'] },
    ])
    expect(chipsFor(cardMode())[1]).toStrictEqual({ id: 'NVIDIA L4', name: 'NVIDIA L4', kind: 'card', pool: 'AMPERE_24', priceHr: 0.75, got: ['NVIDIA L4'] })
  })
  it('a catalog from a server without card lists: the pool gpuNames are the cards on offer', () => {
    const old = catalog().map(({ cards: _cards, ...p }) => p) as unknown as Pool[]
    expect(chipsFor(naturalTwoPool(), old)[0].cardsOffered).toEqual(['RTX 2000 Ada', 'RTX A4000', 'RTX A4500'])
  })
  it('a card-mode run that stored pools as null: chips from the cards, no crash', () => {
    const d = cardMode()
    d.run.params = { ...d.run.params, pools: null as unknown as string[] }
    expect(chipsFor(d, catalog()).map((c) => c.name)).toEqual(['RTX A4500', 'L4'])
    d.run.params = { ...d.run.params, cards: undefined }
    expect(chipsFor(d, catalog())).toEqual([])
  })
})
