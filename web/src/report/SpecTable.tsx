import { Help } from '../components'
import { flashLabel, secs, usdPer1k } from '../format'
import type { LiveWords } from '../phases'
import { topLevelWords, type SpecRow } from '../specRows'
import type { Trace } from '../trace'
import { TraceCell } from './TraceCell'

// Every trace cell shares `extent` so rows read against each other and the
// big chart. `live` holds the live view's phase words by row key.
export function SpecTable({
  rows,
  traces,
  extent,
  forced,
  live,
}: {
  rows: SpecRow[]
  traces: Trace[]
  extent: { xMax: number; yMax: number }
  forced: boolean
  live?: Record<string, LiveWords>
}) {
  const byKey = new Map(traces.map((t) => [t.key, t]))
  const rps = rows[0]?.topLevel.rps ?? 0
  const cold = forced ? 'New-worker cold start' : 'Cold start'
  return (
    <div className="table-wrap">
      <table className="spec">
        <thead>
          <tr>
            <th>Configuration</th>
            <th>
              Latency through the run <span className="count nowrap">0 to {Math.round(extent.xMax)} s</span>
              <Help text="Each configuration's latency from the run's start to its end: a dot per first boot and cold start, then the rolling median of the last 10 requests; every row uses the same scales." />
            </th>
            <th className="n">
              First boot
              <Help text="Time for a brand-new endpoint's first worker to answer, image pull and boot included; one sample per configuration." />
            </th>
            <th className="n">
              {cold}, typical
              <Help
                text={
                  forced
                    ? "The median extra time a request took when the endpoint had been paused, so it started a brand-new worker; FlashBoot can't help one."
                    : 'The median extra time a request took when no worker was running.'
                }
              />
            </th>
            <th className="n">
              Warm, typical
              <Help text="The median time for a request answered by a worker that was already running." />
            </th>
            <th className="n">
              {rps > 0 ? `At ${rps} req/s` : 'At the top load'}
              <Help text="At the run's highest load level: whether every request sent was served, or how many were still left queued when the level's drain time ran out." />
            </th>
            <th className="n">
              Est. $ / 1k requests
              <Help text="Estimated cost per 1,000 completed requests: execution, boot and idle-timeout time at the pool's hourly price (the card's own price in card mode)." />
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => {
            const trace = byKey.get(r.key)
            const words = live?.[r.key] ?? {}
            return (
              <tr key={r.key}>
                <td>
                  <span className="cfg">{r.subject}</span> <span className="mut">FlashBoot {flashLabel(r.flashboot)}</span>
                </td>
                <td className="trace-cell">
                  {trace && (
                    <TraceCell
                      trace={trace}
                      extent={extent}
                      poolIndex={r.poolIndex}
                      dashed={r.flashboot === 'FLASHBOOT'}
                      label={`Latency trace for ${r.name}`}
                    />
                  )}
                </td>
                <td className="n">
                  {r.firstBootMs === null ? '—' : secs(r.firstBootMs)}
                  <span className="count">({r.firstBootMs === null ? 0 : 1})</span>
                  <Word text={words.firstBoot} />
                </td>
                <td className="n">
                  {r.cold.p50 !== null ? (
                    secs(r.cold.p50)
                  ) : r.coldNote ? (
                    <span className="warn">
                      {r.coldNote.short}
                      <Help text={r.coldNote.help} />
                    </span>
                  ) : (
                    '—'
                  )}
                  <span className="count">({r.cold.n})</span>
                  <Word text={words.cold} />
                </td>
                <td className="n">
                  {r.warm.p50 === null ? '—' : secs(r.warm.p50)}
                  <span className="count">({r.warm.n})</span>
                  <Word text={words.warm} />
                </td>
                <td className="n">
                  {topLevelWords(r.topLevel)}
                  <Word text={words.topLevel} />
                </td>
                <td className="n">{r.per1k === null ? '—' : usdPer1k(r.per1k)}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

const Word = ({ text }: { text?: string }) => (text ? <span className="live-word">{text}</span> : null)
