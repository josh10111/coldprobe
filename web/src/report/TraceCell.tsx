import { POOL_COLORS } from '../chartData'
import type { Trace } from '../trace'

const W = 150
const H = 28
const PAD = 3 // room for a dot's radius at the edges

// The big latency chart at row size.
export function TraceCell({
  trace,
  extent,
  poolIndex,
  dashed,
  label,
}: {
  trace: Trace
  extent: { xMax: number; yMax: number }
  poolIndex: number
  dashed: boolean
  label: string
}) {
  const color = POOL_COLORS[poolIndex] ?? POOL_COLORS[0]
  const sx = (x: number) => PAD + (x / extent.xMax) * (W - 2 * PAD)
  const sy = (y: number) => H - PAD - (y / extent.yMax) * (H - 2 * PAD)
  return (
    <svg className="trace" width={W} height={H} viewBox={`0 0 ${W} ${H}`} role="img" aria-label={label}>
      {trace.line.length > 0 && (
        <polyline
          points={trace.line.map((p) => `${sx(p.x).toFixed(1)},${sy(p.y).toFixed(1)}`).join(' ')}
          fill="none"
          stroke={color}
          strokeWidth={1.5}
          strokeDasharray={dashed ? '4 3' : undefined}
        />
      )}
      {trace.dots.map((p, i) => (
        <circle key={i} cx={sx(p.x)} cy={sy(p.y)} r={2} fill={dashed ? 'none' : color} stroke={color} strokeWidth={1} />
      ))}
    </svg>
  )
}
