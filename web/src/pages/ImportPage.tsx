import { useCallback, useEffect, useRef, useState } from 'react'
import {
  cancelImport,
  importStatus,
  listAccounts,
  listConnections,
  listImports,
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
  return status?.state === 'running' || status?.state === 'counting'
}

export function ImportPage() {
  const { run, busy, error } = useApiAction()
  const { data: shares } = useApiData(() => listShares())
  const { data: connections } = useApiData(() => listConnections())
  const [workgroup, setWorkgroup] = useState('')
  const [share, setShare] = useState('')
  const [source, setSource] = useState<'renterd' | 'indexd'>('renterd')
  const [address, setAddress] = useState('')
  const [password, setPassword] = useState('')
  const [bucket, setBucket] = useState('default')
  const [appKey, setAppKey] = useState('')
  const [keyFrom, setKeyFrom] = useState('')
  const [prefix, setPrefix] = useState('')
  const [copy, setCopy] = useState(false)
  const [username, setUsername] = useState('')
  const [status, setStatus] = useState<ImportStatusResponse | null>(null)
  const [pollError, setPollError] = useState<string | null>(null)
  const [probe, setProbe] = useState<ImportProbeResponse | null>(null)
  const [sorted, setSorted] = useState<ImportSortResponse | null>(null)
  const [sorting, setSorting] = useState(false)
  const [sortError, setSortError] = useState<string | null>(null)

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

  // The app key of an indexd source is pasted, or borrowed from a connection this
  // server holds, named by its workgroup and share.
  const connectionOf = (id: string) =>
    (connections || []).find((c) => `${c.workgroup}/${c.share}` === id)
  const borrowed = connectionOf(keyFrom)
  const ready = Boolean(
    workgroup.trim() &&
      share.trim() &&
      backend === 'indexd' &&
      username.trim() &&
      address.trim() &&
      (source === 'renterd' || appKey.trim() || borrowed),
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
      if (running(res)) {
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

  // A page that was left and come back to, or reloaded, knows neither the
  // workgroup nor the share, so it asks the server what it has in hand and
  // takes that up: an import nobody can see is one nobody can call off either.
  useEffect(() => {
    if (workgroup || share) return

    let cancelled = false
    listImports()
      .then((imports) => {
        if (cancelled || !imports?.length) return

        // Naming the pair is enough: the poll that follows it answers with the
        // status of its own accord.
        const adopt = imports.find((i) => running(i.status)) ?? imports[0]
        setWorkgroup(adopt.workgroup)
        setShare(adopt.share)
      })
      .catch(() => {
        // Nothing is lost by not finding an import to take up.
      })

    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only on arrival
  }, [])

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
      : {
          appKey: borrowed ? undefined : appKey.trim(),
          keyFrom: borrowed ? { workgroup: borrowed.workgroup, share: borrowed.share } : undefined,
          prefix: prefix.trim() || undefined,
        }),
  })

  // sortOut reads the slabs whose file names are gone, a round at a time,
  // adding up what the rounds found: each slab is downloaded whole, so one
  // call of its own would take as long as all of them. A sort that was broken
  // off is taken up where it stopped rather than from the start.
  const sortOut = async () => {
    setSorting(true)
    setSortError(null)
    const resuming = sorted?.more && sorted.last
    const total: ImportSortResponse = resuming
      ? { ...sorted }
      : { objects: 0, recovered: 0, skipped: 0, unread: 0, bytes: 0, leftover: 0 }
    try {
      let after: string | undefined = resuming ? sorted.last : undefined
      for (;;) {
        const round = await sortLostAndFound(workgroup.trim(), share.trim(), {
          username: username.trim(),
          prefix: prefix.trim() || undefined,
          after,
        })
        total.objects += round.objects
        total.recovered += round.recovered
        total.skipped += round.skipped
        total.unread += round.unread
        total.failure = total.failure || round.failure
        total.bytes += round.bytes
        total.leftover += round.leftover
        total.last = round.last
        setSorted({ ...total, more: round.more })
        if (!round.more || !round.last) break
        after = round.last
      }
      setSorted({ ...total, more: false })
    } catch (e) {
      setSortError(e instanceof Error ? e.message : String(e))
    } finally {
      setSorting(false)
    }
  }

  // follow takes over from a call that started or called off an import: what it
  // answered is the first status, and the poll carries it from there.
  const follow = (res: ImportStatusResponse) => {
    setStatus(res)
    clearTimeout(timer.current)
    if (running(res)) {
      timer.current = setTimeout(() => poll(workgroup.trim(), share.trim()), pollInterval)
    }
  }

  return (
    <div className="page">
      <Card title="Import from another server">
        <p className="muted">
          Copy the files of a <code>renterd</code> server, or of another <code>indexd</code>{' '}
          account, into this share. If the indexer can pin the data where it already is, nothing
          is transferred and this account simply starts paying for it. Otherwise the files are
          downloaded from the source and uploaded again, so you pay for two copies until you
          delete the original. All <code>renterd</code> data has to be copied this way.
        </p>
        <div className="group-label">Import into</div>
        <div className="grid">
          <Field label="Workgroup">
            <WorkgroupSelect value={workgroup} onChange={setWorkgroup} />
          </Field>
          <Field label="Share">
            <ShareSelect value={share} onChange={setShare} />
          </Field>
          <Field label="Files will belong to">
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

        <div className="group-label">Import from</div>
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
              {(connections?.length ?? 0) > 0 && (
                <Field label="Use the app key of">
                  <select
                    value={keyFrom}
                    disabled={running(status)}
                    onChange={(e) => {
                      setKeyFrom(e.target.value)
                      const c = connectionOf(e.target.value)
                      if (c) setAddress(c.server)
                    }}
                  >
                    <option value="">the key pasted below</option>
                    {(connections || []).map((c) => (
                      <option key={`${c.workgroup}/${c.share}`} value={`${c.workgroup}/${c.share}`}>
                        {c.name || c.workgroup} on {c.share} ({c.server})
                      </option>
                    ))}
                  </select>
                </Field>
              )}
              <Field label="App key (hex)">
                <input
                  type="text"
                  value={borrowed ? '' : appKey}
                  onChange={(e) => setAppKey(e.target.value)}
                  disabled={Boolean(borrowed) || running(status)}
                  placeholder={borrowed ? 'taken from the connection' : ''}
                  autoComplete="off"
                />
              </Field>
              <Field label="Import into folder">
                <input
                  type="text"
                  value={prefix}
                  onChange={(e) => setPrefix(e.target.value)}
                  placeholder="/lost+found if left empty"
                  autoComplete="off"
                />
              </Field>
            </>
          )}
        </div>

        {source === 'indexd' && (
          <p className="muted">
            An <code>indexd</code> account stores slabs, not file names. The names are kept in
            the database of the server that uploaded the data, so unless those slabs were
            written by a sombrero that labelled them, the import creates one file per slab in
            the folder above and you have to sort them out afterwards.
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
            title="See what the source holds without importing anything"
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
            Only an <code>indexd</code> share can import data. A <code>renterd</code> share keeps
            its own file list.
          </p>
        )}

        <ImportStatusView status={status} />
        <ErrorBanner error={pollError} />
        <ErrorBanner error={error} />
      </Card>

      <Card title="Recover files from unlabelled slabs">
        <p className="muted">
          Slabs imported without file names land in <span className="mono">/lost+found</span>,
          and each one holds whatever files happened to be packed into it. Sorting reads through
          them and picks out the files it can recognise from their contents: pictures (JPEG,
          PNG, GIF, HEIC, WebP), video and audio (MP4, MOV, M4A, AVI, WAV), PDFs and ZIP
          archives, which covers Office documents too. Each one it finds becomes a file in{' '}
          <span className="mono">/lost+found/recovered</span>. The contents will be correct, but
          the original names are gone for good, so the files are named after where they were
          found.
        </p>
        <p className="muted">
          This costs nothing in storage: a recovered file points at data the share already has.
          What is found is cut out of the slab it was found in, so what remains in{' '}
          <span className="mono">/lost+found</span> is exactly the data nothing could recognise,
          and a slab that was all files disappears from it. Sorting does download each slab to
          read it, so it takes a while on a large share.
        </p>
        <p className="muted">
          What remains is mostly pieces of files bigger than a slab — long videos, archives,
          disk images — spread over many slabs with nothing left to say which belong together.
          They cannot be put back together from here. A general recovery tool such as PhotoRec,
          run over a downloaded remainder, knows a few hundred more formats but has the same
          limit; untrunc can make the opening piece of a video playable given a healthy video
          from the same camera. Deleting a recovered file deletes those bytes for good, and once
          nothing more is wanted from the remainders, deleting them unpins the slabs so they
          stop costing anything.
        </p>
        <div className="row">
          <button
            className="btn btn-primary"
            disabled={busy || sorting || !workgroup.trim() || !share.trim() || !username.trim()}
            onClick={() => run(sortOut)}
          >
            {sorting ? 'Sorting…' : sorted?.more ? 'Continue sorting' : 'Sort /lost+found'}
          </button>
        </div>
        <SortResult sorted={sorted} sorting={sorting} />
        <ErrorBanner error={sortError} />
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
        Read {sorted.objects} slab
        {sorted.objects === 1 ? '' : 's'} and recovered {sorted.recovered} file
        {sorted.recovered === 1 ? '' : 's'} ({formatBytes(sorted.bytes)}).
      </div>
      <table className="table table-kv">
        <tbody>
          <tr>
            <th>Unrecognised data</th>
            <td>{formatBytes(sorted.leftover)}</td>
          </tr>
          {sorted.skipped > 0 && (
            <tr>
              <th>Skipped</th>
              <td>
                {sorted.skipped} file{sorted.skipped === 1 ? '' : 's'}
              </td>
            </tr>
          )}
          {sorted.unread > 0 && (
            <tr>
              <th>Could not be read</th>
              <td>
                {sorted.unread} slab{sorted.unread === 1 ? '' : 's'}
              </td>
            </tr>
          )}
          {sorted.failure && (
            <tr>
              <th>First failure</th>
              <td className="mono">{sorted.failure}</td>
            </tr>
          )}
          {sorting && sorted.last && (
            <tr>
              <th>Currently at</th>
              <td className="mono">{sorted.last}</td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  )
}

