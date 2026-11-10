import { CartesianGrid, ComposedChart, Line, ReferenceLine, ResponsiveContainer, Scatter, Tooltip, XAxis, YAxis } from 'recharts'
import { placeMarkLabels, POOL_COLORS, unlabelledCaption, type PhaseMark } from '../chartData'
import { secs } from '../format'
import type { SpecRow } from '../specRows'
import { yTickLabel, yTicks, type Trace } from '../trace'
import { axis, Frame, grid, NoData, Swatch, tooltip } from './common'

// x ticks are whole seconds (the axis is at least 10 s); y ticks can be fractions.
const seconds = (v: number) => `${Math.round(v)} s`

export function LatencyChart({
  rows,
  traces,
  extent,
  marks,
  notDrawn,
}: {
  rows: SpecRow[]
  traces: Trace[]
  extent: { xMax: number; yMax: number }
  marks: PhaseMark[]
  notDrawn: number
}) {
  const byKey = new Map(traces.map((t) => [t.key, t]))
  const drawn = traces.reduce((n, t) => n + t.dots.length + t.line.length, 0)
  const placed = placeMarkLabels(marks, extent.xMax)
  const unlabelled = unlabelledCaption(placed)
  const color = (r: SpecRow) => POOL_COLORS[r.poolIndex] ?? POOL_COLORS[0]
  const dashed = (r: SpecRow) => r.flashboot === 'FLASHBOOT'

  return (
    <Frame
      legend={
        <>
          {rows.map((r) => (
            <Swatch key={r.key} color={color(r)} dashed={dashed(r)} label={r.name} />
          ))}
          <span className="leg-item">
            <b className="leg-dot" />
            one first boot or cold start (hollow with FlashBoot on)
          </span>
          <span className="leg-item mut">line: median of the last 10 requests · dashed: FlashBoot on</span>
        </>
      }
    >
      {drawn === 0 ? (
        <NoData text="No completed requests yet." />
      ) : (
        <ResponsiveContainer width="100%" height={280}>
          <ComposedChart margin={{ top: 34, right: 16, bottom: 4, left: 4 }}>
            <CartesianGrid {...grid} vertical={false} />
            <XAxis type="number" dataKey="x" name="sent at" domain={[0, extent.xMax]} tickFormatter={seconds} allowDataOverflow {...axis} />
            {/* Same y max as the trace cells, so both read on one scale. */}
            <YAxis
              type="number"
              dataKey="y"
              name="took"
              domain={[0, extent.yMax]}
              ticks={yTicks(extent.yMax)}
              allowDataOverflow
              width={56}
              tickFormatter={yTickLabel}
              {...axis}
            />
            <Tooltip
              {...tooltip}
              shared={false}
              cursor={{ stroke: '#3a3d44', strokeDasharray: '3 3' }}
              labelFormatter={(x) => `sent at ${seconds(Number(x))}`}
              formatter={(v, name) => [secs(Number(v) * 1000), name]}
            />
            {placed.map((m) => (
              // Labels alternate between two heights so close phases don't overlap.
              <ReferenceLine
                key={m.label}
                x={m.x}
                stroke="#3a3d44"
                strokeDasharray="3 3"
                label={m.showLabel ? { value: m.label, position: 'insideTopLeft', fill: '#9a9890', fontSize: 11, dy: m.row * 15 - 16 } : undefined}
              />
            ))}
            {rows.map((r) => (
              <Line
                key={`${r.key}-line`}
                data={byKey.get(r.key)?.line ?? []}
                dataKey="y"
                name={r.name}
                type="linear"
                stroke={color(r)}
                strokeWidth={1.8}
                strokeDasharray={dashed(r) ? '6 4' : undefined}
                dot={false}
                activeDot={false}
                isAnimationActive={false}
              />
            ))}
            {rows.map((r) => (
              <Scatter
                key={`${r.key}-dots`}
                data={byKey.get(r.key)?.dots ?? []}
                dataKey="y"
                name={`${r.name}, single request`}
                fill={dashed(r) ? '#1b1d21' : color(r)}
                stroke={color(r)}
                isAnimationActive={false}
              />
            ))}
          </ComposedChart>
        </ResponsiveContainer>
      )}
      {drawn > 0 && unlabelled && <p className="cap">{unlabelled}</p>}
      {notDrawn > 0 && (
        <p className="cap">
          {notDrawn} request{notDrawn === 1 ? '' : 's'} didn't complete (left queued when a load level ended, failed or cut short) and{' '}
          {notDrawn === 1 ? "isn't" : "aren't"} drawn; the tables count them.
        </p>
      )}
    </Frame>
  )
}
