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
  // Who the share admits besides the accounts its policies name: the
  // passwordless accounts of a workgroup, and clients presenting no
  // credentials at all. `publicDir` is the one folder the latter may use.
  allowGuest?: boolean
  allowAnonymous?: boolean
  publicDir?: string
}

// ShareSettings is what PUT /share/:name changes. serverName is only changed
// when it is given, since a share has no use for an empty one; bucket is only
// a renterd share's to have.
export interface ShareSettings {
  serverName?: string
  bucket?: string
  remark?: string
  allowGuest?: boolean
  allowAnonymous?: boolean
  publicDir?: string
}

// ServerSettings is how the server itself is configured, as far as the UI needs
// to know. Anonymous access has to be allowed here before any share can offer
// it.
export interface ServerSettings {
  mode: string
  anonymous: boolean
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

// What POST /probe found at the address of a share's backend: whether anything
// is listening there, and what looks off about the address itself.
export interface ProbeResponse {
  address: string
  reachable: boolean
  error?: string
  warning?: string
}

export type ConnectState =
  | 'idle'
  | 'awaiting-approval'
  | 'registering'
  | 'connecting'
  | 'connected'
  | 'failed'

// The progress of one workgroup's connection to one share. appKey is the key a
// first-time registration derived: it is reported once, when the attempt reaches
// 'connected', and never again.
export interface ConnectStatusResponse {
  state: ConnectState
  started?: string
  since?: string
  url?: string
  appKey?: string
  error?: string
}

export interface OrphanedSlab {
  workgroup: string
  key: string
  size: number
  pinnedAt: string
}

export interface OrphansResponse {
  slabs: OrphanedSlab[]
  count: number
  size: number
  // The age, in seconds, a slab had to reach to be reported.
  minAge: number
  // Keyed by the workgroup whose connection could not be scanned.
  errors?: Record<string, string>
}

export interface FragmentedSlab {
  workgroup: string
  key: string
  size: number
  // How far into the slab the pieces reached when it was uploaded. What is
  // left between it and `used` is what deleting and editing punched out.
  filled: number
  used: number
  wasted: number
  pieces: number
  // The dead space as a fraction of the slab size, between 0 and 1.
  fragmentation: number
}

export interface FragmentationResponse {
  slabs: FragmentedSlab[]
  // Every slab of the share, and the dead space in all of them.
  total: number
  wasted: number
  // The listed slabs alone, i.e. those reaching the threshold.
  fragmented: number
  fragmentedWasted: number
  // The dead space a slab had to hold to be listed, as a fraction.
  threshold: number
  // Keyed by the workgroup whose connection could not be checked.
  errors?: Record<string, string>
}

export interface DefragmentResponse {
  // What one round emptied: the slabs whose contents went back into the upload
  // queue, how much that was, and the dead space those slabs held.
  slabs: number
  moved: number
  reclaimed: number
  // Keyed by the workgroup whose connection could not be repacked.
  errors?: Record<string, string>
}

export interface UnpinOrphansResponse {
  unpinned: number
  freed: number
  failed: number
  errors?: Record<string, string>
}
