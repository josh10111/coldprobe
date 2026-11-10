import { useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import { useActiveRun } from '../ActiveRun'
import { getRun } from '../api'
import { totalCost } from '../chartData'
import { Help, MODE_HELP, StatusBadge } from '../components'
import { duration, matmulSuffix, modeWord, reportTitle, usd, when } from '../format'
import { LiveView } from '../LiveView'
import { ReportCharts } from '../report/ReportCharts'
import type { Pool, RunDetail } from '../types'
import { useCatalog } from '../useCatalog'

// The live view while the run is going, the report once it has ended.
export function RunPage() {
  const { id = '' } = useParams()
  const { live, activeId, cancel } = useActiveRun()
  const [fetched, setFetched] = useState<RunDetail | null>(null)
  const [problem, setProblem] = useState<string | null>(null)
  const catalog = useCatalog()

  // Also re-fetched when the active run changes: the final snapshot has the summaries.
  useEffect(() => {
    let current = true
    setProblem(null)
    getRun(id)
      .then((d) => current && setFetched(d))
      .catch((err) => current && setProblem(err.message))
    return () => {
      current = false
    }
  }, [id, activeId])

  const isActive = id === activeId && live?.detail.run.id === id
  // Keep the fallback fresh, or the live view flashes back when the provider
  // lets go of the run before the re-fetch lands.
  useEffect(() => {
    if (isActive) setFetched(live.detail)
  }, [isActive, live])
  const detail = isActive ? live.detail : fetched?.run.id === id ? fetched : null
  if (problem && !detail) return <div className="problem" role="alert">{problem}</div>
  if (!detail) return <p className="mut">Loading…</p>

  if (detail.run.status === 'running') {
    return <LiveView detail={detail} live={isActive ? live : null} catalog={catalog} onCancel={id === activeId ? cancel : undefined} />
  }
  return <Report detail={detail} catalog={catalog} />
}

const ENDED: Record<string, string> = {
  cancelled: 'Cancelled',
  aborted_budget: 'Stopped by the budget guard',
  failed: 'Failed',
  aborted_crash: 'Crashed',
}

function Report({ detail, catalog }: { detail: RunDetail; catalog?: Pool[] }) {
  const { run } = detail
  const p = run.params
  const took = run.finishedAt ? duration(new Date(run.finishedAt).getTime() - new Date(run.createdAt).getTime()) : undefined
  const why = run.error ?? (run.status === 'aborted_crash' ? 'coldprobe stopped mid-run' : undefined)
  const mode = modeWord(p)

  return (
    <>
      <h1>
        {reportTitle(p, catalog)} <StatusBadge status={run.status} />
      </h1>
      <p className="mut params">
        {when(run.createdAt)}
        {took && ` · ${took}`} · {p.maxWorkers} worker{p.maxWorkers === 1 ? '' : 's'} max · {p.jobSleepMs} ms jobs ·{' '}
        {p.levelsRps.join(' / ')} req/s for {p.levelSeconds} s each · {mode} cold starts
        <Help text={MODE_HELP[mode]} />
        {matmulSuffix(p)} · estimated cost <span className="price">{usd(totalCost(detail))}</span> of the{' '}
        <span className="mono">{usd(run.budgetUsd)}</span> budget
        <Help text="The sum of each configuration's estimated cost: execution, boot and idle-timeout time at the pool's hourly price (the card's own price in card mode)." />{' '}
        · guard bound at most <span className="price">{usd(run.estMaxUsd)}</span>
        <Help text="The most the run could have spent, from the estimate at start: every endpoint billed for max workers + 1 through every phase's whole timeout, or the budget if that is lower, since the guard stops the run there." />
      </p>
      {run.status !== 'completed' && (
        <p className={`status-line ${run.status === 'failed' || run.status === 'aborted_crash' ? 'bad' : 'warn'}`}>
          {ENDED[run.status]}
          {why && `: ${why}`}. The numbers below cover what finished before that.
        </p>
      )}
      <ReportCharts detail={detail} catalog={catalog} />
    </>
  )
}
