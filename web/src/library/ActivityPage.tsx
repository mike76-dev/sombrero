import { ReactNode, useCallback, useEffect, useState } from 'react'
import { ActivityEntry, FilesError, activity } from './api'
import { describe } from './batch'
import { hashFor } from './route'
import { formatBytes } from '../format'

const page = 50

const dayFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'full' })
const clockFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'short' })

function folderOf(path: string): string {
  return path.slice(0, path.lastIndexOf('/')) || '/'
}

function nameOf(path: string): string {
  return path.slice(path.lastIndexOf('/') + 1)
}

// ActivityPage lists what the accounts of the workgroup did on the share,
// newest first and day by day, with more loaded on request.
export function ActivityPage({ share, onExpired }: { share: string; onExpired: () => void }) {
  const [entries, setEntries] = useState<ActivityEntry[] | undefined>(undefined)
  const [more, setMore] = useState(true)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(
    (before?: number) => {
      setLoading(true)
      setError(null)
      activity(share, before, page)
        .then((got) => {
          setEntries((es) => (before === undefined ? got : [...(es ?? []), ...got]))
          setMore(got.length === page)
        })
        .catch((e) => {
          if (e instanceof FilesError && e.status === 401) onExpired()
          else setError(describe(e))
        })
        .finally(() => setLoading(false))
    },
    [share, onExpired],
  )

  useEffect(() => {
    setEntries(undefined)
    setMore(true)
    load()
  }, [load])

  const days: { day: string; entries: ActivityEntry[] }[] = []
  for (const e of entries ?? []) {
    const day = dayFormat.format(new Date(e.at))
    const last = days[days.length - 1]
    if (last && last.day === day) last.entries.push(e)
    else days.push({ day, entries: [e] })
  }

  return (
    <div className="page">
      <nav className="crumbs" aria-label="Location">
        <a href="#/">Shares</a>
        <span className="crumb-sep">/</span>
        <a href={hashFor(share)}>{share}</a>
        <span className="crumb-sep">/</span>
        <span className="crumb-current">Activity</span>
      </nav>
      <p className="muted">What the accounts of your workgroup did on this share, over SMB, the file API, WebDAV and the editor alike, for the last 90 days.</p>
      {error && !entries ? (
        <div className="banner banner-error">{error}</div>
      ) : !entries ? (
        <p className="muted">Loading…</p>
      ) : entries.length === 0 ? (
        <p className="muted">Nothing has happened on this share yet.</p>
      ) : (
        <div className="activity">
          {days.map((d) => (
            <section key={d.day} className="activity-day">
              <h2 className="activity-date">{d.day}</h2>
              <ul className="activity-list">
                {d.entries.map((e) => (
                  <li key={e.id} className="activity-item">
                    <span className="activity-time muted">{clockFormat.format(new Date(e.at))}</span>
                    <span className="activity-text">
                      <strong>{e.username || 'Somebody'}</strong> <Sentence share={share} entry={e} />
                    </span>
                  </li>
                ))}
              </ul>
            </section>
          ))}
          {error && <div className="banner banner-error">{error}</div>}
          {more && (
            <button className="btn" disabled={loading} onClick={() => load(entries[entries.length - 1].id)}>
              {loading ? 'Loading…' : 'Show earlier activity'}
            </button>
          )}
        </div>
      )}
    </div>
  )
}

// Place links the folder a path is in, or the folder itself.
function Place({ share, path, folder }: { share: string; path: string; folder?: boolean }) {
  const name = path === '/' ? share : nameOf(path)
  if (folder) {
    return (
      <>
        the folder <a href={hashFor(share, path)}>{name}</a>
        <Within share={share} path={path} />
      </>
    )
  }
  return (
    <>
      <a href={hashFor(share, folderOf(path))}>{name}</a>
      <Within share={share} path={path} />
    </>
  )
}

// Within names the folder a path is in, unless that is the root.
function Within({ share, path }: { share: string; path: string }) {
  const folder = folderOf(path)
  if (folder === '/') return null
  return (
    <span className="muted">
      {' '}
      in <a href={hashFor(share, folder)}>{folder}</a>
    </span>
  )
}

// Sentence says what the entry records.
function Sentence({ share, entry: e }: { share: string; entry: ActivityEntry }): ReactNode {
  const size = e.size ? <span className="muted"> ({formatBytes(e.size)})</span> : null
  switch (e.action) {
    case 'upload':
      return (
        <>
          uploaded <Place share={share} path={e.path} />
          {size}
        </>
      )
    case 'update':
      return (
        <>
          updated <Place share={share} path={e.path} />
          {size}
        </>
      )
    case 'delete':
      return (
        <>
          deleted <Place share={share} path={e.path} />
        </>
      )
    case 'restore':
      return (
        <>
          restored a previous version of <Place share={share} path={e.path} />
          {size}
        </>
      )
    case 'mkdir':
      return (
        <>
          created <Place share={share} path={e.path} folder />
        </>
      )
    case 'rmdir':
      return (
        <>
          deleted <Place share={share} path={e.path} folder />
        </>
      )
    case 'rename': {
      const target = e.target ?? e.path
      if (folderOf(target) === folderOf(e.path)) {
        return (
          <>
            renamed <Place share={share} path={e.path} /> to <a href={hashFor(share, folderOf(target))}>{nameOf(target)}</a>
          </>
        )
      }
      return (
        <>
          moved <Place share={share} path={e.path} /> to <a href={hashFor(share, folderOf(target))}>{folderOf(target)}</a>
        </>
      )
    }
  }
}
