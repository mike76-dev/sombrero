import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Deleted, FilesError, Rights, deleteFile, deleteVersions, deleted, restore } from './api'
import { describe, outcome, runEach } from './batch'
import { ConfirmDialog } from './Dialogs'
import { hashFor } from './route'
import { doneLingers } from './Uploads'
import { formatBytes, formatTime } from '../format'

type Notice = { text: string; undo?: () => Promise<void> }
const undoLingers = 8000

function folderOf(path: string): string {
  return path.slice(0, path.lastIndexOf('/')) || '/'
}

// DeletedPage lists the deleted files of a share: what is gone but for its
// versions. Each is restored to where it was, or deleted for good.
export function DeletedPage({ share, shareRights, onExpired }: { share: string; shareRights?: Rights; onExpired: () => void }) {
  const [list, setList] = useState<Deleted[] | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)
  const [tick, setTick] = useState(0)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<Notice | null>(null)
  const [problem, setProblem] = useState<string | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [confirm, setConfirm] = useState<Deleted[] | null>(null)
  const lastPicked = useRef<string | null>(null)

  const reload = useCallback(() => setTick((t) => t + 1), [])

  useEffect(() => {
    let cancelled = false
    setError(null)
    deleted(share)
      .then((ds) => {
        if (!cancelled) setList(ds)
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof FilesError && e.status === 401) onExpired()
        else setError(describe(e))
      })
    return () => {
      cancelled = true
    }
  }, [share, tick, onExpired])

  useEffect(() => {
    setList(undefined)
    setNotice(null)
    setProblem(null)
    setSelected(new Set())
    lastPicked.current = null
  }, [share])

  useEffect(() => {
    if (!list) return
    const present = new Set(list.map((d) => d.path))
    setSelected((s) => {
      const kept = new Set([...s].filter((p) => present.has(p)))
      return kept.size === s.size ? s : kept
    })
  }, [list])

  useEffect(() => {
    if (!notice) return
    const timer = setTimeout(() => setNotice(null), notice.undo ? undoLingers : doneLingers)
    return () => clearTimeout(timer)
  }, [notice])

  useEffect(() => {
    if (confirm || selected.size === 0) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setSelected(new Set())
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [confirm, selected.size])

  const rights = { write: shareRights?.write ?? false, delete: shareRights?.delete ?? false }
  const selectable = rights.write || rights.delete
  const items = list ?? []
  const picked = useMemo(() => items.filter((d) => selected.has(d.path)), [items, selected])

  const pick = (d: Deleted, shift: boolean) => {
    const from = shift && lastPicked.current ? items.findIndex((e) => e.path === lastPicked.current) : -1
    const to = items.findIndex((e) => e.path === d.path)
    lastPicked.current = d.path
    setSelected((s) => {
      const next = new Set(s)
      if (from >= 0 && to >= 0) {
        for (let i = Math.min(from, to); i <= Math.max(from, to); i++) next.add(items[i].path)
      } else if (next.has(d.path)) {
        next.delete(d.path)
      } else {
        next.add(d.path)
      }
      return next
    })
  }
  const pickAll = (all: boolean) => setSelected(all ? new Set(items.map((d) => d.path)) : new Set())

  // run applies a change to each file in turn and reports the outcome, as the
  // browser does; the files that failed stay selected.
  const run = async (ds: Deleted[], f: (d: Deleted) => Promise<unknown>, done: string, undone: string, undo?: (ds: Deleted[]) => () => Promise<void>) => {
    setBusy(true)
    setProblem(null)
    const { failed, expired } = await runEach(ds, f)
    setBusy(false)
    if (expired) {
      onExpired()
      return
    }
    setConfirm(null)
    reload()
    const result = outcome(ds, failed, done, undone)
    if (result.ok) setNotice({ text: result.notice, undo: undo?.(ds) })
    else setProblem(result.problem)
    setSelected(new Set(failed.map((f) => f.item.path)))
  }

  // A restored file is deleted again to undo, which puts it back here.
  const restoreAll = (ds: Deleted[]) =>
    run(
      ds,
      (d) => restore(share, d.path, { id: d.id }),
      'restored',
      'restored',
      (ds) => () => run(ds, (d) => deleteFile(share, d.path, false), 'returned to the deleted files', 'returned'),
    )
  const purgeAll = (ds: Deleted[]) => run(ds, (d) => deleteVersions(share, d.path), 'deleted for good', 'deleted')

  return (
    <div className="page">
      <nav className="crumbs" aria-label="Location">
        <a href="#/">Shares</a>
        <span className="crumb-sep">/</span>
        <a href={hashFor(share)}>{share}</a>
        <span className="crumb-sep">/</span>
        <span className="crumb-current">Deleted files</span>
      </nav>
      <p className="muted">
        A deleted file is kept here with its versions until it is restored or deleted for good, or its versions expire. A restored file goes back to
        where it was; a folder that is gone is made again for it.
      </p>
      {error ? (
        <div className="banner banner-error">{error}</div>
      ) : !list ? (
        <p className="muted">Loading…</p>
      ) : items.length === 0 ? (
        <p className="muted">No deleted files are retained.</p>
      ) : (
        <table className="table files">
          <thead>
            <tr>
              {selectable && (
                <th className="col-select">
                  <input
                    type="checkbox"
                    aria-label="Select all"
                    checked={picked.length === items.length}
                    ref={(el) => {
                      if (el) el.indeterminate = picked.length > 0 && picked.length < items.length
                    }}
                    onChange={(e) => pickAll(e.target.checked)}
                  />
                </th>
              )}
              <th>Name</th>
              <th className="col-size">Size</th>
              <th className="col-time">Deleted</th>
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {items.map((d) => (
              <tr key={d.path}>
                {selectable && (
                  <td className="col-select">
                    <input type="checkbox" aria-label={`Select ${d.name}`} checked={selected.has(d.path)} readOnly onClick={(e) => pick(d, e.shiftKey)} />
                  </td>
                )}
                <td className="col-name">
                  <span className="file-icon" aria-hidden>
                    {'\u{1F4C4}'}
                  </span>
                  {d.name}
                  <div className="deleted-folder muted">{folderOf(d.path)}</div>
                </td>
                <td className="col-size muted">{formatBytes(d.size)}</td>
                <td className="col-time muted">
                  {formatTime(d.deletedAt)}
                  {d.deletedBy && <div className="deleted-folder">by {d.deletedBy}</div>}
                </td>
                <td className="col-actions">
                  {rights.write && (
                    <button className="btn btn-small" disabled={busy} onClick={() => restoreAll([d])}>
                      Restore
                    </button>
                  )}
                  {rights.delete && (
                    <button className="btn btn-small btn-danger" disabled={busy} onClick={() => setConfirm([d])}>
                      Delete
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {confirm && (
        <ConfirmDialog title="Delete for good" action="Delete" busy={busy} error={null} onClose={() => setConfirm(null)} onConfirm={() => purgeAll(confirm)}>
          <p>
            Delete {confirm.length === 1 ? <strong>{confirm[0].name}</strong> : `these ${confirm.length} files`} for good, with every version? This cannot
            be undone.
          </p>
          {confirm.length > 1 && (
            <ul className="name-list">
              {confirm.map((d) => (
                <li key={d.path}>{d.name}</li>
              ))}
            </ul>
          )}
        </ConfirmDialog>
      )}

      {(picked.length > 0 || notice || problem) && (
        <div className="dock">
          {picked.length > 0 && (
            <div className="selection-bar">
              <span>{picked.length === 1 ? '1 file selected' : `${picked.length} files selected`}</span>
              <div className="toolbar">
                {rights.write && (
                  <button className="btn" disabled={busy} onClick={() => restoreAll(picked)}>
                    Restore
                  </button>
                )}
                {rights.delete && (
                  <button className="btn btn-danger" disabled={busy} onClick={() => setConfirm(picked)}>
                    Delete
                  </button>
                )}
                <button className="btn" onClick={() => pickAll(false)}>
                  Clear
                </button>
              </div>
            </div>
          )}
          {problem && (
            <div className="banner banner-error dock-banner">
              <span>{problem}</span>
              <button className="btn btn-small" onClick={() => setProblem(null)}>
                Dismiss
              </button>
            </div>
          )}
          {notice && (
            <div className="banner banner-success dock-banner">
              <span>{notice.text}</span>
              {notice.undo && (
                <button
                  className="btn btn-small"
                  disabled={busy}
                  onClick={() => {
                    const undo = notice.undo
                    setNotice(null)
                    undo?.()
                  }}
                >
                  Undo
                </button>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
