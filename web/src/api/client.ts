// Minimal fetch wrapper for the Sombrero API.
//
// The API is protected by HTTP basic auth where only the password is
// checked, so the username part is left empty. Error responses carry a
// JSON-encoded message string.

export interface ApiSettings {
  apiBase: string
  password: string
}

const SETTINGS_KEY = 'sombrero.settings'

export const defaultSettings: ApiSettings = {
  apiBase: '/api',
  password: '',
}

export function loadSettings(): ApiSettings {
  try {
    const raw = localStorage.getItem(SETTINGS_KEY)
    if (raw) return { ...defaultSettings, ...JSON.parse(raw) }
  } catch {
    // fall through to defaults
  }
  return { ...defaultSettings }
}

export function saveSettings(s: ApiSettings) {
  localStorage.setItem(SETTINGS_KEY, JSON.stringify(s))
}

export class ApiError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

export type Query = Record<string, string | number | boolean | undefined>

interface RequestOptions {
  method?: string
  query?: Query
  body?: unknown
}

export async function request<T = void>(path: string, opts: RequestOptions = {}): Promise<T> {
  const settings = loadSettings()
  const base = settings.apiBase.replace(/\/+$/, '')

  let url = base + path
  if (opts.query) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(opts.query)) {
      if (value !== undefined && value !== '') params.set(key, String(value))
    }
    const qs = params.toString()
    if (qs) url += '?' + qs
  }

  const headers: Record<string, string> = {
    Authorization: 'Basic ' + btoa(':' + settings.password),
  }
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'

  let res: Response
  try {
    res = await fetch(url, {
      method: opts.method ?? 'GET',
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    })
  } catch {
    throw new ApiError('cannot reach the API server — check the address in Settings', 0)
  }

  if (!res.ok) {
    let message = res.statusText
    try {
      const parsed = await res.json()
      if (typeof parsed === 'string' && parsed) message = parsed
    } catch {
      // keep the status text
    }
    if (res.status === 401) message = 'unauthorized — check the API password in Settings'
    throw new ApiError(message, res.status)
  }

  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}
