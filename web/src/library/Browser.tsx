import { DragEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Entry,
  FilesError,
  Listing,
  Rights,
  deleteFile,
  deleteFolder,
  fileURL,
  list,
  makeFolder,
  rename,
  restore,
  snapshots,
  versionURL,
  versions,
} from './api'
import { describe, outcome, runEach } from './batch'
import { ConfirmDialog, Modal, NameDialog, PathDialog } from './Dialogs'
import { binHash, hashFor } from './route'
import { UploadPanel, doneLingers, useUploads } from './Uploads'
import { VersionsDialog } from './VersionsDialog'
import { formatBytes, formatTime, formatTimeExact } from '../format'

type SortKey = 'name' | 'size' | 'modified'
interface Sort {
  key: SortKey
  asc: boolean
}

const sortKey = 'sombrero.library.sort'

function loadSort(): Sort {
  try {
    const raw = localStorage.getItem(sortKey)
    if (raw) return JSON.parse(raw)
  } catch {
    // The default stands.
  }
  return { key: 'name', asc: true }
}

// viewable lists the kinds of file every browser shows by itself. The server
// sandboxes what could carry code, so pages and SVG are safe to show.
const viewable = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'avif', 'svg', 'pdf', 'txt', 'md', 'csv', 'json', 'log', 'html', 'htm'])

// media maps the extensions of audio and video to the types the server sends
// them as: whether this browser plays one is its own to say.
const media: Record<string, string> = {
  mp4: 'video/mp4', m4v: 'video/x-m4v', webm: 'video/webm', mov: 'video/quicktime', mkv: 'video/x-matroska',
  avi: 'video/x-msvideo', mpg: 'video/mpeg', mpeg: 'video/mpeg', ogv: 'video/ogg', '3gp': 'video/3gpp',
  mp3: 'audio/mpeg', m4a: 'audio/mp4', aac: 'audio/aac', ogg: 'audio/ogg', oga: 'audio/ogg', opus: 'audio/ogg',
  wav: 'audio/x-wav', flac: 'audio/flac', weba: 'audio/webm',
}

const player = document.createElement('video')

function canShow(name: string): boolean {
  const ext = extension(name)
  if (viewable.has(ext)) return true
  const type = media[ext]
  return type !== undefined && player.canPlayType(type) !== ''
}

function extension(name: string): string {
  const i = name.lastIndexOf('.')
  return i < 0 ? '' : name.slice(i + 1).toLowerCase()
}

function compare(a: Entry, b: Entry, sort: Sort): number {
  if (a.dir !== b.dir) return a.dir ? -1 : 1
  let c = 0
  switch (sort.key) {
    case 'size':
      c = a.size - b.size
      break
    case 'modified':
      c = a.modifiedAt.localeCompare(b.modifiedAt)
      break
  }
  if (c === 0) c = a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' })
  return sort.asc ? c : -c
}

type FolderRights = { write: boolean; delete: boolean }

// A dialog the browser has open, if any, and what it is about.
type Dialog =
  | { kind: 'folder' }
  | { kind: 'rename'; entry: Entry }
  | { kind: 'move'; entry: Entry }
  | { kind: 'delete'; entries: Entry[] }
  | { kind: 'moveMany'; entries: Entry[] }
  | { kind: 'replace'; folder: string; files: File[]; names: string[] }
  | { kind: 'versions'; entry: Entry }
  | { kind: 'history' }
  | { kind: 'restoreAt'; entry: Entry }

// A notice reports a change that went through, with a way to take it back
// while the notice is shown. It then lingers long enough to be taken.
type Notice = { text: string; undo?: () => Promise<void> }
const undoLingers = 8000

function into(folder: string, e: Entry): string {
  return (folder === '/' ? '' : folder) + '/' + e.name
}

