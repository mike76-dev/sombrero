import { useEffect, useState } from 'react'
import { thumbnailURL } from './api'
import { FileIcon } from '../icons'

// thumbnailed lists the kinds of image the server makes thumbnails of.
const thumbnailed = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'tif', 'tiff'])

export function hasThumbnail(name: string): boolean {
  const i = name.lastIndexOf('.')
  return i >= 0 && thumbnailed.has(name.slice(i + 1).toLowerCase())
}

// Thumb shows a small image of a file in place of its icon, where the server
// has one; the icon stands until it loads and where there is none. The image
// is fetched only when it comes into view, and the key makes a changed file
// start over.
export function Thumb({ share, path, name, dir, modifiedAt }: { share: string; path: string; name: string; dir: boolean; modifiedAt: string }) {
  const [state, setState] = useState<'loading' | 'shown' | 'none'>('loading')
  useEffect(() => setState('loading'), [share, path, modifiedAt])
  if (dir || !hasThumbnail(name) || state === 'none') return <FileIcon name={name} dir={dir} />
  return (
    <>
      {state === 'loading' && <FileIcon name={name} dir={false} />}
      <img
        key={modifiedAt}
        className={state === 'shown' ? 'thumb' : 'thumb thumb-loading'}
        src={thumbnailURL(share, path)}
        alt=""
        loading="lazy"
        decoding="async"
        onLoad={() => setState('shown')}
        onError={() => setState('none')}
      />
    </>
  )
}
