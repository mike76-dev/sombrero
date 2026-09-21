import { getStats, getVersion } from '../api/endpoints'
import { Card, ErrorBanner, formatBytes, useApiData } from '../components/common'

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
  const { data, error, busy, reload } = useApiData(async () => {
    const [stats, { version }] = await Promise.all([getStats(), getVersion()])
    return { ...stats, version }
  })

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
                <th>Version</th>
                <td>{data.version}</td>
              </tr>
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
              {data.backlog && (
                <>
                  <tr>
                    <th>Waiting to be uploaded</th>
                    <td>
                      {formatBytes(data.backlog.buffered)}
                      {data.backlog.limit > 0
                        ? ` of ${formatBytes(data.backlog.limit)}`
                        : ' (no limit)'}
                    </td>
                  </tr>
                  <tr>
                    <th>Database space for buffers</th>
                    <td>{formatBytes(data.backlog.onDisk)}</td>
                  </tr>
                </>
              )}
            </tbody>
          </table>
        )}
      </Card>
    </div>
  )
}
