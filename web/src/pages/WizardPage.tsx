import { ReactNode, useEffect, useState } from 'react'
import { PublicDir, Share } from '../api/types'
import {
  addAccount,
  connect,
  createWorkgroup,
  getPolicy,
  listAccounts,
  registerShare,
  requestConnection,
  setPolicy,
  updateWorkgroup,
} from '../api/endpoints'
import { Card, ErrorBanner, Field, SuccessBanner, useApiAction } from '../components/common'
import { ConnectStatusView, ShareKeyField, useConnectAttempt } from '../components/connect'
import { PublicDirsEditor, cleanPublicDirs, samePublicDirs } from '../components/publicdirs'
import { ServerAddressField } from '../components/serveraddress'
import { ShareSelect, WorkgroupSelect } from '../components/selects'

// The wizard walks through the setup the README describes: a workgroup, an
// account in it, a share, the connection between the two, and the policy that
// lets the account onto the share. Every step does what its own page does, in
// the order that gets a client onto a share; the pages stay where they are for
// everything else.

const STEPS = [
  { key: 'workgroup', label: 'Workgroup' },
  { key: 'account', label: 'Account' },
  { key: 'share', label: 'Share' },
  { key: 'connect', label: 'Connection' },
  { key: 'policy', label: 'Access' },
] as const

// What the wizard has made so far. Each step fills in its part and the ones
// after it work from that.
interface Setup {
  workgroup: string
  workgroupLabel: string
  publicDirs: string[]
  username: string

  // accountSkipped tells an empty username apart from one that is not filled in
  // yet: the setup that leaves the accounts for later also leaves out the policy.
  accountSkipped: boolean
  share: string
  shareType: string
}

const emptySetup: Setup = {
  workgroup: '',
  workgroupLabel: '',
  publicDirs: [],
  username: '',
  accountSkipped: false,
  share: '',
  shareType: '',
}

function StepBar({ at, steps }: { at: number; steps: readonly { key: string; label: string }[] }) {
  return (
    <ol className="steps steps-row">
      {steps.map((step, i) => (
        <li
          key={step.key}
          className={`step step-${i < at ? 'done' : i === at ? 'active' : 'pending'}`}
        >
          <span className="step-mark" aria-hidden="true">
            {i < at ? '✓' : ''}
          </span>
          <span className="step-label">{step.label}</span>
        </li>
      ))}
    </ol>
  )
}

// StepCard is the frame every step shares: what the step is for, the form, and
// the way on to the next one.
function StepCard({
  title,
  intro,
  children,
  onBack,
  onNext,
  nextLabel = 'Continue',
  nextDisabled,
  onSkip,
  skipLabel = 'Skip',
  skipDisabled,
}: {
  title: string
  intro: ReactNode
  children: ReactNode
  onBack?: () => void
  onNext: () => void
  nextLabel?: string
  nextDisabled: boolean

  // onSkip is for a step that can be left out altogether, which is not the same
  // as continuing: it says the setup wants nothing of what the step makes.
  onSkip?: () => void
  skipLabel?: string
  skipDisabled?: boolean
}) {
  return (
    <Card title={title}>
      <p className="muted">{intro}</p>
      {children}
      <div className="row">
        {onBack && (
          <button className="btn" onClick={onBack}>
            Back
          </button>
        )}
        <button className="btn btn-primary" disabled={nextDisabled} onClick={onNext}>
          {nextLabel}
        </button>
        {onSkip && (
          <button className="btn" disabled={skipDisabled} onClick={onSkip}>
            {skipLabel}
          </button>
        )}
      </div>
    </Card>
  )
}

// Choice is the pick every step but the last offers: make a new one, use
// something that is there already, or leave the step out where it may be left out.
function Choice<T extends string>({
  value,
  onChange,
  options,
}: {
  value: T
  onChange: (value: T) => void
  options: readonly (readonly [T, string])[]
}) {
  return (
    <div className="row">
      {options.map(([key, label]) => (
        <label className="checkbox" key={key}>
          <input type="radio" checked={value === key} onChange={() => onChange(key)} />
          {label}
        </label>
      ))}
    </div>
  )
}

