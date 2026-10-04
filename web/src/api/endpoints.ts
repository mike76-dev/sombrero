import { request } from './client'
import type {
  Account,
  AccessRights,
  AppKeyResponse,
  BackupResponse,
  Connection,
  ConnectStatusResponse,
  DefragmentResponse,
  FragmentationResponse,
  ImportProbeResponse,
  ImportRequest,
  ImportSortRequest,
  ImportSortResponse,
  ImportStatusResponse,
  ImportSummary,
  IsBannedResponse,
  OrphansResponse,
  ProbeResponse,
  PublicDir,
  ServerSettings,
  ServerStats,
  RestoreResponse,
  ShareSettings,
  Share,
  UnpinOrphansResponse,
  VersionResponse,
  Workgroup,
  WorkgroupResponse,
} from './types'

// Bans

export const getBanStatus = (host: string) =>
  request<IsBannedResponse>(`/ban/${encodeURIComponent(host)}`)

export const banHost = (host: string, reason: string) =>
  request(`/ban/${encodeURIComponent(host)}`, { method: 'PUT', query: { reason } })

export const unbanHost = (host: string) =>
  request(`/ban/${encodeURIComponent(host)}`, { method: 'DELETE' })

export const clearBans = () => request('/bans', { method: 'DELETE' })

// Accounts

export const getAccountById = (id: number) =>
  request<Account>('/account', { query: { id } })

export const addAccount = (username: string, password: string, workgroup: string) =>
  request('/account', { method: 'POST', body: { username, password, workgroup } })

export const removeAccount = (username: string, workgroup: string) =>
  request('/account', { method: 'DELETE', query: { username, workgroup } })

export const listAccounts = (workgroup: string) =>
  request<Account[] | null>('/accounts', { query: { workgroup } })

export const removeAccounts = (workgroup: string) =>
  request('/accounts', { method: 'DELETE', query: { workgroup } })

export const getAccountShares = (username: string, workgroup: string) =>
  request<Share[] | null>('/account/shares', { query: { username, workgroup } })

export const clearAccountPolicies = (username: string, workgroup: string) =>
  request('/account/policy', { method: 'DELETE', query: { username, workgroup } })

// Shares

export const registerShare = (share: Share) =>
  request('/share', { method: 'POST', body: share })

export const listShares = () => request<Share[] | null>('/shares')

export const getShare = (name: string) =>
  request<Share>(`/share/${encodeURIComponent(name)}`)

export const updateShare = (name: string, settings: ShareSettings) =>
  request(`/share/${encodeURIComponent(name)}`, { method: 'PUT', body: settings })

export const removeShare = (name: string) =>
  request(`/share/${encodeURIComponent(name)}`, { method: 'DELETE' })

export const getShareAccounts = (name: string) =>
  request<AccessRights[] | null>(`/share/${encodeURIComponent(name)}/accounts`)

// Orphaned slabs (indexd shares only)

export const scanOrphans = (share: string) =>
  request<OrphansResponse>(`/share/${encodeURIComponent(share)}/orphans`)

export const unpinOrphans = (share: string) =>
  request<UnpinOrphansResponse>(`/share/${encodeURIComponent(share)}/orphans`, {
    method: 'DELETE',
  })

// Fragmentation (indexd shares only)

export const checkFragmentation = (share: string) =>
  request<FragmentationResponse>(`/share/${encodeURIComponent(share)}/fragmentation`)

export const defragment = (share: string) =>
  request<DefragmentResponse>(`/share/${encodeURIComponent(share)}/fragmentation`, {
    method: 'POST',
  })

// Access policies

export const getPolicy = (share: string, username: string, workgroup: string) =>
  request<AccessRights>(`/share/${encodeURIComponent(share)}/policy`, {
    query: { username, workgroup },
  })

export const setPolicy = (
  share: string,
  username: string,
  workgroup: string,
  rights: { read: boolean; write: boolean; delete: boolean; execute: boolean },
) =>
  request(`/share/${encodeURIComponent(share)}/policy`, {
    method: 'PUT',
    query: { username, workgroup, ...rights },
  })

export const removePolicy = (share: string, username: string, workgroup: string) =>
  request(`/share/${encodeURIComponent(share)}/policy`, {
    method: 'DELETE',
    query: { username, workgroup },
  })

// Workgroups

export const createWorkgroup = (name?: string) =>
  request<WorkgroupResponse>('/workgroup', {
    method: 'POST',
    body: name ? { name } : {},
  })

export const listWorkgroups = () => request<Workgroup[] | null>('/workgroups')

export const getWorkgroup = (id: string) =>
  request<Workgroup>(`/workgroup/${encodeURIComponent(id)}`)

export const updateWorkgroup = (id: string, publicDirs: PublicDir[]) =>
  request(`/workgroup/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: { publicDirs },
  })

export const removeWorkgroup = (id: string) =>
  request(`/workgroup/${encodeURIComponent(id)}`, { method: 'DELETE' })

// Stats

export const getStats = () => request<ServerStats>('/stats')

export const getSettings = () => request<ServerSettings>('/settings')

export const getVersion = () => request<VersionResponse>('/version')

export const probeServer = (serverName: string) =>
  request<ProbeResponse>('/probe', { method: 'POST', body: { serverName } })

// Connections

export const requestConnection = (workgroup: string, share: string) =>
  request<ConnectStatusResponse>(
    `/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
    { method: 'POST' },
  )

// An indexd share is connected with a saved app key, with the key another
// workgroup holds for the same indexer, or with the workgroup's own where it has
// one; a renterd share needs none of them.
export const connect = (
  workgroup: string,
  share: string,
  from?: { appKey?: string; fromWorkgroup?: string },
) =>
  request<ConnectStatusResponse>(
    `/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
    { method: 'PUT', body: from?.appKey || from?.fromWorkgroup ? from : undefined },
  )

export const connectStatus = (workgroup: string, share: string) =>
  request<ConnectStatusResponse>(
    `/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
  )

export const disconnect = (workgroup: string, share: string) =>
  request(`/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`, {
    method: 'DELETE',
  })

// The app key a connection was made with, for whoever did not keep it.
export const connectionKey = (workgroup: string, share: string) =>
  request<AppKeyResponse>(
    `/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}/key`,
  )

export const listConnections = () => request<Connection[] | null>('/connections')

// Backups

export const backupStatus = () => request<BackupResponse>('/backup')

// A catalog is restored by sending the file as it is; force applies it over a
// connection the server has already.
export const restoreCatalog = (file: Blob, force: boolean) =>
  request<RestoreResponse>('/restore', {
    method: 'POST',
    query: { force: force || undefined },
    rawBody: file,
  })

// Imports

export const startImport = (workgroup: string, share: string, body: ImportRequest) =>
  request<ImportStatusResponse>(
    `/import/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
    { method: 'POST', body },
  )

export const probeImportSource = (workgroup: string, share: string, body: ImportRequest) =>
  request<ImportProbeResponse>(
    `/import/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}/probe`,
    { method: 'POST', body },
  )

export const sortLostAndFound = (workgroup: string, share: string, body: ImportSortRequest) =>
  request<ImportSortResponse>(
    `/import/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}/sort`,
    { method: 'POST', body },
  )

export const listImports = () => request<ImportSummary[] | null>('/imports')

export const importStatus = (workgroup: string, share: string) =>
  request<ImportStatusResponse>(
    `/import/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
  )

export const cancelImport = (workgroup: string, share: string) =>
  request<ImportStatusResponse>(
    `/import/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
    { method: 'DELETE' },
  )
