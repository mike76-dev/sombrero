import { FormEvent, ReactNode, useEffect, useRef, useState } from 'react'

// Modal is a card over the page. Escape and a click beside it close it; a
// modal over a modal takes the Escape, so the one below listens only while
// it is the topmost.
export function Modal({ title, onClose, wide, children }: { title: string; onClose: () => void; wide?: boolean; children: ReactNode }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.defaultPrevented) {
        e.preventDefault()
        onClose()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div className={wide ? 'modal modal-wide card' : 'modal card'} role="dialog" aria-modal aria-label={title} onMouseDown={(e) => e.stopPropagation()}>
        <h2 className="card-title">{title}</h2>
        {children}
      </div>
    </div>
  )
}

// validName says what is wrong with a name for a file or a folder, if anything.
export function validName(name: string): string | null {
  const n = name.trim()
  if (n === '') return 'A name is required.'
  if (n === '.' || n === '..') return 'This name is reserved.'
  if (/[/\\]/.test(n)) return 'A name cannot contain a slash.'
  return null
}

// NameDialog asks for a name, for a new folder or a renamed item.
export function NameDialog({
  title,
  label,
  initial = '',
  action,
  busy,
  error,
  onSubmit,
  onClose,
}: {
  title: string
  label: string
  initial?: string
  action: string
  busy: boolean
  error: string | null
  onSubmit: (name: string) => void
  onClose: () => void
}) {
  const [name, setName] = useState(initial)
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => {
    const el = input.current
    if (!el) return
    el.focus()
    // A renamed file keeps its extension selected out of the way.
    const dot = initial.lastIndexOf('.')
    el.setSelectionRange(0, dot > 0 ? dot : initial.length)
  }, [initial])

  const problem = validName(name)
  const submit = (e: FormEvent) => {
    e.preventDefault()
    if (!problem && !busy) onSubmit(name.trim())
  }
  return (
    <Modal title={title} onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <label className="field">
          <span className="field-label">{label}</span>
          <input ref={input} type="text" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" />
          {name !== '' && problem && <span className="field-error field-hint">{problem}</span>}
        </label>
        {error && <div className="banner banner-error">{error}</div>}
        <div className="row modal-actions">
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !!problem || name.trim() === initial}>
            {action}
          </button>
        </div>
      </form>
    </Modal>
  )
}

// PathDialog asks for the folder to move an item to.
export function PathDialog({
  title,
  name,
  initial,
  busy,
  error,
  onSubmit,
  onClose,
}: {
  title: string
  name: string
  initial: string
  busy: boolean
  error: string | null
  onSubmit: (folder: string) => void
  onClose: () => void
}) {
  const [folder, setFolder] = useState(initial)
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => input.current?.focus(), [])

  const target = '/' + folder.split(/[/\\]/).filter((s) => s !== '').join('/')
  const submit = (e: FormEvent) => {
    e.preventDefault()
    if (!busy && target !== initial) onSubmit(target)
  }
  return (
    <Modal title={title} onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <p className="muted">
          Move <strong>{name}</strong> to the folder below. The folder must exist.
        </p>
        <label className="field">
          <span className="field-label">Destination folder</span>
          <input ref={input} type="text" value={folder} onChange={(e) => setFolder(e.target.value)} autoComplete="off" placeholder="/" />
        </label>
        {error && <div className="banner banner-error">{error}</div>}
        <div className="row modal-actions">
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || target === initial}>
            Move
          </button>
        </div>
      </form>
    </Modal>
  )
}

// ConfirmDialog asks before something that cannot be undone, or that is
// marked as not dangerous and merely worth a second look.
export function ConfirmDialog({
  title,
  action,
  danger = true,
  busy,
  error,
  children,
  onConfirm,
  onClose,
}: {
  title: string
  action: string
  danger?: boolean
  busy: boolean
  error: string | null
  children: ReactNode
  onConfirm: () => void
  onClose: () => void
}) {
  return (
    <Modal title={title} onClose={onClose}>
      <div className="stack">
        {children}
        {error && <div className="banner banner-error">{error}</div>}
        <div className="row modal-actions">
          <button type="button" className="btn" onClick={onClose} disabled={busy} autoFocus>
            Cancel
          </button>
          <button type="button" className={danger ? 'btn btn-danger' : 'btn btn-primary'} onClick={onConfirm} disabled={busy}>
            {action}
          </button>
        </div>
      </div>
    </Modal>
  )
}
