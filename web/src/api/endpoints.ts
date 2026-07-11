import { request } from './client'
import type {
  Account,
  AccessRights,
  ConnectRequestResponse,
  ConnectResponse,
  IsBannedResponse,
  Share,
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

export const removeShare = (name: string) =>
  request(`/share/${encodeURIComponent(name)}`, { method: 'DELETE' })

export const getShareAccounts = (name: string) =>
  request<AccessRights[] | null>(`/share/${encodeURIComponent(name)}/accounts`)

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

export const updateWorkgroup = (id: string, publicDirs: string[], caseSensitive: boolean) =>
  request(`/workgroup/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: { publicDirs, caseSensitive },
  })

export const removeWorkgroup = (id: string) =>
  request(`/workgroup/${encodeURIComponent(id)}`, { method: 'DELETE' })

// Connections

export const requestConnection = (workgroup: string, share: string) =>
  request<ConnectRequestResponse>(
    `/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
    { method: 'POST' },
  )

export const connect = (workgroup: string, share: string, appKey?: string) =>
  request<ConnectResponse | void>(
    `/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`,
    { method: 'PUT', body: appKey ? { appKey } : undefined },
  )

export const disconnect = (workgroup: string, share: string) =>
  request(`/connect/${encodeURIComponent(workgroup)}/${encodeURIComponent(share)}`, {
    method: 'DELETE',
  })
