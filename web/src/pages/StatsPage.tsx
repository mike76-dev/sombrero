import { getStats } from '../api/endpoints'
import { Card, ErrorBanner, useApiData } from '../components/common'

function formatBytes(n: number): string {
  if (n < 1000) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB', 'PB']
  let value = n
  let i = -1
  while (value >= 1000 && i < units.length - 1) {
    value /= 1000
    i++
  }
  return `${value.toFixed(2)} ${units[i]}`
}

function formatUptime(start: string): string {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(start).getTime()) / 1000))
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const parts = []
  if (days > 0) parts.push(`${days}d`)
  if (hours > 0 || days > 0) parts.push(`${hours}h`)
  parts.push(`${minutes}m`)
  return parts.join(' ')
}

export function StatsPage() {
  const { data, error, busy, reload } = useApiData(() => getStats())

  return (
    <div className="page">
      <Card
        title={
          <span className="row row-spread">
            <span>Server statistics</span>
            <button className="btn btn-small" onClick={reload} disabled={busy}>
              Reload
            </button>
          </span>
        }
      >
        <ErrorBanner error={error} />
        {data && (
          <table className="table table-kv">
            <tbody>
              <tr>
                <th>Started</th>
                <td>{new Date(data.start).toLocaleString()}</td>
              </tr>
              <tr>
                <th>Uptime</th>
                <td>{formatUptime(data.start)}</td>
              </tr>
              <tr>
                <th>Total opens</th>
                <td>{data.fOpens}</td>
              </tr>
              <tr>
                <th>Sessions established</th>
                <td>{data.sOpens}</td>
              </tr>
              <tr>
                <th>Password violations</th>
                <td>{data.pwErrors}</td>
              </tr>
              <tr>
                <th>Access permission errors</th>
                <td>{data.permErrors}</td>
              </tr>
              <tr>
                <th>Total data sent</th>
                <td>{formatBytes(data.bytesSent)}</td>
              </tr>
              <tr>
                <th>Total data received</th>
                <td>{formatBytes(data.bytesRcvd)}</td>
              </tr>
            </tbody>
          </table>
        )}
      </Card>
    </div>
  )
}
