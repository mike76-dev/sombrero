import { useEffect, useState } from 'react'
import { thumbnailURL } from './api'
import { FileIcon } from '../icons'

// thumbnailed lists the kinds of image the server makes thumbnails of.
const thumbnailed = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'tif', 'tiff'])

export function hasThumbnail(name: string): boolean {
  const i = name.lastIndexOf('.')
  return i >= 0 && thumbnailed.has(name.slice(i + 1).toLowerCase())
}

// retryAfter is how long a thumbnail that failed to load waits before it is
// asked for once more, as a file just uploaded may still be settling.
const retryAfter = 2000

// Thumb shows a small image of a file in place of its icon, where the server
// has one, and the icon where it has none. The image is fetched only when it
// comes into view, and a changed file starts over.
export function Thumb({ share, path, name, dir, modifiedAt }: { share: string; path: string; name: string; dir: boolean; modifiedAt: string }) {
  const [attempt, setAttempt] = useState(0)
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    setAttempt(0)
    setFailed(false)
  }, [share, path, modifiedAt])
  useEffect(() => {
    if (attempt !== 1) return
    const timer = setTimeout(() => setAttempt(2), retryAfter)
    return () => clearTimeout(timer)
  }, [attempt])
  if (dir || !hasThumbnail(name) || failed) return <FileIcon name={name} dir={dir} />
  // Between the failure and the retry, the image is not asked for.
  if (attempt === 1) return <span className="thumb" aria-hidden />
  return (
    <img
      key={`${modifiedAt}/${attempt}`}
      className="thumb"
      src={thumbnailURL(share, path)}
      alt=""
      loading="lazy"
      decoding="async"
      onError={() => (attempt === 0 ? setAttempt(1) : setFailed(true))}
    />
  )
}
