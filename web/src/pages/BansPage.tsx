import { useState } from 'react'
import { banHost, clearBans, getBanStatus, unbanHost } from '../api/endpoints'
import { Card, ErrorBanner, Field, SuccessBanner, useApiAction } from '../components/common'

export function BansPage() {
  const { run, busy, error, message, setMessage } = useApiAction()
  const clear = useApiAction()
  const [host, setHost] = useState('')
  const [reason, setReason] = useState('')
  const ready = !!host.trim()

  return (
    <div className="page">
      <Card title="Host bans">
        <div className="row row-form">
          <Field label="Host (IP address)">
            <input
              type="text"
              value={host}
              onChange={(e) => setHost(e.target.value)}
              placeholder="192.168.1.100"
            />
          </Field>
          <Field label="Ban reason (optional)">
            <input type="text" value={reason} onChange={(e) => setReason(e.target.value)} />
          </Field>
        </div>
        <div className="row">
          <button
            className="btn"
            disabled={busy || !ready}
            onClick={() =>
              run(async () => {
                const res = await getBanStatus(host.trim())
                setMessage(
                  res.banned
                    ? `${host.trim()} is banned${res.reason ? ': ' + res.reason : ''}`
                    : `${host.trim()} is not banned`,
                )
              })
            }
          >
            Check status
          </button>
          <button
            className="btn btn-danger"
            disabled={busy || !ready}
            onClick={() => run(() => banHost(host.trim(), reason.trim()), 'Host banned.')}
          >
            Ban
          </button>
          <button
            className="btn"
            disabled={busy || !ready}
            onClick={() => run(() => unbanHost(host.trim()), 'Host unbanned.')}
          >
            Unban
          </button>
        </div>
        <ErrorBanner error={error} />
        <SuccessBanner message={message} />
      </Card>

      <Card title="Clear all bans">
        <div className="row">
          <button
            className="btn btn-danger"
            disabled={clear.busy}
            onClick={() => {
              if (!window.confirm('Remove ALL host bans?')) return
              clear.run(() => clearBans(), 'All bans cleared.')
            }}
          >
            Clear all bans
          </button>
        </div>
        <ErrorBanner error={clear.error} />
        <SuccessBanner message={clear.message} />
      </Card>
    </div>
  )
}
