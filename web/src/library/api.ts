// The file API, as the library calls it. The calls carry the session cookie
// the login set, so there is nothing to configure: the API is served under
// /files by the same server as the page.

const base = '/files'

export interface Identity {
  username: string
  workgroup: { uuid: string; name?: string }
  editor?: boolean
}

// An EditSession is where the editor is loaded from, the access token it is
// posted, and when the token expires, in milliseconds since the epoch.
export interface EditSession {
  url: string
  token: string
  ttl: number
}

// editSession opens the file in the editor: to edit where the account may
// write, to view otherwise.
export function editSession(share: string, path: string): Promise<EditSession> {
  return call<EditSession>(`/share/${encodeURIComponent(share)}/edit`, { query: { path } })
}

export interface Rights {
  read: boolean
  write: boolean
  delete: boolean
  execute: boolean
}

export interface ShareInfo {
  name: string
  type: string
  remark?: string
  rights: Rights
}

export interface Entry {
  name: string
  path: string
  dir: boolean
  size: number
  createdAt: string
  modifiedAt: string
}

export interface Listing {
  share: string
  path: string
  rights: { write: boolean; delete: boolean }
  entries: Entry[]
}

export class FilesError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

type Query = Record<string, string | undefined>

function url(path: string, query?: Query): string {
  let u = base + path
  if (query) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined) params.set(key, value)
    }
    const qs = params.toString()
    if (qs) u += '?' + qs
  }
  return u
}