function WorkgroupStep({
  setup,
  onDone,
}: {
  setup: Setup
  onDone: (patch: Partial<Setup>) => void
}) {
  const { run, busy, error } = useApiAction()
  const [mode, setMode] = useState<'new' | 'existing'>('new')
  const [name, setName] = useState('')
  const [existing, setExisting] = useState(setup.workgroup)
  const [existingName, setExistingName] = useState(setup.workgroupLabel)

  // The public folders on show, and the ones the workgroup already has, so that
  // picking an existing workgroup does not rewrite folders nobody touched.
  const [dirs, setDirs] = useState<PublicDir[]>([])
  const [savedDirs, setSavedDirs] = useState<PublicDir[]>([])

  // An unnamed workgroup is a workgroup like any other, so there is nothing to
  // fill in before a new one can be made.
  const ready = mode === 'new' || Boolean(existing)

  return (
    <StepCard
      title="1. Create a workgroup"
      intro={
        <>
          A workgroup holds the accounts, and it is the workgroup — not the account — that
          connects to a share and has a storage quota there. A name is optional and only worth
          giving on a private server, where no other workgroup will ever claim the same one.
          Public folders are shared among the members of this workgroup, and can be left for
          later.
        </>
      }
      onNext={() =>
        run(async () => {
          const wanted = cleanPublicDirs(dirs)
          const paths = wanted.map((d) => d.path)
          if (mode === 'existing') {
            if (!samePublicDirs(wanted, savedDirs)) await updateWorkgroup(existing, wanted)
            onDone({
              workgroup: existing,
              workgroupLabel: existingName || existing,
              publicDirs: paths,
            })
            return
          }

          const res = await createWorkgroup(name.trim() || undefined)
          if (wanted.length > 0) await updateWorkgroup(res.uuid, wanted)
          onDone({
            workgroup: res.uuid,
            workgroupLabel: res.name || res.uuid,
            publicDirs: paths,
          })
        })
      }
      nextLabel={mode === 'new' ? 'Create and continue' : 'Continue'}
      nextDisabled={busy || !ready}
    >
      <Choice
        value={mode}
        onChange={setMode}
        options={[
          ['new', 'Create a new workgroup'],
          ['existing', 'Use an existing one'],
        ]}
      />
      <div className="grid">
        {mode === 'new' ? (
          <Field label="Name (optional)">
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. home"
              autoComplete="off"
            />
          </Field>
        ) : (
          <Field label="Workgroup">
            <WorkgroupSelect
              value={existing}
              onChange={setExisting}
              onSelect={(wg) => {
                setExistingName(wg?.name || '')
                setDirs(wg?.publicDirs || [])
                setSavedDirs(wg?.publicDirs || [])
              }}
            />
          </Field>
        )}
      </div>
      <PublicDirsEditor dirs={dirs} onChange={setDirs} disabled={busy} />
      <ErrorBanner error={error} />
    </StepCard>
  )
}

