import { useState } from 'react'
import {
  backupStatus,
  listStoredCatalogs,
  recoverFromNetwork,
  restoreCatalog,
  restoreStoredCatalog,
} from '../api/endpoints'
import { RecoverResponse, RestoreResponse, TierStatus } from '../api/types'
import {
  Card,
  ErrorBanner,
  Field,
  formatBytes,
  useApiAction,
  useApiData,
} from '../components/common'
import { ServerAddressField } from '../components/serveraddress'

// formatInterval shortens what Go prints for a duration: "15m0s" is "15m".
function formatInterval(s: string): string {
  return s.replace(/(\d+[hm])0s$/, '$1').replace(/(\d+h)0m$/, '$1')
}

// RestoredBanner says what restoring a catalog came to, in the words of the
// kind of catalog it was.
function RestoredBanner({ restored, from }: { restored: RestoreResponse; from?: string }) {
  const n = (count: number | undefined, one: string, many: string) =>
    `${count ?? 0} ${count === 1 ? one : many}`

  if (restored.kind === 'server') {
    return (
      <div className="banner banner-success">
        The server is restored. Added: {n(restored.shares, 'share', 'shares')},{' '}
        {n(restored.workgroups, 'workgroup', 'workgroups')},{' '}
        {n(restored.accounts, 'account', 'accounts')} and {n(restored.bans, 'ban', 'bans')}.
        Whatever was there already has been left as it was.
      </div>
    )
  }

  const incomplete = restored.incomplete ?? 0

  return (
    <div className="banner banner-success stack">
      <div>
        <strong>{restored.share}</strong> is restored for workgroup{' '}
        <span className="mono">{restored.workgroup}</span>
        {from ? (
          <>
            {' '}
            from <span className="mono">{from}</span>
          </>
        ) : null}
        : {n(restored.accounts, 'account', 'accounts')}, {n(restored.policies, 'policy', 'policies')}
        , {n(restored.directories, 'folder', 'folders')} and {n(restored.files, 'file', 'files')}
        {(restored.alreadyThere ?? 0) > 0 ? `; ${restored.alreadyThere} already there` : ''}.
      </div>
      {incomplete > 0 && (
        <div>
          {incomplete === 1 ? 'One file is' : `${incomplete} files are`} incomplete: the catalog
          was written before all of {incomplete === 1 ? 'its' : 'their'} data had reached the
          network.
        </div>
      )}
      {from && (
        <div>
          Files written after this catalog are not in it. Run an import of the same account into
          the share to pick them up.
        </div>
      )}
    </div>
  )
}

