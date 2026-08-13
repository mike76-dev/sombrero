package main

import (
	"log"
	"strings"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

const (
	// defaultDurableTimeout is how long a durable open is kept for a client that didn't ask
	// for any particular time.
	defaultDurableTimeout = 60 * time.Second

	// maxDurableTimeout caps what a client may ask for. An open waiting to be reclaimed
	// holds on to whatever work has been done on it, an unfinished upload above all, so it
	// is worth keeping for a while; it also holds memory, so not for too long.
	maxDurableTimeout = 5 * time.Minute

	// durableSweepInterval is how often the opens that were never reclaimed are collected.
	durableSweepInterval = 30 * time.Second
)

// grantDurability marks the open as one that outlives the loss of its connection, and
// returns the timeout granted, in milliseconds, to be reported back to the client.
func (op *open) grantDurability(req smb2.DurableHandleRequestV2) uint32 {
	timeout := time.Duration(req.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultDurableTimeout
	}
	if timeout > maxDurableTimeout {
		timeout = maxDurableTimeout
	}

	op.mu.Lock()
	defer op.mu.Unlock()

	op.isDurable = true
	op.durableTimeout = timeout
	op.createGuid = req.CreateGuid

	return uint32(timeout / time.Millisecond)
}

// replayCreate answers a create that carries the create GUID of one the server has already made
// an open for. A client whose answer never came back reissues the same create marked as a
// replay, and gets the open the first attempt made instead of a second one on the same file.
//
// It returns false if this create is not about an open that already exists, in which case it is
// an ordinary create and is carried out as one.
func (c *connection) replayCreate(cr smb2.CreateRequest, ss *session, tc *treeConnect, contexts map[uint32][]byte, lr *smb2.LeaseRequest) (smb2.GenericResponse, bool) {
	if len(c.clientGuid) != 16 {
		return nil, false
	}

	ctx, found := contexts[smb2.SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2]
	if !found {
		return nil, false
	}

	dh, ok := smb2.ParseDurableHandleRequestV2(ctx)
	if !ok {
		return nil, false
	}

	op := c.server.findReplayableOpen([16]byte(c.clientGuid), dh.CreateGuid)
	if op == nil {
		return nil, false
	}

	// The same GUID without the replay flag is not a retry but a client reusing a GUID it
	// still owes an open for.
	if !cr.Header().IsFlagSet(smb2.FLAGS_REPLAY_OPERATION) {
		return smb2.NewErrorResponse(cr, smb2.STATUS_DUPLICATE_OBJECTID, 0, nil), true
	}

	op.mu.Lock()
	owner := op.session
	held := op.lease
	onShare := op.treeConnect.share
	op.mu.Unlock()

	// The handle goes back to the user who made it and to nobody else, and a client that held
	// a lease on it has to name the same lease key it named the first time.
	if !strings.EqualFold(owner.userName, ss.userName) || !strings.EqualFold(owner.workgroup, ss.workgroup) {
		return smb2.NewErrorResponse(cr, smb2.STATUS_ACCESS_DENIED, 0, nil), true
	}
	if held != nil && (lr == nil || held.leaseKey != lr.LeaseKey) {
		return smb2.NewErrorResponse(cr, smb2.STATUS_ACCESS_DENIED, 0, nil), true
	}

	// A replay belongs to the session the open was made on. The same user on another session
	// is a different client as far as the handle is concerned.
	if owner.sessionID != ss.sessionID {
		return smb2.NewErrorResponse(cr, smb2.STATUS_DUPLICATE_OBJECTID, 0, nil), true
	}

	// It belongs to the share it was made on as well, as a handle taken up again does. A GUID
	// that names an open on another share is one being reused rather than replayed, and answering
	// it would hand a handle on one share back through a tree connect to a different one.
	if onShare != tc.share {
		return smb2.NewErrorResponse(cr, smb2.STATUS_DUPLICATE_OBJECTID, 0, nil), true
	}

	// The replay may well have arrived over another channel, which then becomes the connection
	// of the open.
	op.mu.Lock()
	op.connection = c
	op.mu.Unlock()

	return c.replayResponse(op, held, cr, tc, contexts, lr), true
}

// replayResponse builds the answer to a replayed create out of the open the first attempt made.
//
// The lease is the one its caller read rather than one read again here, so that the check the
// caller made and the response built from it are the same observation. Read a second time, an
// open that gained a lease in between would be answered with a lease context taken from a request
// that carried none - which is to say, from a nil pointer.
func (c *connection) replayResponse(op *open, held *lease, cr smb2.CreateRequest, tc *treeConnect, contexts map[uint32][]byte, lr *smb2.LeaseRequest) smb2.GenericResponse {
	size, allocated, _, modified, attr := op.file.stat()
	op.mu.Lock()
	access := op.grantedAccess
	handle := op.handle
	fileID, durableFileID := op.fileID, op.durableFileID
	timeout := uint32(op.durableTimeout / time.Millisecond)
	oplockLevel := op.oplockLevel
	op.mu.Unlock()

	respContexts := make(map[uint32][]byte)
	for id, ctx := range contexts {
		switch id {
		case smb2.CREATE_QUERY_MAXIMAL_ACCESS_REQUEST:
			respContexts[id] = smb2.HandleCreateQueryMaximalAccessRequest(ctx, modified, access)
		case smb2.CREATE_QUERY_ON_DISK_ID:
			respContexts[id] = smb2.HandleCreateQueryOnDiskID(handle, tc.volumeID)
		case smb2.SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2:
			// The timeout is the one the open is being kept for, not a fresh grant: the clock
			// started with the create that is being replayed.
			respContexts[id] = smb2.HandleCreateDurableHandleRequestV2(timeout)
		}
	}

	if held != nil {
		oplockLevel = smb2.OPLOCK_LEVEL_LEASE
		respContexts[smb2.CREATE_REQUEST_LEASE] = smb2.HandleCreateRequestLease(*lr, held.stateNow(), held.currentEpoch())
	}

	resp := &smb2.CreateResponse{}
	resp.FromRequest(cr)

	// The open already existed, so the file was opened rather than made, whatever the create
	// being replayed did the first time round.
	resp.Generate(
		oplockLevel,
		smb2.FILE_OPENED,
		size,
		allocated,
		modified,
		attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0,
		fileID,
		durableFileID,
		respContexts,
	)

	return resp
}

// orphanDurableOpens detaches the durable opens of the session from it and leaves them in
// the global open table for the client to reclaim. They are removed from the open table of
// the session, so that tearing the session down afterwards leaves them alone.
func (ss *session) orphanDurableOpens() int {
	ss.mu.Lock()
	orphaned := make([]*open, 0, len(ss.openTable))
	for fid, op := range ss.openTable {
		if op.isDurable {
			orphaned = append(orphaned, op)
			delete(ss.openTable, fid)
		}
	}
	ss.mu.Unlock()

	// The oplock goes with the connection. A client that cannot be reached cannot be told to
	// give the file up, so it must not be left believing it still has it to itself; and the
	// open, still counted among those on the file, keeps anybody else from being granted one
	// until it is either reclaimed or swept.
	for _, op := range orphaned {
		op.releaseCaching()
	}

	now := time.Now()
	for _, op := range orphaned {
		op.mu.Lock()
		op.disconnectTime = now

		// The cached chunks are worth a lot of memory and nothing of what the open has
		// achieved so far, so they go while the open waits. An unfinished upload is the
		// opposite and is kept: redoing it is the expense worth avoiding.
		op.buffer = make(map[uint64]*readChunk)
		op.cacheOrder = nil
		op.mu.Unlock()
	}

	return len(orphaned)
}

// reclaimDurableOpen hands a durable open back to the client that created it. The open is
// looked up in the global table and reattached to the session, the tree connect and the
// connection the reconnect request arrived on. It returns nil if there is nothing to
// reclaim, or if the request doesn't come from the client that owns the open.
func (c *connection) reclaimDurableOpen(rec smb2.DurableHandleReconnectV2, ss *session, tc *treeConnect) *open {
	c.server.mu.Lock()
	op, found := c.server.globalOpenTable[rec.DurableID]
	c.server.mu.Unlock()
	if !found {
		return nil
	}

	// The whole decision is made under the one lock, so that an open cannot be reclaimed
	// and expired at the same time: whichever of the two gets the lock first wins, and the
	// other finds the open no longer durable.
	op.mu.Lock()
	owner := op.session
	granted := op.isDurable &&
		// The GUID is what proves the request comes from the client that created the open:
		// the file ID alone travels in the clear on every request that uses the handle.
		op.createGuid == rec.CreateGuid &&
		op.fileID == rec.FileID &&
		// An open still attached to a session is in use by somebody; only one that has lost
		// its connection is up for reclaiming.
		!op.disconnectTime.IsZero() &&
		// The handle goes back to the same user, on the same share, and to nobody else.
		op.treeConnect.share == tc.share &&
		strings.EqualFold(owner.userName, ss.userName) &&
		strings.EqualFold(owner.workgroup, ss.workgroup)
	if granted {
		op.session = ss
		op.treeConnect = tc
		op.connection = c
		op.disconnectTime = time.Time{}
	}
	op.mu.Unlock()

	if !granted {
		return nil
	}

	ss.mu.Lock()
	ss.openTable[op.fileID] = op
	ss.mu.Unlock()

	tc.mu.Lock()
	tc.openCount++
	tc.mu.Unlock()

	return op
}

// sweepDurableOpens closes the durable opens that were never reclaimed within the time they
// were granted.
func (s *server) sweepDurableOpens() {
	now := time.Now()

	s.mu.Lock()
	candidates := make([]*open, 0, len(s.globalOpenTable))
	for _, op := range s.globalOpenTable {
		candidates = append(candidates, op)
	}
	s.mu.Unlock()

	var expired []*open
	for _, op := range candidates {
		op.mu.Lock()
		if op.isDurable && !op.disconnectTime.IsZero() && now.Sub(op.disconnectTime) > op.durableTimeout {
			// Taking the durability away shuts the door on a reclaim that may be running
			// at this very moment: it fails its own check before it can reattach the open.
			op.isDurable = false
			expired = append(expired, op)
		}
		op.mu.Unlock()
	}

	if len(expired) == 0 {
		return
	}

	s.mu.Lock()
	for _, op := range expired {
		delete(s.globalOpenTable, op.durableFileID)
	}
	s.mu.Unlock()

	for _, op := range expired {
		if s.debug {
			log.Printf("Durable handle for %s was not reclaimed in time", op.pathName)
		}

		// An open that has left the global table leaves the indexes with it, and the create
		// that made it can no longer be replayed.
		s.unindexOpen(op)
		s.clearReplayEligible(op)

		// An upload that is never coming back has to be called off at the backend as well,
		// or the multipart upload it started is left hanging there. Calling it off puts the
		// file back to the size the store holds it at, and the handle that was keeping the
		// state on the share is gone with the open.
		op.cancelUpload()
		op.releaseFile()
		op.cancel()
	}
}

// reapDurableOpens collects the durable opens that nobody came back for, until the server
// shuts down.
func (s *server) reapDurableOpens() {
	ticker := time.NewTicker(durableSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// Recovered per sweep rather than around the loop, so that a sweep that panics
			// costs that sweep and not every one after it.
			func() {
				defer recoverGoroutine("sweeping the durable opens")
				s.sweepDurableOpens()
			}()
		}
	}
}
