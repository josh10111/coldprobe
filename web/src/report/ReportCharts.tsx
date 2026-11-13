import { buildSeries, coldGroupName, gpuRows, phaseMarks } from '../chartData'
import { chipsFor } from '../chips'
import { finding, flashbootReadouts } from '../finding'
import { isForced } from '../format'
import { levelRows, LOAD_LEVELS_SUB, loadLevelsNote, specRows } from '../specRows'
import { notDrawn, runTraces, traceExtent } from '../trace'
import type { Pool, RunDetail } from '../types'
import { ColdBreakdownChart } from './ColdBreakdownChart'
import { Section } from './common'
import { DetailTable, GpuTable, LevelTable } from './DetailTable'
import { FlashBootFrame } from './FlashBootFrame'
import { Finding } from './Finding'
import { matmulFinding } from '../matmulFinding'
import { LatencyChart } from './LatencyChart'
import { PoolChips } from './PoolChips'
import { SpecTable } from './SpecTable'

// `catalog` is undefined when /api/gpus couldn't be fetched.
export function ReportCharts({ detail, catalog }: { detail: RunDetail; catalog?: Pool[] }) {
  const forced = isForced(detail.run.params)
  const cardMode = (detail.run.params.cards?.length ?? 0) > 0
  const rows = specRows(detail, catalog)
  const traces = runTraces(detail)
  const extent = traceExtent(traces)
  const series = buildSeries([{ detail, catalog }])
  const coldLabel = coldGroupName([{ detail }])

  return (
    <>
      <Section
        title={cardMode ? 'Cards in this run' : 'Pools in this run'}
        sub={
          cardMode
            ? 'Each card was pinned within its pool; the card Runpod reported is recorded per job.'
            : 'A pool is Runpod\'s unit of GPU selection. Each lists the cards it may hand out; the card you actually got is recorded per job.'
        }
      >
        <PoolChips chips={chipsFor(detail, catalog)} />
      </Section>

      <Section
        title="What you wait for and what you pay"
        sub={'One row per configuration. "Typical" is the median; the small number is how many samples it rests on.'}
        more="The trace is that configuration's latency through the whole run, left to right, on the same scale on every row."
      >
        <SpecTable rows={rows} traces={traces} extent={extent} forced={forced} />
        <Finding finding={finding(rows, cardMode ? 'card' : 'pool')} />
      </Section>

      <Section
        title="Latency through the run"
        help="Each line point is the median wall time of that configuration's last 10 completed requests (fewer at the start), in send order, from the warm phase on."
        sub="Every completed request, as a rolling median of the last 10."
        more="The median keeps the line showing the trend instead of a cloud of dots. The first boot and single cold starts stay as dots because they are one request each. Dashed lines mark where each phase began."
      >
        <LatencyChart rows={rows} traces={traces} extent={extent} marks={phaseMarks(detail)} notDrawn={notDrawn(detail)} />
      </Section>

      <div className="grid2 section">
        <ColdBreakdownChart series={series} coldLabel={coldLabel} />
        <FlashBootFrame readouts={flashbootReadouts(rows, forced)} forced={forced} />
      </div>

      {gpuRows(series).length > 0 && (
        <Section
          title="GPU matmul, per GPU model"
          sub="fp16 matmul speed, its price and CUDA start-up for each GPU model the jobs ran on."
          more="Rows appear for runs on the GPU worker image; TFLOPS and matmul time need a matmul size. Pools can place a run on several GPU models, so a configuration can have more than one row."
        >
          <GpuTable series={series} />
          <Finding finding={matmulFinding(detail, catalog)} />
        </Section>
      )}

      <Section
        title="Every number, with its sample size"
        sub="coldprobe's own measurements per configuration."
        more={
          <>
            <p>
              Left queued: requests still waiting when a load level's drain time ran out (or stopped by a cancel or the budget guard).
              Expected when you offer more than the workers can serve; it is the signal that a pool is at capacity.
            </p>
            <p>
              Runpod's boot count is kept in its own column because it counts things we didn't ask for: the first boot, our cold
              samples, and any worker Runpod added on its own under load. "—" when Runpod's metrics couldn't be read before teardown.
            </p>
          </>
        }
      >
        <DetailTable series={series} coldLabel={coldLabel} />
      </Section>

      {levelRows(detail).length > 0 && (
        <Section title="Load levels" sub={LOAD_LEVELS_SUB} more={loadLevelsNote(detail.run.params)}>
          <LevelTable rows={levelRows(detail, catalog)} />
        </Section>
      )}
    </>
  )
}

// The spec table and finding alone, for Home's latest results.
export function SpecSummary({ detail, catalog }: { detail: RunDetail; catalog?: Pool[] }) {
  const rows = specRows(detail, catalog)
  const traces = runTraces(detail)
  return (
    <>
      <SpecTable rows={rows} traces={traces} extent={traceExtent(traces)} forced={isForced(detail.run.params)} />
      <Finding finding={finding(rows, (detail.run.params.cards?.length ?? 0) > 0 ? 'card' : 'pool')} />
    </>
  )
}
