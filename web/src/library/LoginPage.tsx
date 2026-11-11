import { FormEvent, useState } from 'react'
import { FilesError, Identity, login } from './api'

const workgroupKey = 'sombrero.library.workgroup'

export function LoginPage({ notice, onLogin }: { notice?: string; onLogin: (identity: Identity) => void }) {
  const [workgroup, setWorkgroup] = useState(() => localStorage.getItem(workgroupKey) ?? '')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const identity = await login(workgroup.trim(), username.trim(), password)
      localStorage.setItem(workgroupKey, workgroup.trim())
      onLogin(identity)
    } catch (err) {
      if (err instanceof FilesError && err.status === 401) {
        setError('The login failed. Check the workgroup, the username and the password.')
      } else {
        setError(err instanceof Error ? err.message : String(err))
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="login" onSubmit={submit}>
      <p className="muted">Log in with the account you use for the shares.</p>
      {notice && <div className="banner banner-success">{notice}</div>}
      <label className="field">
        <span className="field-label">Workgroup</span>
        <input type="text" value={workgroup} onChange={(e) => setWorkgroup(e.target.value)} autoComplete="organization" required />
      </label>
      <label className="field">
        <span className="field-label">Username</span>
        <input type="text" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
      </label>
      <label className="field">
        <span className="field-label">Password</span>
        <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" />
        <span className="muted field-hint">A guest account has no password.</span>
      </label>
      {error && <div className="banner banner-error">{error}</div>}
      <button className="btn btn-primary" type="submit" disabled={busy || !workgroup.trim() || !username.trim()}>
        Log in
      </button>
    </form>
  )
}
