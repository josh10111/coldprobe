import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ApiError, cancelRun, getRun, listRuns, startRun, subscribe } from './api'
import { applyEvent, withSnapshot, type LiveRun } from './liveRun'
import {
  cancelRequestedToast,
  dismissKeysFor,
  errorToast,
  loadShownKeys,
  lostToast,
  missedConfigToasts,
  reconnectedToast,
  saveShownKeys,
  startedToast,
  toastFor,
  type ToastSpec,
} from './toasts'
import { useToasts } from './Toaster'
import type { RunBody } from './form'
import type { Estimate, Run, RunDetail, RunEvent } from './types'

// Follows the one running run (the server allows one at a time) from every
// page: run list, live state (snapshot + SSE), toasts and tab title.

interface ActiveRunApi {
  runs: Run[] // newest first
  activeId: string | null
  live: LiveRun | null // null until the snapshot loads
  start: (b: RunBody, est: Estimate, opts?: { allowLowStock?: boolean }) => Promise<string>
  cancel: () => Promise<void>
}

const ActiveRunContext = createContext<ActiveRunApi | null>(null)

export function useActiveRun(): ActiveRunApi {
  const api = useContext(ActiveRunContext)
  if (!api) throw new Error('useActiveRun must be used inside <ActiveRunProvider>')
  return api
}

const RESUBSCRIBE_MS = 5000

// Touching sessionStorage can throw (private mode, blocked site data).
function session(): Storage | undefined {
  try {
    return window.sessionStorage
  } catch {
    return undefined
  }
}

