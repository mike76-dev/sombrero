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

export interface BacklogStats {
  buffered: number
  limit: number
  onDisk: number
}

export interface ServerStats {
  start: string
  fOpens: number
  sOpens: number
  pwErrors: number
  permErrors: number
  bytesSent: number
  bytesRcvd: number
  backlog?: BacklogStats
}

export interface VersionResponse {
  version: string
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
// 'connected', and never again. reusable says that an indexd share can be
// connected from the key the workgroup has for the same indexer, without approval,
// and keyFrom names the other workgroups whose key could be shared instead.
export interface ConnectStatusResponse {
  state: ConnectState
  started?: string
  since?: string
  url?: string
  appKey?: string
  reusable?: boolean
  keyFrom?: AppKeyHolder[]
  error?: string
}

// A workgroup whose app key for an indexer another workgroup can be connected
// with, which puts both on one indexer account and one quota.
export interface AppKeyHolder {
  workgroup: string
  name?: string
}

export type ImportState = 'idle' | 'counting' | 'running' | 'done' | 'failed' | 'cancelled'

// What a source holds, from a look at it before anything is taken over. For an
// indexd account, objects is what it has pinned and tagged how many of the ones
// looked at say which files they hold.
export interface ImportProbeResponse {
  source: string
  objects?: number
  looked?: number
  tagged?: number

  // more says the account holds at least that many: the look stops a few pages
  // into the object log rather than counting all of a long one.
  more?: boolean
  warning?: string
}

// What an import of another server's data is to read, and who the files it
// brings over belong to here.
export interface ImportRequest {
  source: 'renterd' | 'indexd'
  address: string
  username: string
  password?: string
  bucket?: string
  appKey?: string
  prefix?: string
  copy?: boolean
}

// One import the server has in hand: which workgroup is importing into which
// share, and how it is getting on. It is what a page that does not know where to
// look goes by.
export interface ImportSummary {
  workgroup: string
  share: string
  status: ImportStatusResponse
}

// What to sort out of the objects whose names are gone: whose the recovered
// files are, where to look, and where the round before this one stopped.
export interface ImportSortRequest {
  username: string
  prefix?: string
  after?: string
  limit?: number
}

// What one round of sorting came to. last is where it stopped and more says
// there is further to go.
export interface ImportSortResponse {
  objects: number
  recovered: number
  skipped: number
  bytes: number
  leftover: number
  last?: string
  more?: boolean
}

// The progress of one import. path is the file it has in hand, with fileBytes of
// its fileSize moved so far; pinned is what it took over where it lies, copied
// what it had to move, and waits how often it had to wait for room in the share's
// staging area.
export interface ImportStatusResponse {
  state: ImportState
  source?: string
  started?: string
  since?: string
  path?: string
  fileBytes?: number
  fileSize?: number
  directories: number
  pinned: number
  copied: number
  skipped: number
  failed: number
  bytes: number
  waits: number

  // total is how many files the source turned out to hold, counted before any
  // of them were moved, and done how many of them are behind us.
  total?: number
  done: number

  // refused counts the files the indexer would not take over, which were copied
  // instead, and refusal is what it said about the first of them. They are not
  // failures.
  refused: number
  refusal?: string
  failures?: string[]
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
