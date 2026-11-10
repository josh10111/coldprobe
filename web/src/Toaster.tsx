import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import type { ToastSpec } from './toasts'

const AUTO_DISMISS_MS = 5000

interface Toast extends ToastSpec {
  id: number
}

interface ToastApi {
  push: (t: ToastSpec) => number
  dismiss: (id: number) => void
}

const ToastContext = createContext<ToastApi | null>(null)

export function useToasts(): ToastApi {
  const api = useContext(ToastContext)
  if (!api) throw new Error('useToasts must be used inside <ToastProvider>')
  return api
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const nextId = useRef(1)

  const dismiss = useCallback((id: number) => setToasts((ts) => ts.filter((t) => t.id !== id)), [])

  const push = useCallback(
    (spec: ToastSpec) => {
      const id = nextId.current++
      setToasts((ts) => [...ts, { ...spec, id }])
      if (!spec.sticky) setTimeout(() => dismiss(id), spec.ms ?? AUTO_DISMISS_MS)
      return id
    },
    [dismiss],
  )

  const api = useMemo(() => ({ push, dismiss }), [push, dismiss])

  return (
    <ToastContext.Provider value={api}>
      {children}
      {/* The live region must exist before toasts are added, or screen readers may not announce them. */}
      <div className="toasts" aria-live="polite">
        {toasts.map((t) => (
          <div key={t.id} className={`toast tone-${t.tone}`} role={t.tone === 'bad' ? 'alert' : undefined}>
            <button type="button" className="toast-x" aria-label="Dismiss" onClick={() => dismiss(t.id)}>
              ×
            </button>
            <b>{t.title}</b>
            {t.body && <span className="mut">{t.body}</span>}
            {t.action && (
              <button
                type="button"
                className="btn act-btn"
                onClick={() => {
                  dismiss(t.id)
                  t.action!.onClick()
                }}
              >
                {t.action.label}
              </button>
            )}
            {t.link && (
              <Link className="act" to={t.link.to} onClick={() => dismiss(t.id)}>
                {t.link.label}
              </Link>
            )}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}
