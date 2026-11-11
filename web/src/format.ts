export function formatBytes(n: number): string {
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

const timeFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })

// formatTime shows a time the API returned, which is in UTC, in the browser's zone.
export function formatTime(iso: string): string {
  const t = new Date(iso)
  return isNaN(t.getTime()) ? '' : timeFormat.format(t)
}
