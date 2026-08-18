package main

import (
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/smb2"
	"lukechampine.com/frand"
)

var (
	errNoShare       = errors.New("no share name provided")
	errNoTreeConnect = errors.New("tree already disconnected")
	errAccessDenied  = errors.New("access denied")
	errTooManyUses   = errors.New("too many users connected")
)

// treeConnect represents a TreeConnect object.
type treeConnect struct {
	treeID        uint32
	session       *session
	share         *share
	openCount     uint64
	maximalAccess uint32

	// Resolved at connect time from the share; for indexd these are per-workgroup.
	client        client.Client
	maxUploadSize uint64
	createdAt     time.Time
	volumeID      uint64

	mu sync.Mutex
}

// The files a client has created but not yet uploaded are kept on the share, under the workgroup
// whose namespace they are in. The tree connect is where the two are known together, so the asking
// is done through it: a client that loses its connection and comes back finds the file it made,
// which it would not if the table went with the tree connect it was made on.

// persistedFile returns the state of such a file, if the share is holding one under this name.
func (tc *treeConnect) persistedFile(path string) (*fileState, bool) {
	tc.share.mu.Lock()
	defer tc.share.mu.Unlock()

	fs, found := tc.share.persisted[persistedKey{tc.session.workgroup, path}]

	return fs, found
}

// persistFile takes the file on, so that everything on the share sees it from now on.
func (tc *treeConnect) persistFile(path string, fs *fileState) {
	tc.share.mu.Lock()
	defer tc.share.mu.Unlock()
	tc.share.ensurePersisted()

	tc.share.persisted[persistedKey{tc.session.workgroup, path}] = fs
}

// forgetPersistedFile drops the file, which is what happens once it has been uploaded or deleted:
// from then on the store answers for it.
func (tc *treeConnect) forgetPersistedFile(path string) {
	tc.share.mu.Lock()
	defer tc.share.mu.Unlock()

	delete(tc.share.persisted, persistedKey{tc.session.workgroup, path})
}

// movePersistedFile carries the file over to another name.
func (tc *treeConnect) movePersistedFile(from, to string, fs *fileState) {
	tc.share.mu.Lock()
	defer tc.share.mu.Unlock()
	tc.share.ensurePersisted()

	delete(tc.share.persisted, persistedKey{tc.session.workgroup, from})
	tc.share.persisted[persistedKey{tc.session.workgroup, to}] = fs
}

// movePersistedTree carries everything under a directory over with it, so that a renamed directory
// takes the files that were made in it and never uploaded. The inserts wait until the walk is over:
// a key added while the map is ranged may or may not be visited.
func (tc *treeConnect) movePersistedTree(from, to string) {
	tc.share.mu.Lock()
	defer tc.share.mu.Unlock()
	tc.share.ensurePersisted()

	prefix := from + "/"
	moved := make(map[persistedKey]*fileState)
	for key, fs := range tc.share.persisted {
		if key.workgroup == tc.session.workgroup && strings.HasPrefix(key.path, prefix) {
			moved[persistedKey{key.workgroup, to + "/" + key.path[len(prefix):]}] = fs
			delete(tc.share.persisted, key)
		}
	}
	for key, fs := range moved {
		tc.share.persisted[key] = fs
	}
}

// persistedObjects turns the files the tree connect is holding that have been created but not yet
// uploaded into the entries a listing would have carried, so that a client sees them beside what
// the backend already knows about. Only the paths the caller asks for are taken, and only the files
// the backend has nothing for: the table also holds the state of every file that is merely open,
// which the backend lists itself.
func (tc *treeConnect) persistedObjects(want func(path string) bool) []client.ObjectInfo {
	tc.share.mu.Lock()
	paths := make([]string, 0, len(tc.share.persisted))
	files := make([]*fileState, 0, len(tc.share.persisted))
	for key, fs := range tc.share.persisted {
		if key.workgroup == tc.session.workgroup && want(key.path) {
			paths = append(paths, key.path)
			files = append(files, fs)
		}
	}
	tc.share.mu.Unlock()

	ois := make([]client.ObjectInfo, 0, len(files))
	for i, fs := range files {
		if fs.isStored() {
			continue
		}

		size, _, _, modified, _ := fs.stat()

		ois = append(ois, client.ObjectInfo{
			Key:        "/" + paths[i],
			CreatedAt:  modified,
			ModifiedAt: modified,
			Size:       size,
		})
	}

	return ois
}

