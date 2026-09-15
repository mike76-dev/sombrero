import { useState } from 'react'
import { connect, disconnect, listShares, requestConnection } from '../api/endpoints'
import {
  Card,
  ErrorBanner,
  Field,
  SuccessBanner,
  useApiAction,
  useApiData,
} from '../components/common'
import { ConnectStatusView, useConnectAttempt } from '../components/connect'
import { ShareSelect, WorkgroupSelect } from '../components/selects'

export function ConnectionsPage() {
  const { run, busy, error, message } = useApiAction()
  const { data: shares } = useApiData(() => listShares())
  const [workgroup, setWorkgroup] = useState('')
  const [share, setShare] = useState('')
  const [appKey, setAppKey] = useState('')
  const ready = Boolean(workgroup.trim() && share.trim())
  const attempt = useConnectAttempt(workgroup.trim(), share.trim())

  // What the selected share is backed by decides how it is connected: an indexd
  // share is connected for the first time by approving a request, and only a
  // reconnection of one takes an app key.
  const backend = shares?.find((s) => s.name === share.trim())?.type
  const reconnecting = Boolean(appKey.trim())
  const connectable = backend === 'renterd' || (backend === 'indexd' && reconnecting)
  const running = attempt.running

  return (
    <div className="page">
      <Card title="Connect a workgroup to a share">
        <p className="muted">
          renterd shares: just press <em>Connect</em>. indexd shares connecting for the first
          time: press <em>Request approval</em> and open the approval link — approving the
          registration with the indexer is what carries the connection through, and the app key
          it derives is shown here once it has. Reconnecting an indexd share: paste the saved
          app key and press <em>Connect</em>.
        </p>
        <div className="grid">
          <Field label="Workgroup">
            <WorkgroupSelect value={workgroup} onChange={setWorkgroup} />
          </Field>
          <Field label="Share">
            <ShareSelect value={share} onChange={setShare} />
          </Field>
          <Field label="App key (optional, hex)">
            <input
              type="text"
              value={appKey}
              onChange={(e) => setAppKey(e.target.value)}
              placeholder="only for indexd reconnection"
              autoComplete="off"
            />
          </Field>
        </div>
        <div className="row">
          <button
            className="btn"
            disabled={busy || running || !ready || backend !== 'indexd'}
            onClick={() =>
              run(async () => {
                attempt.begin(await requestConnection(workgroup.trim(), share.trim()), true)
              })
            }
          >
            Request approval (indexd)
          </button>
          <button
            className="btn btn-primary"
            disabled={busy || running || !ready || !connectable}
            onClick={() =>
              run(async () => {
                const key = appKey.trim() || undefined
                attempt.begin(await connect(workgroup.trim(), share.trim(), key), false)
              })
            }
          >
            Connect
          </button>
          <button
            className="btn btn-danger"
            disabled={busy || running || !ready}
            onClick={() => {
              if (!window.confirm(`Disconnect ${share.trim()} from this workgroup?`)) return
              run(async () => {
                await disconnect(workgroup.trim(), share.trim())
                attempt.idle()
              }, 'Disconnected.')
            }}
          >
            Disconnect
          </button>
        </div>
        {ready && backend === 'indexd' && !reconnecting && !running && (
          <p className="muted">
            This indexd share is connected by approving a request, not by pressing{' '}
            <em>Connect</em>. Paste the app key of an existing connection to reconnect one.
          </p>
        )}
        <ConnectStatusView attempt={attempt} />
        <ErrorBanner error={error} />
        {!attempt.appKey && <SuccessBanner message={message} />}
      </Card>
    </div>
  )
}
