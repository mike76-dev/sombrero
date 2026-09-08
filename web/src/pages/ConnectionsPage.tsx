import { useEffect, useState } from 'react'
import { connect, connectStatus, disconnect, listShares, requestConnection } from '../api/endpoints'
import { ConnectState, ConnectStatusResponse } from '../api/types'
import {
  Card,
  CopyButton,
  ErrorBanner,
  Field,
  SuccessBanner,
  useApiAction,
  useApiData,
} from '../components/common'
import { ShareSelect, WorkgroupSelect } from '../components/selects'

// The phases a connection goes through, in the order it goes through them. How
// long each takes is not something the server can say in advance — the first
// waits for a person, the last for every host to answer — so what is shown is
// which phase the connection is in and how long it has been running.
const PHASES: { state: ConnectState; label: string; hint: string }[] = [
  {
    state: 'awaiting-approval',
    label: 'Approval',
    hint: 'Waiting for the registration to be approved with the indexer.',
  },
  {
    state: 'registering',
    label: 'Registration',
    hint: 'Deriving the app key and registering it with the indexer.',
  },
  {
    state: 'connecting',
    label: 'Connection',
    hint: 'Starting the client and warming up a connection to every host.',
  },
]

// pollInterval is how often a running attempt is asked how far along it is, and
// maxPollFailures how many unanswered looks in a row it takes to stop asking.
// The attempt runs at the server either way, so a poll that fails is worth
// repeating rather than taking for the end of it.
const pollInterval = 1000
const maxPollFailures = 5

function isInFlight(state: ConnectState): boolean {
  return state === 'awaiting-approval' || state === 'registering' || state === 'connecting'
}

// duration formats a span of time as m:ss.
function duration(from: string, to: number): string {
  const seconds = Math.max(0, Math.round((to - new Date(from).getTime()) / 1000))
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
}

// ConnectProgress shows the phases of an attempt: the ones it has passed, the
// one it is in, and the one it failed at, if it did. phase is the last phase
// seen in flight, which is what a failure is reported against, and approval says
// whether the attempt began with an approval request or went straight to
// connecting with a key it was given.
function ConnectProgress({
  status,
  phase,
  approval,
}: {
  status: ConnectStatusResponse
  phase: ConnectState | null
  approval: boolean
}) {
  const [now, setNow] = useState(() => Date.now())
  const running = isInFlight(status.state)

  // The elapsed time counts on while the attempt runs, and stops with it.
  useEffect(() => {
    if (!running) return
    const timer = setInterval(() => setNow(Date.now()), 500)
    return () => clearInterval(timer)
  }, [running])

  const steps = approval ? PHASES : PHASES.slice(2)
  const at = steps.findIndex((step) => step.state === (running ? status.state : phase))
  const done = status.state === 'connected'
  const failed = status.state === 'failed'
  const until = running || !status.since ? now : new Date(status.since).getTime()

  return (
    <div className="progress">
      <ol className="steps">
        {steps.map((step, i) => {
          let mark = 'pending'
          if (done || (at >= 0 && i < at)) mark = 'done'
          else if (at === i) mark = failed ? 'failed' : 'active'

          return (
            <li key={step.state} className={`step step-${mark}`}>
              <span className="step-mark" aria-hidden="true">
                {mark === 'done' ? '✓' : mark === 'failed' ? '✕' : ''}
              </span>
              <span>
                <span className="step-label">{step.label}</span>
                {mark === 'active' && <span className="muted"> — {step.hint}</span>}
              </span>
            </li>
          )
        })}
      </ol>
      {status.started && (
        <div className="muted">
          {running ? 'Running for ' : done ? 'Connected in ' : 'Stopped after '}
          {duration(status.started, until)}
        </div>
      )}
    </div>
  )
}