// extractShareName extracts the share name from the provided string of the format \\SERVER\SHARE.
func extractShareName(path string) string {
	var ok bool
	path, ok = strings.CutPrefix(path, "\\\\")
	if !ok {
		return ""
	}

	pos := strings.Index(path, "\\")
	if pos == -1 {
		return ""
	}

	if pos == len(path)-1 {
		return ""
	}

	return strings.ToLower(path[pos+1:])
}

// newTreeConnectState returns a TreeConnect object with its tables in place, but with no object
// store behind it yet. It is the half of newTreeConnect the tests share; the same reasoning
// applies as for newServerState.
func newTreeConnectState(tid uint32, ss *session, sh *share, access uint32) *treeConnect {
	return &treeConnect{
		treeID:        tid,
		session:       ss,
		share:         sh,
		maximalAccess: access,
	}
}

// newTreeConnect creates a new TreeConnect object and attaches it to the session.
func (c *connection) newTreeConnect(ss *session, path string) (*treeConnect, error) {
	name := extractShareName(path)
	if name == "" {
		return nil, errNoShare
	}

	var sh *share
	var access uint32
	if name == "ipc$" { // A special case of the IPC (Inter-Protocol Communication) share
		sh = &share{
			name:            name,
			shareType:       smb2.SHARE_TYPE_PIPE,
			remark:          "IPC service",
			connectSecurity: map[string]struct{}{},
			fileSecurity:    make(map[string]uint32),
			persisted:       make(map[persistedKey]*fileState),
		}
		sh.connectSecurity[ss.workgroup+"/"+ss.userName] = struct{}{}
		access = smb2.FILE_READ_DATA |
			smb2.FILE_READ_EA |
			smb2.FILE_EXECUTE |
			smb2.FILE_READ_ATTRIBUTES |
			smb2.DELETE |
			smb2.READ_CONTROL |
			smb2.WRITE_DAC |
			smb2.WRITE_OWNER |
			smb2.SYNCHRONIZE
		sh.fileSecurity[ss.workgroup+"/"+ss.userName] = access
	} else {
		var exists bool
		c.server.mu.Lock()
		sh, exists = c.server.shareList[name]
		c.server.mu.Unlock()
		if !exists {
			// A store with no such share answers with a zero-valued one rather than an error.
			s, err := c.server.store.GetShare(name)
			if err != nil || s.Name != name {
				return nil, errShareNotFound
			}
			if err := c.server.RegisterShare(s); err != nil {
				return nil, err
			} else {
				c.server.mu.Lock()
				sh, exists = c.server.shareList[name]
				c.server.mu.Unlock()
				if !exists {
					return nil, errShareNotFound
				}
			}
		}

		if smb2.Is3X(c.negotiateDialect) && sh.encryptData && c.clientCapabilities&smb2.GLOBAL_CAP_ENCRYPTION == 0 {
			return nil, errAccessDenied
		}

		sh.mu.Lock()
		if sh.currentUses >= maxShareUses {
			sh.mu.Unlock()
			return nil, errTooManyUses
		}
		sh.mu.Unlock()

		// For indexd, lazily restore a connection from the DB if not yet initialized.
		if sh.backend == "indexd" {
			sh.mu.Lock()
			_, hasConn := sh.indexdConns[ss.workgroup]
			sh.mu.Unlock()
			if !hasConn {
				if u, err := uuid.Parse(ss.workgroup); err == nil {
					if wg, err := c.server.store.FindWorkgroup(u); err == nil && wg.ID != 0 {
						if fullShare, err := c.server.store.GetShare(name); err == nil {
							if connected, appKey, err := c.server.store.IsConnected(wg, fullShare); err == nil && connected {
								if err := c.server.AddConnection(wg, fullShare, appKey); err != nil {
									log.Printf("lazy indexd init %q/%q: %v", name, ss.workgroup, err)
								}
							}
						}
					}
				}
			}
		}

		access, exists = sh.fileAccess(ss.workgroup, ss.userName)
		if !exists {
			return nil, errAccessDenied
		}
		sh.mu.Lock()
		sh.currentUses++
		sh.mu.Unlock()
	}

	// Resolve the per-workgroup connection info for this tree connect.
	var (
		cl            client.Client
		maxUploadSize uint64
		createdAt     time.Time
		volumeID      uint64
	)
	if sh.backend == "indexd" {
		sh.mu.Lock()
		conn, ok := sh.indexdConns[ss.workgroup]
		if ok {
			cl = conn.client
			maxUploadSize = conn.maxUploadSize
			createdAt = conn.createdAt
			volumeID = conn.volumeID
		}
		sh.mu.Unlock()
		if cl == nil {
			sh.mu.Lock()
			sh.currentUses--
			sh.mu.Unlock()
			return nil, errShareUnavailable
		}
	} else {
		cl = sh.client
		maxUploadSize = sh.maxUploadSize
		createdAt = sh.createdAt
		volumeID = sh.volumeID
	}

	var id [4]byte
	frand.Read(id[:])

	tc := newTreeConnectState(binary.LittleEndian.Uint32(id[:]), ss, sh, access)
	tc.client = cl
	tc.maxUploadSize = maxUploadSize
	tc.createdAt = createdAt
	tc.volumeID = volumeID

	ss.mu.Lock()
	ss.treeConnectTable[tc.treeID] = tc
	ss.mu.Unlock()

	return tc, nil
}

