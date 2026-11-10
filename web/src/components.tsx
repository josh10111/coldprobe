import { useState } from 'react'
import { statusLabel, statusTone, stockWord, usd, type ModeWord } from './format'
import type { Availability, ConfigStatus, RunStatus } from './types'

// Near the right edge the "?" focus box opens leftwards so it never widens the page.
const TOOLTIP_ROOM_PX = 290 // the box's max width plus a margin

export function Help({ text }: { text: string }) {
  const [openLeft, setOpenLeft] = useState(false)
  return (
    <span
      className={openLeft ? 'q q-left' : 'q'}
      title={text}
      aria-label={text}
      role="img"
      tabIndex={0}
      onFocus={(e) => setOpenLeft(e.currentTarget.getBoundingClientRect().left > window.innerWidth - TOOLTIP_ROOM_PX)}
    >
      ?
    </span>
  )
}

export function StatusBadge({ status, suffix }: { status: RunStatus; suffix?: string }) {
  return (
    <span className={`status st-${statusTone[status]}`}>
      {status === 'running' && suffix && <span className="pulse" aria-hidden="true" />}
      {statusLabel[status]}
      {suffix && ` · ${suffix}`}
    </span>
  )
}

export const configTone = (s: ConfigStatus) =>
  s === 'done' ? 'ok' : s === 'failed' ? 'bad' : s === 'cancelled' ? 'warn' : ''

// An upper bound on spend, not the spend itself.
export function BoundBar({ boundUsd, capUsd }: { boundUsd: number; capUsd: number }) {
  const pct = capUsd > 0 ? Math.min(100, (boundUsd / capUsd) * 100) : 0
  return (
    <>
      <div className="mut small">
        Spend bound <span className="price">{usd(boundUsd)}</span> of <span className="mono">{usd(capUsd)}</span>{' '}
        <span className="mut">(upper bound, not the bill)</span>
        <Help text="The most this run can have cost so far, if every worker billed non-stop; the real cost is lower." />
      </div>
      <div className="bar">
        <div style={{ width: `${pct}%` }} />
      </div>
    </>
  )
}

export const MODE_HELP: Record<ModeWord, string> = {
  forced:
    "Forced cold starts: the endpoint was paused before each cold sample, so every sample started a brand-new worker (FlashBoot can't help there).",
  natural:
    'Natural cold starts: coldprobe waits for every worker to stop before each cold sample, so Runpod keeping a worker alive can mean fewer samples than asked for.',
}

export function ForcedBadge() {
  return (
    <span className="mode">
      forced
      <Help text={MODE_HELP.forced} />
    </span>
  )
}

export function ModeBadge({ mode }: { mode: ModeWord }) {
  if (mode === 'forced') return <ForcedBadge />
  return (
    <span className="mode">
      natural
      <Help text={MODE_HELP.natural} />
    </span>
  )
}

export function StockWord({ level }: { level: Availability }) {
  const { word, className } = stockWord(level)
  return <span className={className}>{word}</span>
}

export function Price({ usd: x, unit }: { usd: number; unit?: string }) {
  return (
    <span className="price">
      {usd(x)}
      {unit}
    </span>
  )
}
