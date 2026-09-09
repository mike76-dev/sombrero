import { PublicDir } from '../api/types'
import { Field } from './common'

// The folders a workgroup shares among its own members. The Workgroups page and
// the setup wizard both set them, and both do it through this.

// clean drops the folders with no path and trims the rest, which is what the
// server is given rather than whatever the rows happen to hold.
export function cleanPublicDirs(dirs: PublicDir[]): PublicDir[] {
  return dirs.map((d) => ({ ...d, path: d.path.trim() })).filter((d) => d.path)
}

// samePublicDirs reports whether two lists say the same thing, for the callers
// that only save what has changed.
export function samePublicDirs(a: PublicDir[], b: PublicDir[]): boolean {
  return (
    a.length === b.length &&
    a.every(
      (d, i) =>
        d.path === b[i].path &&
        !!d.readOnly === !!b[i].readOnly &&
        !!d.caseSensitive === !!b[i].caseSensitive,
    )
  )
}

function PublicDirRow({
  dir,
  onChange,
  onRemove,
  disabled,
}: {
  dir: PublicDir
  onChange: (dir: PublicDir) => void
  onRemove: () => void
  disabled?: boolean
}) {
  return (
    <div className="row">
      <div className="field">
        <input
          type="text"
          value={dir.path}
          disabled={disabled}
          onChange={(e) => onChange({ ...dir, path: e.target.value })}
          placeholder="e.g. Public"
        />
      </div>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={!!dir.readOnly}
          disabled={disabled}
          onChange={(e) => onChange({ ...dir, readOnly: e.target.checked })}
        />
        Read-only
      </label>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={!!dir.caseSensitive}
          disabled={disabled}
          onChange={(e) => onChange({ ...dir, caseSensitive: e.target.checked })}
        />
        Case-sensitive
      </label>
      <button className="btn btn-small" onClick={onRemove} disabled={disabled}>
        Remove
      </button>
    </div>
  )
}

export function PublicDirsEditor({
  dirs,
  onChange,
  disabled,
}: {
  dirs: PublicDir[]
  onChange: (dirs: PublicDir[]) => void
  disabled?: boolean
}) {
  return (
    <>
      <Field label="Public folders">
        {dirs.length === 0 && <p className="muted">No public folders.</p>}
        {dirs.map((dir, i) => (
          <PublicDirRow
            key={i}
            dir={dir}
            disabled={disabled}
            onChange={(next) => onChange(dirs.map((d, j) => (i === j ? next : d)))}
            onRemove={() => onChange(dirs.filter((_, j) => i !== j))}
          />
        ))}
      </Field>
      <div className="row">
        <button
          className="btn btn-small"
          disabled={disabled}
          onClick={() => onChange([...dirs, { path: '' }])}
        >
          Add folder
        </button>
      </div>
      <p className="muted">
        Files in a public folder are visible to every member of the workgroup. In a read-only
        folder, only the account that uploaded a file may overwrite or delete it.
      </p>
    </>
  )
}
