// Types mirroring the Go structs serialized by the API (see api/api.go
// and the stores package).

export interface Account {
  id: number
  username: string
  password: string
  workgroup: string
}

export interface Share {
  name: string
  type: string
  serverName: string
  password?: string
  bucket?: string
  remark?: string
  createdAt?: string
  dataShards?: number
  parityShards?: number
}

export interface PublicDir {
  path: string
  readOnly?: boolean
  caseSensitive?: boolean
}

export interface Workgroup {
  id: number
  uuid: string
  name?: string
  publicDirs?: PublicDir[]
}

// stores.AccessRights has no json tags, so the fields serialize
// with their Go names.
export interface AccessRights {
  ShareName: string
  AccountID: number
  ReadAccess: boolean
  WriteAccess: boolean
  DeleteAccess: boolean
  ExecuteAccess: boolean
}

export interface ServerStats {
  start: string
  fOpens: number
  sOpens: number
  pwErrors: number
  permErrors: number
  bytesSent: number
  bytesRcvd: number
}

export interface IsBannedResponse {
  banned: boolean
  reason: string
}

export interface WorkgroupResponse {
  uuid: string
  name?: string
}

export interface ConnectRequestResponse {
  url: string
}

export interface ConnectResponse {
  appKey: string
}
