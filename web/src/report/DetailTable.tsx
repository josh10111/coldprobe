import { gpuRows, rpColdCount, type GpuRow, type Series } from '../chartData'
import { Help } from '../components'
import { secs, sig3, usd3sf, usdPer1k } from '../format'
import type { LevelRow } from '../specRows'
import type { Summary } from '../types'

// p50 / p95.
const pair = (s: Summary) => (s.n > 0 ? `${secs(s.p50)} / ${secs(s.p95)}` : '—')

const Count = ({ n }: { n: number }) => <span className="count">({n})</span>

function ConfigCell({ s }: { s: { name: string; color: string; dashed: boolean } }) {
  return (
    <span className="nowrap">
      <span className="sw" style={{ borderTopColor: s.color, borderTopStyle: s.dashed ? 'dashed' : 'solid' }} />
      {s.name}
    </span>
  )
}

export function DetailTable({ series, coldLabel }: { series: Series[]; coldLabel: string }) {
  return (
    <>
      <div className="table-wrap">
        <table className="spec">
          <thead>
            <tr>
              <th>Configuration</th>
              <th className="n">
                First boot
                <Help text="Time for a brand-new endpoint's first worker to answer, image pull and boot included; one sample." />
              </th>
              <th className="n">
                {coldLabel === 'cold start' ? 'Cold start' : 'New-worker cold start'} <span className="count">typical / slowest 5% (n)</span>
                <Help text="The extra time a cold start added to a request (total minus handler time): the median, the 95th percentile, and how many samples." />
              </th>
              <th className="n">
                Warm <span className="count">typical / slowest 5% (n)</span>
                <Help text="A whole request to a running worker: the median, the 95th percentile, and how many samples." />
              </th>
              <th className="n">
                Completed
                <Help text="Requests that finished successfully, across every phase; the $/1k estimate divides by this." />
              </th>
              <th className="n">
                Failed
                <Help text="Requests Runpod reported as failed." />
              </th>
              <th className="n">
                Left queued
                <Help text="Requests still waiting when a load level's drain time ran out, or stopped by a cancel or the budget guard." />
              </th>
              <th className="n">
                Est. $ / 1k
                <Help text="Estimated cost per 1,000 completed requests: execution, boot and idle-timeout time at the pool's hourly price (the card's own price in card mode)." />
              </th>
              <th className="n rp">
                Runpod's boot count
                <Help text="How many worker boots Runpod itself counted for the endpoint; a cross-check, not one of coldprobe's measurements." />
              </th>
            </tr>
          </thead>
          <tbody>
            {series.map((s) => {
              const { summary: sum, config } = s.cd
              const kept = sum.cold.n === 0 && s.coldNote
              return (
                <tr key={s.key}>
                  <td>
                    <ConfigCell s={s} />
                    {config.error && <div className={config.status === 'failed' ? 'bad small' : 'mut small'}>{config.error}</div>}
                    {s.coldNote && (
                      <div className="warn small">
                        cold: {s.coldNote.text}
                        <Help text={s.coldNote.help} />
                      </div>
                    )}
                  </td>
                  <td className="n">
                    {sum.firstBootMs > 0 ? secs(sum.firstBootMs) : '—'}
                    <Count n={sum.firstBootMs > 0 ? 1 : 0} />
                  </td>
                  <td className="n">
                    {kept ? <span className="warn">{s.coldNote!.short}</span> : pair(sum.cold)}
                    <Count n={sum.cold.n} />
                  </td>
                  <td className="n">
                    {pair(sum.warm)}
                    <Count n={sum.warm.n} />
                  </td>
                  <td className="n">{sum.completed}</td>
                  <td className="n">{sum.failed}</td>
                  <td className="n">{sum.unfinished}</td>
                  {/* The first boot alone isn't a per-request cost. */}
                  <td className="n">{sum.completed > 1 ? usdPer1k(sum.costPer1k) : '—'}</td>
                  <td className="n rp">{rpColdCount(config.rpMetrics) ?? '—'}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      <p className="cap">
        Left queued: requests still waiting when a load level's drain time ran out (or stopped by a cancel or the budget
        guard). Expected when you offer more than the workers can serve; it is the signal that a pool is at capacity.
        Runpod's boot count: the first boot, our cold samples, and any worker Runpod added on its own under load; "—" when
        Runpod's metrics couldn't be read before teardown.
      </p>
    </>
  )
}

export function GpuTable({ series }: { series: Series[] }) {
  const rows: GpuRow[] = gpuRows(series)
  return (
    <>
      <div className="table-wrap">
        <table className="spec">
          <thead>
            <tr>
              <th>Configuration</th>
              <th>
                GPU model
                <Help text="The GPU Runpod reported for the worker that ran the jobs (gpuTypeId); MIG slices are named too." />
              </th>
              <th className="n">
                TFLOPS, typical <span className="count">(n)</span>
                <Help text="fp16 tera-FLOP/s the job measured (10 iterations, median) on that worker's GPU." />
              </th>
              <th className="n">
                Matmul ms <span className="count">typical / slowest 5%</span>
                <Help text="Time of one fp16 matmul at the run's matmul size, across the jobs: typical and the slowest 5%." />
              </th>
              <th className="n">
                $ / TFLOP-hr
                <Help text="The pool's hourly price (the card's own price in card mode) divided by the typical TFLOPS: what a sustained TFLOP costs per hour on this GPU." />
              </th>
              <th className="n">
                Cold CUDA init, typical <span className="count">(n)</span>
                <Help text="Time to initialise CUDA on a fresh worker (first boot and cold jobs only)." />
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.key}>
                <td>
                  <ConfigCell s={r} />
                </td>
                <td className="mono small">{r.gpuTypeId || 'unknown'}</td>
                <td className="n">
                  {r.tflopsP50 === null ? '—' : sig3(r.tflopsP50)}
                  <Count n={r.tflopsN} />
                </td>
                <td className="n">{r.matmulMsP50 === null || r.matmulMsP95 === null ? '—' : `${sig3(r.matmulMsP50)} / ${sig3(r.matmulMsP95)}`}</td>
                <td className="n">{r.usdPerTflopHr === null ? '—' : usd3sf(r.usdPerTflopHr)}</td>
                <td className="n">
                  {r.coldCudaInitP50 === null ? '—' : secs(r.coldCudaInitP50)}
                  <Count n={r.coldCudaInitN} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="cap">
        Rows appear for runs on the GPU worker image; TFLOPS and matmul time need a matmul size. Pools can place a run on
        several GPU models, so a configuration can have more than one row.
      </p>
    </>
  )
}

export function LevelTable({ rows }: { rows: LevelRow[] }) {
  return (
    <div className="table-wrap">
      <table className="spec">
        <thead>
          <tr>
            <th>Configuration</th>
            <th className="n">
              Offered
              <Help text="The load level: requests sent per second on a fixed schedule, whether or not earlier ones had returned." />
            </th>
            <th className="n">
              Completed
              <Help text="Requests at this level that finished successfully." />
            </th>
            <th className="n">
              Failed
              <Help text="Requests at this level that failed: Runpod reported them failed, or the request itself errored." />
            </th>
            <th className="n">
              Left queued
              <Help text="Requests at this level still waiting when its drain time ran out, or stopped by a cancel or the budget guard." />
            </th>
            <th className="n">
              Latency, slowest 5% <span className="count">(n)</span>
              <Help text="The wall time that 95% of this level's completed requests beat, counted from when each was scheduled to be sent." />
            </th>
            <th className="n">
              Completed req/s
              <Help text="Completed requests per second over the level, from its first scheduled send to its last completion." />
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.key}>
              <td>{r.name}</td>
              <td className="n">{r.rps} req/s</td>
              <td className="n">{r.completed}</td>
              <td className="n">{r.failed}</td>
              <td className="n">{r.leftQueued}</td>
              <td className="n">
                {r.p95Ms === null ? '—' : secs(r.p95Ms)}
                <Count n={r.latencyN} />
              </td>
              <td className="n">{sig3(r.throughputRps)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
