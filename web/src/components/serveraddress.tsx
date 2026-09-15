import { useState } from 'react'
import { probeServer } from '../api/endpoints'
import { ProbeResponse } from '../api/types'
import { useApiAction } from './common'

// The address of a share's storage backend, with a way to find out whether
// anything is listening at it before the share is registered with it. A typed
// address is the one thing on the form that nothing else can check.

// ServerAddressField stands where a Field would, and holds the input, the Test
// button, and what the test found. backend decides what the placeholder
// suggests; the rest is the same for either one.
export function ServerAddressField({
  value,
  onChange,
  backend,
  disabled,
}: {
  value: string
  onChange: (value: string) => void
  backend: string
  disabled?: boolean
}) {
  const { run, busy, error } = useApiAction()
  const [probe, setProbe] = useState<ProbeResponse | null>(null)

  return (
    <div className="field">
      <span className="field-label">
        {backend === 'indexd' ? 'Indexer address' : 'renterd address'}
      </span>
      <div className="row row-form">
        <input
          type="text"
          value={value}
          disabled={disabled}
          onChange={(e) => {
            onChange(e.target.value)
            setProbe(null)
          }}
          placeholder={
            backend === 'indexd' ? 'https://indexer.example.com' : 'http://127.0.0.1:9980'
          }
        />
        <button
          className="btn btn-small"
          disabled={disabled || busy || !value.trim()}
          onClick={() =>
            run(async () => {
              setProbe(null)
              setProbe(await probeServer(value.trim()))
            })
          }
        >
          {busy ? 'Testing…' : 'Test'}
        </button>
      </div>
      {probe && (
        <span className={probe.reachable ? 'muted' : 'field-error'}>
          {probe.reachable ? `Something is listening at ${probe.address}.` : probe.error + '.'}
          {probe.warning && ` ${probe.warning}.`}
        </span>
      )}
      {error && <span className="field-error">{error}</span>}
    </div>
  )
}