// SourceProbe is what the check found: how much is in the source, and whether
// its files will arrive with their names.
function SourceProbe({ probe }: { probe: ImportProbeResponse | null }) {
  if (!probe) return null

  if (probe.source !== 'indexd') {
    return (
      <div className="banner banner-success">
        The server answered. <code>renterd</code> knows its own file names, so the files keep
        them.
      </div>
    )
  }

  const slabs = probe.objects ?? 0
  if (slabs === 0) {
    return <div className="banner banner-success">There is nothing in that account.</div>
  }

  return (
    <div className={probe.warning ? 'banner banner-error' : 'banner banner-success'}>
      That account holds {probe.more ? 'at least ' : ''}
      {slabs} slab{slabs === 1 ? '' : 's'}.{' '}
      {probe.warning ||
        `All ${probe.looked ?? 0} that were checked are labelled with their file names, so` +
          ' the files keep them.'}
    </div>
  )
}

// Overall is how far through the import is: how much of what the source turned
// out to hold is behind it. Nothing is shown while the counting is still going,
// since there is nothing yet to measure against.
function Overall({ status }: { status: ImportStatusResponse }) {
  const total = status.total ?? 0
  if (total === 0) return null

  const share = Math.min(100, Math.round((status.done / total) * 100))

  return (
    <div className="stack">
      <div className="bar">
        <div className="bar-fill" style={{ width: `${share}%` }} />
      </div>
      <div className="muted">
        {status.done} of {total} {countedAs(total, status.slabs ?? 0)} ({share}%)
      </div>
    </div>
  )
}

// countedAs names what an import counts: the files its source names, or the
// nameless slabs it holds, which come over as lost+found entries.
function countedAs(total: number, slabs: number): string {
  const plural = total === 1 ? '' : 's'
  if (slabs === 0) return `file${plural}`
  if (slabs === total) return `slab${plural}`
  return 'files and slabs'
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
    counting: 'Counting what there is to import',
    running: 'Importing',
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
      <Overall status={status} />
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
          {status.refused > 0 && (
            <tr>
              <th>Copied, not pinned</th>
              <td>{status.refused}</td>
            </tr>
          )}
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
      {status.refusal && (
        <div className="muted">
          The indexer could not pin this data, so it was copied instead. It replied:{' '}
          <span className="mono">{status.refusal}</span>
        </div>
      )}
      {(status.failures?.length ?? 0) > 0 && (
        <div className="stack">
          <div className="muted">These files could not be imported:</div>
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
