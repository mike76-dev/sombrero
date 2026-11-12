import { DragEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Entry, FilesError, Listing, deleteFile, deleteFolder, fileURL, list, makeFolder, rename } from './api'
import { ConfirmDialog, NameDialog, PathDialog } from './Dialogs'
import { hashFor } from './route'
import { UploadPanel, useUploads } from './Uploads'
import { formatBytes, formatTime } from '../format'

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

// viewable lists the kinds of file a browser shows by itself. The server
// sandboxes what could carry code, so pages and SVG are safe to show.
const viewable = new Set([
  'jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'avif', 'svg',
  'mp4', 'webm', 'm4v', 'mov',
  'mp3', 'ogg', 'wav', 'm4a', 'flac',
  'pdf', 'txt', 'md', 'csv', 'json', 'log', 'html', 'htm',
])

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

// A dialog the browser has open, if any, and what it is about.
type Dialog =
  | { kind: 'folder' }
  | { kind: 'rename'; entry: Entry }
  | { kind: 'move'; entry: Entry; replace?: boolean }
  | { kind: 'delete'; entry: Entry }
  | { kind: 'replace'; files: File[]; names: string[] }

// describe turns an error of the file API into a sentence.
function describe(e: unknown): string {
  if (e instanceof FilesError) {
    switch (e.message) {
      case 'already exists':
      case 'destination already exists':
        return 'Something of that name is already there.'
      case 'access denied':
        return 'This account may not do that here.'
      case 'not found':
        return 'It is no longer there.'
      case 'share unavailable':
        return 'The share is not available at the moment.'
      case 'destination is of the other kind':
        return 'A file cannot replace a folder, nor a folder a file.'
    }
    return e.message
  }
  return e instanceof Error ? e.message : String(e)
}

