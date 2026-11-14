import { ShareInfo } from './api'
import { hashFor } from './route'

export function SharesPage({ shares, error }: { shares?: ShareInfo[]; error: string | null }) {
  if (error) return <div className="banner banner-error">{error}</div>
  if (!shares) return <p className="muted">Loading the shares…</p>
  if (shares.length === 0) return <p className="muted">No shares are available to this account.</p>

  return (
    <div className="page">
      <h1 className="page-title">Shares</h1>
      <div className="grid">
        {shares.map((s) => (
          <a key={s.name} className="card share-card" href={hashFor(s.name)}>
            <h2 className="card-title">{s.name}</h2>
            {s.remark && <p className="muted">{s.remark}</p>}
            <div className="share-rights">
              {s.rights.read && <span className="flag flag-on">read</span>}
              {s.rights.write && <span className="flag flag-on">write</span>}
              {s.rights.delete && <span className="flag flag-on">delete</span>}
            </div>
          </a>
        ))}
      </div>
    </div>
  )
}
