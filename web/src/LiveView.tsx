import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { phaseMarks } from './chartData'
import { BoundBar, ForcedBadge, ProgressBar, StatusBadge } from './components'
import { configLabel, duration, isForced, runHeading } from './format'
import type { LiveRun, LogLine } from './liveRun'
import { firstBootWait, liveWords, runProgress } from './phases'
import { Section } from './report/common'
import { LatencyChart } from './report/LatencyChart'
import { SpecTable } from './report/SpecTable'
import { liveSpecRows } from './specRows'
import { notDrawn, runTraces, traceExtent } from './trace'
import type { ConfigDetail, Pool, RunDetail } from './types'
import { useNow } from './useNow'

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
        <ProgressBar {...runProgress(p, configs)} />
      </div>

      <Section
        title="What you wait for and what you pay"
        sub={'One row per configuration, filling in as each phase completes. "Typical" is the median; the small number is how many samples it rests on.'}
        more="The cell of the phase a configuration is in says how far it has got, such as “warm · 4/10”."
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
        <LatencyChart
          rows={rows}
          traces={traces}
          extent={extent}
          marks={phaseMarks(detail)}
          notDrawn={notDrawn(detail)}
          empty={run.status === 'running' ? <WaitingForWorker configs={configs} now={now} /> : undefined}
        />
      </Section>

      <Section
        title="Latest activity log"
        sub="Green: a step succeeded. Amber: waiting on Runpod, or a caveat. Red: an error. Activity from before this page connected isn't replayed."
      >
        {live && live.logs.length > 0 ? (
          <ActivityLog logs={live.logs} labelOf={labelOf} />
        ) : (
          <p className="mut small">Waiting for the next step.</p>
        )}
      </Section>

      {onCancel && <CancelButton onCancel={onCancel} />}
    </>
  )
}

function WaitingForWorker({ configs, now }: { configs: ConfigDetail[]; now: number }) {
  return (
    <div className="waiting" role="status">
      <p className="waiting-title">
        <span className="pulse pulse-wait" aria-hidden="true" />
        Waiting for a GPU worker
      </p>
      <p className="mut small">
        Runpod has to place a GPU, pull the worker image and boot it before the first request is answered. That first boot can
        take anywhere from a few seconds to several minutes; the chart starts as soon as one finishes.
      </p>
      <ul className="waiting-list">
        {configs.map((cd) => {
          const w = firstBootWait(cd, now)
          return (
            <li key={cd.config.id}>
              <span>{configLabel(cd.config)}</span> <span className={w.tone ?? 'mut'}>{w.text}</span>
            </li>
          )
        })}
      </ul>
      <div className="waiting-bar" aria-hidden="true" />
    </div>
  )
}

// Follows the newest line unless the reader has scrolled up.
function ActivityLog({ logs, labelOf }: { logs: LogLine[]; labelOf: (id?: number) => string }) {
  const ref = useRef<HTMLOListElement>(null)
  const [follow, setFollow] = useState(true)
  useLayoutEffect(() => {
    const el = ref.current
    if (el && follow) el.scrollTop = el.scrollHeight
  }, [logs, follow])

  return (
    <div className="log-wrap">
      <ol
        ref={ref}
        className="log"
        tabIndex={0}
        aria-label="Activity log"
        onScroll={(e) => {
          const el = e.currentTarget
          setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 8)
        }}
      >
        {logs.map((l) => (
          <li key={l.seq} className={l.tone ? `log-${l.tone}` : undefined}>
            <span className="log-time">{new Date(l.ts).toLocaleTimeString('en-GB')}</span>{' '}
            {l.configId !== undefined && <span className="log-who">{labelOf(l.configId)}: </span>}
            {l.msg}
          </li>
        ))}
      </ol>
      {!follow && (
        <button type="button" className="btn log-jump" onClick={() => setFollow(true)}>
          Jump to latest
        </button>
      )}
    </div>
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
