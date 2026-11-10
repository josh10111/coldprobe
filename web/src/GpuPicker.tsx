import { useId } from 'react'
import { Help, Price, StockWord } from './components'
import { MAX_GPUS, type GpuMode, type PickerRow } from './form'

interface Props {
  mode: GpuMode
  rows: PickerRow[]
  selected: string[]
  onMode: (m: GpuMode) => void
  onToggle: (id: string) => void
}

const MODES: { mode: GpuMode; label: string }[] = [
  { mode: 'pool', label: 'By pool' },
  { mode: 'card', label: 'By card' },
]

export function GpuPicker({ mode, rows, selected, onMode, onToggle }: Props) {
  const idBase = useId()
  const full = selected.length >= MAX_GPUS
  const byPool = mode === 'pool'
  return (
    <>
      <div className="toggles" role="radiogroup" aria-label="Pick GPUs by">
        {MODES.map((m) => (
          <button
            key={m.mode}
            type="button"
            role="radio"
            aria-checked={mode === m.mode}
            className={mode === m.mode ? 'toggle on' : 'toggle'}
            onClick={() => onMode(m.mode)}
          >
            {m.label}
          </button>
        ))}
      </div>
      <div className="table-wrap">
        <table className="spec picker">
          <thead>
            <tr>
              <th className="picker-check">
                <span className="sr-only">Selected</span>
              </th>
              <th className="picker-name">{byPool ? 'Pool' : 'Card'}</th>
              <th className="picker-other">{byPool ? 'Cards it may hand out' : 'Pool'}</th>
              <th className="n">
                VRAM
                <Help text={byPool ? "The pool's GPU memory per card, in GB." : "This card's GPU memory, in GB."} />
              </th>
              <th className="n">
                Price
                <Help
                  text={
                    byPool
                      ? "The pool's dearest card, per GPU-hour on Serverless; the estimate uses it, since any card may be handed out."
                      : "This card's own Serverless price per GPU-hour."
                  }
                />
              </th>
              <th className="n">
                Stock
                <Help
                  text={
                    byPool
                      ? "Runpod's stock level now for the pool's best-stocked card; none means no card can be placed."
                      : "Runpod's stock level now for this card."
                  }
                />
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => {
              const on = selected.includes(r.id)
              const inputId = `${idBase}-${i}`
              return (
                <tr key={r.id}>
                  <td className="picker-check">
                    <input
                      id={inputId}
                      type="checkbox"
                      checked={on}
                      disabled={!on && full}
                      onChange={() => onToggle(r.id)}
                    />
                  </td>
                  <td className="picker-name">
                    <label htmlFor={inputId}>{r.name}</label>
                  </td>
                  <td className="picker-other">{r.other}</td>
                  <td className="n">{r.memoryGb} GB</td>
                  <td className="n">
                    <Price usd={r.priceHr} unit="/hr" />
                  </td>
                  <td className="n">
                    <StockWord level={r.availability} />
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </>
  )
}
