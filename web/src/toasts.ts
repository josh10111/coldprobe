import { ApiError } from './api'
import { coldHeadline } from './headlineToast'
import { finding } from './finding'
import { configLabel, duration, runSubject, statusLabel, usd } from './format'
import { specRows } from './specRows'
import type { RunBody } from './form'
import type { Config, RunDetail, RunEvent } from './types'

export type Tone = 'info' | 'done' | 'warn' | 'bad'

export interface ToastSpec {
  tone: Tone
  title: string
  body?: string
  sticky: boolean
  ms?: number // auto-dismiss delay; default 5 s
  link?: { to: string; label: string }
  action?: { label: string; onClick: () => void }
  // Run toasts with a key show at most once per run, so replayed events and
  // reconnects never repeat them.
  key?: string
}

export interface ToastState {
  configs: Config[]
  shown: ReadonlySet<string> // keys already toasted for this run
}

// Built from a re-fetched snapshot after the final run_status, for the summaries.
export interface RunFinished {
  type: 'run_finished'
  detail: RunDetail
  ts: string // the final event's time, if the snapshot has no finishedAt
}

export const BUDGET_WARN_FRACTION = 0.8

export function toastFor(input: RunEvent | RunFinished, state: ToastState): ToastSpec | null {
  const spec = build(input, state)
  if (spec?.key && state.shown.has(spec.key)) return null
  return spec
}

function build(input: RunEvent | RunFinished, state: ToastState): ToastSpec | null {
  switch (input.type) {
    case 'config_status': {
      const { status, detail } = input.data
      if (status !== 'done' && status !== 'cancelled' && status !== 'failed') return null
      const c = state.configs.find((x) => x.id === input.configId)
      const name = c ? configLabel(c) : `config ${input.configId}`
      const key = `config:${input.configId}:${status}`
      if (status === 'done') return { tone: 'done', title: `${name} done`, body: detail, sticky: false, key }
      if (status === 'cancelled') return { tone: 'warn', title: `${name} cancelled`, body: detail, sticky: false, key }
      return { tone: 'bad', title: `${name} failed`, body: detail ?? 'No reason given.', sticky: true, key }
    }
    case 'cost': {
      const { boundUsd, capUsd } = input.data
      if (capUsd <= 0 || boundUsd <= BUDGET_WARN_FRACTION * capUsd) return null
      return {
        tone: 'warn',
        title: 'Spend bound past 80% of the budget',
        body: `At most ${usd(boundUsd)} of ${usd(capUsd)} so far. The budget guard stops the run at ${usd(capUsd)}.`,
        sticky: true,
        key: 'budget',
      }
    }
    case 'log': {
      // Only "warn" lines (no worker placed yet) are toasted.
      if (input.data.level !== 'warn' || input.configId === undefined) return null
      const c = state.configs.find((x) => x.id === input.configId)
      const name = c ? configLabel(c) : `config ${input.configId}`
      return { tone: 'warn', title: `${name} has no worker yet`, body: input.data.msg, sticky: true, key: warnKey(input.configId) }
    }
    case 'run_finished':
      return runFinished(input)
    default:
      // run_status waits for the re-fetched snapshot (RunFinished).
      return null
  }
}

const warnKey = (configId: number) => `warn:${configId}`

// Which open toasts an event makes stale: a config's no-worker warning goes
// when its first boot ends or it stops; every warning goes when the run ends.
export function dismissKeysFor(e: RunEvent, openKeys: Iterable<string>): string[] {
  const open = [...openKeys]
  switch (e.type) {
    case 'run_status':
      return e.data.status === 'running' ? [] : open.filter((k) => k.startsWith('warn:'))
    case 'job':
      return e.data.phase === 'first_boot' ? open.filter((k) => k === warnKey(e.data.configId)) : []
    case 'config_status': {
      const s = e.data.status
      const stillBooting = s === 'pending' || s === 'creating' || s === 'running'
      return stillBooting || e.configId === undefined ? [] : open.filter((k) => k === warnKey(e.configId!))
    }
    default:
      return []
  }
}

