package main

import (
	"strings"

	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/utils"
)

// This file keeps the two indexes that answer for the global open table without walking it:
// the opens on a file, which every create consults before it may grant an oplock or a lease,
// and the open a create GUID already made, which every durable create consults for a replay.
// Both grow and shrink with the global open table, and both are guarded by openIndexMu.
//
// The lock of an open is taken inside openIndexMu wherever an index entry and the fields of
// the open it stands for have to change together: a rename moves the open between buckets in
// the same breath as it changes the path, so that nothing ever finds the open under a name it
// no longer carries, or misses it under the name it does. Nothing takes openIndexMu while
// holding the lock of an open.

// fileKey names a file the way the opens on it are looked up: the share it lives on, and its
// path within the share.
type fileKey struct {
	share *share
	path  string
}

// replayKey names the create that made an open: the client that sent it, and the create GUID
// that client chose for it.
type replayKey struct {
	clientGuid [16]byte
	createGuid [16]byte
}

// fileKeyLocked is the key the open is filed under. The lock of the open must be held.
func (op *open) fileKeyLocked() fileKey {
	var sh *share
	if op.treeConnect != nil {
		sh = op.treeConnect.share
	}

	return fileKey{share: sh, path: op.pathName}
}

// indexOpen puts the open into the index of opens by file, under the share and the path it
// carries. It is called wherever the open joins the global open table.
func (s *server) indexOpen(op *open) {
	s.openIndexMu.Lock()
	defer s.openIndexMu.Unlock()

	op.mu.Lock()
	key := op.fileKeyLocked()
	dfid := op.durableFileID
	op.mu.Unlock()

	bucket, found := s.opensByFile[key]
	if !found {
		bucket = make(map[uint64]*open)
		s.opensByFile[key] = bucket
	}
	bucket[dfid] = op
}

// unindexOpen takes the open out of the index of opens by file. It is called wherever the
// open leaves the global open table, and reads the share and the path under the same locks
// that a move changes them under, so that it always clears the bucket the open is actually
// in.
func (s *server) unindexOpen(op *open) {
	s.openIndexMu.Lock()
	defer s.openIndexMu.Unlock()

	op.mu.Lock()
	key := op.fileKeyLocked()
	dfid := op.durableFileID
	op.mu.Unlock()

	s.dropFromBucket(key, dfid)
}

// dropFromBucket removes one open from the bucket of its file, and the bucket itself once it
// stands empty, so that the index does not grow with every path ever opened. openIndexMu must
// be held.
func (s *server) dropFromBucket(key fileKey, dfid uint64) {
	bucket, found := s.opensByFile[key]
	if !found {
		return
	}

	delete(bucket, dfid)
	if len(bucket) == 0 {
		delete(s.opensByFile, key)
	}
}

// moveOpen points the open at a new path, moving it between the buckets of the index in the
// same breath. Holding the index lock across both is what keeps every reader to one view: an
// open is always found under exactly the name it carries.
func (s *server) moveOpen(op *open, newPath string) {
	s.openIndexMu.Lock()
	defer s.openIndexMu.Unlock()

	s.moveOpenLocked(op, newPath)
}

// moveOpenLocked is moveOpen with openIndexMu already held, so that the children of a renamed
// directory can move in one breath with each other.
func (s *server) moveOpenLocked(op *open, newPath string) {
	op.mu.Lock()
	defer op.mu.Unlock()

	oldKey := op.fileKeyLocked()
	dfid := op.durableFileID

	op.pathName = newPath
	op.fileName = utils.TrimPath(newPath)
	newKey := op.fileKeyLocked()

	// An open that is not where the index says its file is has already been closed, by a race
	// the moment before: its name changes and nothing else, so that the move cannot resurrect
	// it.
	bucket, found := s.opensByFile[oldKey]
	if !found {
		return
	}
	if _, present := bucket[dfid]; !present {
		return
	}

	s.dropFromBucket(oldKey, dfid)
	target, found := s.opensByFile[newKey]
	if !found {
		target = make(map[uint64]*open)
		s.opensByFile[newKey] = target
	}
	target[dfid] = op
}

