import { useEffect, useMemo, useState } from 'react'
import { Entry, FilesError, Listing, fileURL, list } from './api'
import { hashFor } from './route'
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

export function Browser({ share, path, onExpired }: { share: string; path: string; onExpired: () => void }) {
  const [listing, setListing] = useState<Listing | undefined>(undefined)
  const [error, setError] = useState<{ message: string; status: number } | null>(null)
  const [sort, setSort] = useState<Sort>(loadSort)

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
  }, [share, path, onExpired])

  useEffect(() => {
    localStorage.setItem(sortKey, JSON.stringify(sort))
  }, [sort])

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

  const stale = listing && (listing.share !== share || listing.path !== path)
  return (
    <div className="page">
      <Breadcrumbs share={share} path={path} />
      {error ? (
        <ErrorNotice share={share} error={error} />
      ) : !listing || stale ? (
        <p className="muted">Loading…</p>
      ) : entries.length === 0 ? (
        <p className="muted">This folder is empty.</p>
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
              <Row key={e.path} share={share} entry={e} />
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

function Row({ share, entry }: { share: string; entry: Entry }) {
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
        {!entry.dir && (
          <>
            {viewable.has(extension(entry.name)) && (
              <a className="btn btn-small" href={fileURL(share, entry.path, true)} target="_blank" rel="noreferrer">
                Open
              </a>
            )}
            <a className="btn btn-small" href={fileURL(share, entry.path)} download={entry.name}>
              Download
            </a>
          </>
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
