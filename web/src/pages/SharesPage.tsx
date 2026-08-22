import { Fragment, useEffect, useState } from 'react'
import {
  AccessRights,
  Account,
  FragmentationResponse,
  OrphansResponse,
  Share,
} from '../api/types'
import {
  checkFragmentation,
  defragment,
  getAccountById,
  getPolicy,
  getShareAccounts,
  listAccounts,
  listShares,
  registerShare,
  removePolicy,
  removeShare,
  scanOrphans,
  setPolicy,
  unpinOrphans,
} from '../api/endpoints'
import {
  Card,
  ErrorBanner,
  Field,
  Flag,
  SuccessBanner,
  formatBytes,
  useApiAction,
  useApiData,
} from '../components/common'
import { WorkgroupSelect } from '../components/selects'

const emptyShare: Share = {
  name: '',
  type: 'indexd',
  serverName: '',
  password: '',
  bucket: '',
  remark: '',
}

function RegisterShareCard({ onRegistered }: { onRegistered: () => void }) {
  const { run, busy, error, message } = useApiAction()
  const [share, setShare] = useState<Share>({ ...emptyShare })
  const set = (patch: Partial<Share>) => setShare((s) => ({ ...s, ...patch }))

  return (
    <Card title="Register share">
      <div className="grid">
        <Field label="Name">
          <input type="text" value={share.name} onChange={(e) => set({ name: e.target.value })} />
        </Field>
        <Field label="Type">
          <select value={share.type} onChange={(e) => set({ type: e.target.value })}>
            <option value="indexd">indexd</option>
            <option value="renterd">renterd</option>
          </select>
        </Field>
        <Field label={share.type === 'indexd' ? 'Indexer address' : 'renterd address'}>
          <input
            type="text"
            value={share.serverName}
            onChange={(e) => set({ serverName: e.target.value })}
            placeholder={share.type === 'indexd' ? 'https://indexer.example.com' : 'http://127.0.0.1:9980'}
          />
        </Field>
        {share.type === 'renterd' && (
          <>
            <Field label="API password">
              <input
                type="password"
                value={share.password}
                onChange={(e) => set({ password: e.target.value })}
                autoComplete="new-password"
              />
            </Field>
            <Field label="Bucket">
              <input
                type="text"
                value={share.bucket}
                onChange={(e) => set({ bucket: e.target.value })}
                placeholder="default"
              />
            </Field>
          </>
        )}
        {share.type === 'indexd' && (
          <>
            <Field label="Data shards">
              <input
                type="number"
                min={1}
                max={255}
                value={share.dataShards ?? ''}
                onChange={(e) => set({ dataShards: e.target.value ? Number(e.target.value) : undefined })}
              />
            </Field>
            <Field label="Parity shards">
              <input
                type="number"
                min={1}
                max={255}
                value={share.parityShards ?? ''}
                onChange={(e) => set({ parityShards: e.target.value ? Number(e.target.value) : undefined })}
              />
            </Field>
          </>
        )}
        <Field label="Remark">
          <input type="text" value={share.remark} onChange={(e) => set({ remark: e.target.value })} />
        </Field>
      </div>
      <div className="row">
        <button
          className="btn btn-primary"
          disabled={busy || !share.name.trim() || !share.serverName.trim()}
          onClick={() =>
            run(async () => {
              await registerShare({ ...share, name: share.name.trim() })
              setShare({ ...emptyShare })
              onRegistered()
            }, 'Share registered.')
          }
        >
          Register
        </button>
      </div>
      <ErrorBanner error={error} />
      <SuccessBanner message={message} />
    </Card>
  )
}

interface NamedRights extends AccessRights {
  username?: string
  workgroup?: string
}

// How many of the found slabs are listed before the rest is summarized. The
// listing is there to show what the button would drop, not to be read in full.
const maxListedSlabs = 20

function formatAge(seconds: number): string {
  if (seconds % 3600 === 0) {
    const hours = seconds / 3600
    return hours === 1 ? 'an hour' : `${hours} hours`
  }
  if (seconds % 60 === 0) return `${seconds / 60} minutes`
  return `${seconds} seconds`
}

