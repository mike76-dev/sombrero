import { useEffect, useState } from 'react'
import { connectStatus } from '../api/endpoints'
import { ConnectState, ConnectStatusResponse } from '../api/types'
import { CopyButton, ErrorBanner } from './common'

// Following a connection: the phases it goes through, the poll that keeps them
// up to date, and the panel that shows them. The Connections page and the setup
// wizard both connect a workgroup to a share, and both do it through this.

// How long each phase takes is not something the server can say in advance — the
// first waits for a person, the last for every host to answer — so what is shown
// is which phase the connection is in and how long it has been running.
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

export function isInFlight(state: ConnectState): boolean {
  return state === 'awaiting-approval' || state === 'registering' || state === 'connecting'
}

// duration formats a span of time as m:ss.
function duration(from: string, to: number): string {
  const seconds = Math.max(0, Math.round((to - new Date(from).getTime()) / 1000))
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
}

export interface ConnectAttempt {
  status: ConnectStatusResponse | null
  phase: ConnectState | null
  approval: boolean
  appKey: string | null
  error: string | null

  // running is true while an attempt is being followed, connected once the
  // workgroup is on the share, whether by this attempt or an earlier one.
  running: boolean
  connected: boolean

  // begin takes over from an action that started an attempt: what the action
  // returned is the first status, and the poll carries it from there.
  begin: (res: ConnectStatusResponse, fromApproval: boolean) => void

  // idle drops what is on show, for when the connection has just been removed.
  idle: () => void
}

// useConnectAttempt follows the connection of one workgroup to one share:
// it reports what that pair is doing as soon as both are named, and keeps the
// phase up to date while an attempt runs.
export function useConnectAttempt(workgroup: string, share: string): ConnectAttempt {
  const [status, setStatus] = useState<ConnectStatusResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [appKey, setAppKey] = useState<string | null>(null)
  const [phase, setPhase] = useState<ConnectState | null>(null)
  const [approval, setApproval] = useState(false)
  const [tracking, setTracking] = useState<{ workgroup: string; share: string } | null>(null)

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
        setError(null)

        // The app key is handed over once, so it is kept as soon as it arrives.
        if (res.appKey) setAppKey(res.appKey)
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
        setError(e instanceof Error ? e.message : String(e))
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

  // Naming another workgroup or share shows what that pair is doing, rather than
  // what the last one was. An attempt started in the meantime is the newer news,
  // so a reply that arrives after one has begun is dropped.
  useEffect(() => {
    setStatus(null)
    setError(null)
    setAppKey(null)
    setPhase(null)
    setTracking(null)
    if (!workgroup || !share) return

    let cancelled = false
    connectStatus(workgroup, share)
      .then((res) => {
        if (cancelled) return

        // Any read of the status is the one that takes the app key with it, this
        // one included: an attempt that finished while another pair was on show
        // hands it over here.
        if (res.appKey) setAppKey(res.appKey)
        if (isInFlight(res.state)) {
          setPhase(res.state)
          setApproval(res.state !== 'connecting')
          setTracking({ workgroup, share })
        }
        setStatus(res)
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      })

    return () => {
      cancelled = true
    }
  }, [workgroup, share])

  return {
    status,
    phase,
    approval,
    appKey,
    error,
    running: tracking !== null,
    connected: status?.state === 'connected',
    begin: (res, fromApproval) => {
      setApproval(fromApproval)
      setPhase(isInFlight(res.state) ? res.state : null)
      setAppKey(null)
      setError(null)
      setStatus(res)
      if (isInFlight(res.state)) setTracking({ workgroup, share })
    },
    idle: () => {
      setAppKey(null)
      setPhase(null)
      setStatus({ state: 'idle' })
    },
  }
}

// ConnectProgress shows the phases of an attempt: the ones it has passed, the
// one it is in, and the one it failed at, if it did.
function ConnectProgress({ attempt }: { attempt: ConnectAttempt }) {
  const [now, setNow] = useState(() => Date.now())
  const status = attempt.status as ConnectStatusResponse
  const running = isInFlight(status.state)

  // The elapsed time counts on while the attempt runs, and stops with it.
  useEffect(() => {
    if (!running) return
    const timer = setInterval(() => setNow(Date.now()), 500)
    return () => clearInterval(timer)
  }, [running])

  const steps = attempt.approval ? PHASES : PHASES.slice(2)
  const at = steps.findIndex((step) => step.state === (running ? status.state : attempt.phase))
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

// ConnectStatusView is everything an attempt has to say: the approval link while
// it waits for one, the phase it is in, the app key it derived, and what went
// wrong if something did.
export function ConnectStatusView({ attempt }: { attempt: ConnectAttempt }) {
  const { status } = attempt

  return (
    <>
      {status?.url && (
        <div className="banner banner-success">
          Approval requested. Open{' '}
          <a href={status.url} target="_blank" rel="noreferrer">
            this link
          </a>{' '}
          to approve the registration. The connection is made as soon as it is approved.
        </div>
      )}
      {status && status.started && <ConnectProgress attempt={attempt} />}
      {status && !status.started && (
        <div className="muted connection-state">
          {status.state === 'connected'
            ? 'This workgroup is connected to this share.'
            : 'This workgroup is not connected to this share.'}
        </div>
      )}
      {attempt.appKey && (
        <div className="banner banner-success stack">
          <div>
            Save this app key — it is required to reconnect this workgroup to the share and is
            shown only once:
          </div>
          <div className="mono appkey">
            {attempt.appKey} <CopyButton value={attempt.appKey} />
          </div>
        </div>
      )}
      <ErrorBanner error={status?.error || attempt.error || null} />
    </>
  )
}
