import { useEffect, useState } from 'react'

// A place in the library is a hash: #/ for the shares, #/<share> for the root
// of a share and #/<share>/<folder>/<folder> for a folder in it, each segment
// URL-encoded. A folder as it was carries the time: #/<share>/<folder>?at=<time>.
export interface Route {
  share: string | null
  path: string
  at?: string
}

export function parseRoute(hash: string): Route {
  const [location, query = ''] = hash.replace(/^#\/?/, '').split('?', 2)
  const segments = location
    .split('/')
    .filter((s) => s !== '')
    .map(decodeURIComponent)
  if (segments.length === 0) return { share: null, path: '/' }
  const at = new URLSearchParams(query).get('at') ?? undefined
  return { share: segments[0], path: '/' + segments.slice(1).join('/'), at }
}

export function hashFor(share: string | null, path = '/', at?: string): string {
  if (share === null) return '#/'
  const segments = [share, ...path.split('/').filter((s) => s !== '')]
  const hash = '#/' + segments.map(encodeURIComponent).join('/')
  return at ? hash + '?at=' + encodeURIComponent(at) : hash
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
