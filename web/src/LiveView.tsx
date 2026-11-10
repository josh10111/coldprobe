import { useEffect, useRef, useState } from 'react'
import { phaseMarks } from './chartData'
import { BoundBar, ForcedBadge, StatusBadge } from './components'
import { configLabel, duration, isForced, runHeading } from './format'
import type { LiveRun } from './liveRun'
import { liveWords } from './phases'
import { Section } from './report/common'
import { LatencyChart } from './report/LatencyChart'
import { SpecTable } from './report/SpecTable'
import { liveSpecRows } from './specRows'
import { notDrawn, runTraces, traceExtent } from './trace'
import type { Pool, RunDetail } from './types'
import { useNow } from './useNow'

const LOG_TAIL = 5

// `live` comes from ActiveRunProvider, so this page opens no stream of its
// own; it is null until the provider follows this run.
export function LiveView({ detail, live, catalog, onCancel }: { detail: RunDetail; live: LiveRun | null; catalog?: Pool[]; onCancel?: () => void }) {
  const now = useNow()
  const { run, configs } = detail
  const p = run.params
  const rows = liveSpecRows(detail, catalog)
  const traces = runTraces(detail)
  const extent = traceExtent(traces, (now - new Date(run.createdAt).getTime()) / 1000)
  const words = Object.fromEntries(rows.map((r, i) => [r.key, liveWords(p, configs[i], now, live?.activity[configs[i].config.id])]))
  const labelOf = (id?: number) => {
    const c = configs.find((x) => x.config.id === id)
    return c ? configLabel(c.config) : ''
  }

  return (
    <>
      <h1>
        {runHeading(p, catalog)} {isForced(p) && <ForcedBadge />}{' '}
        <StatusBadge status={run.status} suffix={duration(now - new Date(run.createdAt).getTime())} />
      </h1>
      <div className="params">
        <BoundBar boundUsd={live?.bound?.boundUsd ?? 0} capUsd={live?.bound?.capUsd ?? run.budgetUsd} />
      </div>

      <Section
        title="What you wait for and what you pay"
        sub={'One row per configuration, filling in as each phase completes; the cell of the phase a configuration is in says how far it has got. "Typical" is the median; the small number is how many samples it rests on.'}
      >
        <SpecTable rows={rows} traces={traces} extent={extent} forced={isForced(p)} live={words} />
        {configs
          .filter((cd) => cd.config.error)
          .map((cd) => (
            <p key={cd.config.id} className={cd.config.status === 'failed' ? 'bad small' : 'mut small'}>
              {configLabel(cd.config)}: {cd.config.error}
            </p>
          ))}
      </Section>

      <Section
        title="Latency through the run"
        help="Each line point is the median wall time of that configuration's last 10 completed requests (fewer at the start), in send order, from the warm phase on."
        sub="Fills in live; the time axis runs to now. Dashed lines mark where each phase began."
      >
        <LatencyChart rows={rows} traces={traces} extent={extent} marks={phaseMarks(detail)} notDrawn={notDrawn(detail)} />
      </Section>

      <Section title="Latest activity">
        {live && live.logs.length > 0 ? (
          <ol className="log">
            {live.logs.slice(-LOG_TAIL).map((l) => (
              <li key={l.seq} className={l.error ? 'bad' : undefined}>
                {new Date(l.ts).toLocaleTimeString('en-GB')} {l.configId !== undefined && `${labelOf(l.configId)}: `}
                {l.msg}
              </li>
            ))}
          </ol>
        ) : (
          <p className="mut small">Waiting for the next step (activity from before this page connected isn't replayed).</p>
        )}
      </Section>

      {onCancel && <CancelButton onCancel={onCancel} />}
    </>
  )
}

function CancelButton({ onCancel }: { onCancel: () => void }) {
  const [asking, setAsking] = useState(false)
  // The run stays `running` through teardown (up to 30 s).
  const [sent, setSent] = useState(false)
  const noRef = useRef<HTMLButtonElement>(null)
  useEffect(() => {
    if (asking) noRef.current?.focus()
  }, [asking])

  if (sent) return <p className="actions mut">Cancel requested: deleting the endpoints…</p>
  if (!asking) {
    return (
      <p className="actions">
        <button type="button" className="btn danger" onClick={() => setAsking(true)}>
          Cancel run
        </button>{' '}
        <span className="mut small">deletes the endpoints within 30 s</span>
      </p>
    )
  }
  return (
    <p className="actions confirm" role="group" aria-label="Confirm cancel">
      <span>Really cancel this run? Its endpoints are deleted and it can't be resumed.</span>{' '}
      <button
        type="button"
        className="btn danger"
        onClick={() => {
          setAsking(false)
          setSent(true)
          onCancel()
        }}
      >
        Yes, cancel
      </button>{' '}
      <button type="button" className="btn" ref={noRef} onClick={() => setAsking(false)}>
        No, keep running
      </button>
    </p>
  )
}
