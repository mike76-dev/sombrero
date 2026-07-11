import { useEffect, useState } from 'react'
import { WorkgroupsPage } from './pages/WorkgroupsPage'
import { AccountsPage } from './pages/AccountsPage'
import { SharesPage } from './pages/SharesPage'
import { ConnectionsPage } from './pages/ConnectionsPage'
import { BansPage } from './pages/BansPage'
import { SettingsPage } from './pages/SettingsPage'

type Theme = 'light' | 'dark'

function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(
    () => (document.documentElement.dataset.theme as Theme) || 'light',
  )
  useEffect(() => {
    document.documentElement.dataset.theme = theme
    localStorage.setItem('sombrero.theme', theme)
  }, [theme])
  return [theme, () => setTheme((t) => (t === 'light' ? 'dark' : 'light'))]
}

const pages = {
  workgroups: { label: 'Workgroups', component: WorkgroupsPage },
  accounts: { label: 'Accounts', component: AccountsPage },
  shares: { label: 'Shares', component: SharesPage },
  connections: { label: 'Connections', component: ConnectionsPage },
  bans: { label: 'Bans', component: BansPage },
  settings: { label: 'Settings', component: SettingsPage },
} as const

type PageKey = keyof typeof pages

function pageFromHash(): PageKey {
  const hash = window.location.hash.replace(/^#/, '')
  return hash in pages ? (hash as PageKey) : 'workgroups'
}

export default function App() {
  const [theme, toggleTheme] = useTheme()
  const [page, setPage] = useState<PageKey>(pageFromHash)
  const Page = pages[page].component

  useEffect(() => {
    window.location.hash = page
    const onHashChange = () => setPage(pageFromHash())
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [page])

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          <span className="brand-icon" aria-hidden>
            <img src="/favicon.svg" width="30" height="30" alt="" />
          </span>
          Sombrero
        </div>
        <nav className="nav">
          {(Object.keys(pages) as PageKey[]).map((key) => (
            <button
              key={key}
              className={page === key ? 'nav-item nav-item-active' : 'nav-item'}
              onClick={() => setPage(key)}
            >
              {pages[key].label}
            </button>
          ))}
        </nav>
        <div className="sidebar-footer">
          <button className="btn" onClick={toggleTheme} title="Toggle theme">
            {theme === 'light' ? '\u{1F319} Dark mode' : '☀️ Light mode'}
          </button>
        </div>
      </aside>
      <main className="content">
        <h1 className="page-title">{pages[page].label}</h1>
        <Page />
      </main>
    </div>
  )
}
