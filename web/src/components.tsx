import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { statusLabel, statusTone, stockWord, usd, type ModeWord } from './format'
import type { Availability, ConfigStatus, RunStatus } from './types'

const TIP_MAX_W = 260
const TIP_GAP = 6
const EDGE = 8

// Portalled with fixed positioning so .table-wrap's overflow can't clip it.
export function Help({ text }: { text: string }) {
  const ref = useRef<HTMLSpanElement>(null)
  const id = useId()
  const [pos, setPos] = useState<{ left: number; top: number; above: boolean } | null>(null)
  const pinned = useRef(false)

  const show = useCallback(() => {
    const el = ref.current
    if (!el) return
    const r = el.getBoundingClientRect()
    const vw = document.documentElement.clientWidth
    const left = Math.max(EDGE, Math.min(r.left, vw - TIP_MAX_W - EDGE))
    const above = r.bottom + 120 > window.innerHeight && r.top > 120
    setPos({ left, top: above ? r.top - TIP_GAP : r.bottom + TIP_GAP, above })
  }, [])
  const hide = useCallback(() => {
    pinned.current = false
    setPos(null)
  }, [])

  useEffect(() => {
    if (!pos) return
    const onAway = (e: PointerEvent) => {
      if (!ref.current?.contains(e.target as Node)) hide()
    }
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && hide()
    const onScroll = () => hide()
    document.addEventListener('pointerdown', onAway)
    document.addEventListener('keydown', onKey)
    window.addEventListener('scroll', onScroll, true)
    window.addEventListener('resize', onScroll)
    return () => {
      document.removeEventListener('pointerdown', onAway)
      document.removeEventListener('keydown', onKey)
      window.removeEventListener('scroll', onScroll, true)
      window.removeEventListener('resize', onScroll)
    }
  }, [pos, hide])

  return (
    <>
      <span
        ref={ref}
        className="q"
        role="button"
        tabIndex={0}
        aria-label="More info"
        aria-describedby={pos ? id : undefined}
        aria-expanded={pos !== null}
        onMouseEnter={show}
        onMouseLeave={() => !pinned.current && hide()}
        onFocus={show}
        onBlur={hide}
        onClick={(e) => {
          e.preventDefault()
          e.stopPropagation() // a "?" inside a <label> must not toggle its input
          if (pinned.current) hide()
          else {
            pinned.current = true
            show()
          }
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault()
            if (pos) hide()
            else show()
          }
        }}
      >
        ?
      </span>
      {pos &&
        createPortal(
          <span
            id={id}
            role="tooltip"
            className="tip"
            style={{ left: pos.left, top: pos.top, transform: pos.above ? 'translateY(-100%)' : undefined }}
          >
            {text}
          </span>,
          document.body,
        )}
    </>
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
        Worst-case spend so far: <span className="price">{usd(boundUsd)}</span> of the <span className="mono">{usd(capUsd)}</span> budget{' '}
        <span className="mut">(not the bill)</span>
        <Help text="The bar is money, not progress: the most this run can have cost so far if every worker billed non-stop. The real cost is lower. The budget guard stops the run if it reaches the budget." />
      </div>
      <div className="bar">
        <div style={{ width: `${pct}%` }} />
      </div>
    </>
  )
}

export function ProgressBar({ fraction, configsDone, configs }: { fraction: number; configsDone: number; configs: number }) {
  const pct = Math.round(Math.min(1, Math.max(0, fraction)) * 100)
  return (
    <>
      <div className="mut small progress-label">
        Run progress: <span className="ok mono">{pct}%</span> · {configsDone} of {configs} configuration{configs === 1 ? '' : 's'} done
        <Help text="Each configuration's phases (first boot, cold starts, warm, each load level) count the same and fill as their requests go out and come back. Phases take very different times, so this is how far through the run is, not a time estimate." />
      </div>
      <div className="bar bar-progress" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label="Run progress">
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
