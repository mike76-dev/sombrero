import { useState } from 'react'
import { Workgroup } from '../api/types'
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

function WorkgroupCard({ wg, onChanged }: { wg: Workgroup; onChanged: () => void }) {
  const { run, busy, error, message } = useApiAction()
  const [publicDirs, setPublicDirs] = useState((wg.publicDirs || []).join('\n'))
  const [caseSensitive, setCaseSensitive] = useState(!!wg.caseSensitive)

  const save = () =>
    run(async () => {
      await updateWorkgroup(
        wg.uuid,
        publicDirs
          .split('\n')
          .map((d) => d.trim())
          .filter(Boolean),
        caseSensitive,
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
        <Field label="Public folders (one per line)">
          <textarea
            rows={3}
            value={publicDirs}
            onChange={(e) => setPublicDirs(e.target.value)}
            placeholder="e.g. share1/public"
          />
        </Field>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={caseSensitive}
            onChange={(e) => setCaseSensitive(e.target.checked)}
          />
          Case-sensitive paths
        </label>
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
