import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { getRun } from '../api'
import { modesDiffer, phaseMarks } from '../chartData'
import { Help } from '../components'
import { finding } from '../finding'
import { isForced, modeWord, runSubject, when } from '../format'
import { Section } from '../report/common'
import { Finding } from '../report/Finding'
import { LatencyChart } from '../report/LatencyChart'
import { RunHeading } from '../report/RunHeading'
import { SpecTable } from '../report/SpecTable'
import { specRows } from '../specRows'
import { notDrawn, runTraces, sharedExtent, traceExtent } from '../trace'
import type { RunDetail } from '../types'
import { useCatalog } from '../useCatalog'

const TAGS = ['A', 'B']

// /compare?a=&b=. Each run gets its own chart on identical axes rather than a
// third visual encoding.
export function ComparePage() {
  const [q] = useSearchParams()
  const a = q.get('a') ?? ''
  const b = q.get('b') ?? ''
  const catalog = useCatalog()
  const [runs, setRuns] = useState<RunDetail[] | null>(null)
  const [problem, setProblem] = useState<string | null>(null)

  useEffect(() => {
    let current = true
    setRuns(null)
    setProblem(null)
    Promise.all([getRun(a), getRun(b)])
      .then((rs) => current && setRuns(rs))
      .catch((err) => current && setProblem(err.message))
    return () => {
      current = false
    }
  }, [a, b])

  if (a === b) return <div className="problem" role="alert">Pick two different runs to compare.</div>
  if (problem) return <div className="problem" role="alert">{problem}</div>
  if (!runs) return <p className="mut">Loading…</p>

  const each = runs.map((d) => {
    const rows = specRows(d, catalog)
    const traces = runTraces(d)
    return { d, rows, traces, cardMode: (d.run.params.cards?.length ?? 0) > 0 }
  })
  const extent = sharedExtent(each.map((e) => traceExtent(e.traces)))
  const label = (d: RunDetail, i: number) => `${TAGS[i]} · ${when(d.run.createdAt)} · ${runSubject(d.run.params)} · ${modeWord(d.run.params)}`

  return (
    <>
      <h1>Compare</h1>
      {modesDiffer(runs.map((d) => d.run.params)) && (
        <p className="mut small">
          These runs measured cold starts differently: natural waits for workers to scale to zero, forced pauses the endpoint
          first. Compare their cold-start numbers with that in mind.
          <Help text="Forced cold starts always land on a brand-new worker (about 65 to 80 s). Natural cold starts measure what Runpod does on its own, which with FlashBoot can mean no cold start at all." />
        </p>
      )}

      <Section
        title="What you wait for and what you pay"
        sub={'Each run\'s spec table, one row per configuration. "Typical" is the median; the small number is how many samples it rests on. Every trace in both tables uses the same scales.'}
      >
        {each.map(({ d, rows, traces }, i) => (
          <div key={d.run.id} className="brief">
            <RunHeading run={d.run} catalog={catalog} tag={TAGS[i]} />
            <SpecTable rows={rows} traces={traces} extent={extent} forced={isForced(d.run.params)} />
          </div>
        ))}
      </Section>

      <Section
        title="Latency through the run"
        help="Each line point is the median wall time of that configuration's last 10 completed requests (fewer at the start), in send order, from the warm phase on."
        sub={`One chart per run, on identical axes (0 to ${Math.round(extent.xMax)} s across, the same latency scale up), so a higher or longer line is a real difference. Colour is the run's first or second pool or card, dashed is FlashBoot on, as in the report.`}
      >
        {each.map(({ d, rows, traces }, i) => (
          <div key={d.run.id} className="multiple">
            <h3 className="multiple-h">{label(d, i)}</h3>
            <LatencyChart rows={rows} traces={traces} extent={extent} marks={phaseMarks(d)} notDrawn={notDrawn(d)} />
          </div>
        ))}
      </Section>

      <Section title="Findings" sub="Each run's own finding, from its spec table above.">
        {each.map(({ d, rows, cardMode }, i) => {
          const f = finding(rows, cardMode ? 'card' : 'pool')
          return (
            <div key={d.run.id} className="multiple">
              <h3 className="multiple-h">{label(d, i)}</h3>
              {f ? <Finding finding={f} /> : <p className="mut small">No finding for this run: it has no configurations.</p>}
            </div>
          )
        })}
      </Section>
    </>
  )
}
