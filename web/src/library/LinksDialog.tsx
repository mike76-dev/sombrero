import { FormEvent, useEffect, useState } from 'react'
import { Entry, FilesError, SharingLink, createLink, deleteLink, links } from './api'
import { describe } from './batch'
import { ConfirmDialog, Modal } from './Dialogs'
import { formatTime } from '../format'

// The choices of how long a link lasts, in days, or forever.
const lifetimes: { label: string; days?: number }[] = [
  { label: 'Never expires' },
  { label: 'Expires in a day', days: 1 },
  { label: 'Expires in a week', days: 7 },
  { label: 'Expires in 30 days', days: 30 },
]

// copy puts the text on the clipboard, and says whether it could.
async function copy(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}

// LinksDialog lists the sharing links to a file and makes new ones: each is
// copied to the clipboard, and deleted when it is no longer wanted.
export function LinksDialog({ share, entry, onExpired, onClose }: { share: string; entry: Entry; onExpired: () => void; onClose: () => void }) {
  const [list, setList] = useState<SharingLink[] | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  const [lifetime, setLifetime] = useState(0)
  const [confirm, setConfirm] = useState<SharingLink | null>(null)
  const [tick, setTick] = useState(0)

  useEffect(() => {
    let cancelled = false
    links(share, entry.path)
      .then((ls) => {
        if (!cancelled) setList(ls)
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof FilesError && e.status === 401) onExpired()
        else setError(describe(e))
      })
    return () => {
      cancelled = true
    }
  }, [share, entry.path, tick, onExpired])

  // act carries out a change and reports it, reloading the list after.
  const act = async (f: () => Promise<string>) => {
    setBusy(true)
    setError(null)
    try {
      setNotice(await f())
      setConfirm(null)
      setTick((t) => t + 1)
    } catch (e) {
      if (e instanceof FilesError && e.status === 401) onExpired()
      else setError(describe(e))
    } finally {
      setBusy(false)
    }
  }

  const create = (e: FormEvent) => {
    e.preventDefault()
    const days = lifetimes[lifetime].days
    const expiresAt = days === undefined ? undefined : new Date(Date.now() + days * 24 * 3600 * 1000).toISOString()
    act(async () => {
      const l = await createLink(share, entry.path, expiresAt)
      return (await copy(l.url)) ? 'The link is created and copied to the clipboard.' : 'The link is created.'
    })
  }

  return (
    <Modal title={`Share ${entry.name}`} onClose={onClose} wide>
      <div className="stack">
        <p className="muted">
          Whoever holds a link reads the file as you, without an account, for as long as the link lasts and you may read the file. The links made by
          your workgroup are listed here, and any of them can be deleted.
        </p>
        <form className="row link-form" onSubmit={create}>
          <select value={lifetime} onChange={(e) => setLifetime(Number(e.target.value))} disabled={busy} aria-label="How long the link lasts">
            {lifetimes.map((l, i) => (
              <option key={l.label} value={i}>
                {l.label}
              </option>
            ))}
          </select>
          <button type="submit" className="btn btn-primary" disabled={busy}>
            Create link
          </button>
        </form>
        {notice && <div className="banner banner-success">{notice}</div>}
        {error && <div className="banner banner-error">{error}</div>}
        {!list ? (
          <p className="muted">Loading…</p>
        ) : list.length === 0 ? (
          <p className="muted">There are no links to this file.</p>
        ) : (
          <div className="table-scroll">
            <table className="table links">
              <thead>
                <tr>
                  <th>Link</th>
                  <th>Created</th>
                  <th>Expires</th>
                  <th className="col-actions"></th>
                </tr>
              </thead>
              <tbody>
                {list.map((l) => (
                  <tr key={l.token}>
                    <td className="col-link">
                      <input type="text" className="link-url" value={l.url} readOnly onFocus={(e) => e.target.select()} aria-label="The link" />
                    </td>
                    <td className="muted">
                      {formatTime(l.createdAt)}
                      <div className="version-by">by {l.createdBy}</div>
                    </td>
                    <td className="muted">{l.expiresAt ? formatTime(l.expiresAt) : 'Never'}</td>
                    <td className="col-actions">
                      <button
                        className="btn btn-small"
                        disabled={busy}
                        onClick={() => copy(l.url).then((ok) => setNotice(ok ? 'The link is copied to the clipboard.' : 'The link could not be copied.'))}
                      >
                        Copy
                      </button>
                      <button className="btn btn-small btn-danger" disabled={busy} onClick={() => setConfirm(l)}>
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="row modal-actions">
          <button type="button" className="btn" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
      {confirm && (
        <ConfirmDialog
          title="Delete the link"
          action="Delete"
          busy={busy}
          error={null}
          onClose={() => setConfirm(null)}
          onConfirm={() => act(async () => (await deleteLink(share, confirm.token), 'The link is deleted.'))}
        >
          <p>Delete this link? Whoever holds it will no longer reach the file.</p>
        </ConfirmDialog>
      )}
    </Modal>
  )
}
