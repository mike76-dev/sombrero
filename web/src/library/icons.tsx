import { AnchorHTMLAttributes, ButtonHTMLAttributes } from 'react'

// The icons are drawn here as paths on a 24-point grid, stroked in the text
// colour, so that nothing is loaded from elsewhere and they follow the theme.
const paths = {
  folder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
  file: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z M14 3v5h5',
  document: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z M14 3v5h5 M9 13h6 M9 17h6',
  sheet: 'M3 6a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z M3 10h18 M3 15h18 M9 10v10',
  slides: 'M3 6a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z M12 17v4 M8 21h8',
  image: 'M3 6a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z M7.5 9a1.5 1.5 0 1 0 3 0a1.5 1.5 0 1 0-3 0 M21 15l-4.5-4.5L6 20',
  video: 'M3 7a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z M7 5v14 M17 5v14 M3 10h4 M3 14h4 M17 10h4 M17 14h4',
  audio: 'M9 18V6l10-2v12 M3 18a3 3 0 1 0 6 0a3 3 0 1 0-6 0 M13 16a3 3 0 1 0 6 0a3 3 0 1 0-6 0',
  archive: 'M3 5a1 1 0 0 1 1-1h16a1 1 0 0 1 1 1v2a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z M5 8v10a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8 M10 12h4',
  code: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z M14 3v5h5 M10 12l-2 2 2 2 M14 12l2 2-2 2',
  download: 'M12 4v11 M7 10l5 5 5-5 M4 19h16',
  open: 'M14 4h6v6 M20 4l-9 9 M19 13v5a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h5',
  edit: 'M17 3a2.8 2.8 0 0 1 4 4L7.5 20.5 2 22l1.5-5.5z M15 5l4 4',
  view: 'M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z M9 12a3 3 0 1 0 6 0a3 3 0 1 0-6 0',
  more: 'M4 12a1 1 0 1 0 2 0a1 1 0 1 0-2 0 M11 12a1 1 0 1 0 2 0a1 1 0 1 0-2 0 M18 12a1 1 0 1 0 2 0a1 1 0 1 0-2 0',
  restore: 'M3 12a9 9 0 1 0 2.6-6.4 M3 4v5h5',
  pin: 'M12 17v5 M9 3h6l-1 7 3 3H7l3-3z',
  trash: 'M3 6h18 M8 6V4h8v2 M6 6l1 14h10l1-14 M10 11v6 M14 11v6',
  copy: 'M9 11a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-8a2 2 0 0 1-2-2z M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1',
  link: 'M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1 M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1',
  close: 'M18 6L6 18 M6 6l12 12',
  upload: 'M12 16V5 M7 10l5-5 5 5 M4 19h16',
  uploadFolder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z M12 17v-6 M9.5 13.5L12 11l2.5 2.5',
  newFolder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z M12 10.5v6 M9 13.5h6',
  history: 'M3 12a9 9 0 1 0 2.6-6.4 M3 4v5h5 M12 8v4l3 2',
  activity: 'M3 12h4l3-8 4 16 3-8h4',
  search: 'M4 11a7 7 0 1 0 14 0a7 7 0 1 0-14 0 M20 20l-4-4',
  moon: 'M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z',
  sun: 'M8 12a4 4 0 1 0 8 0a4 4 0 1 0-8 0 M12 2v2 M12 20v2 M4.9 4.9l1.4 1.4 M17.7 17.7l1.4 1.4 M2 12h2 M20 12h2 M4.9 19.1l1.4-1.4 M17.7 6.3l1.4-1.4',
  logout: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4 M16 17l5-5-5-5 M21 12H9',
}

export type IconName = keyof typeof paths

// kinds maps the extensions with an icon of their own; the rest are files.
const kinds: Record<string, IconName> = {
  jpg: 'image', jpeg: 'image', png: 'image', gif: 'image', webp: 'image', bmp: 'image', avif: 'image', svg: 'image', heic: 'image', tif: 'image', tiff: 'image',
  mp4: 'video', m4v: 'video', webm: 'video', mov: 'video', mkv: 'video', avi: 'video', mpg: 'video', mpeg: 'video', ogv: 'video', '3gp': 'video', wmv: 'video',
  mp3: 'audio', m4a: 'audio', aac: 'audio', ogg: 'audio', oga: 'audio', opus: 'audio', wav: 'audio', flac: 'audio', weba: 'audio',
  pdf: 'document', doc: 'document', docx: 'document', odt: 'document', rtf: 'document', txt: 'document', md: 'document',
  xls: 'sheet', xlsx: 'sheet', ods: 'sheet', csv: 'sheet',
  ppt: 'slides', pptx: 'slides', odp: 'slides',
  zip: 'archive', tar: 'archive', gz: 'archive', tgz: 'archive', bz2: 'archive', xz: 'archive', '7z': 'archive', rar: 'archive',
  html: 'code', htm: 'code', js: 'code', ts: 'code', json: 'code', xml: 'code', yml: 'code', yaml: 'code', go: 'code', py: 'code', sh: 'code', css: 'code',
}

// iconFor picks the icon of a file by its name, or the folder's.
export function iconFor(name: string, dir: boolean): IconName {
  if (dir) return 'folder'
  const i = name.lastIndexOf('.')
  return (i >= 0 && kinds[name.slice(i + 1).toLowerCase()]) || 'file'
}

export function Icon({ name, size = 18 }: { name: IconName; size?: number }) {
  return (
    <svg className="icon" width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.75} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d={paths[name]} />
    </svg>
  )
}

export function FileIcon({ name, dir }: { name: string; dir: boolean }) {
  return (
    <span className={dir ? 'file-icon file-icon-folder' : 'file-icon'} aria-hidden>
      <Icon name={iconFor(name, dir)} size={20} />
    </span>
  )
}

// An IconButton shows its label as a tooltip and to assistive technology.
export function IconButton({ icon, label, danger, className, ...rest }: { icon: IconName; label: string; danger?: boolean } & ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button type="button" className={['btn-icon', danger ? 'btn-icon-danger' : '', className ?? ''].join(' ').trim()} aria-label={label} data-tip={label} {...rest}>
      <Icon name={icon} />
    </button>
  )
}

export function IconLink({ icon, label, className, ...rest }: { icon: IconName; label: string } & AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <a className={['btn-icon', className ?? ''].join(' ').trim()} aria-label={label} data-tip={label} {...rest}>
      <Icon name={icon} />
    </a>
  )
}