// closeTreeConnect destroys the TreeConnect object by removing any references to it.
func (ss *session) closeTreeConnect(tid uint32) error {
	ss.mu.Lock()

	tc, ok := ss.treeConnectTable[tid]
	if !ok {
		ss.mu.Unlock()
		return errNoTreeConnect
	}

	var closed []*open
	for fid, op := range ss.openTable {
		if op.treeConnect == tc {
			delete(ss.openTable, fid)
			// The global table is keyed by the durable ID, not by the one the open table
			// of the session uses.
			ss.connection.server.mu.Lock()
			delete(ss.connection.server.globalOpenTable, op.durableFileID)
			ss.connection.server.mu.Unlock()
			closed = append(closed, op)
		}
	}

	tc.share.mu.Lock()
	tc.share.currentUses--
	tc.share.mu.Unlock()

	delete(ss.treeConnectTable, tid)
	ss.mu.Unlock()

	// The oplocks are given up outside the lock of the session: releasing one takes the lock
	// of the open, and picking a channel to break an oplock over takes the two in the other
	// order. An open that has left the global table leaves the indexes with it, and the
	// create that made it can no longer be replayed.
	//
	// An upload nobody is going to finish is called off here, which is also what puts the file
	// back to the size the store holds it at: left at the size the writer reached, the file
	// would read as the object the store still has cut off at a length it never had. The context
	// of the open is cancelled last, so that the abort still has one to travel on.
	for _, op := range closed {
		op.releaseCaching()
		ss.connection.server.clearReplayEligible(op)
		ss.connection.server.unindexOpen(op)
		op.cancelUpload()
		op.releaseFile()

		// The directory watches on the open are answered here rather than left outstanding. A
		// change notify is a request the client is still waiting on, and a tree disconnect is
		// where it ends: what a client counts as an unfinished directory search on the
		// connection is exactly this.
		ss.connection.completeWatches(ss, op.id())

		op.cancel()
	}

	return nil
}
