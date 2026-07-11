import { useState } from 'react'
import { ApiSettings, loadSettings, saveSettings } from '../api/client'
import { getBanStatus } from '../api/endpoints'
import { Card, ErrorBanner, Field, SuccessBanner, useApiAction } from '../components/common'

export function SettingsPage() {
  const [settings, setSettings] = useState<ApiSettings>(loadSettings())
  const { run, busy, error, message, setMessage } = useApiAction()

  const save = () => {
    saveSettings({ ...settings, apiBase: settings.apiBase.trim() || '/api' })
    setMessage('Settings saved.')
  }

  return (
    <div className="page">
      <Card title="API connection">
        <div className="grid">
          <Field label="API address">
            <input
              type="text"
              value={settings.apiBase}
              onChange={(e) => setSettings((s) => ({ ...s, apiBase: e.target.value }))}
              placeholder="/api"
            />
          </Field>
          <Field label="API password">
            <input
              type="password"
              value={settings.password}
              onChange={(e) => setSettings((s) => ({ ...s, password: e.target.value }))}
              autoComplete="off"
            />
          </Field>
        </div>
        <p className="muted">
          The default <span className="mono">/api</span> works with the dev server proxy
          (see <span className="mono">vite.config.ts</span>) or a reverse proxy that maps{' '}
          <span className="mono">/api</span> to the Sombrero API port. A full URL such as{' '}
          <span className="mono">http://localhost:9999</span> also works if the browser is
          allowed to reach it directly.
        </p>
        <div className="row">
          <button className="btn btn-primary" onClick={save}>
            Save
          </button>
          <button
            className="btn"
            disabled={busy}
            onClick={() => {
              save()
              run(async () => {
                await getBanStatus('connection-test')
                setMessage('Connection OK.')
              })
            }}
          >
            Save &amp; test connection
          </button>
        </div>
        <ErrorBanner error={error} />
        <SuccessBanner message={message} />
      </Card>
    </div>
  )
}
