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
import { ConnectStatusView, ShareKeyField, useConnectAttempt } from '../components/connect'
import { ShareSelect, WorkgroupSelect } from '../components/selects'

export function ConnectionsPage() {
  const { run, busy, error, message } = useApiAction()
  const { data: shares } = useApiData(() => listShares())
  const [workgroup, setWorkgroup] = useState('')
  const [share, setShare] = useState('')
  const [appKey, setAppKey] = useState('')
  const [keyFrom, setKeyFrom] = useState('')
  const ready = Boolean(workgroup.trim() && share.trim())
  const attempt = useConnectAttempt(workgroup.trim(), share.trim())

  // What the selected share is backed by decides how it is connected: an indexd
  // share is connected for the first time by approving a request, and only a
  // reconnection of one takes an app key.
  const backend = shares?.find((s) => s.name === share.trim())?.type
  const reconnecting = Boolean(appKey.trim())
  const reusable = backend === 'indexd' && attempt.reusable

  // Another workgroup's key is offered where this one has none of its own, and the
  // choice is dropped as soon as the pair on show is no longer the one it was for.
  const holders = backend === 'indexd' && !reusable && !reconnecting ? attempt.keyFrom : []
  const sharing = holders.some((holder) => holder.workgroup === keyFrom) ? keyFrom : ''
  const connectable =
    backend === 'renterd' ||
    (backend === 'indexd' && (reconnecting || reusable || Boolean(sharing)))
  const running = attempt.running

  return (
    <div className="page">
      <Card title="Connect a workgroup to a share">
        <p className="muted">
          renterd shares: just press <em>Connect</em>. indexd shares connecting for the first
          time: press <em>Request approval</em> and open the approval link — approving the
          registration with the indexer is what carries the connection through, and the app key
          it derives is shown here once it has. Reconnecting an indexd share: paste the saved
          app key and press <em>Connect</em>. A workgroup that is already on another share of
          the same indexer needs neither — its key for that indexer is reused. A workgroup with
          no key of its own can instead share another workgroup's, which puts both on that
          workgroup's indexer account and its quota.
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
          <ShareKeyField holders={holders} value={sharing} onChange={setKeyFrom} />
        </div>
        <div className="row">
          <button
            className="btn"
            disabled={busy || running || !ready || backend !== 'indexd' || attempt.connected}
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
            disabled={busy || running || !ready || !connectable || attempt.connected}
            onClick={() =>
              run(async () => {
                const from = {
                  appKey: appKey.trim() || undefined,
                  fromWorkgroup: sharing || undefined,
                }
                attempt.begin(await connect(workgroup.trim(), share.trim(), from), false)
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
        {ready && backend === 'indexd' && !reconnecting && !running && !attempt.connected && (
          <p className="muted">
            {reusable ? (
              <>
                This workgroup is already connected to another share of this indexer, so{' '}
                <em>Connect</em> reuses the app key it has there — no approval needed.
              </>
            ) : sharing ? (
              <>
                <em>Connect</em> will use that workgroup's app key, which shares its indexer
                account: one quota for both, and revoking the key at the indexer cuts off both.
              </>
            ) : (
              <>
                This indexd share is connected by approving a request, not by pressing{' '}
                <em>Connect</em>. Paste the app key of an existing connection to reconnect one.
              </>
            )}
          </p>
        )}
        <ConnectStatusView attempt={attempt} />
        <ErrorBanner error={error} />
        {!attempt.appKey && <SuccessBanner message={message} />}
      </Card>
    </div>
  )
}