function runFinished({ detail, ts }: RunFinished): ToastSpec {
  const run = detail.run
  const ended = new Date(run.finishedAt ?? ts).getTime()
  const took = duration(ended - new Date(run.createdAt).getTime())
  const tone: Tone =
    run.status === 'completed' ? 'done' : run.status === 'cancelled' || run.status === 'aborted_budget' ? 'warn' : 'bad'
  // aborted_crash is the one status the server gives no error for.
  let body: string | undefined
  if (run.status === 'completed') {
    const f = finding(specRows(detail), (run.params.cards?.length ?? 0) > 0 ? 'card' : 'pool')
    body = f?.sentences[0] ?? coldHeadline(detail) ?? 'No cold start completed.'
  }
  else if (run.status === 'aborted_crash') body = 'coldprobe stopped mid-run.'
  else body = run.error
  return {
    tone,
    title: `Run ${statusLabel[run.status]} · ${took}`,
    body,
    sticky: true,
    link: { to: `/runs/${run.id}`, label: 'View report' },
    key: 'final',
  }
}

export function startedToast(p: RunBody, maxSpendUsd: number): ToastSpec {
  const n = (p.cards?.length || (p.pools ?? []).length) * p.flashboot.length
  return {
    tone: 'info',
    title: 'Run started',
    body: `${n} config${n === 1 ? '' : 's'} · ${runSubject(p)}${p.coldStartMode === 'forced' ? ' · forced cold starts' : ''} · may spend up to ${usd(maxSpendUsd)}`,
    sticky: false,
  }
}

export function cancelRequestedToast(): ToastSpec {
  return { tone: 'info', title: 'Cancel requested', body: 'The endpoints are deleted within 30 s.', sticky: false }
}

// 8 s rather than 5: an error takes longer to read.
export function errorToast(title: string, err: unknown): ToastSpec {
  return { tone: 'bad', title, body: err instanceof Error ? err.message : String(err), sticky: false, ms: 8000 }
}

// Fallback for servers that predate the "no_stock" error code.
const NO_STOCK_SUFFIX = 'has no stock right now'

// The form keeps one refusal toast at a time, dismissing the previous one on retry.
export const START_REFUSED_KEY = 'start-refused'

// isLowStock reports the one stock refusal the user may override with "Start anyway".
export function isLowStock(err: unknown): err is ApiError {
  return err instanceof ApiError && err.status === 400 && err.code === 'low_stock'
}

export function startFailedToast(err: unknown): ToastSpec {
  if (err instanceof ApiError && err.status === 400) {
    if (err.code === 'low_stock' || err.code === 'no_stock' || err.message.endsWith(NO_STOCK_SUFFIX))
      return { tone: 'bad', title: 'Start refused', body: err.message, sticky: true, key: START_REFUSED_KEY }
  }
  return errorToast('Start failed', err)
}

export function lostToast(): ToastSpec {
  return {
    tone: 'bad',
    title: "Can't reach the coldprobe server",
    body: 'Retrying. The run carries on on the server.',
    sticky: true,
  }
}

export function reconnectedToast(): ToastSpec {
  return { tone: 'done', title: 'Reconnected to the server', body: 'Live updates resumed.', sticky: false }
}

// Configs may have finished while events were lost (reconnect or seq gap);
// toast each final one from the fresh snapshot, deduped by key.
export function missedConfigToasts(d: RunDetail, shown: ReadonlySet<string>): ToastSpec[] {
  const state = { configs: d.configs.map((c) => c.config), shown }
  const specs: ToastSpec[] = []
  for (const { config } of d.configs) {
    const e: RunEvent = {
      runId: d.run.id,
      seq: 0,
      ts: '',
      type: 'config_status',
      configId: config.id,
      data: { status: config.status, detail: config.error },
    }
    const spec = toastFor(e, state)
    if (spec) specs.push(spec)
  }
  return specs
}

// Toasted keys persist in sessionStorage so a reload doesn't repeat them.
// Storage can be missing or throw (private mode); then keys last until reload.
type KeyStore = Pick<Storage, 'getItem' | 'setItem'>
const storageKey = (runId: string) => `coldprobe:toasts:${runId}`

export function loadShownKeys(store: KeyStore | undefined, runId: string): Set<string> {
  try {
    const raw = store?.getItem(storageKey(runId))
    return new Set(raw ? (JSON.parse(raw) as string[]) : [])
  } catch {
    return new Set()
  }
}

export function saveShownKeys(store: KeyStore | undefined, runId: string, keys: ReadonlySet<string>) {
  try {
    store?.setItem(storageKey(runId), JSON.stringify([...keys]))
  } catch {
    // best effort
  }
}
