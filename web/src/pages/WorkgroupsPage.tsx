import { useState } from 'react'
import { PublicDir, Workgroup } from '../api/types'
import {
  createWorkgroup,
  listWorkgroups,
  removeWorkgroup,
  updateWorkgroup,
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

function PublicDirRow({
  dir,
  onChange,
  onRemove,
}: {
  dir: PublicDir
  onChange: (dir: PublicDir) => void
  onRemove: () => void
}) {
  return (
    <div className="row">
      <div className="field">
        <input
          type="text"
          value={dir.path}
          onChange={(e) => onChange({ ...dir, path: e.target.value })}
          placeholder="e.g. Public"
        />
      </div>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={!!dir.readOnly}
          onChange={(e) => onChange({ ...dir, readOnly: e.target.checked })}
        />
        Read-only
      </label>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={!!dir.caseSensitive}
          onChange={(e) => onChange({ ...dir, caseSensitive: e.target.checked })}
        />
        Case-sensitive
      </label>
      <button className="btn btn-small" onClick={onRemove}>
        Remove
      </button>
    </div>
  )
}

function WorkgroupCard({ wg, onChanged }: { wg: Workgroup; onChanged: () => void }) {
  const { run, busy, error, message } = useApiAction()
  const [publicDirs, setPublicDirs] = useState<PublicDir[]>(wg.publicDirs || [])

  const save = () =>
    run(async () => {
      await updateWorkgroup(
        wg.uuid,
        publicDirs.map((d) => ({ ...d, path: d.path.trim() })).filter((d) => d.path),
      )
      onChanged()
    }, 'Workgroup updated.')

  const remove = () => {
    if (!window.confirm(`Delete workgroup ${wg.name || wg.uuid} from the server?`)) return
    run(async () => {
      await removeWorkgroup(wg.uuid)
      onChanged()
    })
  }

  return (
    <div className="subcard">
      <div className="row row-spread">
        <div>
          <strong>{wg.name || 'unnamed'}</strong>
          <div className="mono muted">
            {wg.uuid} <CopyButton value={wg.uuid} />
          </div>
        </div>
        <button className="btn btn-danger" onClick={remove} disabled={busy}>
          Delete
        </button>
      </div>
      <div className="stack">
        <Field label="Public folders">
          {publicDirs.length === 0 && <p className="muted">No public folders.</p>}
          {publicDirs.map((dir, i) => (
            <PublicDirRow
              key={i}
              dir={dir}
              onChange={(next) =>
                setPublicDirs(publicDirs.map((d, j) => (i === j ? next : d)))
              }
              onRemove={() => setPublicDirs(publicDirs.filter((_, j) => i !== j))}
            />
          ))}
        </Field>
        <div className="row">
          <button
            className="btn btn-small"
            onClick={() => setPublicDirs([...publicDirs, { path: '' }])}
          >
            Add folder
          </button>
        </div>
        <p className="muted">
          Files in a public folder are visible to every member of the workgroup. In a
          read-only folder, only the account that uploaded a file may overwrite or delete
          it.
        </p>
        <div className="row">
          <button className="btn btn-primary" onClick={save} disabled={busy}>
            Save
          </button>
        </div>
      </div>
      <ErrorBanner error={error} />
      <SuccessBanner message={message} />
    </div>
  )
}

export function WorkgroupsPage() {
  const { data, error, busy, reload } = useApiData(() => listWorkgroups())
  const create = useApiAction()
  const [newName, setNewName] = useState('')
  const [created, setCreated] = useState<string | null>(null)
  const workgroups = data || []

  return (
    <div className="page">
      <Card title="Create workgroup">
        <div className="row row-form">
          <Field label="Name (optional)">
            <input
              type="text"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              placeholder="my-workgroup"
            />
          </Field>
          <button
            className="btn btn-primary"
            disabled={create.busy}
            onClick={() =>
              create.run(async () => {
                const wg = await createWorkgroup(newName.trim() || undefined)
                setCreated(wg.uuid)
                setNewName('')
                reload()
              })
            }
          >
            Create
          </button>
        </div>
        {created && (
          <div className="banner banner-success">
            Workgroup created: <span className="mono">{created}</span>{' '}
            <CopyButton value={created} />
          </div>
        )}
        <ErrorBanner error={create.error} />
      </Card>

      <Card
        title={
          <span className="row row-spread">
            <span>Workgroups{data ? ` (${workgroups.length})` : ''}</span>
            <button className="btn btn-small" onClick={reload} disabled={busy}>
              Reload
            </button>
          </span>
        }
      >
        <ErrorBanner error={error} />
        {data && workgroups.length === 0 && (
          <p className="muted">No workgroups registered yet.</p>
        )}
        {workgroups.map((wg) => (
          <WorkgroupCard key={wg.uuid} wg={wg} onChanged={reload} />
        ))}
      </Card>
    </div>
  )
}
