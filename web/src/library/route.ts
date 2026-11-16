import { useEffect, useState } from 'react'

// A place in the library is a hash: #/ for the shares, #/<share> for the root
// of a share and #/<share>/<folder>/<folder> for a folder in it, each segment
// URL-encoded. A folder as it was carries the time: #/<share>/<folder>?at=<time>.
// The deleted files of a share are at #/<share>?bin, its activity at
// #/<share>?activity, a search of it at #/<share>?q=<text>, and a file in the
// editor at #/<share>/<folder>/<file>?edit.
export interface Route {
  share: string | null
  path: string
  at?: string
  bin?: boolean
  activity?: boolean
  q?: string
  edit?: boolean
}

// decode reads a segment of the hash. One with a malformed escape, as a link
// passed on with its encoding undone has, is taken as it stands.
function decode(segment: string): string {
  try {
    return decodeURIComponent(segment)
  } catch {
    return segment
  }
}

export function parseRoute(hash: string): Route {
  const [location, query = ''] = hash.replace(/^#\/?/, '').split('?', 2)
  const segments = location
    .split('/')
    .filter((s) => s !== '')
    .map(decode)
  if (segments.length === 0) return { share: null, path: '/' }
  const params = new URLSearchParams(query)
  const at = params.get('at') ?? undefined
  return {
    share: segments[0],
    path: '/' + segments.slice(1).join('/'),
    at,
    bin: params.has('bin') || undefined,
    activity: params.has('activity') || undefined,
    q: params.get('q') ?? undefined,
    edit: params.has('edit') || undefined,
  }
}

export function searchHash(share: string, q: string): string {
  return hashFor(share) + '?q=' + encodeURIComponent(q)
}

export function editHash(share: string, path: string): string {
  return hashFor(share, path) + '?edit'
}

export function hashFor(share: string | null, path = '/', at?: string): string {
  if (share === null) return '#/'
  const segments = [share, ...path.split('/').filter((s) => s !== '')]
  const hash = '#/' + segments.map(encodeURIComponent).join('/')
  return at ? hash + '?at=' + encodeURIComponent(at) : hash
}

export function binHash(share: string): string {
  return hashFor(share) + '?bin'
}

export function activityHash(share: string): string {
  return hashFor(share) + '?activity'
}

export function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => parseRoute(window.location.hash))
  useEffect(() => {
    const onHashChange = () => setRoute(parseRoute(window.location.hash))
    window.addEventListener('hashchange', onHashChange)
    return () => window.removeEventListener('hashchange', onHashChange)
  }, [])
  return route
}
