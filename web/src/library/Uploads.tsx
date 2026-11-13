import { useCallback, useEffect, useRef, useState } from 'react'
import { FilesError, upload } from './api'
import { formatBytes } from '../format'

export interface Upload {
  id: number
  name: string
  path: string
  size: number
  sent: number
  state: 'waiting' | 'uploading' | 'done' | 'failed'
  error?: string
}

export const doneLingers = 3000

// useUploads keeps the queue of uploads to a share, sending one at a time.
// onDone is told the folder an upload landed in, so the listing can follow.
export function useUploads(share: string, onDone: (folder: string) => void, onExpired: () => void) {
  const [uploads, setUploads] = useState<Upload[]>([])
  const files = useRef(new Map<number, File>())
  const abort = useRef<(() => void) | null>(null)
  const inFlight = useRef<number | null>(null)
  const next = useRef(1)

  const update = useCallback((id: number, change: Partial<Upload>) => {
    setUploads((us) => us.map((u) => (u.id === id ? { ...u, ...change } : u)))
  }, [])

  // One upload at a time; the ref guards the moment before the state says so.
  useEffect(() => {
    if (inFlight.current !== null || uploads.some((u) => u.state === 'uploading')) return
    const u = uploads.find((u) => u.state === 'waiting')
    const file = u && files.current.get(u.id)
    if (!u || !file) return

    inFlight.current = u.id
    update(u.id, { state: 'uploading' })
    const { done, abort: stop } = upload(share, u.path, file, (sent) => update(u.id, { sent }))
    abort.current = stop
    done
      .then(() => {
        update(u.id, { state: 'done', sent: u.size })
        onDone(u.path.slice(0, u.path.lastIndexOf('/')) || '/')
        // Done is shown long enough to be seen, then makes room.
        setTimeout(() => setUploads((us) => us.filter((x) => x.id !== u.id)), doneLingers)
      })
      .catch((e) => {
        if (e instanceof FilesError && e.status === 401) onExpired()
        update(u.id, { state: 'failed', error: e instanceof Error ? e.message : String(e) })
      })
      .finally(() => {
        abort.current = null
        inFlight.current = null
        files.current.delete(u.id)
      })
  }, [uploads, share, update, onDone, onExpired])

  const add = useCallback((folder: string, list: File[]) => {
    const added: Upload[] = list.map((f) => {
      const id = next.current++
      files.current.set(id, f)
      const path = (folder === '/' ? '' : folder) + '/' + f.name
      return { id, name: f.name, path, size: f.size, sent: 0, state: 'waiting' }
    })
    setUploads((us) => [...us, ...added])
  }, [])

  // cancel stops an upload, or takes it off the list if it is waiting or has
  // failed. One under way is stopped and reports the cancellation itself.
  const cancel = useCallback((id: number) => {
    setUploads((us) => {
      const u = us.find((u) => u.id === id)
      if (u?.state === 'uploading') abort.current?.()
      files.current.delete(id)
      return us.filter((u) => u.id !== id || u.state === 'uploading')
    })
  }, [])

  return { uploads, add, cancel }
}

export function UploadPanel({ uploads, onCancel }: { uploads: Upload[]; onCancel: (id: number) => void }) {
  if (uploads.length === 0) return null
  const active = uploads.filter((u) => u.state === 'waiting' || u.state === 'uploading').length
  return (
    <div className="uploads card">
      <div className="row row-spread">
        <strong>{active > 0 ? `Uploading ${active} of ${uploads.length}` : 'Uploads'}</strong>
      </div>
      <ul className="upload-list">
        {uploads.map((u) => (
          <li key={u.id} className={`upload upload-${u.state}`}>
            <div className="row row-spread">
              <span className="upload-name">{u.name}</span>
              <span className="muted upload-status">
                {u.state === 'waiting' && 'Waiting'}
                {u.state === 'uploading' && `${formatBytes(u.sent)} of ${formatBytes(u.size)}`}
                {u.state === 'done' && 'Done'}
                {u.state === 'failed' && <span className="field-error">{u.error}</span>}
                {u.state !== 'done' && (
                  <button className="btn btn-small" onClick={() => onCancel(u.id)}>
                    {u.state === 'failed' ? 'Dismiss' : 'Cancel'}
                  </button>
                )}
              </span>
            </div>
            {u.state === 'uploading' && (
              <div className="bar">
                <div className="bar-fill" style={{ width: `${u.size > 0 ? Math.min(100, (100 * u.sent) / u.size) : 0}%` }} />
              </div>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}
