import { useEffect, useState } from 'react'
import {
  connect,
  connectionKey,
  disconnect,
  listShares,
  requestConnection,
} from '../api/endpoints'
import {
  Card,
  CopyButton,
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
  const [preKey, setPreKey] = useState('')
  const [keyFrom, setKeyFrom] = useState('')
  const [shownKey, setShownKey] = useState<string | null>(null)
  const ready = Boolean(workgroup.trim() && share.trim())
  const attempt = useConnectAttempt(workgroup.trim(), share.trim())

  // A key on show belongs to the pair it was asked for.
  useEffect(() => setShownKey(null), [workgroup, share])

  // What the selected share is backed by decides how it is connected: an indexd
  // share is connected for the first time by approving a request or with a key
  // the indexer authorized in advance, and only a reconnection takes an app key.
  const backend = shares?.find((s) => s.name === share.trim())?.type
  const reconnecting = Boolean(appKey.trim())
  const preAuthorized = Boolean(preKey.trim())
  const reusable = backend === 'indexd' && attempt.reusable

  // Another workgroup's key is offered where this one has none of its own, and the
  // choice is dropped as soon as the pair on show is no longer the one it was for.
  const holders =
    backend === 'indexd' && !reusable && !reconnecting && !preAuthorized ? attempt.keyFrom : []
  const sharing = holders.some((holder) => holder.workgroup === keyFrom) ? keyFrom : ''
  const connectable =
    backend === 'renterd' ||
    (backend === 'indexd' && (reconnecting || preAuthorized || reusable || Boolean(sharing)))
  const running = attempt.running

  return (
    <div className="page">
      <Card title="Connect a workgroup to a share">
        <p className="muted">
          For a <code>renterd</code> share, press <em>Connect</em>. An <code>indexd</code>{' '}
          share needs an app key. The first time, either press <em>Request approval</em>, open
          the link and approve the registration with the indexer, or paste a pre-authorized key
          obtained from the indexer and press <em>Connect</em>: the connection completes by
          itself either way, and the new app key is shown here once. Keep it, because
          reconnecting later means pasting it back in. If the workgroup is already connected to
          another share on the same indexer, it reuses the key it has there and needs none of
          this. It can also borrow another workgroup's key, but then the two share one account
          at the indexer, including its quota.
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
          <Field label="Pre-authorized key (optional, hex)">
            <input
              type="text"
              value={preKey}
              onChange={(e) => setPreKey(e.target.value)}
              placeholder="a first indexd connection without approval"
              autoComplete="off"
            />
          </Field>
          <ShareKeyField holders={holders} value={sharing} onChange={setKeyFrom} />
        </div>
        <div className="row">
          <button
            className="btn"
            disabled={
              busy ||
              running ||
              !ready ||
              backend !== 'indexd' ||
              preAuthorized ||
              reconnecting ||
              attempt.connected
            }
            onClick={() =>
              run(async () => {
                attempt.begin(await requestConnection(workgroup.trim(), share.trim()))
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
                  preAuthorizedKey: !appKey.trim() ? preKey.trim() || undefined : undefined,
                  fromWorkgroup: sharing || undefined,
                }
                attempt.begin(await connect(workgroup.trim(), share.trim(), from))
                setPreKey('')
              })
            }
          >
            Connect
          </button>
          <button
            className="btn"
            disabled={busy || running || !ready || backend !== 'indexd' || !attempt.connected}
            onClick={() =>
              run(async () => {
                setShownKey((await connectionKey(workgroup.trim(), share.trim())).appKey)
              })
            }
          >
            Show app key
          </button>
          <button
            className="btn btn-danger"
            disabled={busy || running || !ready}
            onClick={() => {
              if (!window.confirm(`Disconnect ${share.trim()} from this workgroup?`)) return
              run(async () => {
                await disconnect(workgroup.trim(), share.trim())
                attempt.idle()
                setShownKey(null)
              }, 'Disconnected.')
            }}
          >
            Disconnect
          </button>
        </div>
        {shownKey && (
          <div className="banner banner-success stack">
            <div>
              The app key of this connection. Keep a copy somewhere safe: it is the only thing
              that can read the account's data.
            </div>
            <div className="mono appkey">
              {shownKey} <CopyButton value={shownKey} />
            </div>
          </div>
        )}
        {ready && backend === 'indexd' && !reconnecting && !running && !attempt.connected && (
          <p className="muted">
            {preAuthorized ? (
              <>
                <em>Connect</em> will register a new app key with the pre-authorized key, which
                is spent by it and not kept.
              </>
            ) : reusable ? (
              <>
                This workgroup is already connected to another share on this indexer.{' '}
                <em>Connect</em> will reuse the app key from there, so no approval is needed.
              </>
            ) : sharing ? (
              <>
                <em>Connect</em> will use that workgroup's app key. Both workgroups then share
                one account at the indexer, so they share its quota, and revoking the key there
                cuts off both.
              </>
            ) : (
              <>
                This share has not been connected yet. Press <em>Request approval</em> to
                register a new app key, paste a pre-authorized key to register one without the
                approval, or paste a saved app key to reconnect.
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