export function Browser({
  share,
  shareRights,
  path,
  at,
  onExpired,
}: {
  share: string
  shareRights?: Rights
  path: string
  at?: string
  onExpired: () => void
}) {
  const [listing, setListing] = useState<Listing | undefined>(undefined)
  const [error, setError] = useState<{ message: string; status: number } | null>(null)
  const [sort, setSort] = useState<Sort>(loadSort)
  const [tick, setTick] = useState(0)
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [busy, setBusy] = useState(false)
  const [dialogError, setDialogError] = useState<string | null>(null)
  const [notice, setNotice] = useState<Notice | null>(null)
  const [dropping, setDropping] = useState(false)
  const [dragging, setDragging] = useState<Entry[] | null>(null)
  const [overFolder, setOverFolder] = useState<string | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const lastPicked = useRef<string | null>(null)
  const [problem, setProblem] = useState<string | null>(null)
  const [nowRights, setNowRights] = useState<FolderRights | undefined>(undefined)
  const picker = useRef<HTMLInputElement>(null)

  const reload = useCallback(() => setTick((t) => t + 1), [])
  const landed = useCallback(
    (folder: string) => {
      if (folder === path) reload()
    },
    [path, reload],
  )
  const { uploads, add, cancel } = useUploads(share, landed, onExpired)

  useEffect(() => {
    let cancelled = false
    setError(null)
    list(share, path, at)
      .then((l) => {
        if (!cancelled) setListing(l)
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof FilesError && e.status === 401) onExpired()
        else setError({ message: e instanceof Error ? e.message : String(e), status: e instanceof FilesError ? e.status : 0 })
      })
    return () => {
      cancelled = true
    }
  }, [share, path, at, tick, onExpired])

  // The past is read-only, so what may be restored into the present is the
  // present folder's to say, or the share's where the folder is gone.
  useEffect(() => {
    if (!at) {
      setNowRights(undefined)
      return
    }
    let cancelled = false
    list(share, path)
      .then((l) => {
        if (!cancelled) setNowRights(l.rights)
      })
      .catch(() => {
        if (!cancelled) setNowRights(shareRights ? { write: shareRights.write, delete: shareRights.delete } : { write: false, delete: false })
      })
    return () => {
      cancelled = true
    }
  }, [share, path, at, shareRights])

  useEffect(() => {
    localStorage.setItem(sortKey, JSON.stringify(sort))
  }, [sort])

  useEffect(() => {
    setNotice(null)
    setProblem(null)
    setSelected(new Set())
    lastPicked.current = null
  }, [share, path, at])

  // A selection outlives a reload only as far as its items do.
  useEffect(() => {
    if (!listing) return
    const present = new Set(listing.entries.map((e) => e.path))
    setSelected((s) => {
      const kept = new Set([...s].filter((p) => present.has(p)))
      return kept.size === s.size ? s : kept
    })
  }, [listing])

  // A success notice goes away by itself, as a finished upload does.
  useEffect(() => {
    if (!notice) return
    const timer = setTimeout(() => setNotice(null), notice.undo ? undoLingers : doneLingers)
    return () => clearTimeout(timer)
  }, [notice])

  const entries = useMemo(() => (listing ? [...listing.entries].sort((a, b) => compare(a, b, sort)) : []), [listing, sort])

  const sortBy = (key: SortKey) => setSort((s) => (s.key === key ? { key, asc: !s.asc } : { key, asc: key === 'name' }))
  const header = (key: SortKey, label: string) => (
    <th className={key === 'size' ? 'col-size' : key === 'modified' ? 'col-time' : ''}>
      <button className="sort-button" onClick={() => sortBy(key)}>
        {label}
        {sort.key === key && <span className="sort-mark">{sort.asc ? '▲' : '▼'}</span>}
      </button>
    </th>
  )

  const open = (d: Dialog) => {
    setDialogError(null)
    setDialog(d)
  }
  const close = useCallback(() => setDialog(null), [])

  // act runs a change, closes the dialog when it worked and reports it there
  // when it did not. A lost session goes back to the login.
  const act = async (f: () => Promise<unknown>, done: string, undo?: () => Promise<void>) => {
    setBusy(true)
    setDialogError(null)
    try {
      await f()
      setDialog(null)
      setNotice({ text: done, undo })
      reload()
    } catch (e) {
      if (e instanceof FilesError && e.status === 401) onExpired()
      else setDialogError(describe(e))
    } finally {
      setBusy(false)
    }
  }

  const within = (name: string) => (path === '/' ? '' : path) + '/' + name

  // Files to upload replace what is already there, which is asked about first.
  // Another folder than this one is listed to find that out.
  const takeFiles = async (files: File[], folder = path) => {
    if (files.length === 0 || !listing?.rights.write || at) return
    let entries = listing.entries
    if (folder !== path) {
      try {
        entries = (await list(share, folder)).entries
      } catch (e) {
        if (e instanceof FilesError && e.status === 401) onExpired()
        else setProblem(describe(e))
        return
      }
    }
    const have = new Set(entries.filter((e) => !e.dir).map((e) => e.name))
    const names = files.map((f) => f.name).filter((n) => have.has(n))
    if (names.length > 0) open({ kind: 'replace', folder, files, names })
    else add(folder, files)
  }

  // runBatch applies a change to each item in turn and reports the outcome:
  // what went through in the notice, what did not by name in the problem
  // banner. The items that failed stay selected, so that they can be retried.
  // Where everything went through, undo makes the notice's way back.
  const runBatch = async (
    items: Entry[],
    f: (e: Entry) => Promise<unknown>,
    done: string,
    undone: string,
    undo?: (items: Entry[]) => () => Promise<void>,
  ) => {
    setBusy(true)
    setProblem(null)
    const { failed, expired } = await runEach(items, f)
    setBusy(false)
    if (expired) {
      onExpired()
      return
    }
    setDialog(null)
    reload()
    const result = outcome(items, failed, done, undone)
    if (result.ok) setNotice({ text: result.notice, undo: undo?.(items) })
    else setProblem(result.problem)
    setSelected(new Set(failed.map((f) => f.item.path)))
  }

  // moveBack undoes a move or a rename: each item goes from where it was put
  // back to where it was.
  const moveBack = (to: (e: Entry) => string, word: string) => (items: Entry[]) => () =>
    runBatch(items, (e) => rename(share, to(e), e.path, false), word, word)
  const moveAll = (items: Entry[], folder: string) =>
    runBatch(items, (e) => rename(share, e.path, into(folder, e), false), `moved to ${folder}`, 'moved', moveBack((e) => into(folder, e), 'moved back'))

  // A deleted file is restored from the version its deletion left, which is
  // the newest one. A folder leaves none, and a purge leaves none either.
  const restoreDeleted = (e: Entry) =>
    versions(share, e.path).then((vs) => {
      if (vs.length === 0) throw new Error('No version of it is retained.')
      const latest = vs.reduce((a, b) => (Date.parse(a.replacedAt) >= Date.parse(b.replacedAt) ? a : b))
      return restore(share, e.path, { id: latest.id })
    })
  const undelete = (items: Entry[]) => () => runBatch(items, restoreDeleted, 'restored', 'restored')
  const deleteAll = (items: Entry[], purge: boolean) =>
    runBatch(
      items,
      (e) => (e.dir ? deleteFolder(share, e.path) : deleteFile(share, e.path, purge)),
      'deleted',
      'deleted',
      purge || items.some((e) => e.dir) ? undefined : undelete,
    )

  const stale = listing && (listing.share !== share || listing.path !== path)
  const none: FolderRights = { write: false, delete: false }
  // In the present the listing's rights rule; in the past nothing is changed
  // in place, and restoring is decided by the present's rights.
  const rights = at || !listing || stale ? none : listing.rights
  const restoreRights = at ? (nowRights ?? none) : rights
  const canDrag = rights.write && rights.delete
  const selectable = rights.delete && !at
  const picked = useMemo(() => entries.filter((e) => selected.has(e.path)), [entries, selected])

  // pick toggles a row, or with Shift the run of rows from the last one picked.
  const pick = (entry: Entry, shift: boolean) => {
    const from = shift && lastPicked.current ? entries.findIndex((e) => e.path === lastPicked.current) : -1
    const to = entries.findIndex((e) => e.path === entry.path)
    lastPicked.current = entry.path
    setSelected((s) => {
      const next = new Set(s)
      if (from >= 0 && to >= 0) {
        for (let i = Math.min(from, to); i <= Math.max(from, to); i++) next.add(entries[i].path)
      } else if (next.has(entry.path)) {
        next.delete(entry.path)
      } else {
        next.add(entry.path)
      }
      return next
    })
  }
  const pickAll = (all: boolean) => setSelected(all ? new Set(entries.map((e) => e.path)) : new Set())

  // A drag is either rows, which a folder takes as a move, or files from
  // outside, which a folder takes as an upload into it. Dropping beside the
  // rows is an upload into this folder. A selected row drags the selection.
  const takes = (folder: string, e: DragEvent): boolean => {
    if (dragging) return canDrag && folder !== path && !dragging.some((d) => d.path === folder)
    return rights.write && e.dataTransfer.types.includes('Files')
  }
  const allowDrop = (folder: string, e: DragEvent) => {
    if (!takes(folder, e)) return
    e.preventDefault()
    e.stopPropagation()
    e.dataTransfer.dropEffect = dragging ? 'move' : 'copy'
    setOverFolder(folder)
    setDropping(false)
  }
  const dropInto = (folder: string, e: DragEvent) => {
    if (!takes(folder, e)) return
    e.preventDefault()
    e.stopPropagation()
    setOverFolder(null)
    setDropping(false)
    if (dragging) {
      const items = dragging
      setDragging(null)
      moveAll(items, folder)
    } else {
      takeFiles(Array.from(e.dataTransfer.files), folder)
    }
  }
  const leave = (folder: string) => setOverFolder((f) => (f === folder ? null : f))

  return (
    <div
      className={dropping ? 'page dropping' : 'page'}
      onDragOver={(e) => {
        if (!dragging && rights.write && e.dataTransfer.types.includes('Files')) {
          e.preventDefault()
          setDropping(true)
        }
      }}
      onDragLeave={() => setDropping(false)}
      onDrop={(e) => {
        e.preventDefault()
        setDropping(false)
        if (!dragging) takeFiles(Array.from(e.dataTransfer.files))
      }}
    >
      <div className="row row-spread">
        <Breadcrumbs share={share} path={path} at={at} overFolder={overFolder} onDragOver={allowDrop} onDragLeave={leave} onDrop={dropInto} />
        <div className="toolbar">
          {rights.write && (
            <>
              <button className="btn" onClick={() => picker.current?.click()}>
                Upload files
              </button>
              <button className="btn" onClick={() => open({ kind: 'folder' })}>
                New folder
              </button>
              <input
                ref={picker}
                type="file"
                multiple
                hidden
                onChange={(e) => {
                  takeFiles(Array.from(e.target.files ?? []))
                  e.target.value = ''
                }}
              />
            </>
          )}
          {!error && (
            <button className="btn" onClick={() => open({ kind: 'history' })}>
              History
            </button>
          )}
          {!error && !at && (
            <a className="btn" href={binHash(share)}>
              Deleted files
            </a>
          )}
        </div>
      </div>
      {at && (
        <div className="banner history-banner">
          This folder as it was on <strong>{formatTimeExact(at)}</strong>, deleted files included. Nothing here can be changed; a file may be restored to
          the present. <a href={hashFor(share, path)}>Back to the present.</a>
        </div>
      )}
      {error ? (
        <ErrorNotice share={share} error={error} at={at} />
      ) : !listing || stale ? (
        <p className="muted">Loading…</p>
      ) : entries.length === 0 ? (
        <p className="muted">{rights.write ? 'This folder is empty. Drop files here to upload them.' : 'This folder is empty.'}</p>
      ) : (
        <table className="table files">
          <thead>
            <tr>
              {selectable && (
                <th className="col-select">
                  <input
                    type="checkbox"
                    aria-label="Select all"
                    checked={picked.length === entries.length}
                    ref={(el) => {
                      if (el) el.indeterminate = picked.length > 0 && picked.length < entries.length
                    }}
                    onChange={(e) => pickAll(e.target.checked)}
                  />
                </th>
              )}
              {header('name', 'Name')}
              {header('size', 'Size')}
              {header('modified', 'Modified')}
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <Row
                key={e.path}
                share={share}
                entry={e}
                at={at}
                rights={rights}
                canRestore={restoreRights.write}
                draggable={canDrag && !at}
                dragged={dragging?.some((d) => d.path === e.path) ?? false}
                over={overFolder === e.path}
                selected={selectable ? selected.has(e.path) : undefined}
                onPick={(shift) => pick(e, shift)}
                onAction={open}
                onDragStart={() => setDragging(selected.has(e.path) ? picked : [e])}
                onDragEnd={() => {
                  setDragging(null)
                  setOverFolder(null)
                }}
                onDragOver={allowDrop}
                onDragLeave={leave}
                onDrop={dropInto}
              />
            ))}
          </tbody>
        </table>
      )}

      {dialog?.kind === 'folder' && (
        <NameDialog
          title="New folder"
          label="Name"
          action="Create"
          busy={busy}
          error={dialogError}
          onClose={close}
          onSubmit={(name) => act(() => makeFolder(share, within(name)), `The folder ${name} was created.`)}
        />
      )}
      {dialog?.kind === 'rename' && (
        <NameDialog
          title={dialog.entry.dir ? 'Rename folder' : 'Rename file'}
          label="New name"
          initial={dialog.entry.name}
          action="Rename"
          busy={busy}
          error={dialogError}
          onClose={close}
          onSubmit={(name) =>
            act(
              () => rename(share, dialog.entry.path, within(name), false),
              `${dialog.entry.name} was renamed to ${name}.`,
              moveBack(() => within(name), 'renamed back')([dialog.entry]),
            )
          }
        />
      )}
      {dialog?.kind === 'move' && (
        <PathDialog
          title={dialog.entry.dir ? 'Move folder' : 'Move file'}
          subject={<strong>{dialog.entry.name}</strong>}
          initial={path}
          busy={busy}
          error={dialogError}
          onClose={close}
          onSubmit={(folder) =>
            act(
              () => rename(share, dialog.entry.path, into(folder, dialog.entry), false),
              `${dialog.entry.name} was moved to ${folder}.`,
              moveBack((e) => into(folder, e), 'moved back')([dialog.entry]),
            )
          }
        />
      )}
      {dialog?.kind === 'moveMany' && (
        <PathDialog
          title="Move items"
          subject={`the ${dialog.entries.length} selected items`}
          initial={path}
          busy={busy}
          error={null}
          onClose={close}
          onSubmit={(folder) => moveAll(dialog.entries, folder)}
        />
      )}
      {dialog?.kind === 'delete' && (
        <DeleteDialog
          entries={dialog.entries}
          busy={busy}
          error={dialogError}
          onClose={close}
          onConfirm={(purge) => {
            const one = dialog.entries.length === 1 ? dialog.entries[0] : undefined
            if (!one) deleteAll(dialog.entries, purge)
            else if (one.dir) act(() => deleteFolder(share, one.path), `${one.name} was deleted.`)
            else act(() => deleteFile(share, one.path, purge), `${one.name} was deleted.`, purge ? undefined : undelete([one]))
          }}
        />
      )}
      {dialog?.kind === 'replace' && (
        <ConfirmDialog
          title="Replace files"
          action="Replace"
          busy={false}
          error={null}
          onClose={close}
          onConfirm={() => {
            add(dialog.folder, dialog.files)
            setDialog(null)
          }}
        >
          <p>
            {dialog.names.length === 1 ? 'A file of this name is already in' : 'Files of these names are already in'}{' '}
            {dialog.folder === path ? 'the folder' : <strong>{dialog.folder}</strong>} and will be replaced:
          </p>
          <ul className="name-list">
            {dialog.names.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
        </ConfirmDialog>
      )}
      {dialog?.kind === 'versions' && (
        <VersionsDialog
          share={share}
          entry={dialog.entry}
          canShow={canShow(dialog.entry.name)}
          rights={restoreRights}
          onChanged={reload}
          onExpired={onExpired}
          onClose={close}
        />
      )}
      {dialog?.kind === 'history' && <HistoryDialog share={share} path={path} at={at} onExpired={onExpired} onClose={close} />}
      {dialog?.kind === 'restoreAt' && at && (
        <ConfirmDialog
          title="Restore file"
          action="Restore"
          danger={false}
          busy={busy}
          error={dialogError}
          onClose={close}
          onConfirm={() =>
            act(() => restore(share, dialog.entry.path, { at }), `${dialog.entry.name} was restored as it was on ${formatTimeExact(at)}.`)
          }
        >
          <p>
            Restore <strong>{dialog.entry.name}</strong> as it was on {formatTimeExact(at)}? A file that is still there keeps its current contents as a
            version; a deleted file comes back.
          </p>
        </ConfirmDialog>
      )}

      {(picked.length > 0 || uploads.length > 0 || notice || problem) && (
        <div className="dock">
          {picked.length > 0 && (
            <div className="selection-bar">
              <span>{picked.length === 1 ? '1 item selected' : `${picked.length} items selected`}</span>
              <div className="toolbar">
                {canDrag && (
                  <button className="btn" disabled={busy} onClick={() => open({ kind: 'moveMany', entries: picked })}>
                    Move
                  </button>
                )}
                <button className="btn btn-danger" disabled={busy} onClick={() => open({ kind: 'delete', entries: picked })}>
                  Delete
                </button>
                <button className="btn" onClick={() => pickAll(false)}>
                  Clear
                </button>
              </div>
            </div>
          )}
          <UploadPanel uploads={uploads} onCancel={cancel} />
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

// HistoryDialog lists the times at which the folder has a previous state, each
// a way into the folder as it was then.
function HistoryDialog({ share, path, at, onExpired, onClose }: { share: string; path: string; at?: string; onExpired: () => void; onClose: () => void }) {
  const [times, setTimes] = useState<string[] | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    snapshots(share, path)
      .then((ts) => {
        if (!cancelled) setTimes(ts)
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof FilesError && e.status === 401) onExpired()
        else if (e instanceof FilesError && e.status === 404) setTimes([])
        else setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      cancelled = true
    }
  }, [share, path, onExpired])

  return (
    <Modal title="History" onClose={onClose}>
      {error && <div className="banner banner-error">{error}</div>}
      {!times ? (
        <p className="muted">Loading…</p>
      ) : times.length === 0 ? (
        <p className="muted">No previous states of this folder are retained.</p>
      ) : (
        <>
          <p className="muted">The folder has a previous state at each of these times. Open one to see the folder as it was then.</p>
          <ul className="history-list">
            {at && (
              <li>
                <a className="history-time" href={hashFor(share, path)} onClick={onClose}>
                  Now
                </a>
              </li>
            )}
            {times.map((t) => (
              <li key={t}>
                <a className={t === at ? 'history-time history-current' : 'history-time'} href={hashFor(share, path, t)} onClick={onClose}>
                  {formatTimeExact(t)}
                </a>
              </li>
            ))}
          </ul>
        </>
      )}
    </Modal>
  )
}

// DeleteDialog asks before one item or a selection is deleted. Deleted files
// keep their versions, a folder's included, unless they are purged too.
function DeleteDialog({
  entries,
  busy,
  error,
  onClose,
  onConfirm,
}: {
  entries: Entry[]
  busy: boolean
  error: string | null
  onClose: () => void
  onConfirm: (purge: boolean) => void
}) {
  const [purge, setPurge] = useState(false)
  const one = entries.length === 1 ? entries[0] : undefined
  return (
    <ConfirmDialog
      title={one ? (one.dir ? 'Delete folder' : 'Delete file') : 'Delete items'}
      action="Delete"
      busy={busy}
      error={error}
      onClose={onClose}
      onConfirm={() => onConfirm(purge)}
    >
      {one ? (
        <p>
          Delete the {one.dir ? 'folder' : 'file'} <strong>{one.name}</strong>
          {one.dir ? ' and everything in it' : ''}?
        </p>
      ) : (
        <>
          <p>Delete these {entries.length} items?</p>
          <ul className="name-list">
            {entries.map((e) => (
              <li key={e.path}>{e.name}</li>
            ))}
          </ul>
        </>
      )}
      <label className="checkbox">
        <input type="checkbox" checked={purge} onChange={(e) => setPurge(e.target.checked)} />
        {one && !one.dir ? 'Delete its previous versions as well' : 'Delete the previous versions of the files as well'}
      </label>
      <p className="muted field-hint">
        On a share that retains versions, a deleted file is kept as a version until it is purged or its versions expire, the files of a deleted folder
        included. The folder itself is not kept.
      </p>
    </ConfirmDialog>
  )
}

type DropHandlers = {
  onDragOver: (folder: string, e: DragEvent) => void
  onDragLeave: (folder: string) => void
  onDrop: (folder: string, e: DragEvent) => void
}

function Row({
  share,
  entry,
  at,
  rights,
  canRestore,
  draggable,
  dragged,
  over,
  selected,
  onPick,
  onAction,
  onDragStart,
  onDragEnd,
  onDragOver,
  onDragLeave,
  onDrop,
}: {
  share: string
  entry: Entry
  at?: string
  rights: FolderRights
  canRestore: boolean
  draggable: boolean
  dragged: boolean
  over: boolean
  selected?: boolean
  onPick: (shift: boolean) => void
  onAction: (d: Dialog) => void
  onDragStart: () => void
  onDragEnd: () => void
} & DropHandlers) {
  const menu = useRef<HTMLDetailsElement>(null)
  const pick = (d: Dialog) => {
    if (menu.current) menu.current.open = false
    onAction(d)
  }
  // In the past a file is read at the time; in the present as it is.
  const href = (inline: boolean) => (at ? versionURL(share, entry.path, { at }, inline) : fileURL(share, entry.path, inline))
  // A file the browser can show opens from its name; any other downloads.
  const showable = !entry.dir && canShow(entry.name)
  // Renaming and moving take the right to delete as well, so the menu hangs on it.
  const manage = !entry.dir || rights.delete
  const classes = [dragged ? 'row-dragging' : '', over ? 'drop-target' : ''].filter(Boolean).join(' ')
  return (
    <tr
      className={classes || undefined}
      draggable={draggable}
      onDragStart={(e) => {
        e.dataTransfer.effectAllowed = 'move'
        e.dataTransfer.setData('text/plain', entry.name)
        onDragStart()
      }}
      onDragEnd={onDragEnd}
      onDragOver={entry.dir && !at ? (e) => onDragOver(entry.path, e) : undefined}
      onDragLeave={entry.dir ? () => onDragLeave(entry.path) : undefined}
      onDrop={entry.dir && !at ? (e) => onDrop(entry.path, e) : undefined}
    >
      {selected !== undefined && (
        <td className="col-select">
          <input type="checkbox" aria-label={`Select ${entry.name}`} checked={selected} readOnly onClick={(e) => onPick(e.shiftKey)} />
        </td>
      )}
      <td className="col-name">
        <span className="file-icon" aria-hidden>
          {entry.dir ? '\u{1F4C1}' : '\u{1F4C4}'}
        </span>
        {entry.dir ? (
          <a className="file-name" href={hashFor(share, entry.path, at)}>
            {entry.name}
          </a>
        ) : showable ? (
          <a className="file-name" href={href(true)} target="_blank" rel="noreferrer">
            {entry.name}
          </a>
        ) : (
          <a className="file-name" href={href(false)} download={entry.name}>
            {entry.name}
          </a>
        )}
      </td>
      <td className="col-size muted">{entry.dir ? '' : formatBytes(entry.size)}</td>
      <td className="col-time muted">{formatTime(entry.modifiedAt)}</td>
      <td className="col-actions">
        {showable && (
          <a className="btn btn-small" href={href(true)} target="_blank" rel="noreferrer">
            Open
          </a>
        )}
        {!entry.dir && (
          <a className="btn btn-small" href={href(false)} download={entry.name}>
            Download
          </a>
        )}
        {manage && (
          <details ref={menu} className="menu">
            <summary className="btn btn-small" aria-label="More actions">
              ⋯
            </summary>
            <div className="menu-items">
              {!entry.dir && (
                <button className="menu-item" onClick={() => pick({ kind: 'versions', entry })}>
                  Versions
                </button>
              )}
              {!entry.dir && at && canRestore && (
                <button className="menu-item" onClick={() => pick({ kind: 'restoreAt', entry })}>
                  Restore
                </button>
              )}
              {rights.write && rights.delete && (
                <>
                  <button className="menu-item" onClick={() => pick({ kind: 'rename', entry })}>
                    Rename
                  </button>
                  <button className="menu-item" onClick={() => pick({ kind: 'move', entry })}>
                    Move
                  </button>
                </>
              )}
              {rights.delete && (
                <button className="menu-item menu-item-danger" onClick={() => pick({ kind: 'delete', entries: [entry] })}>
                  Delete
                </button>
              )}
            </div>
          </details>
        )}
      </td>
    </tr>
  )
}

// Breadcrumbs lead back up, and in the present each folder on the way takes
// a drop, so that a row can be moved up without opening the folder first.
function Breadcrumbs({
  share,
  path,
  at,
  overFolder,
  onDragOver,
  onDragLeave,
  onDrop,
}: {
  share: string
  path: string
  at?: string
  overFolder: string | null
} & DropHandlers) {
  const segments = path.split('/').filter((s) => s !== '')
  let here = ''
  const crumb = (folder: string, label: string) => (
    <a
      href={hashFor(share, folder, at)}
      className={overFolder === folder ? 'drop-target' : undefined}
      onDragOver={at ? undefined : (e) => onDragOver(folder, e)}
      onDragLeave={() => onDragLeave(folder)}
      onDrop={at ? undefined : (e) => onDrop(folder, e)}
    >
      {label}
    </a>
  )
  return (
    <nav className="crumbs" aria-label="Location">
      <a href="#/">Shares</a>
      <span className="crumb-sep">/</span>
      {segments.length === 0 ? <span className="crumb-current">{share}</span> : crumb('/', share)}
      {segments.map((s, i) => {
        here += '/' + s
        const last = i === segments.length - 1
        return (
          <span key={here}>
            <span className="crumb-sep">/</span>
            {last ? <span className="crumb-current">{s}</span> : crumb(here, s)}
          </span>
        )
      })}
    </nav>
  )
}

function ErrorNotice({ share, error, at }: { share: string; error: { message: string; status: number }; at?: string }) {
  let text: string
  switch (error.status) {
    case 404:
      text = error.message === 'share not found' ? 'There is no share of this name.' : at ? 'The folder was not there at that time.' : 'The folder was not found.'
      break
    case 403:
      text = 'This account may not read here.'
      break
    case 503:
      text = 'The share is not available at the moment. Please try again later.'
      break
    default:
      text = error.message
  }
  return (
    <div className="banner banner-error">
      {text} <a href={hashFor(share)}>Back to the root of the share.</a>
    </div>
  )
}
