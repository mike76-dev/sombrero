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
	creationTime  time.Time
	maximalAccess uint32

	// Resolved at connect time from the share; for indexd these are per-workgroup.
	client        client.Client
	maxUploadSize uint64
	createdAt     time.Time
	volumeID      uint64

	// When a request to create a file comes in, the client assumes that the file exists from then on.
	// However, it's not possible to upload empty files to the Sia network, so we have to work around that.
	// The workaround is to remember the names of the files that were created (for the lifetime
	// of the TreeConnect or until the files are deleted).
	persistedOpens map[string]*open
	mu             sync.Mutex
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
			s, err := c.server.store.GetShare(name)
			if err != nil {
				return nil, errNoShare
			}
			if err := c.server.RegisterShare(s); err != nil {
				return nil, err
			} else {
				sh, exists = c.server.shareList[name]
				if !exists {
					return nil, errNoShare
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

		access, exists = sh.fileSecurity[ss.workgroup+"/"+ss.userName]
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

	tc := &treeConnect{
		treeID:         binary.LittleEndian.Uint32(id[:]),
		session:        ss,
		share:          sh,
		creationTime:   time.Now(),
		maximalAccess:  access,
		client:         cl,
		maxUploadSize:  maxUploadSize,
		createdAt:      createdAt,
		volumeID:       volumeID,
		persistedOpens: make(map[string]*open),
	}

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
			op.cancel()
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
	// order.
	for _, op := range closed {
		op.releaseOplock()
	}

	return nil
}
