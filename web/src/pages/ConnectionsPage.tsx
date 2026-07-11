import { useState } from 'react'
import { connect, disconnect, requestConnection } from '../api/endpoints'
import { Card, CopyButton, ErrorBanner, Field, SuccessBanner, useApiAction } from '../components/common'
import { ShareSelect, WorkgroupSelect } from '../components/selects'

export function ConnectionsPage() {
  const { run, busy, error, message } = useApiAction()
  const [workgroup, setWorkgroup] = useState('')
  const [share, setShare] = useState('')
  const [appKey, setAppKey] = useState('')
  const [approvalUrl, setApprovalUrl] = useState<string | null>(null)
  const [newAppKey, setNewAppKey] = useState<string | null>(null)
  const ready = workgroup.trim() && share.trim()

  return (
    <div className="page">
      <Card title="Connect a workgroup to a share">
        <p className="muted">
          renterd shares: just press <em>Connect</em>. indexd shares connecting for the first
          time: press <em>Request approval</em>, open the approval link, approve the
          registration with the indexer, then press <em>Connect</em>. Reconnecting an indexd
          share: paste the saved app key and press <em>Connect</em>.
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
            disabled={busy || !ready}
            onClick={() =>
              run(async () => {
                const res = await requestConnection(workgroup.trim(), share.trim())
                setApprovalUrl(res.url)
              })
            }
          >
            Request approval (indexd)
          </button>
          <button
            className="btn btn-primary"
            disabled={busy || !ready}
            onClick={() =>
              run(async () => {
                setNewAppKey(null)
                const res = await connect(workgroup.trim(), share.trim(), appKey.trim() || undefined)
                if (res && 'appKey' in res) {
                  setNewAppKey(res.appKey)
                  setApprovalUrl(null)
                }
              }, 'Connected.')
            }
          >
            Connect
          </button>
          <button
            className="btn btn-danger"
            disabled={busy || !ready}
            onClick={() => {
              if (!window.confirm(`Disconnect ${share.trim()} from this workgroup?`)) return
              run(() => disconnect(workgroup.trim(), share.trim()), 'Disconnected.')
            }}
          >
            Disconnect
          </button>
        </div>
        {approvalUrl && (
          <div className="banner banner-success">
            Approval requested. Open{' '}
            <a href={approvalUrl} target="_blank" rel="noreferrer">
              this link
            </a>{' '}
            to approve the registration, then press <em>Connect</em> (within 10 minutes).
          </div>
        )}
        {newAppKey && (
          <div className="banner banner-success stack">
            <div>
              Connected. Save this app key — it is required to reconnect this workgroup to
              the share and is shown only once:
            </div>
            <div className="mono appkey">
              {newAppKey} <CopyButton value={newAppKey} />
            </div>
          </div>
        )}
        <ErrorBanner error={error} />
        {!newAppKey && <SuccessBanner message={message} />}
      </Card>
    </div>
  )
}
