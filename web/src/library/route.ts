import { useEffect, useState } from 'react'

// A place in the library is a hash: #/ for the shares, #/<share> for the root
// of a share and #/<share>/<folder>/<folder> for a folder in it, each segment
// URL-encoded.
export interface Route {
  share: string | null
  path: string
}

export function parseRoute(hash: string): Route {
  const segments = hash
    .replace(/^#\/?/, '')
    .split('/')
    .filter((s) => s !== '')
    .map(decodeURIComponent)
  if (segments.length === 0) return { share: null, path: '/' }
  return { share: segments[0], path: '/' + segments.slice(1).join('/') }
}

export function hashFor(share: string | null, path = '/'): string {
  if (share === null) return '#/'
  const segments = [share, ...path.split('/').filter((s) => s !== '')]
  return '#/' + segments.map(encodeURIComponent).join('/')
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

// parentOf is the folder a path is in, the root being its own parent.
export function parentOf(path: string): string {
  const i = path.lastIndexOf('/')
  return i <= 0 ? '/' : path.slice(0, i)
}