// OrphanedSlabsCard scans the share's storage backend for slabs that it pins
// while no file references them, and offers to unpin them. indexd shares only:
// renterd keeps its objects in its own database.
function OrphanedSlabsCard({ share }: { share: Share }) {
  const { run, busy, error, message, setMessage } = useApiAction()
  const [scan, setScan] = useState<OrphansResponse | null>(null)

  const failures = scan?.errors ? Object.entries(scan.errors) : []

  return (
    <div className="stack">
      <p className="muted">
        Slabs that this share has pinned and pays for, while no file references them — what an
        upload interrupted at the wrong moment can leave behind. Slabs pinned within the last
        hour are never reported: they may belong to an upload still in flight.
      </p>
      <div className="row">
        <button
          className="btn"
          disabled={busy}
          onClick={() =>
            run(async () => {
              setScan(await scanOrphans(share.name))
            })
          }
        >
          {scan ? 'Rescan' : 'Scan for orphaned slabs'}
        </button>
        {!!scan?.count && (
          <button
            className="btn btn-danger"
            disabled={busy}
            onClick={() => {
              if (
                !window.confirm(
                  `Unpin ${scan.count} orphaned slab(s) of ${share.name}, freeing ${formatBytes(
                    scan.size,
                  )}? This cannot be undone.`,
                )
              )
                return
              run(async () => {
                const res = await unpinOrphans(share.name)
                // Show what is left rather than what was found before.
                setScan(await scanOrphans(share.name))
                setMessage(
                  `Unpinned ${res.unpinned} slab(s), freeing ${formatBytes(res.freed)}.` +
                    (res.failed
                      ? ` ${res.failed} could not be dropped and stay staged for the background retry.`
                      : ''),
                )
              })
            }}
          >
            Unpin all ({scan.count})
          </button>
        )}
      </div>
      <ErrorBanner error={error} />
      <SuccessBanner message={message} />
      {scan &&
        (scan.count === 0 ? (
          <p className="muted">
            No orphaned slabs found
            {scan.minAge ? ` among the slabs pinned more than ${formatAge(scan.minAge)} ago` : ''}.
          </p>
        ) : (
          <>
            <div className="banner banner-error">
              Found <strong>{scan.count}</strong> orphaned slab{scan.count === 1 ? '' : 's'},{' '}
              {formatBytes(scan.size)} in total.
            </div>
            <table className="table">
              <thead>
                <tr>
                  <th>Slab</th>
                  <th>Size</th>
                  <th>Pinned</th>
                  <th>Workgroup</th>
                </tr>
              </thead>
              <tbody>
                {scan.slabs.slice(0, maxListedSlabs).map((slab) => (
                  <tr key={`${slab.workgroup}/${slab.key}`}>
                    <td className="mono">{slab.key.slice(0, 16)}…</td>
                    <td>{formatBytes(slab.size)}</td>
                    <td>{new Date(slab.pinnedAt).toLocaleString()}</td>
                    <td className="mono muted">{slab.workgroup}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {scan.slabs.length > maxListedSlabs && (
              <p className="muted">…and {scan.slabs.length - maxListedSlabs} more.</p>
            )}
          </>
        ))}
      {failures.map(([workgroup, reason]) => (
        <div className="banner banner-error" key={workgroup}>
          Workgroup <span className="mono">{workgroup}</span> could not be reached, so its slabs
          are not counted here: {reason}
        </div>
      ))}
    </div>
  )
}

// formatPercent renders a fraction as a percentage. A slab that still holds
// something never rounds up to a full 100%, which would read as an orphan.
function formatPercent(fraction: number): string {
  const pct = fraction * 100
  if (pct >= 100) return '100%'
  return `${Math.min(Math.round(pct), 99)}%`
}

// FragmentationCard reports the dead space that editing and deleting files
// leaves behind in the slabs they were packed into, and offers to repack them.
// indexd shares only: renterd packs its objects itself.
function FragmentationCard({ share }: { share: Share }) {
  const { run, busy, error, message, setMessage } = useApiAction()
  const [check, setCheck] = useState<FragmentationResponse | null>(null)

  const failures = check?.errors ? Object.entries(check.errors) : []

  return (
    <div className="stack">
      <p className="muted">
        Editing or deleting a file leaves a hole in the slab it was packed into, and the share
        keeps paying for the whole slab. Space a slab was never filled with does not count — only
        what was taken back out of it. Repacking moves what is left in those slabs back into the
        upload queue, to be packed into fewer slabs than they take up now.
      </p>
      <div className="row">
        <button
          className="btn"
          disabled={busy}
          onClick={() =>
            run(async () => {
              setCheck(await checkFragmentation(share.name))
            })
          }
        >
          {check ? 'Recheck' : 'Check fragmentation'}
        </button>
        {!!check?.fragmented && (
          <button
            className="btn"
            disabled={busy}
            onClick={() => {
              if (
                !window.confirm(
                  `Repack the fragmented slabs of ${share.name}? What is left in them is ` +
                    `downloaded and uploaded again, which is paid for like any other upload, ` +
                    `and the slabs they came out of are unpinned once nothing reads from them.`,
                )
              )
                return
              run(async () => {
                const res = await defragment(share.name)
                // Show what is left rather than what was found before.
                setCheck(await checkFragmentation(share.name))
                setMessage(
                  res.slabs
                    ? `Moved ${formatBytes(res.moved)} out of ${res.slabs} slab` +
                        `${res.slabs === 1 ? '' : 's'} to be packed again, freeing ` +
                        `${formatBytes(res.reclaimed)}. The new slab goes up in the background.`
                    : 'Nothing was repacked: what is left in these slabs would fill as many ' +
                        'slabs again, and a slab short of full is paid for like a full one. ' +
                        'Try again once more slabs are fragmented, or once there is data ' +
                        'waiting to be uploaded that fits in the dead space.',
                )
              })
            }}
          >
            Repack
          </button>
        )}
      </div>
      <ErrorBanner error={error} />
      <SuccessBanner message={message} />
      {check &&
        (check.total === 0 ? (
          <p className="muted">This share has no uploaded slabs yet.</p>
        ) : (
          <>
            {check.fragmented === 0 ? (
              <p className="muted">
                No slab has {formatPercent(check.threshold)} or more dead space, with{' '}
                {formatBytes(check.wasted)} wasted across {check.total} slab
                {check.total === 1 ? '' : 's'}.
              </p>
            ) : (
              <div className="banner banner-error">
                <strong>{check.fragmented}</strong> of {check.total} slab
                {check.total === 1 ? '' : 's'} {check.fragmented === 1 ? 'has' : 'have'}{' '}
                {formatPercent(check.threshold)} or more dead space, which is{' '}
                {formatBytes(check.fragmentedWasted)} of the total {formatBytes(check.wasted)}{' '}
                wasted.
              </div>
            )}
            {check.fragmented > 0 && (
              <>
                <table className="table">
                  <thead>
                    <tr>
                      <th>Slab</th>
                      <th>Dead space</th>
                      <th>Wasted</th>
                      <th>In use</th>
                      <th>Pieces</th>
                      <th>Workgroup</th>
                    </tr>
                  </thead>
                  <tbody>
                    {check.slabs.slice(0, maxListedSlabs).map((slab) => (
                      <tr key={`${slab.workgroup}/${slab.key}`}>
                        <td className="mono">{slab.key.slice(0, 16)}…</td>
                        <td>{formatPercent(slab.fragmentation)}</td>
                        <td>{formatBytes(slab.wasted)}</td>
                        <td>
                          {formatBytes(slab.used)} of {formatBytes(slab.filled)} written
                        </td>
                        <td>{slab.pieces}</td>
                        <td className="mono muted">{slab.workgroup}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {check.slabs.length > maxListedSlabs && (
                  <p className="muted">…and {check.slabs.length - maxListedSlabs} more.</p>
                )}
              </>
            )}
          </>
        ))}
      {failures.map(([workgroup, reason]) => (
        <div className="banner banner-error" key={workgroup}>
          Workgroup <span className="mono">{workgroup}</span> could not be reached, so its slabs
          are not counted here: {reason}
        </div>
      ))}
    </div>
  )
}

function ShareDetails({ share, onChanged }: { share: Share; onChanged: () => void }) {
  const { run, busy, error } = useApiAction()
  const [accounts, setAccounts] = useState<NamedRights[] | null>(null)

  const loadAccounts = () =>
    run(async () => {
      const ars = (await getShareAccounts(share.name)) || []
      const named = await Promise.all(
        ars.map(async (ar): Promise<NamedRights> => {
          try {
            const acc = await getAccountById(ar.AccountID)
            return { ...ar, username: acc.username, workgroup: acc.workgroup }
          } catch {
            return { ...ar }
          }
        }),
      )
      setAccounts(named)
    })

  return (
    <div className="stack">
      <table className="table table-kv">
        <tbody>
          <tr>
            <th>Server</th>
            <td className="mono">{share.serverName}</td>
          </tr>
          {share.bucket && (
            <tr>
              <th>Bucket</th>
              <td>{share.bucket}</td>
            </tr>
          )}
          {!!share.dataShards && (
            <tr>
              <th>Redundancy</th>
              <td>
                {share.dataShards} data / {share.parityShards} parity shards
              </td>
            </tr>
          )}
          {share.remark && (
            <tr>
              <th>Remark</th>
              <td>{share.remark}</td>
            </tr>
          )}
          {share.createdAt && (
            <tr>
              <th>Created</th>
              <td>{new Date(share.createdAt).toLocaleString()}</td>
            </tr>
          )}
        </tbody>
      </table>
      <div className="row">
        <button className="btn" disabled={busy} onClick={loadAccounts}>
          {accounts ? 'Reload accounts' : 'Show accounts with access'}
        </button>
        <button
          className="btn btn-danger"
          disabled={busy}
          onClick={() => {
            if (!window.confirm(`Unregister share ${share.name}?`)) return
            run(async () => {
              await removeShare(share.name)
              onChanged()
            })
          }}
        >
          Unregister share
        </button>
      </div>
      <ErrorBanner error={error} />
      {share.type === 'indexd' && <OrphanedSlabsCard share={share} />}
      {share.type === 'indexd' && <FragmentationCard share={share} />}
      {accounts &&
        (accounts.length === 0 ? (
          <p className="muted">No accounts have access to this share.</p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Account</th>
                <th>Workgroup</th>
                <th>Access</th>
              </tr>
            </thead>
            <tbody>
              {accounts.map((ar) => (
                <tr key={ar.AccountID}>
                  <td>{ar.username || `#${ar.AccountID}`}</td>
                  <td className="mono muted">{ar.workgroup || '—'}</td>
                  <td>
                    <Flag on={ar.ReadAccess} label="read" />
                    <Flag on={ar.WriteAccess} label="write" />
                    <Flag on={ar.DeleteAccess} label="delete" />
                    <Flag on={ar.ExecuteAccess} label="execute" />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ))}
    </div>
  )
}

function SharesListCard({
  shares,
  loaded,
  busy,
  error,
  onChanged,
}: {
  shares: Share[]
  loaded: boolean
  busy: boolean
  error: string | null
  onChanged: () => void
}) {
  const [expanded, setExpanded] = useState<string | null>(null)

  return (
    <Card
      title={
        <span className="row row-spread">
          <span>Registered shares{loaded ? ` (${shares.length})` : ''}</span>
          <button className="btn btn-small" onClick={onChanged} disabled={busy}>
            Reload
          </button>
        </span>
      }
    >
      <ErrorBanner error={error} />
      {loaded && shares.length === 0 && <p className="muted">No shares registered yet.</p>}
      {shares.length > 0 && (
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>Server</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {shares.map((s) => (
              <Fragment key={s.name}>
                <tr>
                  <td>
                    <strong>{s.name}</strong>
                  </td>
                  <td>{s.type}</td>
                  <td className="mono muted">{s.serverName}</td>
                  <td>
                    <button
                      className="btn btn-small"
                      onClick={() => setExpanded(expanded === s.name ? null : s.name)}
                    >
                      {expanded === s.name ? 'Hide' : 'Details'}
                    </button>
                  </td>
                </tr>
                {expanded === s.name && (
                  <tr>
                    <td colSpan={4}>
                      <ShareDetails share={s} onChanged={onChanged} />
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </Card>
  )
}

const noRights = { read: false, write: false, delete: false, execute: false }

function PolicyCard({ shares }: { shares: Share[] }) {
  const { run, busy, error, message } = useApiAction()
  const accountsAction = useApiAction()
  const policyAction = useApiAction()
  const [share, setShare] = useState('')
  const [workgroup, setWorkgroup] = useState('')
  const [accounts, setAccounts] = useState<Account[]>([])
  const [username, setUsername] = useState('')
  const [rights, setRights] = useState(noRights)
  const ready = share && username && workgroup
  const loadAccounts = accountsAction.run
  const loadPolicy = policyAction.run

  useEffect(() => {
    setUsername('')
    setAccounts([])
    if (!workgroup) return
    loadAccounts(async () => {
      setAccounts((await listAccounts(workgroup)) || [])
    })
  }, [workgroup, loadAccounts])

  // Populate the checkboxes as soon as a full share/workgroup/account
  // selection is made.
  useEffect(() => {
    let cancelled = false
    setRights(noRights)
    if (!share || !username || !workgroup) return
    loadPolicy(async () => {
      const ar = await getPolicy(share, username, workgroup)
      if (cancelled) return
      setRights({
        read: ar.ReadAccess,
        write: ar.WriteAccess,
        delete: ar.DeleteAccess,
        execute: ar.ExecuteAccess,
      })
    })
    return () => {
      cancelled = true
    }
  }, [share, username, workgroup, loadPolicy])

  const toggle = (key: keyof typeof rights) => (
    <label className="checkbox" key={key}>
      <input
        type="checkbox"
        checked={rights[key]}
        disabled={!ready || policyAction.busy}
        onChange={(e) => setRights((r) => ({ ...r, [key]: e.target.checked }))}
      />
      {key}
    </label>
  )

  return (
    <Card title="Access policy">
      <div className="grid">
        <Field label="Share">
          <select value={share} onChange={(e) => setShare(e.target.value)}>
            <option value="">
              {shares.length === 0 ? '— no shares —' : '— select share —'}
            </option>
            {shares.map((s) => (
              <option key={s.name} value={s.name}>
                {s.name} ({s.type})
              </option>
            ))}
          </select>
        </Field>
        <Field label="Workgroup">
          <WorkgroupSelect value={workgroup} onChange={setWorkgroup} />
        </Field>
        <Field label="Account">
          <select
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            disabled={!workgroup || accountsAction.busy}
          >
            <option value="">
              {!workgroup
                ? '— select a workgroup first —'
                : accountsAction.busy
                  ? 'loading…'
                  : accounts.length === 0
                    ? '— no accounts in this workgroup —'
                    : '— select account —'}
            </option>
            {accounts.map((acc) => (
              <option key={acc.username} value={acc.username}>
                {acc.username}
              </option>
            ))}
          </select>
        </Field>
      </div>
      <ErrorBanner error={accountsAction.error} />
      <div className="row">{(['read', 'write', 'delete', 'execute'] as const).map(toggle)}</div>
      <div className="row">
        <button
          className="btn btn-primary"
          disabled={busy || policyAction.busy || !ready}
          onClick={() => run(() => setPolicy(share, username, workgroup, rights), 'Policy saved.')}
        >
          Save
        </button>
        <button
          className="btn btn-danger"
          disabled={busy || policyAction.busy || !ready}
          onClick={() =>
            run(async () => {
              await removePolicy(share, username, workgroup)
              setRights(noRights)
            }, 'Policy removed.')
          }
        >
          Remove
        </button>
      </div>
      <ErrorBanner error={policyAction.error} />
      <ErrorBanner error={error} />
      <SuccessBanner message={message} />
    </Card>
  )
}

export function SharesPage() {
  const { data, error, busy, reload } = useApiData(() => listShares())
  const shares = data || []

  return (
    <div className="page">
      <RegisterShareCard onRegistered={reload} />
      <SharesListCard
        shares={shares}
        loaded={data !== undefined}
        busy={busy}
        error={error}
        onChanged={reload}
      />
      <PolicyCard shares={shares} />
    </div>
  )
}
