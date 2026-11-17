import { useEffect, useState } from 'react'
import { Entry, FilesError, fileURL, search } from './api'
import { describe } from './batch'
import { canShow } from './Browser'
import { FileIcon } from './icons'
import { hashFor } from './route'
import { SearchBox } from './SearchBox'
import { formatBytes, formatTime } from '../format'

function folderOf(path: string): string {
  return path.slice(0, path.lastIndexOf('/')) || '/'
}

// SearchPage lists the files and folders of a share whose names contain the
// text, each with the folder it is in.
export function SearchPage({ share, q, onExpired }: { share: string; q: string; onExpired: () => void }) {
  const [entries, setEntries] = useState<Entry[] | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setEntries(undefined)
    setError(null)
    search(share, q)
      .then((es) => {
        if (!cancelled) setEntries(es)
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof FilesError && e.status === 401) onExpired()
        else setError(describe(e))
      })
    return () => {
      cancelled = true
    }
  }, [share, q, onExpired])

  return (
    <div className="page">
      <div className="row row-spread">
        <nav className="crumbs" aria-label="Location">
          <a href="#/">Shares</a>
          <span className="crumb-sep">/</span>
          <a href={hashFor(share)}>{share}</a>
          <span className="crumb-sep">/</span>
          <span className="crumb-current">Search</span>
        </nav>
        <SearchBox share={share} initial={q} />
      </div>
      {error ? (
        <div className="banner banner-error">{error}</div>
      ) : !entries ? (
        <p className="muted">Searching…</p>
      ) : entries.length === 0 ? (
        <p className="muted">Nothing on this share is named like “{q}”.</p>
      ) : (
        <>
          <p className="muted">
            {entries.length === 1 ? 'One item is' : `${entries.length} items are`} named like “{q}”{entries.length >= 200 && ', the first 200 by path'}.
          </p>
          <table className="table files">
            <thead>
              <tr>
                <th>Name</th>
                <th>Folder</th>
                <th className="col-size">Size</th>
                <th className="col-time">Modified</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.path}>
                  <td className="col-name">
                    <FileIcon name={e.name} dir={e.dir} />
                    {e.dir ? (
                      <a className="file-name" href={hashFor(share, e.path)}>
                        {e.name}
                      </a>
                    ) : canShow(e.name) ? (
                      <a className="file-name" href={fileURL(share, e.path, true)} target="_blank" rel="noreferrer">
                        {e.name}
                      </a>
                    ) : (
                      <a className="file-name" href={fileURL(share, e.path)} download={e.name}>
                        {e.name}
                      </a>
                    )}
                  </td>
                  <td className="muted">
                    <a href={hashFor(share, folderOf(e.path))}>{folderOf(e.path)}</a>
                  </td>
                  <td className="col-size muted">{e.dir ? '' : formatBytes(e.size)}</td>
                  <td className="col-time muted">{formatTime(e.modifiedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}
