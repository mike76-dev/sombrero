import { useState } from 'react'
import { Account, Share } from '../api/types'
import {
  addAccount,
  clearAccountPolicies,
  getAccountShares,
  listAccounts,
  removeAccount,
  removeAccounts,
} from '../api/endpoints'
import { Card, ErrorBanner, Field, SuccessBanner, useApiAction } from '../components/common'
import { WorkgroupSelect } from '../components/selects'

function AccountRow({
  account,
  workgroup,
  onChanged,
}: {
  account: Account
  workgroup: string
  onChanged: () => void
}) {
  const { run, busy, error, message } = useApiAction()
  const [shares, setShares] = useState<Share[] | null>(null)

  return (
    <>
      <tr>
        <td>{account.username}</td>
        <td className="row">
          <button
            className="btn btn-small"
            disabled={busy}
            onClick={() =>
              run(async () => {
                setShares((await getAccountShares(account.username, workgroup)) || [])
              })
            }
          >
            Shares
          </button>
          <button
            className="btn btn-small"
            disabled={busy}
            onClick={() => {
              if (!window.confirm(`Clear all access policies of ${account.username}?`)) return
              run(() => clearAccountPolicies(account.username, workgroup), 'Policies cleared.')
            }}
          >
            Clear policies
          </button>
          <button
            className="btn btn-small btn-danger"
            disabled={busy}
            onClick={() => {
              if (!window.confirm(`Delete account ${account.username}?`)) return
              run(async () => {
                await removeAccount(account.username, workgroup)
                onChanged()
              })
            }}
          >
            Delete
          </button>
        </td>
      </tr>
      {(shares || error || message) && (
        <tr>
          <td colSpan={2}>
            {shares &&
              (shares.length === 0 ? (
                <span className="muted">No shares accessible by this account.</span>
              ) : (
                <span>
                  Accessible shares:{' '}
                  {shares.map((s) => (
                    <span key={s.name} className="flag flag-on">
                      {s.name}
                    </span>
                  ))}
                </span>
              ))}
            <ErrorBanner error={error} />
            <SuccessBanner message={message} />
          </td>
        </tr>
      )}
    </>
  )
}

export function AccountsPage() {
  const [workgroup, setWorkgroup] = useState('')
  const [accounts, setAccounts] = useState<Account[] | null>(null)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [guest, setGuest] = useState(false)
  const list = useApiAction()
  const add = useApiAction()

  const refresh = () =>
    list.run(async () => {
      setAccounts((await listAccounts(workgroup.trim())) || [])
    })

  return (
    <div className="page">
      <Card title="Workgroup">
        <div className="row row-form">
          <Field label="Workgroup">
            <WorkgroupSelect value={workgroup} onChange={setWorkgroup} />
          </Field>
          <button className="btn btn-primary" disabled={list.busy || !workgroup} onClick={refresh}>
            List accounts
          </button>
        </div>
        <ErrorBanner error={list.error} />
      </Card>

      {accounts !== null && (
        <>
          <Card title="Add account">
            <div className="row row-form">
              <Field label="Username">
                <input
                  type="text"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  autoComplete="off"
                />
              </Field>
              <Field label="Password">
                <input
                  type="password"
                  value={guest ? '' : password}
                  disabled={guest}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="new-password"
                />
              </Field>
              <button
                className="btn btn-primary"
                disabled={add.busy || !username.trim() || (!password && !guest)}
                onClick={() =>
                  add.run(async () => {
                    await addAccount(username.trim(), guest ? '' : password, workgroup.trim())
                    setUsername('')
                    setPassword('')
                    setGuest(false)
                    await refresh()
                  }, 'Account added.')
                }
              >
                Add
              </button>
            </div>
            <label className="checkbox checkbox-spaced">
              <input
                type="checkbox"
                checked={guest}
                onChange={(e) => setGuest(e.target.checked)}
              />
              guest account
            </label>
            <p className="muted">
              A guest account reaches only the shares that offer guest access, and there it holds
              whatever the policies of this workgroup grant it. Asking for one explicitly is what
              tells an account meant to be open from one whose password was left out by mistake.
            </p>
            <ErrorBanner error={add.error} />
            <SuccessBanner message={add.message} />
          </Card>

          <Card
            title={
              <span className="row row-spread">
                <span>Accounts ({accounts.length})</span>
                <button
                  className="btn btn-small btn-danger"
                  disabled={list.busy || accounts.length === 0}
                  onClick={() => {
                    if (!window.confirm('Delete ALL accounts of this workgroup?')) return
                    list.run(async () => {
                      await removeAccounts(workgroup.trim())
                      await refresh()
                    })
                  }}
                >
                  Delete all
                </button>
              </span>
            }
          >
            {accounts.length === 0 ? (
              <p className="muted">No accounts in this workgroup.</p>
            ) : (
              <table className="table">
                <thead>
                  <tr>
                    <th>Username</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {accounts.map((acc) => (
                    <AccountRow
                      key={acc.username}
                      account={acc}
                      workgroup={workgroup.trim()}
                      onChanged={refresh}
                    />
                  ))}
                </tbody>
              </table>
            )}
          </Card>
        </>
      )}
    </div>
  )
}
