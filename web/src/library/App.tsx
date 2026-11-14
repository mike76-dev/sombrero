import { ReactNode, useCallback, useEffect, useState } from 'react'
import { FilesError, Identity, ShareInfo, logout, me, shares } from './api'
import { Browser } from './Browser'
import { DeletedPage } from './DeletedPage'
import { LoginPage } from './LoginPage'
import { SharesPage } from './SharesPage'
import { hashFor, useRoute } from './route'
import { ThemeButton, useTheme } from '../theme'

// The session the page holds: not known yet, none, or an account's. The file
// API being off is a state of its own, so that the page can say so.
type Session = { state: 'loading' } | { state: 'out'; notice?: string } | { state: 'off' } | { state: 'in'; identity: Identity }

export default function App() {
  const [theme, toggleTheme] = useTheme()
  const [session, setSession] = useState<Session>({ state: 'loading' })

  useEffect(() => {
    me()
      .then((identity) => setSession({ state: 'in', identity }))
      .catch((e) => {
        if (e instanceof FilesError && e.status === 404) setSession({ state: 'off' })
        else setSession({ state: 'out' })
      })
  }, [])

  // Whatever call finds the session gone sends the page back to the login.
  const expired = useCallback(() => {
    setSession({ state: 'out', notice: 'The session has expired. Please log in again.' })
  }, [])

  switch (session.state) {
    case 'loading':
      return null
    case 'off':
      return (
        <Centered theme={theme} toggleTheme={toggleTheme}>
          <p>The file API is not enabled on this server, so there are no shares to browse here.</p>
          <p className="muted">
            Server administration is at <a href="/admin/">/admin</a>.
          </p>
        </Centered>
      )
    case 'out':
      return (
        <Centered theme={theme} toggleTheme={toggleTheme}>
          <LoginPage notice={session.notice} onLogin={(identity) => setSession({ state: 'in', identity })} />
        </Centered>
      )
    case 'in':
      return (
        <Library
          identity={session.identity}
          theme={theme}
          toggleTheme={toggleTheme}
          onExpired={expired}
          onLogout={() => {
            logout().finally(() => setSession({ state: 'out' }))
          }}
        />
      )
  }
}

function Brand() {
  return (
    <a className="brand" href="#/">
      <span className="brand-icon" aria-hidden>
        <img src="/favicon.svg" width="30" height="30" alt="" />
      </span>
      Sombrero
    </a>
  )
}

function SourceLink() {
  return (
    <a className="source-link" href="https://github.com/mike76-dev/sombrero" target="_blank" rel="noreferrer">
      Source
    </a>
  )
}

function Centered({ children, theme, toggleTheme }: { children: ReactNode; theme: 'light' | 'dark'; toggleTheme: () => void }) {
  return (
    <div className="centered">
      <div className="centered-card card">
        <Brand />
        {children}
      </div>
      <div className="centered-footer">
        <a className="source-link" href="/admin/">
          Server administration
        </a>
        <SourceLink />
        <ThemeButton theme={theme} toggle={toggleTheme} />
      </div>
    </div>
  )
}

function Library({
  identity,
  theme,
  toggleTheme,
  onExpired,
  onLogout,
}: {
  identity: Identity
  theme: 'light' | 'dark'
  toggleTheme: () => void
  onExpired: () => void
  onLogout: () => void
}) {
  const route = useRoute()
  const [shareList, setShareList] = useState<ShareInfo[] | undefined>(undefined)
  const [shareError, setShareError] = useState<string | null>(null)

  // The tab names the place: the folder, the share, or its deleted files.
  useEffect(() => {
    const folder = route.path.slice(route.path.lastIndexOf('/') + 1)
    const place = route.share === null ? '' : route.bin ? `Deleted files · ${route.share}` : folder || route.share
    document.title = place ? `${place} · Sombrero` : 'Sombrero'
    return () => {
      document.title = 'Sombrero'
    }
  }, [route])

  useEffect(() => {
    shares()
      .then(setShareList)
      .catch((e) => {
        if (e instanceof FilesError && e.status === 401) onExpired()
        else setShareError(e instanceof Error ? e.message : String(e))
      })
  }, [onExpired])

  const workgroup = identity.workgroup.name || identity.workgroup.uuid
  return (
    <div className="app lib">
      <aside className="sidebar">
        <Brand />
        <nav className="nav">
          {shareList?.map((s) => (
            <a
              key={s.name}
              className={route.share === s.name ? 'nav-item nav-item-active' : 'nav-item'}
              href={hashFor(s.name)}
            >
              {s.name}
            </a>
          ))}
        </nav>
        <div className="sidebar-footer">
          <div className="identity" title={`${identity.username} in workgroup ${workgroup}`}>
            <span className="identity-user">{identity.username}</span>
            <span className="identity-workgroup muted">{workgroup}</span>
          </div>
          <SourceLink />
          <ThemeButton theme={theme} toggle={toggleTheme} />
          <button className="btn" onClick={onLogout}>
            Log out
          </button>
        </div>
      </aside>
      <main className="content">
        {route.share === null ? (
          <SharesPage shares={shareList} error={shareError} />
        ) : route.bin ? (
          <DeletedPage share={route.share} shareRights={shareList?.find((s) => s.name === route.share)?.rights} onExpired={onExpired} />
        ) : (
          <Browser
            share={route.share}
            shareRights={shareList?.find((s) => s.name === route.share)?.rights}
            path={route.path}
            at={route.at}
            onExpired={onExpired}
          />
        )}
      </main>
    </div>
  )
}