// moveOpensUnder points every open beneath a renamed directory at its path under the new name,
// and returns the opens moved. The whole tree moves under one hold of the index lock, so that
// a create racing the rename finds every child under exactly one of its names.
func (s *server) moveOpensUnder(sh *share, oldPath, newPath string) []*open {
	prefix := oldPath + "/"

	s.openIndexMu.Lock()
	defer s.openIndexMu.Unlock()

	// The moves reshape the map, so the children are all collected before the first is made.
	var opens []*open
	var paths []string
	for key, bucket := range s.opensByFile {
		if key.share != sh || !strings.HasPrefix(key.path, prefix) {
			continue
		}
		for _, op := range bucket {
			opens = append(opens, op)
			paths = append(paths, newPath+"/"+key.path[len(prefix):])
		}
	}

	for i, op := range opens {
		s.moveOpenLocked(op, paths[i])
	}

	return opens
}

// markReplayEligible records that the create which made this open may still be replayed, and
// files the open under the GUID that create carried. Only a file on a disk share that was
// actually opened for something is worth replaying; a directory or a handle with no access to
// speak of has nothing a second attempt could lose.
func (s *server) markReplayEligible(op *open, tc *treeConnect) {
	if tc.share.name == "ipc$" {
		return
	}

	const useful = smb2.FILE_READ_DATA | smb2.FILE_EXECUTE | smb2.FILE_WRITE_DATA |
		smb2.FILE_APPEND_DATA | smb2.DELETE

	isDir := op.file.isDirectory()
	op.mu.Lock()
	eligible := !isDir && op.grantedAccess&useful > 0
	op.mu.Unlock()

	if !eligible {
		return
	}

	s.openIndexMu.Lock()
	op.mu.Lock()
	op.isReplayEligible = true
	s.replayableOpens[replayKey{clientGuid: op.clientGuid, createGuid: op.createGuid}] = op
	op.mu.Unlock()
	s.openIndexMu.Unlock()
}

// clearReplayEligible records that the handle has been used, which is the point at which the
// create that made it can no longer be replayed: a second copy of that create would now be
// asking for something the client has already moved on from.
//
// It runs on every command that carries a FileId, so the index is only touched when there is
// something to clear: the flag is read first, on its own, and the eligible case — once per
// open at most — pays for the second look under both locks.
func (s *server) clearReplayEligible(op *open) {
	op.mu.Lock()
	eligible := op.isReplayEligible
	op.mu.Unlock()

	if !eligible {
		return
	}

	s.openIndexMu.Lock()
	op.mu.Lock()
	if op.isReplayEligible {
		op.isReplayEligible = false
		key := replayKey{clientGuid: op.clientGuid, createGuid: op.createGuid}
		if s.replayableOpens[key] == op {
			delete(s.replayableOpens, key)
		}
	}
	op.mu.Unlock()
	s.openIndexMu.Unlock()
}

// findReplayableOpen returns the open that a create bearing this GUID already made, if the
// client may still replay that create.
func (s *server) findReplayableOpen(clientGuid, createGuid [16]byte) *open {
	s.openIndexMu.Lock()
	op := s.replayableOpens[replayKey{clientGuid: clientGuid, createGuid: createGuid}]
	s.openIndexMu.Unlock()

	if op == nil {
		return nil
	}

	// The entry is a pointer into a table that changes under its own lock, so what it points
	// at is asked the same questions the old walk of the global table asked.
	op.mu.Lock()
	match := op.isReplayEligible && op.createGuid == createGuid && op.clientGuid == clientGuid
	op.mu.Unlock()
	if !match {
		return nil
	}

	return op
}

// moveOpensOnFile points every open on the file at its new name, and returns the ones it moved.
func (s *server) moveOpensOnFile(sh *share, path, newName string) []*open {
	moved := s.opensOn(sh, path, nil)
	for _, op := range moved {
		s.moveOpen(op, newName)
	}

	return moved
}

// opensOn collects the opens of a file, other than the one given. The bucket is copied before
// the opens in it are examined, so that the lock of the index and the lock of an open are
// never held at the same time here; each candidate then answers for itself, the way the walk
// of the global table used to make it.
func (s *server) opensOn(sh *share, path string, except *open) []*open {
	s.openIndexMu.Lock()
	bucket := s.opensByFile[fileKey{share: sh, path: path}]
	candidates := make([]*open, 0, len(bucket))
	for _, op := range bucket {
		if op != except {
			candidates = append(candidates, op)
		}
	}
	s.openIndexMu.Unlock()

	var opens []*open
	for _, op := range candidates {
		op.mu.Lock()
		match := op.treeConnect.share == sh && op.pathName == path
		op.mu.Unlock()
		if match {
			opens = append(opens, op)
		}
	}

	return opens
}