export function ConnectionsPage() {
  const { run, busy, error, message } = useApiAction()
  const { data: shares } = useApiData(() => listShares())
  const [workgroup, setWorkgroup] = useState('')
  const [share, setShare] = useState('')
  const [appKey, setAppKey] = useState('')
  const [status, setStatus] = useState<ConnectStatusResponse | null>(null)
  const [statusError, setStatusError] = useState<string | null>(null)
  const [newAppKey, setNewAppKey] = useState<string | null>(null)
  const ready = Boolean(workgroup.trim() && share.trim())

  // The last phase seen in flight. A failure is a state of its own, so this is
  // what says which phase the attempt failed at.
  const [phase, setPhase] = useState<ConnectState | null>(null)

  // Whether the attempt on show is one that started with an approval request.
  const [approval, setApproval] = useState(false)

  // The attempt being followed. While it is set the buttons stay out of the way,
  // and the poll below keeps the phase on show up to date.
  const [tracking, setTracking] = useState<{ workgroup: string; share: string } | null>(null)

  // What the selected share is backed by decides how it is connected: an indexd
  // share is connected for the first time by approving a request, and only a
  // reconnection of one takes an app key.
  const backend = shares?.find((s) => s.name === share.trim())?.type
  const reconnecting = Boolean(appKey.trim())
  const connectable = backend === 'renterd' || (backend === 'indexd' && reconnecting)

  useEffect(() => {
    if (!tracking) return

    let cancelled = false
    let failures = 0
    let timer: ReturnType<typeof setTimeout>

    const look = async () => {
      try {
        const res = await connectStatus(tracking.workgroup, tracking.share)
        if (cancelled) return
        failures = 0
        setStatusError(null)

        // The app key is handed over once, so it is kept as soon as it arrives.
        if (res.appKey) setNewAppKey(res.appKey)
        if (isInFlight(res.state)) setPhase(res.state)
        setStatus(res)
        if (!isInFlight(res.state)) {
          setTracking(null)
          return
        }
      } catch (e) {
        if (cancelled) return

        // The phase already on show is left there: it is the last thing known
        // about an attempt that is still running at the server.
        setStatusError(e instanceof Error ? e.message : String(e))
        failures++
        if (failures >= maxPollFailures) {
          setTracking(null)
          return
        }
      }
      timer = setTimeout(look, pollInterval)
    }

    timer = setTimeout(look, pollInterval)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [tracking])

  // Picking another workgroup or share shows what that pair is doing, rather
  // than what the last one was. An attempt started in the meantime is the newer
  // news, so a reply that arrives after one has begun is dropped.
  useEffect(() => {
    setStatus(null)
    setStatusError(null)
    setNewAppKey(null)
    setPhase(null)
    setTracking(null)
    if (!ready) return

    const wg = workgroup.trim()
    const sh = share.trim()
    let cancelled = false
    connectStatus(wg, sh)
      .then((res) => {
        if (cancelled) return

        // Any read of the status is the one that takes the app key with it, this
        // one included: an attempt that finished while another pair was on show
        // hands it over here.
        if (res.appKey) setNewAppKey(res.appKey)
        if (isInFlight(res.state)) {
          setPhase(res.state)
          setApproval(res.state !== 'connecting')
          setTracking({ workgroup: wg, share: sh })
        }
        setStatus(res)
      })
      .catch((e) => {
        if (!cancelled) setStatusError(e instanceof Error ? e.message : String(e))
      })

    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only the selection reopens this
  }, [workgroup, share])

  // start takes over from an action that began an attempt: what it returns is
  // the first status, and the poll above carries it from there.
  const start = (res: ConnectStatusResponse, fromApproval: boolean) => {
    setApproval(fromApproval)
    setPhase(isInFlight(res.state) ? res.state : null)
    setNewAppKey(null)
    setStatusError(null)
    setStatus(res)
    if (isInFlight(res.state)) {
      setTracking({ workgroup: workgroup.trim(), share: share.trim() })
    }
  }

  const running = tracking !== null

  return (
    <div className="page">
      <Card title="Connect a workgroup to a share">
        <p className="muted">
          renterd shares: just press <em>Connect</em>. indexd shares connecting for the first
          time: press <em>Request approval</em> and open the approval link — approving the
          registration with the indexer is what carries the connection through, and the app key
          it derives is shown here once it has. Reconnecting an indexd share: paste the saved
          app key and press <em>Connect</em>.
        </p>
        <div className="grid">
          <Field label="Workgroup">
            <WorkgroupSelect value={workgroup} onChange={setWorkgroup} />
          </Field>
          <Field label="Share">
            <ShareSelect value={share} onChange={setShare} />
          </Field>
          <Field label="App key (optional, hex)">
            <input
              type="text"
              value={appKey}
              onChange={(e) => setAppKey(e.target.value)}
              placeholder="only for indexd reconnection"
              autoComplete="off"
            />
          </Field>
        </div>
        <div className="row">
          <button
            className="btn"
            disabled={busy || running || !ready || backend !== 'indexd'}
            onClick={() =>
              run(async () => {
                start(await requestConnection(workgroup.trim(), share.trim()), true)
              })
            }
          >
            Request approval (indexd)
          </button>
          <button
            className="btn btn-primary"
            disabled={busy || running || !ready || !connectable}
            onClick={() =>
              run(async () => {
                const key = appKey.trim() || undefined
                start(await connect(workgroup.trim(), share.trim(), key), false)
              })
            }
          >
            Connect
          </button>
          <button
            className="btn btn-danger"
            disabled={busy || running || !ready}
            onClick={() => {
              if (!window.confirm(`Disconnect ${share.trim()} from this workgroup?`)) return
              run(async () => {
                await disconnect(workgroup.trim(), share.trim())
                setNewAppKey(null)
                setPhase(null)
                setStatus({ state: 'idle' })
              }, 'Disconnected.')
            }}
          >
            Disconnect
          </button>
        </div>
        {ready && backend === 'indexd' && !reconnecting && !running && (
          <p className="muted">
            This indexd share is connected by approving a request, not by pressing{' '}
            <em>Connect</em>. Paste the app key of an existing connection to reconnect one.
          </p>
        )}
        {status?.url && (
          <div className="banner banner-success">
            Approval requested. Open{' '}
            <a href={status.url} target="_blank" rel="noreferrer">
              this link
            </a>{' '}
            to approve the registration. The connection is made as soon as it is approved.
          </div>
        )}
        {status && status.started && (
          <ConnectProgress status={status} phase={phase} approval={approval} />
        )}
        {status && !status.started && (
          <div className="muted connection-state">
            {status.state === 'connected'
              ? 'This workgroup is connected to this share.'
              : 'This workgroup is not connected to this share.'}
          </div>
        )}
        {newAppKey && (
          <div className="banner banner-success stack">
            <div>
              Save this app key — it is required to reconnect this workgroup to the share and
              is shown only once:
            </div>
            <div className="mono appkey">
              {newAppKey} <CopyButton value={newAppKey} />
            </div>
          </div>
        )}
        <ErrorBanner error={error || status?.error || statusError || null} />
        {!newAppKey && <SuccessBanner message={message} />}
      </Card>
    </div>
  )
}