// Tier is one place the catalogs go: where, how often, and the newest catalog
// of every connection.
function Tier({ title, tier }: { title: string; tier: TierStatus }) {
  return (
    <div className="stack">
      <h3>{title}</h3>
      <div className="muted">
        Written to <span className="mono">{tier.path}</span> every{' '}
        {formatInterval(tier.interval)}; the newest {tier.keep} are kept.
        {tier.lastRun ? ` Last run: ${new Date(tier.lastRun).toLocaleString()}.` : ''}
      </div>
      {tier.error && <ErrorBanner error={tier.error} />}
      {(tier.waiting || []).map((w) => (
        <div className="muted" key={`${w.share}/${w.workgroup}`}>
          Waiting for the connection to <strong>{w.share}</strong> to come up. The catalog is
          written within a minute of that.
        </div>
      ))}
      {tier.server && (
        <div className="muted">
          The catalog of the server itself holds {tier.server.shares} share
          {tier.server.shares === 1 ? '' : 's'}, {tier.server.workgroups} workgroup
          {tier.server.workgroups === 1 ? '' : 's'} with {tier.server.accounts} account
          {tier.server.accounts === 1 ? '' : 's'}, and {tier.server.bans} ban
          {tier.server.bans === 1 ? '' : 's'}; {formatBytes(tier.server.size)}, written{' '}
          {new Date(tier.server.writtenAt).toLocaleString()}.
        </div>
      )}
      {tier.catalogs.length > 0 ? (
        <table className="table">
          <thead>
            <tr>
              <th>Share</th>
              <th>Workgroup</th>
              <th>Folders</th>
              <th>Files</th>
              <th>Incomplete</th>
              <th>Size</th>
              <th>Written</th>
            </tr>
          </thead>
          <tbody>
            {tier.catalogs.map((c) => (
              <tr key={`${c.share}/${c.workgroup}`}>
                <td>{c.share}</td>
                <td className="mono muted">{c.workgroup}</td>
                <td>{c.directories}</td>
                <td>{c.files}</td>
                <td>{c.incomplete}</td>
                <td>{formatBytes(c.size)}</td>
                <td>{new Date(c.writtenAt).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        !tier.error &&
        !(tier.waiting || []).length && <p className="muted">No catalog has been written yet.</p>
      )}
    </div>
  )
}

export function BackupPage() {
  const { data: status, error, busy, reload } = useApiData(() => backupStatus())
  const stored = useApiData(() => listStoredCatalogs())
  const restore = useApiAction()
  const [file, setFile] = useState<File | null>(null)
  const [force, setForce] = useState(false)
  const [restored, setRestored] = useState<RestoreResponse | null>(null)

  // reloadAll refreshes both what the tiers report and what the folder holds.
  const reloadAll = () => {
    reload()
    stored.reload()
  }

  const recover = useApiAction()
  const [address, setAddress] = useState('')
  const [appKey, setAppKey] = useState('')
  const [recoverForce, setRecoverForce] = useState(false)
  const [recovered, setRecovered] = useState<RecoverResponse | null>(null)

  return (
    <div className="page">
      <Card
        title={
          <span className="row row-spread">
            <span>Backups</span>
            <button className="btn btn-small" onClick={reload} disabled={busy}>
              Reload
            </button>
          </span>
        }
      >
        <p className="muted">
          A catalog is a small file that describes one share for one workgroup: the accounts
          and what they may do, the folders and files, and which objects on the network hold
          each file's data. The data itself is not copied. If the database is lost, a catalog and
          the app key are enough to put the share back.
        </p>
        <p className="muted">
          Catalogs are written to two places: a folder on this machine, and the share itself,
          where they go into <span className="mono">/.sombrero/catalog</span> as files that only
          the app key can open. The copy in the share is for the worst case, a server that has
          nothing left.
        </p>
        <ErrorBanner error={error} />
        {status && !status.enabled && (
          <p className="muted">
            Backups are off. To turn them on, set <code>backup.enabled: true</code> in{' '}
            <code>sombrero.yml</code> and restart. The server will then also upload leftover data
            after {status.bufferAge} instead of waiting for a full slab, so that the catalogs have
            something to point at.
          </p>
        )}
        {status && status.enabled && (
          <p className="muted">
            Leftover data is uploaded after {status.bufferAge}, so that a catalog can point at
            it.
          </p>
        )}
        {status?.local && <Tier title="On this machine" tier={status.local} />}
        {status?.network && <Tier title="In the shares" tier={status.network} />}
      </Card>

      <Card title="Restore from a catalog">
        <p className="muted">
          If a catalog describes a share, the server registers the share, creates the workgroup
          with its accounts and their rights, connects to the indexer with the app key from the
          catalog, and recreates the folders and files. Nothing is downloaded: the files point at
          data that is already on the network. If the server already has this connection,
          nothing happens unless you tick the box below; then what is missing is added and the
          rest is left alone. The catalog of the server brings back the shares, the workgroups
          with their accounts, and the bans, and never changes anything that is already there.
        </p>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={force}
            onChange={(e) => setForce(e.target.checked)}
            disabled={restore.busy}
          />
          Apply over a connection this server already has
        </label>
        {(stored.data?.length ?? 0) > 0 && (
          <>
            <p className="muted">
              These are the catalogs in the backup folder on this machine. The server reads them
              itself, so they need not be readable by you.
            </p>
            <table className="table">
              <thead>
                <tr>
                  <th>Catalog</th>
                  <th>Written</th>
                  <th>Size</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {(stored.data || []).map((c) => (
                  <tr key={c.path}>
                    <td>
                      {c.kind === 'server' ? (
                        'The server itself'
                      ) : (
                        <>
                          {c.share} <span className="mono muted">{c.workgroup}</span>
                        </>
                      )}
                    </td>
                    <td>{new Date(c.writtenAt).toLocaleString()}</td>
                    <td>{formatBytes(c.size)}</td>
                    <td>
                      <button
                        className="btn btn-small"
                        disabled={restore.busy}
                        onClick={() =>
                          restore.run(async () => {
                            // The list is refreshed whatever came of it: a row
                            // may have been pruned since the list was loaded.
                            try {
                              setRestored(await restoreStoredCatalog(c.path, force))
                            } finally {
                              reloadAll()
                            }
                          })
                        }
                      >
                        Restore
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </>
        )}
        <ErrorBanner error={stored.error} />
        <p className="muted">
          A catalog from somewhere else, a copy you kept on another machine for instance, can be
          picked as a file.
        </p>
        <div className="row row-form">
          <Field label="Catalog file">
            <input
              type="file"
              onChange={(e) => setFile(e.target.files?.[0] ?? null)}
              disabled={restore.busy}
            />
          </Field>
          <button
            className="btn btn-primary"
            disabled={restore.busy || !file}
            onClick={() =>
              restore.run(async () => {
                if (!file) return
                setRestored(await restoreCatalog(file, force))
                reloadAll()
              })
            }
          >
            {restore.busy ? 'Restoring…' : 'Restore'}
          </button>
        </div>
        {restored && <RestoredBanner restored={restored} />}
        <ErrorBanner error={restore.error} />
      </Card>

      <Card title="Recover from the network">
        <p className="muted">
          This is for a server that has lost everything. Enter the indexer and the app key of
          the account. The server looks through the account for the newest catalog the previous
          server put into the share, opens it with the key, and restores the share from it.
          Files written after that catalog are not in it; run an import of the same account into
          the restored share afterwards to pick them up.
        </p>
        <div className="grid">
          <ServerAddressField
            value={address}
            onChange={setAddress}
            backend="indexd"
            disabled={recover.busy}
          />
          <Field label="App key (hex)">
            <input
              type="text"
              value={appKey}
              onChange={(e) => setAppKey(e.target.value)}
              disabled={recover.busy}
              autoComplete="off"
            />
          </Field>
        </div>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={recoverForce}
            onChange={(e) => setRecoverForce(e.target.checked)}
            disabled={recover.busy}
          />
          Apply over a connection this server already has
        </label>
        <div className="row">
          <button
            className="btn btn-primary"
            disabled={recover.busy || !address.trim() || !appKey.trim()}
            onClick={() =>
              recover.run(async () => {
                setRecovered(
                  await recoverFromNetwork({
                    address: address.trim(),
                    appKey: appKey.trim(),
                    force: recoverForce,
                  }),
                )
                reloadAll()
              })
            }
          >
            {recover.busy ? 'Recovering…' : 'Recover'}
          </button>
        </div>
        {(recovered?.catalogs || []).map((c) =>
          c.error ? (
            <ErrorBanner
              key={c.catalog}
              error={`${c.catalog} could not be restored: ${c.error}`}
            />
          ) : (
            <RestoredBanner key={c.catalog} restored={c} from={c.catalog} />
          ),
        )}
        <ErrorBanner error={recover.error} />
      </Card>
    </div>
  )
}
