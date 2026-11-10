import { buildSeries, coldGroupName, gpuRows, phaseMarks } from '../chartData'
import { chipsFor } from '../chips'
import { finding, flashbootReadouts } from '../finding'
import { isForced } from '../format'
import { levelRows, specRows } from '../specRows'
import { notDrawn, runTraces, traceExtent } from '../trace'
import type { Pool, RunDetail } from '../types'
import { ColdBreakdownChart } from './ColdBreakdownChart'
import { Section } from './common'
import { DetailTable, GpuTable, LevelTable } from './DetailTable'
import { FlashBootFrame } from './FlashBootFrame'
import { Finding } from './Finding'
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
        sub={'One row per configuration. "Typical" is the median; the small number is how many samples it rests on. The trace is that configuration\'s latency through the whole run, left to right, same scale on every row.'}
      >
        <SpecTable rows={rows} traces={traces} extent={extent} forced={forced} />
        <Finding finding={finding(rows, cardMode ? 'card' : 'pool')} />
      </Section>

      <Section
        title="Latency through the run"
        help="Each line point is the median wall time of that configuration's last 10 completed requests (fewer at the start), in send order, from the warm phase on."
        sub="Every completed request, as a rolling median of the last 10, so the line shows the trend without a cloud of dots. The first boot and single cold starts stay as dots because they are one request each. Dashed lines mark where each phase began."
      >
        <LatencyChart rows={rows} traces={traces} extent={extent} marks={phaseMarks(detail)} notDrawn={notDrawn(detail)} />
      </Section>

      <div className="grid2 section">
        <ColdBreakdownChart series={series} coldLabel={coldLabel} />
        <FlashBootFrame readouts={flashbootReadouts(rows, forced)} forced={forced} />
      </div>

      {gpuRows(series).length > 0 && (
        <Section title="GPU matmul, per GPU model" sub="fp16 matmul speed, its price and CUDA start-up for each GPU model the jobs ran on.">
          <GpuTable series={series} />
        </Section>
      )}

      <Section
        title="Every number, with its sample size"
        sub="coldprobe's own measurements per configuration. Runpod's own worker-boot count is kept in its own column because it counts things we didn't ask for (extra workers it added under load)."
      >
        <DetailTable series={series} coldLabel={coldLabel} />
      </Section>

      {levelRows(detail).length > 0 && (
        <Section title="Load levels" sub="Every load level, so nothing the chart smooths over is lost.">
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
