import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { coldBreakdown, type Series } from '../chartData'
import { secs } from '../format'
import { axis, Frame, grid, NoData, Swatch, tooltip } from './common'

// --waiting and --measured as hex: Recharts takes colours as props.
const WAITING = '#b8862b'
const RUNNING = '#7fd1a4'

// Waiting is Runpod's delayTime (boot included); running is executionTime.
export function ColdBreakdownChart({ series, coldLabel }: { series: Series[]; coldLabel: string }) {
  const rows = coldBreakdown(series)
  const any = rows.some((r) => r.waiting !== null)

  return (
    <Frame
      title={`Where a ${coldLabel}'s time goes`}
      help="Typical waiting for a worker (Runpod's delayTime median), then typical handler time (executionTime median); network and client overhead isn't drawn."
      sub="Typical cold start split into waiting for a worker (Runpod's queue time, boot included) and running the handler."
      legend={
        <>
          <Swatch color={WAITING} label="waiting for a worker" />
          <Swatch color={RUNNING} label="handler" />
        </>
      }
    >
      {!any ? (
        <NoData text="No cold start completed." />
      ) : (
        <ResponsiveContainer width="100%" height={Math.max(120, 30 * rows.length + 40)}>
          <BarChart data={rows} layout="vertical" margin={{ top: 4, right: 12, bottom: 4, left: 4 }} barCategoryGap={6}>
            <CartesianGrid {...grid} horizontal={false} />
            <XAxis type="number" unit=" s" {...axis} />
            <YAxis type="category" dataKey="label" width={170} {...axis} />
            <Tooltip
              {...tooltip}
              cursor={{ fill: '#ffffff08' }}
              formatter={(v, name) => (v === null || v === undefined ? ['no data', name] : [secs(Number(v) * 1000), name])}
            />
            <Bar dataKey="waiting" stackId="t" name="waiting" fill={WAITING} isAnimationActive={false} />
            <Bar dataKey="running" stackId="t" name="handler" fill={RUNNING} isAnimationActive={false} />
          </BarChart>
        </ResponsiveContainer>
      )}
      <ul className="plain cap">
        {rows.map((r) => (
          <li key={r.key}>
            {r.label}:{' '}
            {r.waiting === null ? (
              r.note ? (
                <span className="warn">{r.note.short}, nothing to measure</span>
              ) : (
                'no cold start completed'
              )
            ) : (
              <>
                <span className="mono">{secs(r.waiting * 1000)}</span> waiting ·{' '}
                <span className="mono">{r.running === null ? '—' : secs(r.running * 1000)}</span> handler
                <span className="count">({r.n})</span>
              </>
            )}
          </li>
        ))}
      </ul>
    </Frame>
  )
}
