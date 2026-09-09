import { listShares, listWorkgroups } from '../api/endpoints'
import { Share, Workgroup } from '../api/types'
import { ErrorBanner, useApiData } from './common'

// Dropdowns fed by the GET /workgroups and GET /shares endpoints.
// Workgroups are identified by UUID, shares by name.

export function WorkgroupSelect({
  value,
  onChange,
  onSelect,
}: {
  value: string
  onChange: (value: string) => void

  // onSelect reports the workgroup itself, for the callers that want to show
  // its name rather than the UUID they select it by.
  onSelect?: (workgroup: Workgroup | undefined) => void
}) {
  const { data, error, busy } = useApiData(() => listWorkgroups())
  const workgroups = data || []
  return (
    <>
      <select
        value={value}
        onChange={(e) => {
          onChange(e.target.value)
          onSelect?.(workgroups.find((wg) => wg.uuid === e.target.value))
        }}
      >
        <option value="">
          {busy
            ? 'loading…'
            : workgroups.length === 0
              ? '— no workgroups —'
              : '— select workgroup —'}
        </option>
        {workgroups.map((wg) => (
          <option key={wg.uuid} value={wg.uuid}>
            {wg.name || wg.uuid}
          </option>
        ))}
      </select>
      <ErrorBanner error={error} />
    </>
  )
}

export function ShareSelect({
  value,
  onChange,
  onSelect,
}: {
  value: string
  onChange: (value: string) => void

  // onSelect reports the share itself, for the callers that need more of it
  // than the name — what it is backed by, above all.
  onSelect?: (share: Share | undefined) => void
}) {
  const { data, error, busy } = useApiData(() => listShares())
  const shares = data || []
  return (
    <>
      <select
        value={value}
        onChange={(e) => {
          onChange(e.target.value)
          onSelect?.(shares.find((s) => s.name === e.target.value))
        }}
      >
        <option value="">
          {busy ? 'loading…' : shares.length === 0 ? '— no shares —' : '— select share —'}
        </option>
        {shares.map((s) => (
          <option key={s.name} value={s.name}>
            {s.name} ({s.type})
          </option>
        ))}
      </select>
      <ErrorBanner error={error} />
    </>
  )
}
