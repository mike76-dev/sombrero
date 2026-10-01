import { useCallback, useEffect, useRef, useState } from 'react'
import {
  cancelImport,
  importStatus,
  listAccounts,
  listShares,
  startImport,
} from '../api/endpoints'
import { ImportRequest, ImportStatusResponse } from '../api/types'
import {
  Card,
  ErrorBanner,
  Field,
  formatBytes,
  useApiAction,
  useApiData,
} from '../components/common'
import { ServerAddressField } from '../components/serveraddress'
import { ShareSelect, WorkgroupSelect } from '../components/selects'

// How often a running import is asked where it has got to.
const pollInterval = 1000

// running says whether an import is still going, which is what the poll and the
// disabled buttons hang off.
function running(status: ImportStatusResponse | null): boolean {
  return status?.state === 'running'
}

export function ImportPage() {
  const { run, busy, error } = useApiAction()
  const { data: shares } = useApiData(() => listShares())
  const [workgroup, setWorkgroup] = useState('')
  const [share, setShare] = useState('')
  const [source, setSource] = useState<'renterd' | 'indexd'>('renterd')
  const [address, setAddress] = useState('')
  const [password, setPassword] = useState('')
  const [bucket, setBucket] = useState('default')
  const [appKey, setAppKey] = useState('')
  const [prefix, setPrefix] = useState('')
  const [copy, setCopy] = useState(false)
  const [username, setUsername] = useState('')
  const [status, setStatus] = useState<ImportStatusResponse | null>(null)
  const [pollError, setPollError] = useState<string | null>(null)

  const { data: accounts } = useApiData(
    () => (workgroup ? listAccounts(workgroup) : Promise.resolve(null)),
    [workgroup],
  )

  // The share has to be one this server keeps the files of, since that is what
  // an import writes its rows to.
  const backend = shares?.find((s) => s.name === share.trim())?.type

  // Nothing of renterd's can be pinned, so an import from one copies whether it
  // is asked to or not.
  const copying = source === 'renterd' || copy
  const ready = Boolean(
    workgroup.trim() &&
      share.trim() &&
      backend === 'indexd' &&
      username.trim() &&
      address.trim() &&
      (source === 'renterd' || appKey.trim()),
  )

  // One poll follows an import to its end, whether it was started here or was
  // already running when the pair was named: every answer that is still running
  // asks again.
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const stopped = useRef(false)
  const poll = useCallback(async (wg: string, sh: string) => {
    try {
      const res = await importStatus(wg, sh)
      if (stopped.current) return
      setPollError(null)
      setStatus(res)
      if (res.state === 'running') {
        timer.current = setTimeout(() => poll(wg, sh), pollInterval)
      }
    } catch (e) {
      if (!stopped.current) setPollError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    stopped.current = false
    setStatus(null)
    setPollError(null)
    clearTimeout(timer.current)
    if (workgroup.trim() && share.trim()) poll(workgroup.trim(), share.trim())

    return () => {
      stopped.current = true
      clearTimeout(timer.current)
    }
  }, [workgroup, share, poll])

  // follow takes over from a call that started or called off an import: what it
  // answered is the first status, and the poll carries it from there.
  const follow = (res: ImportStatusResponse) => {
    setStatus(res)
    clearTimeout(timer.current)
    if (res.state === 'running') {
      timer.current = setTimeout(() => poll(workgroup.trim(), share.trim()), pollInterval)
    }
  }

  return (
    <div className="page">
      <Card title="Import from another server">
        <p className="muted">
          An import brings files from a renterd server or from another indexd account into this
          share. Where the indexer can pin the data, it is taken over as it is: nothing is
          transferred, and this account starts paying for it. Everything else, including all
          renterd data, is downloaded from the source and uploaded again, so the same data is
          paid for twice until you delete it at the source.
        </p>
        <div className="group-label">Where the files are imported to</div>
        <div className="grid">
          <Field label="Workgroup">
            <WorkgroupSelect value={workgroup} onChange={setWorkgroup} />
          </Field>
          <Field label="Share">
            <ShareSelect value={share} onChange={setShare} />
          </Field>
          <Field label="Owner of the imported files">
            <select value={username} onChange={(e) => setUsername(e.target.value)}>
              <option value="">
                {!workgroup
                  ? '— select a workgroup first —'
                  : (accounts?.length ?? 0) === 0
                    ? '— no accounts in this workgroup —'
                    : '— select account —'}
              </option>
              {(accounts || []).map((a) => (
                <option key={a.username} value={a.username}>
                  {a.username}
                </option>
              ))}
            </select>
          </Field>
        </div>

        <div className="group-label">Where the files come from</div>
        <div className="row">
          {(
            [
              ['renterd', 'A renterd server'],
              ['indexd', 'Another indexd account'],
            ] as const
          ).map(([key, label]) => (
            <label className="checkbox" key={key}>
              <input
                type="radio"
                checked={source === key}
                onChange={() => setSource(key)}
                disabled={running(status)}
              />
              {label}
            </label>
          ))}
        </div>

        <div className="grid">
          <ServerAddressField
            value={address}
            onChange={setAddress}
            backend={source}
            disabled={running(status)}
          />
          {source === 'renterd' ? (
            <>
              <Field label="API password">
                <input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="off"
                />
              </Field>
              <Field label="Bucket">
                <input
                  type="text"
                  value={bucket}
                  onChange={(e) => setBucket(e.target.value)}
                  autoComplete="off"
                />
              </Field>
            </>
          ) : (
            <>
              <Field label="App key (hex)">
                <input
                  type="text"
                  value={appKey}
                  onChange={(e) => setAppKey(e.target.value)}
                  autoComplete="off"
                />
              </Field>
              <Field label="Import into folder">
                <input
                  type="text"
                  value={prefix}
                  onChange={(e) => setPrefix(e.target.value)}
                  placeholder="/lost+found"
                  autoComplete="off"
                />
              </Field>
            </>
          )}
        </div>

        {source === 'indexd' && (
          <p className="muted">
            An indexd account stores objects, not file names — the names live in the database
            of the server that uploaded them. The data therefore arrives as one file per
            object under the folder above, for you to sort out.
          </p>
        )}

        <label className="checkbox">
          <input
            type="checkbox"
            checked={copying}
            onChange={(e) => setCopy(e.target.checked)}
            disabled={running(status) || source === 'renterd'}
          />
          copy everything instead of pinning
        </label>

        <div className="row">
          <button
            className="btn btn-primary"
            disabled={busy || !ready || running(status)}
            onClick={() =>
              run(async () => {
                const body: ImportRequest = {
                  source,
                  address: address.trim(),
                  username: username.trim(),
                  copy: copying,
                  ...(source === 'renterd'
                    ? { password, bucket: bucket.trim() }
                    : { appKey: appKey.trim(), prefix: prefix.trim() || undefined }),
                }
                follow(await startImport(workgroup.trim(), share.trim(), body))
              })
            }
          >
            Start
          </button>
          <button
            className="btn btn-danger"
            disabled={busy || !running(status)}
            onClick={() =>
              run(async () => {
                follow(await cancelImport(workgroup.trim(), share.trim()))
              })
            }
          >
            Cancel
          </button>
        </div>

        {ready && backend !== 'indexd' && (
          <p className="muted">
            Only an indexd share can import data. A renterd share keeps its own file list.
          </p>
        )}

        <ImportStatusView status={status} />
        <ErrorBanner error={pollError} />
        <ErrorBanner error={error} />
      </Card>
    </div>
  )
}

// CurrentFile is the file the import has in hand. A large one is copied a chunk
// at a time, so how much of it has moved is worth showing.
function CurrentFile({ status }: { status: ImportStatusResponse }) {
  const size = status.fileSize ?? 0
  const copied = status.fileBytes ?? 0
  const share = size > 0 ? Math.min(100, Math.round((copied / size) * 100)) : 0

  return (
    <div className="stack">
      <div className="mono">{status.path}</div>
      {size > 0 && (
        <>
          <div className="bar">
            <div className="bar-fill" style={{ width: `${share}%` }} />
          </div>
          <div className="muted">
            {formatBytes(copied)} of {formatBytes(size)} ({share}%)
          </div>
        </>
      )}
    </div>
  )
}

// ImportStatusView is everything an import has to say: where it has got to, what
// it came to, and what it could not bring over.
function ImportStatusView({ status }: { status: ImportStatusResponse | null }) {
  if (!status || status.state === 'idle') return null

  const label: Record<string, string> = {
    running: 'Running',
    done: 'Finished',
    failed: 'Failed',
    cancelled: 'Cancelled',
  }

  return (
    <div className="stack">
      <div
        className={status.state === 'failed' ? 'banner banner-error' : 'banner banner-success'}
      >
        {label[status.state] || status.state}
        {status.source ? ` — from ${status.source}` : ''}
      </div>
      {status.path && <CurrentFile status={status} />}
      <table className="table table-kv">
        <tbody>
          <tr>
            <th>Pinned</th>
            <td>{status.pinned}</td>
          </tr>
          <tr>
            <th>Copied</th>
            <td>
              {status.copied} ({formatBytes(status.bytes)})
            </td>
          </tr>
          <tr>
            <th>Folders</th>
            <td>{status.directories}</td>
          </tr>
          <tr>
            <th>Skipped</th>
            <td>{status.skipped}</td>
          </tr>
          <tr>
            <th>Failed</th>
            <td>{status.failed}</td>
          </tr>
          {status.waits > 0 && (
            <tr>
              <th>Waited for space</th>
              <td>
                {status.waits} {status.waits === 1 ? 'time' : 'times'}
              </td>
            </tr>
          )}
        </tbody>
      </table>
      {(status.failures?.length ?? 0) > 0 && (
        <div className="stack">
          <div className="muted">Could not be imported:</div>
          {(status.failures || []).map((failure) => (
            <div key={failure} className="mono">
              {failure}
            </div>
          ))}
        </div>
      )}
      <ErrorBanner error={status.error || null} />
    </div>
  )
}
