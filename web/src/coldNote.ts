import type { ConfigDetail, Params } from './types'

// The runner waits this long for workers to stop before each cold sample and
// skips the rest if they don't (common with FlashBoot). Mirrors
// Timeouts.ScaleDown in internal/bench/params.go; the API doesn't expose it.
export const SCALE_DOWN_TIMEOUT_MIN = 3

export interface ColdNote {
  taken: number
  requested: number
  short: string // for a tile value or a table cell
  text: string
  help: string
}

const FLASHBOOT_HELP =
  'FlashBoot kept the worker that served the last request running, so there was no cold start to measure. coldprobe only counts a request as cold after every worker has stopped. Forced mode avoids this by pausing the endpoint.'
const OFF_HELP = `With FlashBoot off, Runpod still kept the worker up past coldprobe's ${SCALE_DOWN_TIMEOUT_MIN}-minute wait, so a new request would have hit a warm worker. coldprobe only counts a request as cold after every worker has stopped, so it skipped the remaining samples. Forced mode avoids this by pausing the endpoint.`

// Cancelled or failed configs get no note: their status tells that story.
export function coldNote(p: Params, cd: ConfigDetail): ColdNote | null {
  // Forced mode pauses the endpoint, so nothing is ever kept warm.
  if (p.coldStartMode === 'forced') return null
  const status = cd.config.status
  if (status === 'cancelled' || status === 'failed') return null
  const taken = cd.jobs.filter((j) => j.phase === 'cold').length
  const requested = p.coldStarts
  if (taken >= requested) return null
  const movedOn = status === 'done' || cd.jobs.some((j) => j.phase === 'warm' || j.phase === 'load')
  if (!movedOn) return null

  const flash = cd.config.flashboot === 'FLASHBOOT'
  const stayedUp = `workers stayed up past the ${SCALE_DOWN_TIMEOUT_MIN} min wait`
  let text: string
  if (flash) text = taken === 0 ? `kept warm: no cold start within ${SCALE_DOWN_TIMEOUT_MIN} min` : `${taken} of ${requested} (kept warm after that)`
  else text = taken === 0 ? `no cold start: ${stayedUp}` : `${taken} of ${requested} (then ${stayedUp})`
  return { taken, requested, short: flash ? 'kept warm' : 'no scale-down', text, help: flash ? FLASHBOOT_HELP : OFF_HELP }
}
