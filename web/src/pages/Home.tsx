import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useActiveRun } from '../ActiveRun'
import { getRun } from '../api'
import { configTone, Help, StatusBadge } from '../components'
import { cardName, configName, duration, latestResults, modeWord, runSubject, when } from '../format'
import { phaseWord, type LiveRun } from '../liveRun'
import { Section } from '../report/common'
import { SpecSummary } from '../report/ReportCharts'
import { RunHeading } from '../report/RunHeading'
import { errorToast } from '../toasts'
import { useToasts } from '../Toaster'
import type { Pool, Run, RunDetail } from '../types'
import { useCatalog } from '../useCatalog'
import { useNow } from '../useNow'

const LATEST = 3

export function Home() {
  const { runs, live } = useActiveRun()
  const catalog = useCatalog()
  const latest = latestResults(runs, LATEST)

  return (
    <>
      <h1>Home</h1>
      {live && <RunningNow live={live} catalog={catalog} />}
      {runs.length === 0 && (
        <div className="home-empty">
          <p>Start your first run.</p>
          <Link className="btn primary" to="/new">
            New run
          </Link>
        </div>
      )}
      {latest.length > 0 && (
        <Section title="Latest results" sub="The last three runs that ended, newest first: each one's spec table and finding.">
          {latest.map((r) => (
            <LatestResult key={r.id} run={r} catalog={catalog} />
          ))}
        </Section>
      )}
      {runs.length > 0 && (
        <Section title="History" sub="Every run, newest first.">
          <History runs={runs} />
        </Section>
      )}
    </>
  )
}

function RunningNow({ live, catalog }: { live: LiveRun; catalog?: Pool[] }) {
  const now = useNow()
  const { run, configs } = live.detail
  return (
    <section className="running-now" aria-label="Running now">
      <div className="row">
        <h2>
          <span className="pulse" aria-hidden="true" />
          Running now · {runSubject(run.params)}
        </h2>
        <Link className="act nowrap" to={`/runs/${run.id}`}>
          Open live view
        </Link>
      </div>
      <p className="mut small">
        {duration(now - new Date(run.createdAt).getTime())} elapsed
        <Help text="Time since the run started." />
        {configs.map((cd) => (
          <span key={cd.config.id} className="nowrap">
            {' · '}
            {configName({ ...cd.config, card: cd.config.card ? cardName(cd.config.card, catalog) : undefined })}:{' '}
            <span className={configTone(cd.config.status) || 'fg'}>{phaseWord(run.params, cd)}</span>
          </span>
        ))}
      </p>
    </section>
  )
}

function LatestResult({ run, catalog }: { run: Run; catalog?: Pool[] }) {
  const { push } = useToasts()
  const [detail, setDetail] = useState<RunDetail | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let current = true
    getRun(run.id)
      .then((d) => current && setDetail(d))
      .catch((err) => {
        if (!current) return
        setFailed(true)
        push(errorToast(`Couldn't load the run from ${when(run.createdAt)}`, err))
      })
    return () => {
      current = false
    }
  }, [run.id, run.createdAt, push])

  return (
    <div className="brief">
      <RunHeading run={run} catalog={catalog} />
      {detail ? (
        <SpecSummary detail={detail} catalog={catalog} />
      ) : (
        <p className="mut small">{failed ? "Couldn't load this run." : 'Loading…'}</p>
      )}
    </div>
  )
}

function History({ runs }: { runs: Run[] }) {
  return (
    <div className="table-wrap">
      <table className="spec history">
        <thead>
          <tr>
            <th>Started</th>
            <th>Tested</th>
            <th>Mode</th>
            <th>Status</th>
            <th>
              <span className="sr-only">Link</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id}>
              <td className="nowrap">{when(r.createdAt)}</td>
              <td>{runSubject(r.params)}</td>
              <td className="mut">{modeWord(r.params)}</td>
              <td>
                <StatusBadge status={r.status} />
              </td>
              <td className="link">
                <Link to={`/runs/${r.id}`}>{r.status === 'running' ? 'Open live view' : 'View report'}</Link>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