export function ActiveRunProvider({ children }: { children: ReactNode }) {
  const { push, dismiss } = useToasts()
  const [runs, setRuns] = useState<Run[]>([])
  const [activeId, setActiveId] = useState<string | null>(null)
  const [live, setLive] = useState<LiveRun | null>(null)
  const [attempt, setAttempt] = useState(0)

  const liveRef = useRef<LiveRun | null>(null)
  liveRef.current = live
  const shown = useRef({ runId: '', keys: new Set<string>() })
  const lost = useRef<number | null>(null) // the "can't reach" toast's id while offline
  // Sticky keyed toasts on screen (key -> toast id), so a later event can take them down.
  const openKeyed = useRef(new Map<string, number>())

  const refreshRuns = useCallback(async () => {
    try {
      const list = await listRuns()
      setRuns(list)
      setActiveId(list.find((r) => r.status === 'running')?.id ?? null)
    } catch (err) {
      push(errorToast("Couldn't load the run history", err))
    }
  }, [push])

  useEffect(() => {
    void refreshRuns()
  }, [refreshRuns])

  useEffect(() => {
    document.title = activeId ? '● running · coldprobe' : 'coldprobe'
  }, [activeId])

  useEffect(() => {
    if (!activeId) {
      setLive(null)
      return
    }
    const runId = activeId
    if (shown.current.runId !== runId) {
      shown.current = { runId, keys: loadShownKeys(session(), runId) }
      // Not in the cleanup: that also runs on a re-subscribe to the same run,
      // whose no-worker warning must still be dismissable.
      openKeyed.current.clear()
    }
    let stopped = false
    let retryTimer: ReturnType<typeof setTimeout> | undefined

    // Events arriving during a snapshot fetch are queued and replayed onto it
    // (applyEvent skips what the snapshot has). A newer fetch supersedes older.
    let generation = 0
    let queue: RunEvent[] | null = null
    let opened = false
    // catchUp: events may have been lost, so toast configs the snapshot shows finished.
    async function refetch(catchUp: boolean) {
      const mine = ++generation
      const buffered: RunEvent[] = []
      queue = buffered
      let detail: RunDetail | null = null
      try {
        detail = await getRun(runId)
      } catch (err) {
        if (!stopped && mine === generation) push(errorToast("Couldn't load the active run", err))
      }
      if (stopped || mine !== generation) return
      queue = null
      setLive((prev) => {
        const base = detail ? withSnapshot(prev, detail) : prev
        return base && buffered.reduce(applyEvent, base)
      })
      if (detail && catchUp) missedConfigToasts(detail, shown.current.keys).forEach(showToast)
      // Events alone can't build a first snapshot, so retry by re-subscribing.
      if (!detail && liveRef.current?.detail.run.id !== runId) {
        clearTimeout(retryTimer)
        retryTimer = setTimeout(() => setAttempt((n) => n + 1), RESUBSCRIBE_MS)
      }
    }

    function showToast(spec: ToastSpec | null) {
      if (!spec) return
      if (spec.key) {
        shown.current.keys.add(spec.key)
        saveShownKeys(session(), runId, shown.current.keys)
      }
      const id = push(spec)
      if (spec.key && spec.sticky) openKeyed.current.set(spec.key, id)
    }
    function dismissStale(e: RunEvent) {
      for (const key of dismissKeysFor(e, openKeyed.current.keys())) {
        dismiss(openKeyed.current.get(key)!)
        openKeyed.current.delete(key)
      }
    }
    const toastState = () => ({
      configs: liveRef.current?.detail.configs.map((c) => c.config) ?? [],
      shown: shown.current.keys,
    })

    async function finish(e: RunEvent) {
      // The final toast needs the summaries, which only the snapshot has.
      let fresh: RunDetail | null = null
      try {
        fresh = await getRun(runId)
      } catch {
      }
      const detail = fresh ?? (liveRef.current ? applyEvent(liveRef.current, e).detail : null)
      if (detail) showToast(toastFor({ type: 'run_finished', detail, ts: e.ts }, toastState()))
      if (stopped) return
      // Final status and summaries land together so the report's first frame is complete.
      setLive((prev) => prev && (fresh ? withSnapshot(prev, fresh) : applyEvent(prev, e)))
      // Cleared directly so a failing list call can't leave a run "active".
      setActiveId(null)
      void refreshRuns()
    }

    function onEvent(e: RunEvent) {
      const final = e.type === 'run_status' && e.data.status !== 'running'
      if (final) {
        void finish(e)
      } else if (queue) {
        queue.push(e)
      } else {
        setLive((prev) => (prev ? applyEvent(prev, e) : prev))
      }
      dismissStale(e)
      showToast(toastFor(e, toastState()))
    }

    function markLost() {
      if (lost.current === null) lost.current = push(lostToast())
    }

    const unsubscribe = subscribe(runId, onEvent, (s) => {
      if (s === 'open') {
        // Earlier events aren't replayed; the snapshot covers them.
        void refetch(opened || lost.current !== null)
        opened = true
        if (lost.current !== null) {
          dismiss(lost.current)
          lost.current = null
          push(reconnectedToast())
        }
      } else if (s === 'gap') {
        void refetch(true)
      } else if (s === 'lost') {
        markLost()
      } else {
        // 'closed': EventSource won't retry after a non-200 (e.g. the dev
        // proxy while Go is down), so re-subscribe ourselves.
        markLost()
        retryTimer = setTimeout(async () => {
          try {
            await getRun(runId)
            if (!stopped) setAttempt((n) => n + 1)
          } catch (err) {
            if (stopped) return
            if (err instanceof ApiError && err.status === 404) {
              // The run is gone (e.g. a different database): stop waiting.
              if (lost.current !== null) dismiss(lost.current)
              lost.current = null
              void refreshRuns()
            } else setAttempt((n) => n + 1)
          }
        }, RESUBSCRIBE_MS)
      }
    })

    return () => {
      stopped = true
      clearTimeout(retryTimer)
      unsubscribe()
    }
  }, [activeId, attempt, push, dismiss, refreshRuns])

  const start = useCallback(
    async (b: RunBody, est: Estimate, opts?: { allowLowStock?: boolean }) => {
      const id = await startRun(b, opts)
      push(startedToast(b, est.maxSpendUsd))
      setActiveId(id)
      void refreshRuns()
      return id
    },
    [push, refreshRuns],
  )

  const cancel = useCallback(async () => {
    if (!activeId) return
    try {
      await cancelRun(activeId)
      push(cancelRequestedToast())
    } catch (err) {
      push(errorToast('Cancel failed', err))
    }
  }, [activeId, push])

  const api = useMemo(
    () => ({ runs, activeId, live: live?.detail.run.id === activeId ? live : null, start, cancel }),
    [runs, activeId, live, start, cancel],
  )
  return <ActiveRunContext.Provider value={api}>{children}</ActiveRunContext.Provider>
}
