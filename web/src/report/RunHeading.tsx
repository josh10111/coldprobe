import { Link } from 'react-router-dom'
import { ModeBadge, StatusBadge } from '../components'
import { matmulSuffix, modeWord, reportTitle, when } from '../format'
import type { Pool, Run } from '../types'

// An h3 because callers wrap it in a Section, which owns the h2.
export function RunHeading({ run, catalog, tag }: { run: Run; catalog?: Pool[]; tag?: string }) {
  return (
    <div className="run-head">
      <div className="row">
        <h3>
          {tag && `${tag} · `}
          {reportTitle(run.params, catalog)} <StatusBadge status={run.status} />
        </h3>
        <Link className="act nowrap" to={`/runs/${run.id}`}>
          View report
        </Link>
      </div>
      <p className="mut small">
        {when(run.createdAt)} · <ModeBadge mode={modeWord(run.params)} />
        {matmulSuffix(run.params)}
      </p>
    </div>
  )
}
