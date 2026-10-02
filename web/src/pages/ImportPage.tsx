import { useCallback, useEffect, useRef, useState } from 'react'
import {
  cancelImport,
  importStatus,
  listAccounts,
  listShares,
  probeImportSource,
  sortLostAndFound,
  startImport,
} from '../api/endpoints'
import {
  ImportProbeResponse,
  ImportRequest,
  ImportSortResponse,
  ImportStatusResponse,
} from '../api/types'
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
  const [probe, setProbe] = useState<ImportProbeResponse | null>(null)
  const [sorted, setSorted] = useState<ImportSortResponse | null>(null)
  const [sorting, setSorting] = useState(false)

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

  // An answer about one source says nothing about another, so it is dropped as
  // soon as the fields that name it change.
  useEffect(() => setProbe(null), [source, address, bucket, appKey])

  // request is what the source is named by, for both starting an import of it
  // and looking at it first.
  const request = (): ImportRequest => ({
    source,
    address: address.trim(),
    username: username.trim(),
    copy: copying,
    ...(source === 'renterd'
      ? { password, bucket: bucket.trim() }
      : { appKey: appKey.trim(), prefix: prefix.trim() || undefined }),
  })

  // sortOut looks inside the objects whose names are gone, a round at a time,
  // adding up what the rounds found: each object is downloaded whole, so one
  // call of its own would take as long as all of them.
  const sortOut = async () => {
    setSorting(true)
    const total: ImportSortResponse = {
      objects: 0,
      recovered: 0,
      skipped: 0,
      bytes: 0,
      leftover: 0,
    }
    try {
      let after: string | undefined
      for (;;) {
        const round = await sortLostAndFound(workgroup.trim(), share.trim(), {
          username: username.trim(),
          prefix: prefix.trim() || undefined,
          after,
        })
        total.objects += round.objects
        total.recovered += round.recovered
        total.skipped += round.skipped
        total.bytes += round.bytes
        total.leftover += round.leftover
        total.last = round.last
        setSorted({ ...total, more: round.more })
        if (!round.more || !round.last) break
        after = round.last
      }
      setSorted({ ...total, more: false })
    } finally {
      setSorting(false)
    }
  }

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
                follow(await startImport(workgroup.trim(), share.trim(), request()))
              })
            }
          >
            Start
          </button>
          <button
            className="btn"
            disabled={busy || !ready || running(status)}
            onClick={() =>
              run(async () => {
                setProbe(await probeImportSource(workgroup.trim(), share.trim(), request()))
              })
            }
            title="Look at the source without taking anything from it"
          >
            Check the source
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

        <SourceProbe probe={probe} />

        {ready && backend !== 'indexd' && (
          <p className="muted">
            Only an indexd share can import data. A renterd share keeps its own file list.
          </p>
        )}

        <ImportStatusView status={status} />
        <ErrorBanner error={pollError} />
        <ErrorBanner error={error} />
      </Card>

      <Card title="Sort out what came over without names">
        <p className="muted">
          Objects imported from an account that could not name its files arrive under{' '}
          <span className="mono">/lost+found</span>, each holding whatever was packed into it.
          Sorting looks inside them for files it can recognize by what they begin and end
          with — PDFs, JPEGs, PNGs, GIFs and ZIPs, including Office documents — and makes each
          one a file of its own under <span className="mono">/lost+found/recovered</span>. The
          contents are right; the names are not the ones they had, since nothing on the network
          remembers those.
        </p>
        <p className="muted">
          Nothing is uploaded and nothing is paid for twice: a recovered file points at the
          bytes that are already there. A file larger than one object is left alone, because
          only part of it is in here.
        </p>
        <div className="row">
          <button
            className="btn btn-primary"
            disabled={busy || sorting || !workgroup.trim() || !share.trim() || !username.trim()}
            onClick={() => run(sortOut)}
          >
            {sorting ? 'Sorting…' : 'Sort out /lost+found'}
          </button>
        </div>
        <SortResult sorted={sorted} sorting={sorting} />
      </Card>
    </div>
  )
}

// SortResult is what the rounds of a sort have added up to so far.
function SortResult({
  sorted,
  sorting,
}: {
  sorted: ImportSortResponse | null
  sorting: boolean
}) {
  if (!sorted) return null

  return (
    <div className="stack">
      <div className="banner banner-success">
        {sorting ? 'Sorting' : 'Sorted'} {sorted.objects} object
        {sorted.objects === 1 ? '' : 's'}: {sorted.recovered} file
        {sorted.recovered === 1 ? '' : 's'} recovered ({formatBytes(sorted.bytes)}).
      </div>
      <table className="table table-kv">
        <tbody>
          <tr>
            <th>Belonged to no file</th>
            <td>{formatBytes(sorted.leftover)}</td>
          </tr>
          {sorted.skipped > 0 && (
            <tr>
              <th>Left alone</th>
              <td>
                {sorted.skipped} file{sorted.skipped === 1 ? '' : 's'}
              </td>
            </tr>
          )}
          {sorting && sorted.last && (
            <tr>
              <th>Up to</th>
              <td className="mono">{sorted.last}</td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

// SourceProbe is what a look at the source found: how much is there, and whether
// an import of it would come over as files or as objects.
function SourceProbe({ probe }: { probe: ImportProbeResponse | null }) {
  if (!probe) return null

  if (probe.source !== 'indexd') {
    return (
      <div className="banner banner-success">
        The source answered. A renterd server knows its file names, so they come over with the
        files.
      </div>
    )
  }

  const objects = probe.objects ?? 0
  if (objects === 0) {
    return <div className="banner banner-success">That account holds nothing to import.</div>
  }

  return (
    <div className={probe.warning ? 'banner banner-error' : 'banner banner-success'}>
      That account holds {objects} object{objects === 1 ? '' : 's'}.{' '}
      {probe.warning ||
        `All ${probe.looked ?? 0} looked at say which files they hold, so they` +
          ' come over under their own names.'}
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
