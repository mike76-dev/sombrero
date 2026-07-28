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

		// An upload that is never coming back has to be called off at the backend as well,
		// or the multipart upload it started is left hanging there.
		op.cancelUpload()
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
			s.sweepDurableOpens()
		}
	}
}