function AccountStep({
  setup,
  onBack,
  onDone,
}: {
  setup: Setup
  onBack: () => void
  onDone: (patch: Partial<Setup>) => void
}) {
  const { run, busy, error } = useApiAction()
  const list = useApiAction()
  const [accounts, setAccounts] = useState<string[]>([])
  const [mode, setMode] = useState<'new' | 'existing'>('new')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [guest, setGuest] = useState(false)
  const [existing, setExisting] = useState('')

  useEffect(() => {
    list.run(async () => {
      const accs = (await listAccounts(setup.workgroup)) || []
      setAccounts(accs.map((a) => a.username))
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps -- the workgroup is fixed by now
  }, [setup.workgroup])

  const ready =
    mode === 'new' ? Boolean(username.trim() && (password || guest)) : Boolean(existing)

  return (
    <StepCard
      title="2. Add an account"
      intro={
        <>
          An account is what a client logs in as. This one belongs to workgroup{' '}
          <span className="mono">{setup.workgroupLabel}</span>, and the last step decides what it
          may do on the share. A guest account has no password and can only reach shares that
          allow guests. Accounts can wait, too: the workgroup is what connects to a share, so{' '}
          <em>Skip</em> takes you straight to the connection. You can add accounts later on the
          Accounts page and grant them access on the Shares page.
        </>
      }
      onBack={onBack}
      onNext={() =>
        run(async () => {
          if (mode === 'existing') {
            onDone({ username: existing, accountSkipped: false })
            return
          }
          await addAccount(username.trim(), guest ? '' : password, setup.workgroup)
          onDone({ username: username.trim(), accountSkipped: false })
        })
      }
      nextLabel={mode === 'new' ? 'Add and continue' : 'Continue'}
      nextDisabled={busy || !ready}
      onSkip={() => onDone({ username: '', accountSkipped: true })}
      skipLabel="Skip"
      skipDisabled={busy}
    >
      <Choice
        value={mode}
        onChange={setMode}
        options={[
          ['new', 'Add a new account'],
          ['existing', 'Use an account of this workgroup'],
        ]}
      />
      {mode === 'new' ? (
        <>
          <div className="grid">
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
          </div>
          <label className="checkbox">
            <input type="checkbox" checked={guest} onChange={(e) => setGuest(e.target.checked)} />
            guest account
          </label>
        </>
      ) : (
        <div className="grid">
          <Field label="Account">
            <select value={existing} onChange={(e) => setExisting(e.target.value)}>
              <option value="">
                {list.busy
                  ? 'loading…'
                  : accounts.length === 0
                    ? '— no accounts in this workgroup —'
                    : '— select account —'}
              </option>
              {accounts.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
          </Field>
        </div>
      )}
      <ErrorBanner error={list.error} />
      <ErrorBanner error={error} />
    </StepCard>
  )
}

const emptyShare: Share = {
  name: '',
  type: 'indexd',
  serverName: '',
  password: '',
  bucket: '',
  remark: '',
}

function ShareStep({ onBack, onDone }: { onBack: () => void; onDone: (patch: Partial<Setup>) => void }) {
  const { run, busy, error } = useApiAction()
  const [mode, setMode] = useState<'new' | 'existing'>('new')
  const [share, setShare] = useState<Share>({ ...emptyShare })
  const [existing, setExisting] = useState('')
  const [existingType, setExistingType] = useState('')
  const set = (patch: Partial<Share>) => setShare((s) => ({ ...s, ...patch }))

  const ready =
    mode === 'new' ? Boolean(share.name.trim() && share.serverName.trim()) : Boolean(existing)

  return (
    <StepCard
      title="3. Register a share"
      intro={
        <>
          A share is the storage the workgroup will reach: an <code>indexd</code> indexer, which
          spreads the data over hosts with the redundancy set here, or a <code>renterd</code>{' '}
          node, which keeps it in a bucket of its own.
        </>
      }
      onBack={onBack}
      onNext={() =>
        run(async () => {
          if (mode === 'existing') {
            onDone({ share: existing, shareType: existingType })
            return
          }
          const name = share.name.trim()
          await registerShare({ ...share, name })
          onDone({ share: name, shareType: share.type })
        })
      }
      nextLabel={mode === 'new' ? 'Register and continue' : 'Continue'}
      nextDisabled={busy || !ready}
    >
      <Choice
        value={mode}
        onChange={setMode}
        options={[
          ['new', 'Register a new share'],
          ['existing', 'Use a registered share'],
        ]}
      />
      {mode === 'new' ? (
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
          <ServerAddressField
            value={share.serverName}
            onChange={(serverName) => set({ serverName })}
            backend={share.type}
            disabled={busy}
          />
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
                  placeholder="default if left empty"
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
                  onChange={(e) =>
                    set({ dataShards: e.target.value ? Number(e.target.value) : undefined })
                  }
                />
              </Field>
              <Field label="Parity shards">
                <input
                  type="number"
                  min={1}
                  max={255}
                  value={share.parityShards ?? ''}
                  onChange={(e) =>
                    set({ parityShards: e.target.value ? Number(e.target.value) : undefined })
                  }
                />
              </Field>
            </>
          )}
          <Field label="Remark">
            <input
              type="text"
              value={share.remark}
              onChange={(e) => set({ remark: e.target.value })}
            />
          </Field>
        </div>
      ) : (
        <div className="grid">
          <Field label="Share">
            <ShareSelect
              value={existing}
              onChange={setExisting}
              onSelect={(s) => setExistingType(s?.type || '')}
            />
          </Field>
        </div>
      )}
      <ErrorBanner error={error} />
    </StepCard>
  )
}

function ConnectStep({
  setup,
  onBack,
  onDone,
}: {
  setup: Setup
  onBack: () => void
  onDone: () => void
}) {
  const { run, busy, error } = useApiAction()
  const [appKey, setAppKey] = useState('')
  const attempt = useConnectAttempt(setup.workgroup, setup.share)
  const [keyFrom, setKeyFrom] = useState('')
  const indexd = setup.shareType === 'indexd'
  const reconnecting = Boolean(appKey.trim())
  const reusable = indexd && attempt.reusable

  // Another workgroup's key is offered where this one has none of its own, and the
  // choice is dropped where the workgroup it was for is no longer among them.
  const holders = indexd && !reusable && !reconnecting ? attempt.keyFrom : []
  const sharing = holders.some((holder) => holder.workgroup === keyFrom) ? keyFrom : ''

  return (
    <StepCard
      title="4. Connect the workgroup to the share"
      intro={
        attempt.connected ? (
          <>
            This workgroup is already connected to the share, and the rest of the setup uses
            that connection. To undo it, use the Connections page.
          </>
        ) : indexd ? (
          <>
            An <code>indexd</code> share needs an app key. The first time, press{' '}
            <em>Request approval</em>,
            open the link and approve the registration with the indexer. The rest happens by
            itself, and the new key is shown once, below — keep it. If the workgroup was
            connected before, paste that key instead. If it is already connected to another
            share on this indexer, it reuses the key from there, and it can also borrow another
            workgroup's key, which means sharing one account and quota at the indexer.
          </>
        ) : (
          <>
            A <code>renterd</code> share connects straight away. It takes a moment: the client
            warms up a connection to every host before it reports itself ready.
          </>
        )
      }
      onBack={onBack}
      onNext={onDone}
      nextLabel={attempt.connected ? 'Continue' : 'Skip for now'}
      nextDisabled={attempt.running}
    >
      {indexd && !attempt.connected && (
        <div className="grid">
          <Field label="App key (only to reconnect)">
            <input
              type="text"
              value={appKey}
              onChange={(e) => setAppKey(e.target.value)}
              placeholder="hex, from an earlier connection"
              autoComplete="off"
            />
          </Field>
          <ShareKeyField holders={holders} value={sharing} onChange={setKeyFrom} />
        </div>
      )}
      {!attempt.connected && (
        <div className="row">
          {indexd && (
            <button
              className={reusable && !reconnecting ? 'btn' : 'btn btn-primary'}
              disabled={busy || attempt.running || reconnecting}
              onClick={() =>
                run(async () => {
                  attempt.begin(await requestConnection(setup.workgroup, setup.share), true)
                })
              }
            >
              Request approval
            </button>
          )}
          <button
            className={indexd && !reusable && !sharing ? 'btn' : 'btn btn-primary'}
            disabled={
              busy || attempt.running || (indexd && !reconnecting && !reusable && !sharing)
            }
            onClick={() =>
              run(async () => {
                attempt.begin(
                  await connect(setup.workgroup, setup.share, {
                    appKey: appKey.trim() || undefined,
                    fromWorkgroup: sharing || undefined,
                  }),
                  false,
                )
              })
            }
          >
            {!indexd
              ? 'Connect'
              : reconnecting
                ? 'Reconnect with the app key'
                : reusable
                  ? 'Connect with the key of this indexer'
                  : sharing
                    ? "Connect with that workgroup's key"
                    : 'Reconnect with the app key'}
          </button>
        </div>
      )}
      <ConnectStatusView attempt={attempt} />
      {!attempt.connected && !attempt.running && (
        <p className="muted">
          Access rights hang off this connection, so the next step needs it to exist. Connect
          here, or do it later on the Connections page and grant access on the Shares page.
        </p>
      )}
      <ErrorBanner error={error} />
    </StepCard>
  )
}

const noRights = { read: false, write: false, delete: false, execute: false }

function PolicyStep({
  setup,
  onBack,
  onDone,
}: {
  setup: Setup
  onBack: () => void
  onDone: () => void
}) {
  const { run, busy, error } = useApiAction()
  const load = useApiAction()
  const [rights, setRights] = useState({ ...noRights, read: true, execute: true })

  // A policy is kept with the workgroup's connection to the share, so a setup that
  // skipped the connection finishes without one rather than failing on it.
  const attempt = useConnectAttempt(setup.workgroup, setup.share)
  const connected = attempt.connected

  // The access this account already has on this share is what to show: an account
  // set up before keeps what it was granted then, rather than being offered the
  // defaults again.
  useEffect(() => {
    let cancelled = false
    load.run(async () => {
      const ar = await getPolicy(setup.share, setup.username, setup.workgroup)
      if (cancelled) return
      if (ar.ReadAccess || ar.WriteAccess || ar.DeleteAccess || ar.ExecuteAccess) {
        setRights({
          read: ar.ReadAccess,
          write: ar.WriteAccess,
          delete: ar.DeleteAccess,
          execute: ar.ExecuteAccess,
        })
      }
    })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- the setup is fixed by now
  }, [setup.share, setup.username, setup.workgroup])

  return (
    <StepCard
      title="5. Grant access"
      intro={
        <>
          What <span className="mono">{setup.username}</span> may do on{' '}
          <span className="mono">{setup.share}</span>. Read and execute are the minimum for
          opening the share and browsing it. Write and delete let the account change what is
          there. If this account already had access, what it has now is shown below.
        </>
      }
      onBack={onBack}
      onNext={
        connected
          ? () =>
              run(async () => {
                await setPolicy(setup.share, setup.username, setup.workgroup, rights)
                onDone()
              })
          : onDone
      }
      nextLabel={connected ? 'Save and finish' : 'Finish without access rights'}
      nextDisabled={busy}
    >
      <div className="row">
        {(['read', 'write', 'delete', 'execute'] as const).map((key) => (
          <label className="checkbox" key={key}>
            <input
              type="checkbox"
              checked={rights[key]}
              disabled={busy || load.busy || !connected}
              onChange={(e) => setRights((r) => ({ ...r, [key]: e.target.checked }))}
            />
            {key}
          </label>
        ))}
      </div>
      {!connected && (
        <p className="muted">
          <span className="mono">{setup.workgroupLabel}</span> is not connected to{' '}
          <span className="mono">{setup.share}</span> yet, and access rights need that
          connection. Go back a step to make it, or connect later on the Connections page and
          grant access on the Shares page.
        </p>
      )}
      <ErrorBanner error={load.error} />
      <ErrorBanner error={error} />
    </StepCard>
  )
}

function DoneStep({ setup, onRestart }: { setup: Setup; onRestart: () => void }) {
  return (
    <Card title="Set up">
      <SuccessBanner
        message={
          setup.username
            ? `${setup.username} of ${setup.workgroupLabel} may now use ${setup.share}.`
            : `${setup.workgroupLabel} is now connected to ${setup.share}.`
        }
      />
      <table className="table table-kv">
        <tbody>
          <tr>
            <th>Workgroup</th>
            <td className="mono">{setup.workgroupLabel}</td>
          </tr>
          {setup.publicDirs.length > 0 && (
            <tr>
              <th>Public folders</th>
              <td className="mono">{setup.publicDirs.join(', ')}</td>
            </tr>
          )}
          <tr>
            <th>Account</th>
            <td className="mono">{setup.username || '— none yet —'}</td>
          </tr>
          <tr>
            <th>Share</th>
            <td className="mono">
              {setup.share} ({setup.shareType})
            </td>
          </tr>
        </tbody>
      </table>
      <p className="muted">
        A client reaches it as <span className="mono">\\&lt;server&gt;\{setup.share}</span>
        {setup.username ? (
          <>
            , logging in as <span className="mono">{setup.username}</span>
          </>
        ) : (
          ', once an account of this workgroup has been given access to it'
        )}
        . Anything set up here can be changed from the pages on the left. The wizard only covers
        the first of each.
      </p>
      <div className="row">
        <button className="btn" onClick={onRestart}>
          Set up another
        </button>
      </div>
    </Card>
  )
}

export function WizardPage() {
  const [step, setStep] = useState(0)
  const [setup, setSetup] = useState<Setup>({ ...emptySetup })

  const advance = (patch: Partial<Setup>) => {
    setSetup((s) => ({ ...s, ...patch }))
    setStep((i) => i + 1)
  }
  const back = () => setStep((i) => i - 1)

  // A setup without an account has no access to grant, so the last step is the
  // connection, and the bar says so rather than showing a step that never comes.
  const steps = setup.accountSkipped ? STEPS.filter((s) => s.key !== 'policy') : STEPS

  return (
    <div className="page">
      <StepBar at={step} steps={steps} />
      {step === 0 && <WorkgroupStep setup={setup} onDone={advance} />}
      {step === 1 && <AccountStep setup={setup} onBack={back} onDone={advance} />}
      {step === 2 && <ShareStep onBack={back} onDone={advance} />}
      {step === 3 && (
        <ConnectStep
          setup={setup}
          onBack={back}
          onDone={() => setStep(setup.accountSkipped ? 5 : 4)}
        />
      )}
      {step === 4 && <PolicyStep setup={setup} onBack={back} onDone={() => setStep(5)} />}
      {step === 5 && (
        <DoneStep
          setup={setup}
          onRestart={() => {
            setSetup({ ...emptySetup })
            setStep(0)
          }}
        />
      )}
    </div>
  )
}
