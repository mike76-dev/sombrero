import { useState } from 'react'
import { backupStatus, restoreCatalog } from '../api/endpoints'
import { RestoreResponse, TierStatus } from '../api/types'
import {
  Card,
  ErrorBanner,
  Field,
  formatBytes,
  useApiAction,
  useApiData,
} from '../components/common'

// formatInterval shortens what Go prints for a duration: "15m0s" is "15m".
function formatInterval(s: string): string {
  return s.replace(/(\d+[hm])0s$/, '$1').replace(/(\d+h)0m$/, '$1')
}

// Tier is one place the catalogs go: where, how often, and the newest catalog
// of every connection.
function Tier({ title, tier }: { title: string; tier: TierStatus }) {
  return (
    <div className="stack">
      <h3>{title}</h3>
      <div className="muted">
        <span className="mono">{tier.path}</span>, every {formatInterval(tier.interval)}, keeping{' '}
        {tier.keep}
        {tier.lastRun ? `. Last written ${new Date(tier.lastRun).toLocaleString()}.` : '.'}
      </div>
      {tier.error && <ErrorBanner error={tier.error} />}
      {tier.server && (
        <div className="muted">
          The server itself: {tier.server.shares} share{tier.server.shares === 1 ? '' : 's'},{' '}
          {tier.server.workgroups} workgroup{tier.server.workgroups === 1 ? '' : 's'} with{' '}
          {tier.server.accounts} account{tier.server.accounts === 1 ? '' : 's'}, and{' '}
          {tier.server.bans} ban{tier.server.bans === 1 ? '' : 's'} ({formatBytes(tier.server.size)}
          , written {new Date(tier.server.writtenAt).toLocaleString()}).
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
        !tier.error && <p className="muted">Nothing has been written yet.</p>
      )}
    </div>
  )
}

export function BackupPage() {
  const { data: status, error, busy, reload } = useApiData(() => backupStatus())
  const restore = useApiAction()
  const [file, setFile] = useState<File | null>(null)
  const [force, setForce] = useState(false)
  const [restored, setRestored] = useState<RestoreResponse | null>(null)

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
          A catalog is a description of everything the database knows about one share and one
          workgroup: the accounts and their rights, the folders and files, and where on the
          network the data of every file is. The data itself stays where it is. With a catalog
          and the app key, a share can be put back on a server that has lost its database.
        </p>
        <p className="muted">
          Catalogs go to two places: a folder on this machine, and the share itself, as a file
          in <span className="mono">/.sombrero/catalog</span> that only the app key can open. The
          copy in the share is what a server with nothing left can start from.
        </p>
        <ErrorBanner error={error} />
        {status && !status.enabled && (
          <p className="muted">
            Backups are off. Turn them on with <code>backup.enabled: true</code> in{' '}
            <code>sombrero.yml</code>. The server then also uploads leftover data after{' '}
            {status.bufferAge}, so that the catalogs have something to point at.
          </p>
        )}
        {status && status.enabled && (
          <p className="muted">Leftover data is uploaded after {status.bufferAge}.</p>
        )}
        {status?.local && <Tier title="On this machine" tier={status.local} />}
        {status?.network && <Tier title="In the shares" tier={status.network} />}
      </Card>

      <Card title="Restore from a catalog">
        <p className="muted">
          Pick a catalog file and the server recreates what it describes. For a catalog of a
          connection that is the share, the workgroup with its accounts and their rights, the
          connection, and every folder and file; the files point at the data on the network, so
          nothing is downloaded. A connection the server already has is left alone unless you ask
          for the catalog to be applied over it; then whatever is missing is added and the rest
          is left as it is. The catalog of the server brings back the shares, the workgroups with
          their accounts, and the bans, and never touches what is there already.
        </p>
        <div className="grid">
          <Field label="Catalog">
            <input
              type="file"
              onChange={(e) => setFile(e.target.files?.[0] ?? null)}
              disabled={restore.busy}
            />
          </Field>
        </div>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={force}
            onChange={(e) => setForce(e.target.checked)}
            disabled={restore.busy}
          />
          Apply over a connection the server already has
        </label>
        <div className="row">
          <button
            className="btn btn-primary"
            disabled={restore.busy || !file}
            onClick={() =>
              restore.run(async () => {
                if (!file) return
                setRestored(await restoreCatalog(file, force))
                reload()
              })
            }
          >
            {restore.busy ? 'Restoring…' : 'Restore'}
          </button>
        </div>
        {restored && restored.kind === 'server' && (
          <div className="banner banner-success">
            Restored the server: {restored.shares ?? 0} share{restored.shares === 1 ? '' : 's'},{' '}
            {restored.workgroups ?? 0} workgroup{restored.workgroups === 1 ? '' : 's'},{' '}
            {restored.accounts} account{restored.accounts === 1 ? '' : 's'} and {restored.bans ?? 0}{' '}
            ban{restored.bans === 1 ? '' : 's'} were added; whatever was there already was left
            as it was.
          </div>
        )}
        {restored && restored.kind === 'connection' && (
          <div className="banner banner-success">
            Restored <strong>{restored.share}</strong> for workgroup{' '}
            <span className="mono">{restored.workgroup}</span>: {restored.accounts} account
            {restored.accounts === 1 ? '' : 's'}, {restored.policies ?? 0} polic
            {restored.policies === 1 ? 'y' : 'ies'}, {restored.directories ?? 0} folder
            {restored.directories === 1 ? '' : 's'} and {restored.files ?? 0} file
            {restored.files === 1 ? '' : 's'}
            {(restored.alreadyThere ?? 0) > 0 ? `, ${restored.alreadyThere} already there` : ''}
            {(restored.incomplete ?? 0) > 0
              ? `. ${restored.incomplete} file${restored.incomplete === 1 ? ' is' : 's are'} incomplete: the catalog was written before all of the data had reached the network.`
              : '.'}
          </div>
        )}
        <ErrorBanner error={restore.error} />
      </Card>
    </div>
  )
}