export function Browser({ share, path, onExpired }: { share: string; path: string; onExpired: () => void }) {
  const [listing, setListing] = useState<Listing | undefined>(undefined)
  const [error, setError] = useState<{ message: string; status: number } | null>(null)
  const [sort, setSort] = useState<Sort>(loadSort)
  const [tick, setTick] = useState(0)
  const [dialog, setDialog] = useState<Dialog | null>(null)
  const [busy, setBusy] = useState(false)
  const [dialogError, setDialogError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [dropping, setDropping] = useState(false)
  const picker = useRef<HTMLInputElement>(null)

  const reload = useCallback(() => setTick((t) => t + 1), [])
  const landed = useCallback(
    (folder: string) => {
      if (folder === path) reload()
    },
    [path, reload],
  )
  const { uploads, add, cancel, clear } = useUploads(share, landed, onExpired)

  useEffect(() => {
    let cancelled = false
    setError(null)
    list(share, path)
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
  }, [share, path, tick, onExpired])

  useEffect(() => {
    localStorage.setItem(sortKey, JSON.stringify(sort))
  }, [sort])

  useEffect(() => setNotice(null), [share, path])

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
  const act = async (f: () => Promise<unknown>, done: string) => {
    setBusy(true)
    setDialogError(null)
    try {
      await f()
      setDialog(null)
      setNotice(done)
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
  const takeFiles = (files: File[]) => {
    if (files.length === 0 || !listing?.rights.write) return
    const have = new Set(listing.entries.filter((e) => !e.dir).map((e) => e.name))
    const names = files.map((f) => f.name).filter((n) => have.has(n))
    if (names.length > 0) open({ kind: 'replace', files, names })
    else add(path, files)
  }

  const onDrop = (e: DragEvent) => {
    e.preventDefault()
    setDropping(false)
    takeFiles(Array.from(e.dataTransfer.files))
  }

  const stale = listing && (listing.share !== share || listing.path !== path)
  const rights = listing && !stale ? listing.rights : { write: false, delete: false }
  return (
    <div
      className={dropping ? 'page dropping' : 'page'}
      onDragOver={(e) => {
        if (rights.write && e.dataTransfer.types.includes('Files')) {
          e.preventDefault()
          setDropping(true)
        }
      }}
      onDragLeave={() => setDropping(false)}
      onDrop={onDrop}
    >
      <div className="row row-spread">
        <Breadcrumbs share={share} path={path} />
        {rights.write && (
          <div className="toolbar">
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
          </div>
        )}
      </div>
      {notice && <div className="banner banner-success">{notice}</div>}
      <UploadPanel uploads={uploads} onCancel={cancel} onClear={clear} />
      {error ? (
        <ErrorNotice share={share} error={error} />
      ) : !listing || stale ? (
        <p className="muted">Loading…</p>
      ) : entries.length === 0 ? (
        <p className="muted">{rights.write ? 'This folder is empty. Drop files here to upload them.' : 'This folder is empty.'}</p>
      ) : (
        <table className="table files">
          <thead>
            <tr>
              {header('name', 'Name')}
              {header('size', 'Size')}
              {header('modified', 'Modified')}
              <th className="col-actions"></th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <Row key={e.path} share={share} entry={e} rights={rights} onAction={open} />
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
          onSubmit={(name) => act(() => rename(share, dialog.entry.path, within(name), false), `${dialog.entry.name} was renamed to ${name}.`)}
        />
      )}
      {dialog?.kind === 'move' && (
        <PathDialog
          title={dialog.entry.dir ? 'Move folder' : 'Move file'}
          name={dialog.entry.name}
          initial={path}
          busy={busy}
          error={dialogError}
          onClose={close}
          onSubmit={(folder) => {
            const to = (folder === '/' ? '' : folder) + '/' + dialog.entry.name
            act(() => rename(share, dialog.entry.path, to, false), `${dialog.entry.name} was moved to ${folder}.`)
          }}
        />
      )}
      {dialog?.kind === 'delete' && (
        <DeleteDialog
          entry={dialog.entry}
          busy={busy}
          error={dialogError}
          onClose={close}
          onConfirm={(purge) =>
            act(
              () => (dialog.entry.dir ? deleteFolder(share, dialog.entry.path) : deleteFile(share, dialog.entry.path, purge)),
              `${dialog.entry.name} was deleted.`,
            )
          }
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
            add(path, dialog.files)
            setDialog(null)
          }}
        >
          <p>
            {dialog.names.length === 1 ? 'A file of this name is already in the folder' : 'Files of these names are already in the folder'} and will
            be replaced:
          </p>
          <ul className="name-list">
            {dialog.names.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
        </ConfirmDialog>
      )}
    </div>
  )
}

function DeleteDialog({
  entry,
  busy,
  error,
  onClose,
  onConfirm,
}: {
  entry: Entry
  busy: boolean
  error: string | null
  onClose: () => void
  onConfirm: (purge: boolean) => void
}) {
  const [purge, setPurge] = useState(false)
  return (
    <ConfirmDialog
      title={entry.dir ? 'Delete folder' : 'Delete file'}
      action="Delete"
      busy={busy}
      error={error}
      onClose={onClose}
      onConfirm={() => onConfirm(purge)}
    >
      {entry.dir ? (
        <p>
          Delete the folder <strong>{entry.name}</strong> and everything in it? The files in it lose their previous versions as well, so this cannot be
          undone.
        </p>
      ) : (
        <>
          <p>
            Delete the file <strong>{entry.name}</strong>?
          </p>
          <label className="checkbox">
            <input type="checkbox" checked={purge} onChange={(e) => setPurge(e.target.checked)} />
            Delete its previous versions as well
          </label>
          <p className="muted field-hint">
            On a share that retains versions, a deleted file is kept as a version until it is purged or its versions expire.
          </p>
        </>
      )}
    </ConfirmDialog>
  )
}

function Row({
  share,
  entry,
  rights,
  onAction,
}: {
  share: string
  entry: Entry
  rights: { write: boolean; delete: boolean }
  onAction: (d: Dialog) => void
}) {
  const menu = useRef<HTMLDetailsElement>(null)
  const pick = (d: Dialog) => {
    if (menu.current) menu.current.open = false
    onAction(d)
  }
  // Renaming and moving take the right to delete as well, so the menu hangs on it.
  const manage = rights.delete
  return (
    <tr>
      <td className="col-name">
        <span className="file-icon" aria-hidden>
          {entry.dir ? '\u{1F4C1}' : '\u{1F4C4}'}
        </span>
        {entry.dir ? (
          <a className="file-name" href={hashFor(share, entry.path)}>
            {entry.name}
          </a>
        ) : (
          <a className="file-name" href={fileURL(share, entry.path)} download={entry.name}>
            {entry.name}
          </a>
        )}
      </td>
      <td className="col-size muted">{entry.dir ? '' : formatBytes(entry.size)}</td>
      <td className="col-time muted">{formatTime(entry.modifiedAt)}</td>
      <td className="col-actions">
        {!entry.dir && viewable.has(extension(entry.name)) && (
          <a className="btn btn-small" href={fileURL(share, entry.path, true)} target="_blank" rel="noreferrer">
            Open
          </a>
        )}
        {!entry.dir && (
          <a className="btn btn-small" href={fileURL(share, entry.path)} download={entry.name}>
            Download
          </a>
        )}
        {manage && (
          <details ref={menu} className="menu">
            <summary className="btn btn-small" aria-label="More actions">
              ⋯
            </summary>
            <div className="menu-items">
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
                <button className="menu-item menu-item-danger" onClick={() => pick({ kind: 'delete', entry })}>
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

function Breadcrumbs({ share, path }: { share: string; path: string }) {
  const segments = path.split('/').filter((s) => s !== '')
  let at = ''
  return (
    <nav className="crumbs" aria-label="Location">
      <a href="#/">Shares</a>
      <span className="crumb-sep">/</span>
      {segments.length === 0 ? <span className="crumb-current">{share}</span> : <a href={hashFor(share)}>{share}</a>}
      {segments.map((s, i) => {
        at += '/' + s
        const last = i === segments.length - 1
        return (
          <span key={at}>
            <span className="crumb-sep">/</span>
            {last ? <span className="crumb-current">{s}</span> : <a href={hashFor(share, at)}>{s}</a>}
          </span>
        )
      })}
    </nav>
  )
}

function ErrorNotice({ share, error }: { share: string; error: { message: string; status: number } }) {
  let text: string
  switch (error.status) {
    case 404:
      text = error.message === 'share not found' ? 'There is no share of this name.' : 'The folder was not found.'
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
