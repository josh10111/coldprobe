import { useState } from 'react'
import { Link, NavLink, Outlet } from 'react-router-dom'
import { useActiveRun } from './ActiveRun'
import { StatusBadge } from './components'
import { modeWord, runSubject, when } from './format'

export function Layout() {
  const { runs } = useActiveRun()
  const [picked, setPicked] = useState<string[]>([])

  const toggle = (id: string) =>
    setPicked((p) => (p.includes(id) ? p.filter((x) => x !== id) : p.length < 2 ? [...p, id] : p))

  return (
    <div className="shell">
      <aside className="side">
        <Link className="brand" to="/">
          coldprobe
        </Link>
        <Link className="btn primary block" to="/new">
          New run
        </Link>
        <NavLink to="/" end className={({ isActive }) => (isActive ? 'nav sel' : 'nav')}>
          Home
        </NavLink>
        <h2 className="side-h">History</h2>
        {runs.length === 0 && <p className="mut small">No runs yet.</p>}
        <ul className="runs">
          {runs.map((r) => (
            <li key={r.id} className="run-row">
              <NavLink to={`/runs/${r.id}`} className={({ isActive }) => (isActive ? 'run sel' : 'run')}>
                <div>
                  {r.status === 'running' && <span className="pulse" aria-hidden="true" />}
                  {when(r.createdAt)}
                </div>
                <div className="small ellipsis" title={runSubject(r.params)}>
                  {runSubject(r.params)}
                </div>
                <div className="small">
                  <span className="mut">{modeWord(r.params)}</span> <StatusBadge status={r.status} />
                </div>
              </NavLink>
              <input
                type="checkbox"
                aria-label={`Compare the run from ${when(r.createdAt)}`}
                checked={picked.includes(r.id)}
                disabled={!picked.includes(r.id) && picked.length >= 2}
                onChange={() => toggle(r.id)}
              />
            </li>
          ))}
        </ul>
        {picked.length === 2 ? (
          <Link className="act" to={`/compare?${new URLSearchParams({ a: picked[0], b: picked[1] })}`}>
            Compare these 2
          </Link>
        ) : (
          runs.length > 1 && <p className="mut small">Tick 2 runs to compare.</p>
        )}
      </aside>
      <main className="main">
        <Outlet />
      </main>
    </div>
  )
}
