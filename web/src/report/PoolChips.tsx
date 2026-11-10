import type { Chip } from '../chips'
import { Help, Price, StockWord } from '../components'

export function PoolChips({ chips }: { chips: Chip[] }) {
  return (
    <div className="pool-chips">
      {chips.map((c) => (
        <div key={c.id} className="pool-chip">
          <div>
            <b>{c.name}</b>
            {c.kind === 'card' && c.pool && <span className="mut"> · {c.pool} pool</span>}
            {(c.memoryGb !== undefined || c.cardsOffered) && (
              <span className="soft small">
                {' '}
                {c.memoryGb !== undefined && `${c.memoryGb} GB`}
                {c.memoryGb !== undefined && c.cardsOffered && ' · '}
                {c.cardsOffered?.join(', ')}
              </span>
            )}
          </div>
          <div className="chip-meta small">
            <span>
              <Price usd={c.priceHr} unit="/hr" />
              <Help text="The hourly price coldprobe used for this run's cost estimates." />
            </span>
            {c.stock !== undefined && (
              <span>
                stock now <StockWord level={c.stock} />
                <Help text="Runpod's stock for this right now, from the live catalog; it may have been different when the run started." />
              </span>
            )}
            <span>
              got <span className="mono">{c.got.length > 0 ? c.got.join(', ') : '—'}</span>
              <Help text="The GPU Runpod reported for the workers that ran this run's jobs (each job's gpuTypeId)." />
            </span>
          </div>
        </div>
      ))}
    </div>
  )
}
