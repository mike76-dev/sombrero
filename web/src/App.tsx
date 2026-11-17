import { useEffect, useRef, useState } from 'react'
import { WizardPage } from './pages/WizardPage'
import { WorkgroupsPage } from './pages/WorkgroupsPage'
import { AccountsPage } from './pages/AccountsPage'
import { SharesPage } from './pages/SharesPage'
import { ConnectionsPage } from './pages/ConnectionsPage'
import { ImportPage } from './pages/ImportPage'
import { BackupPage } from './pages/BackupPage'
import { BansPage } from './pages/BansPage'
import { StatsPage } from './pages/StatsPage'
import { SettingsPage } from './pages/SettingsPage'
import { Icon, IconName } from './icons'
import { ThemeButton, useTheme } from './theme'

const pages = {
  wizard: { label: 'Setup wizard', icon: 'wand', component: WizardPage },
  workgroups: { label: 'Workgroups', icon: 'users', component: WorkgroupsPage },
  accounts: { label: 'Accounts', icon: 'user', component: AccountsPage },
  shares: { label: 'Shares', icon: 'folder', component: SharesPage },
  connections: { label: 'Connections', icon: 'link', component: ConnectionsPage },
  import: { label: 'Import', icon: 'import', component: ImportPage },
  backup: { label: 'Backup', icon: 'archive', component: BackupPage },
  bans: { label: 'Bans', icon: 'ban', component: BansPage },
  stats: { label: 'Stats', icon: 'chart', component: StatsPage },
  settings: { label: 'Settings', icon: 'settings', component: SettingsPage },
} satisfies Record<string, { label: string; icon: IconName; component: () => JSX.Element }>

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

  // On a phone the pages are a menu behind a button in the header, closed by
  // a choice, a tap outside or Escape.
  const [menuOpen, setMenuOpen] = useState(false)
  const sidebar = useRef<HTMLElement>(null)
  useEffect(() => {
    if (!menuOpen) return
    const outside = (e: PointerEvent) => {
      if (!sidebar.current?.contains(e.target as Node)) setMenuOpen(false)
    }
    const key = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setMenuOpen(false)
    }
    document.addEventListener('pointerdown', outside)
    document.addEventListener('keydown', key)
    return () => {
      document.removeEventListener('pointerdown', outside)
      document.removeEventListener('keydown', key)
    }
  }, [menuOpen])

  return (
    <div className="app admin">
      <aside className="sidebar" ref={sidebar}>
        <div className="brand">
          <span className="brand-icon" aria-hidden>
            <img src="/favicon.svg" width="30" height="30" alt="" />
          </span>
          Sombrero
        </div>
        <nav className={menuOpen ? 'nav nav-open' : 'nav'} aria-label="Pages">
          {(Object.keys(pages) as PageKey[]).map((key) => (
            <button
              key={key}
              className={page === key ? 'nav-item nav-item-active' : 'nav-item'}
              onClick={() => {
                setPage(key)
                setMenuOpen(false)
              }}
            >
              <Icon name={pages[key].icon} />
              {pages[key].label}
            </button>
          ))}
        </nav>
        <div className="sidebar-footer">
          <SourceLink />
          <ThemeButton theme={theme} toggle={toggleTheme} />
        </div>
        <button
          className="btn-icon menu-button"
          aria-label={menuOpen ? 'Close the menu' : 'Open the menu'}
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen((o) => !o)}
        >
          <Icon name={menuOpen ? 'close' : 'menu'} size={22} />
        </button>
      </aside>
      <main className="content">
        <h1 className="page-title">{pages[page].label}</h1>
        <Page />
        <SourceLink foot />
      </main>
    </div>
  )
}

// The source link sits in the sidebar, or at the foot of the page where the
// sidebar is a header.
function SourceLink({ foot = false }: { foot?: boolean }) {
  return (
    <a className={foot ? 'source-link source-link-foot' : 'source-link'} href="https://github.com/mike76-dev/sombrero" target="_blank" rel="noreferrer">
      Source
    </a>
  )
}
