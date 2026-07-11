import { ReactNode, useCallback, useEffect, useState } from 'react'
import { ApiError } from '../api/client'

export function Card({ title, children }: { title?: ReactNode; children: ReactNode }) {
  return (
    <section className="card">
      {title && <h2 className="card-title">{title}</h2>}
      {children}
    </section>
  )
}

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
    </label>
  )
}

export function ErrorBanner({ error }: { error: string | null }) {
  if (!error) return null
  return <div className="banner banner-error">{error}</div>
}

export function SuccessBanner({ message }: { message: string | null }) {
  if (!message) return null
  return <div className="banner banner-success">{message}</div>
}

export function Flag({ on, label }: { on: boolean; label: string }) {
  return <span className={on ? 'flag flag-on' : 'flag'}>{label}</span>
}

export function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      className="btn btn-small"
      onClick={() => {
        navigator.clipboard.writeText(value).then(() => {
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        })
      }}
    >
      {copied ? 'Copied!' : 'Copy'}
    </button>
  )
}

// Fetches API data on mount (and whenever deps change), with a manual reload.
export function useApiData<T>(fetcher: () => Promise<T>, deps: unknown[] = []) {
  const [data, setData] = useState<T | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [tick, setTick] = useState(0)

  useEffect(() => {
    let cancelled = false
    setBusy(true)
    setError(null)
    fetcher()
      .then((d) => {
        if (!cancelled) setData(d)
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof ApiError ? e.message : String(e))
      })
      .finally(() => {
        if (!cancelled) setBusy(false)
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- fetcher is intentionally not a dep
  }, [tick, ...deps])

  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { data, error, busy, reload }
}

// Wraps an async API action with loading/error/success state.
export function useApiAction() {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [message, setMessage] = useState<string | null>(null)

  const run = useCallback(
    async <T,>(action: () => Promise<T>, successMessage?: string): Promise<T | undefined> => {
      setBusy(true)
      setError(null)
      setMessage(null)
      try {
        const result = await action()
        if (successMessage) setMessage(successMessage)
        return result
      } catch (e) {
        setError(e instanceof ApiError ? e.message : String(e))
        return undefined
      } finally {
        setBusy(false)
      }
    },
    [],
  )

  return { run, busy, error, message, setError, setMessage }
}
