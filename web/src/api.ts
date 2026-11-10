import { EVENT_TYPES } from './types'
import type { RunBody } from './form'
import type { Defaults, Estimate, Pool, Run, RunDetail, RunEvent } from './types'

// ApiError's message is the RFC 9457 problem detail. status is 0 when the
// server couldn't be reached; code is set only on stock refusals.
export class ApiError extends Error {
  status: number
  code?: string
  constructor(status: number, message: string, code?: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

async function request(path: string, init?: RequestInit): Promise<Response> {
  let res: Response
  try {
    res = await fetch(path, init)
  } catch {
    throw new ApiError(0, "can't reach the coldprobe server")
  }
  if (!res.ok) {
    // The Vite dev proxy answers in plain text when Go is down.
    let detail = `${res.status} ${res.statusText}`.trim()
    let code: string | undefined
    try {
      const body = await res.json()
      if (typeof body.detail === 'string') detail = body.detail
      if (typeof body.code === 'string') code = body.code
    } catch {
      // Not JSON; keep the status line.
    }
    throw new ApiError(res.status, detail, code)
  }
  return res
}

async function getJSON<T>(path: string): Promise<T> {
  return (await request(path)).json()
}

// The server answers 415 to a POST without this Content-Type.
async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await request(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  return res.json()
}

export const getGpus = () => getJSON<Pool[]>('/api/gpus')
export const getDefaults = () => getJSON<Defaults>('/api/defaults')
export const listRuns = () => getJSON<Run[]>('/api/runs')
export const getRun = (id: string) => getJSON<RunDetail>(`/api/runs/${encodeURIComponent(id)}`)
export const estimate = (b: RunBody) => postJSON<Estimate>('/api/runs/estimate', b)

// allowLowStock is sent only when true, so a normal Start's body is unchanged.
export async function startRun(b: RunBody, opts: { allowLowStock?: boolean } = {}): Promise<string> {
  const body = opts.allowLowStock ? { ...b, allowLowStock: true } : b
  return (await postJSON<{ id: string }>('/api/runs', body)).id
}

export async function cancelRun(id: string): Promise<void> {
  await request(`/api/runs/${encodeURIComponent(id)}/cancel`, { method: 'POST' })
}

// Detects a gap in SSE seq. Seq 0 is outside the numbering, and a
// connection's first seq is only a baseline.
export function checkSeq(last: number | null, seq: number): { gap: boolean; last: number | null } {
  if (seq === 0) return { gap: false, last }
  if (last === null) return { gap: false, last: seq }
  return { gap: seq > last + 1, last: seq }
}

// open and gap both mean "re-fetch the snapshot": the server doesn't replay
// earlier events. lost: EventSource is retrying. closed: it gave up (non-200).
export type StreamStatus = 'open' | 'gap' | 'lost' | 'closed'

export function subscribe(
  runId: string,
  onEvent: (e: RunEvent) => void,
  onStatus?: (s: StreamStatus) => void,
): () => void {
  const es = new EventSource(`/api/runs/${encodeURIComponent(runId)}/events`)
  let last: number | null = null

  es.onopen = () => {
    last = null
    onStatus?.('open')
  }
  es.onerror = () => {
    onStatus?.(es.readyState === EventSource.CLOSED ? 'closed' : 'lost')
  }

  // Every frame has an `event:` line, so onmessage never fires.
  for (const type of EVENT_TYPES) {
    es.addEventListener(type, (m) => {
      const e = JSON.parse((m as MessageEvent<string>).data) as RunEvent
      // Otherwise EventSource reconnects when the server ends the stream.
      if (e.type === 'run_status' && e.data.status !== 'running') es.close()
      const check = checkSeq(last, e.seq)
      last = check.last
      onEvent(e)
      if (check.gap) onStatus?.('gap')
    })
  }

  return () => es.close()
}