async function call<T = void>(path: string, opts: { method?: string; query?: Query; body?: unknown } = {}): Promise<T> {
  let res: Response
  try {
    res = await fetch(url(path, opts.query), {
      method: opts.method ?? 'GET',
      headers: opts.body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    })
  } catch {
    throw new FilesError('The server cannot be reached.', 0)
  }

  if (!res.ok) {
    let message = res.statusText
    try {
      const parsed = await res.json()
      if (typeof parsed === 'string' && parsed) message = parsed
    } catch {
      // The status text stands.
    }
    throw new FilesError(message, res.status)
  }

  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export function login(workgroup: string, username: string, password: string): Promise<Identity> {
  return call<Identity>('/login', { method: 'POST', body: { workgroup, username, password } })
}

export function logout(): Promise<void> {
  return call('/logout', { method: 'POST' })
}

export function me(): Promise<Identity> {
  return call<Identity>('/me')
}

export function shares(): Promise<ShareInfo[]> {
  return call<ShareInfo[]>('/shares')
}

export interface Version {
  id: number
  size: number
  modifiedAt: string
  replacedAt: string
  replacedBy?: string
  kept: boolean
}

// A version is named by its id, or by a time at which it was the file's contents.
export type VersionRef = { id: number } | { at: string }

function refQuery(ref: VersionRef): Query {
  return 'id' in ref ? { id: String(ref.id) } : { at: ref.at }
}

// list lists a folder as it is or, given a time, as it was then: deleted
// files included and without rights, as the past is read-only.
export function list(share: string, path: string, at?: string): Promise<Listing> {
  return call<Listing>(`/share/${encodeURIComponent(share)}/list`, { query: { path, at } })
}

// search finds the files and folders of the share whose names contain the
// text, among those the account may list, by path and up to 200 of them.
export function search(share: string, q: string): Promise<Entry[]> {
  return call<Entry[]>(`/share/${encodeURIComponent(share)}/search`, { query: { q } })
}

export function snapshots(share: string, path: string): Promise<string[]> {
  return call<string[]>(`/share/${encodeURIComponent(share)}/snapshots`, { query: { path } })
}

export function versions(share: string, path: string): Promise<Version[]> {
  return call<Version[]>(`/share/${encodeURIComponent(share)}/versions`, { query: { path } })
}

// A deleted file is one gone but for its versions, the newest of which, named
// by the id, is what the deletion left.
export interface Deleted {
  name: string
  path: string
  id: number
  size: number
  modifiedAt: string
  deletedAt: string
  deletedBy?: string
}

// deleted lists the deleted files of the share, newest deletion first.
export function deleted(share: string): Promise<Deleted[]> {
  return call<Deleted[]>(`/share/${encodeURIComponent(share)}/deleted`)
}

// An entry of a share's activity feed: what an account of the workgroup did,
// where and when. The target is the new path of a rename or a move, the size
// that of an uploaded or restored file.
export interface ActivityEntry {
  id: number
  at: string
  username: string
  action: 'upload' | 'update' | 'delete' | 'rename' | 'mkdir' | 'rmdir' | 'restore'
  path: string
  target?: string
  size?: number
}

// activity lists a page of the feed of the share, newest first, before the
// entry with the id where one is given.
export function activity(share: string, before?: number, limit?: number): Promise<ActivityEntry[]> {
  return call<ActivityEntry[]>(`/share/${encodeURIComponent(share)}/activity`, {
    query: { before: before === undefined ? undefined : String(before), limit: limit === undefined ? undefined : String(limit) },
  })
}

export function versionURL(share: string, path: string, ref: VersionRef, inline = false): string {
  return url(`/share/${encodeURIComponent(share)}/version`, { path, ...refQuery(ref), inline: inline ? '1' : undefined })
}

export function restore(share: string, path: string, ref: VersionRef): Promise<Entry> {
  return call<Entry>(`/share/${encodeURIComponent(share)}/restore`, { method: 'POST', query: { path, ...refQuery(ref) } })
}

// deleteVersions removes one version of the file, or all of them without an id.
export function deleteVersions(share: string, path: string, id?: number): Promise<void> {
  return call(`/share/${encodeURIComponent(share)}/versions`, { method: 'DELETE', query: { path, id: id === undefined ? undefined : String(id) } })
}

export function keepVersion(share: string, path: string, id: number, keep: boolean): Promise<void> {
  return call(`/share/${encodeURIComponent(share)}/keep`, { method: keep ? 'POST' : 'DELETE', query: { path, id: String(id) } })
}

// A sharing link to a file: whoever holds the address reads the file as the
// account that made the link, until the link expires, where it does, or goes.
export interface SharingLink {
  token: string
  url: string
  path: string
  createdBy: string
  createdAt: string
  expiresAt?: string
}

// createLink makes a link to the file, expiring at the time where one is given.
export function createLink(share: string, path: string, expiresAt?: string): Promise<SharingLink> {
  return call<SharingLink>(`/share/${encodeURIComponent(share)}/link`, { method: 'POST', query: { path }, body: { expiresAt } })
}

// links lists the links to the file made by the workgroup, newest first.
export function links(share: string, path: string): Promise<SharingLink[]> {
  return call<SharingLink[]>(`/share/${encodeURIComponent(share)}/links`, { query: { path } })
}

export function deleteLink(share: string, token: string): Promise<void> {
  return call(`/share/${encodeURIComponent(share)}/link`, { method: 'DELETE', query: { token } })
}

// fileURL is where a file is downloaded from, or shown if the browser can.
export function fileURL(share: string, path: string, inline = false): string {
  return url(`/share/${encodeURIComponent(share)}/file`, { path, inline: inline ? '1' : undefined })
}

export function makeFolder(share: string, path: string): Promise<Entry> {
  return call<Entry>(`/share/${encodeURIComponent(share)}/folder`, { method: 'POST', query: { path } })
}

export function deleteFile(share: string, path: string, purge: boolean): Promise<void> {
  return call(`/share/${encodeURIComponent(share)}/file`, { method: 'DELETE', query: { path, purge: purge ? '1' : undefined } })
}

export function deleteFolder(share: string, path: string): Promise<void> {
  return call(`/share/${encodeURIComponent(share)}/folder`, { method: 'DELETE', query: { path, recursive: '1' } })
}

export function rename(share: string, from: string, to: string, replace: boolean): Promise<Entry> {
  return call<Entry>(`/share/${encodeURIComponent(share)}/rename`, {
    method: 'POST',
    query: { path: from, to, replace: replace ? '1' : undefined },
  })
}

// upload stores the file at the path, reporting how much has been sent as it
// goes. It uses XMLHttpRequest, which is what reports upload progress.
export function upload(
  share: string,
  path: string,
  file: File,
  onProgress: (sent: number) => void,
): { done: Promise<Entry>; abort: () => void } {
  const xhr = new XMLHttpRequest()
  const done = new Promise<Entry>((resolve, reject) => {
    xhr.open('PUT', url(`/share/${encodeURIComponent(share)}/file`, { path }))
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')
    xhr.upload.onprogress = (e) => onProgress(e.loaded)
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(JSON.parse(xhr.responseText) as Entry)
        return
      }
      let message = xhr.statusText
      try {
        const parsed = JSON.parse(xhr.responseText)
        if (typeof parsed === 'string' && parsed) message = parsed
      } catch {
        // The status text stands.
      }
      reject(new FilesError(message, xhr.status))
    }
    xhr.onerror = () => reject(new FilesError('The server cannot be reached.', 0))
    xhr.onabort = () => reject(new FilesError('The upload was cancelled.', 0))
    xhr.send(file)
  })
  return { done, abort: () => xhr.abort() }
}
