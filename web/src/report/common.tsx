import type { ReactNode } from 'react'
import { Help } from '../components'

// Shared report pieces. Legends are HTML, easier to style than Recharts' own.

export function More({ children }: { children: ReactNode }) {
  return (
    <details className="more">
      <summary>How to read this</summary>
      <div className="more-body">{children}</div>
    </details>
  )
}

export function Section({
  title,
  help,
  sub,
  more,
  children,
}: {
  title: string
  help?: string
  sub?: ReactNode
  more?: ReactNode
  children: ReactNode
}) {
  return (
    <section className="section" aria-label={title}>
      <h2>
        {title}
        {help && <Help text={help} />}
      </h2>
      {sub && <p className="section-sub">{sub}</p>}
      {more && <More>{more}</More>}
      {children}
    </section>
  )
}

export function Frame({ title, help, sub, legend, children }: { title?: string; help?: string; sub?: ReactNode; legend?: ReactNode; children: ReactNode }) {
  return (
    <div className="frame" aria-label={title}>
      {title && (
        <h2>
          {title}
          {help && <Help text={help} />}
        </h2>
      )}
      {sub && <p className="section-sub">{sub}</p>}
      {legend && <div className="leg">{legend}</div>}
      {children}
    </div>
  )
}

export function Swatch({ color, dashed, label }: { color: string; dashed?: boolean; label: string }) {
  return (
    <span className="leg-item">
      <i style={{ borderTopColor: color, borderTopStyle: dashed ? 'dashed' : 'solid' }} />
      {label}
    </span>
  )
}

export const NoData = ({ text }: { text: string }) => <p className="mut no-data">{text}</p>

// Recharts takes colours as props, so the CSS tokens are repeated here as hex.
export const axis = { stroke: '#3a3d44', tick: { fill: '#9a9890', fontSize: 11 } }
export const grid = { stroke: '#2a2c31' }
export const tooltip = {
  contentStyle: { background: '#202328', border: '1px solid #3a3d44', borderRadius: 4, fontSize: 12.5 },
  labelStyle: { color: '#e4e2dc' },
  itemStyle: { color: '#c9c6bd' },
}
