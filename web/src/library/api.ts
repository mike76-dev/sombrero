// The file API, as the library calls it. The calls carry the session cookie
// the login set, so there is nothing to configure: the API is served under
// /files by the same server as the page.

const base = '/files'

export interface Identity {
  username: string
  workgroup: { uuid: string; name?: string }
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

export function list(share: string, path: string): Promise<Listing> {
  return call<Listing>(`/share/${encodeURIComponent(share)}/list`, { query: { path } })
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
