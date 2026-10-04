import { useState } from 'react'
import { backupStatus, restoreCatalog } from '../api/endpoints'
import { RestoreResponse } from '../api/types'
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

export function BackupPage() {
  const { data: status, error, busy, reload } = useApiData(() => backupStatus())
  const restore = useApiAction()
  const [file, setFile] = useState<File | null>(null)
  const [force, setForce] = useState(false)
  const [restored, setRestored] = useState<RestoreResponse | null>(null)

  const local = status?.local
  const catalogs = local?.catalogs ?? []

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
        <ErrorBanner error={error} />
        {status && !status.enabled && (
          <p className="muted">
            Backups are off. Turn them on with <code>backup.enabled: true</code> in{' '}
            <code>sombrero.yml</code>, and give them a <code>path</code> on this machine; the
            server then also uploads leftover data after {status.bufferAge}, so that the catalogs
            have something to point at.
          </p>
        )}
        {status && status.enabled && (
          <table className="table table-kv">
            <tbody>
              <tr>
                <th>Written to</th>
                <td>
                  {local ? (
                    <>
                      <span className="mono">{local.path}</span> every{' '}
                      {formatInterval(local.interval)}, keeping {local.keep}
                    </>
                  ) : (
                    'nowhere on this machine'
                  )}
                </td>
              </tr>
              <tr>
                <th>Written into the shares</th>
                <td>
                  {status.network
                    ? `every ${formatInterval(status.network.interval)}, keeping ${status.network.keep}`
                    : 'never'}
                </td>
              </tr>
              <tr>
                <th>Leftover data is uploaded after</th>
                <td>{status.bufferAge}</td>
              </tr>
              {local?.lastRun && (
                <tr>
                  <th>Last written</th>
                  <td>{new Date(local.lastRun).toLocaleString()}</td>
                </tr>
              )}
            </tbody>
          </table>
        )}
        {local?.error && <ErrorBanner error={local.error} />}
        {catalogs.length > 0 && (
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
              {catalogs.map((c) => (
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
        )}
        {local && catalogs.length === 0 && !local.error && (
          <p className="muted">Nothing has been written yet.</p>
        )}
      </Card>

      <Card title="Restore from a catalog">
        <p className="muted">
          Pick a catalog file and the server recreates what it describes: the share, the
          workgroup with its accounts and their rights, the connection, and every folder and
          file. The files point at the data on the network, so nothing is downloaded. A
          connection the server already has is left alone unless you ask for the catalog to be
          applied over it; then whatever is missing is added and the rest is left as it is.
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
        {restored && (
          <div className="banner banner-success">
            Restored <strong>{restored.share}</strong> for workgroup{' '}
            <span className="mono">{restored.workgroup}</span>: {restored.accounts} account
            {restored.accounts === 1 ? '' : 's'}, {restored.policies} polic
            {restored.policies === 1 ? 'y' : 'ies'}, {restored.directories} folder
            {restored.directories === 1 ? '' : 's'} and {restored.files} file
            {restored.files === 1 ? '' : 's'}
            {restored.alreadyThere > 0 ? `, ${restored.alreadyThere} already there` : ''}
            {restored.incomplete > 0
              ? `. ${restored.incomplete} file${restored.incomplete === 1 ? ' is' : 's are'} incomplete: the catalog was written before all of the data had reached the network.`
              : '.'}
          </div>
        )}
        <ErrorBanner error={restore.error} />
      </Card>
    </div>
  )
}
