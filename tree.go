package main

import (
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"time"

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
		access, exists = sh.fileSecurity[ss.workgroup+"/"+ss.userName]
		if !exists {
			return nil, errAccessDenied
		}
		sh.mu.Lock()
		sh.currentUses++
		sh.mu.Unlock()
	}

	var id [4]byte
	frand.Read(id[:])

	tc := &treeConnect{
		treeID:         binary.LittleEndian.Uint32(id[:]),
		session:        ss,
		share:          sh,
		creationTime:   time.Now(),
		maximalAccess:  access,
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
	defer ss.mu.Unlock()

	tc, ok := ss.treeConnectTable[tid]
	if !ok {
		return errNoTreeConnect
	}

	for fid, op := range ss.openTable {
		if op.treeConnect == tc {
			delete(ss.openTable, fid)
			ss.connection.server.mu.Lock()
			delete(ss.connection.server.globalOpenTable, fid)
			ss.connection.server.mu.Unlock()
			op.cancel()
		}
	}

	tc.share.mu.Lock()
	tc.share.currentUses--
	tc.share.mu.Unlock()

	delete(ss.treeConnectTable, tid)

	return nil
}
