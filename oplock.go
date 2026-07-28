package main

import (
	"log"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// oplockBreakTimeout is how long the server waits for a client to acknowledge that it has
// given up an oplock. Whoever wants the file carries on once it runs out: a client that has
// gone quiet must not be able to hold a file hostage.
const oplockBreakTimeout = 35 * time.Second

// oplockEligible reports whether an oplock may be granted for the level asked for, leaving
// aside who else has the file open.
//
// Only the exclusive levels are ever granted. A level II oplock would have to be broken
// whenever anybody writes to the file, and the server breaks oplocks on a create alone, so
// granting one would leave the client caching reads that have gone stale.
func oplockEligible(requested uint8, tc *treeConnect, isDir bool) bool {
	if requested != smb2.OPLOCK_LEVEL_EXCLUSIVE && requested != smb2.OPLOCK_LEVEL_BATCH {
		return false
	}

	// A named pipe has nothing behind it worth caching, and a directory can only be cached
	// through a lease, which the server does not grant either.
	return tc.share.name != "ipc$" && !isDir
}

// opensOn collects the opens of a file, other than the one given. The global table is copied
// before the opens in it are examined, so that the lock of the server and the lock of an open
// are never held at the same time.
func (s *server) opensOn(sh *share, path string, except *open) []*open {
	s.mu.Lock()
	candidates := make([]*open, 0, len(s.globalOpenTable))
	for _, op := range s.globalOpenTable {
		if op != except {
			candidates = append(candidates, op)
		}
	}
	s.mu.Unlock()

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

// startOplockBreak moves an open from holding its oplock to giving it up. It returns the
// channel that is closed once the break is over, and whether this call is the one that started
// it: a break that is already in flight is waited for rather than sent twice.
func (op *open) startOplockBreak() (chan struct{}, bool) {
	op.mu.Lock()
	defer op.mu.Unlock()

	switch op.oplockState {
	case smb2.OplockHeld:
		op.oplockState = smb2.OplockBreaking
		op.oplockBreak = make(chan struct{})
		return op.oplockBreak, true
	case smb2.OplockBreaking:
		return op.oplockBreak, false
	}

	return nil, false
}

// completeOplockBreak ends a break that is in flight and releases whoever was waiting for it.
// Only the first call has any effect: the acknowledgment of the client, the expiry of the
// timer and the death of the open all race to end the same break.
//
// The oplock is always given up in full. A client is allowed to answer a break by dropping to
// level II, but the server grants no level II oplocks and would never break one on a write, so
// leaving the client with one would leave it caching reads that can go stale.
func (op *open) completeOplockBreak() bool {
	op.mu.Lock()
	defer op.mu.Unlock()

	if op.oplockState != smb2.OplockBreaking {
		return false
	}

	op.oplockLevel = smb2.OPLOCK_LEVEL_NONE
	op.oplockState = smb2.OplockNone
	close(op.oplockBreak)
	op.oplockBreak = nil

	return true
}

// releaseOplock takes the oplock away from an open that is going away or has lost its client.
// A break that was in flight ends here too, so that a create waiting on it carries on at once
// instead of sitting out the acknowledgment timer.
func (op *open) releaseOplock() {
	op.mu.Lock()
	defer op.mu.Unlock()

	op.oplockLevel = smb2.OPLOCK_LEVEL_NONE
	op.oplockState = smb2.OplockNone
	if op.oplockBreak != nil {
		close(op.oplockBreak)
		op.oplockBreak = nil
	}
}

// breakConnections lists the connections a break notification may travel over: the one chosen
// for the open first, and the remaining channels of its session after it, so that a send that
// fails can be tried again elsewhere.
func (op *open) breakConnections() []*connection {
	first := op.selectConnection(nil)

	op.mu.Lock()
	ss := op.session
	op.mu.Unlock()

	conns := []*connection{first}
	ss.mu.Lock()
	for _, ch := range ss.channelList {
		if ch.connection != first {
			conns = append(conns, ch.connection)
		}
	}
	ss.mu.Unlock()

	return conns
}

// sendOplockBreak tells the client that the oplock it holds is being revoked, and starts the
// clock on the acknowledgment. A client that cannot be reached on any channel of its session
// loses the oplock straight away: it cannot be caching on the strength of a promise the server
// has no way of withdrawing.
func (s *server) sendOplockBreak(op *open) {
	op.mu.Lock()
	ss := op.session
	path := op.pathName
	fid := op.id()
	op.mu.Unlock()

	// The break never names a level the client could keep. Batch is not a level a break may
	// ask for at all, whichever level the oplock was granted at.
	notification := smb2.NewOplockBreakNotification(smb2.OPLOCK_LEVEL_NONE, fid, ss.sessionID)

	var sent bool
	for _, conn := range op.breakConnections() {
		if s.trySendResponse(conn, ss, notification) {
			sent = true
			break
		}
	}

	if !sent {
		if s.debug {
			log.Printf("Oplock break on %s could not be delivered, revoking it", path)
		}
		op.completeOplockBreak()
		return
	}

	time.AfterFunc(oplockBreakTimeout, func() {
		if op.completeOplockBreak() && s.debug {
			log.Printf("Oplock break on %s was not acknowledged in time", path)
		}
	})
}

// startBreaks moves every open that holds an oplock to breaking, and returns what it takes to
// finish the job: the channels to wait on, and the opens that still have to be told.
// s.oplockMu must be held, so that the opens cannot change hands while they are collected.
func startBreaks(opens []*open) (waits []chan struct{}, notify []*open) {
	for _, op := range opens {
		ch, started := op.startOplockBreak()
		if ch == nil {
			continue
		}
		waits = append(waits, ch)
		if started {
			notify = append(notify, op)
		}
	}

	return
}

// breakOplocksOn revokes every oplock held on a file and waits until the clients holding them
// have answered. It is what a create has to do before it may look at a file: the holder of an
// exclusive oplock may be sitting on writes it has not sent yet.
//
// It must not be called from the goroutine that serves a connection. The wait lasts as long as
// the acknowledgment timer, and the acknowledgment it is waiting for may be on its way in over
// that very connection.
func (s *server) breakOplocksOn(sh *share, path string, except *open) {
	s.oplockMu.Lock()
	waits, notify := startBreaks(s.opensOn(sh, path, except))
	s.oplockMu.Unlock()

	for _, op := range notify {
		s.sendOplockBreak(op)
	}
	for _, ch := range waits {
		<-ch
	}
}

// hasOplockHolders reports whether anybody holds, or is in the middle of giving up, an oplock
// on a file. It is what tells a create whether it has to go asynchronous.
func (s *server) hasOplockHolders(sh *share, path string, except *open) bool {
	for _, op := range s.opensOn(sh, path, except) {
		op.mu.Lock()
		held := op.oplockState != smb2.OplockNone
		op.mu.Unlock()
		if held {
			return true
		}
	}

	return false
}

// grantOplock gives the open the oplock it asked for, and returns the level granted. An
// exclusive oplock is only granted to an open that has the file to itself, which is what makes
// a create the only thing that can conflict with one.
//
// The decision and the granting happen under the one lock, so that two creates racing for the
// same file cannot both come out of it holding an oplock. If the file turns out to have been
// opened elsewhere while this create was under way, nothing is granted, and whoever was
// granted an oplock before this open appeared has to give it up.
func (s *server) grantOplock(op *open, requested uint8, tc *treeConnect, path string) uint8 {
	s.oplockMu.Lock()

	others := s.opensOn(tc.share, path, op)
	if len(others) == 0 {
		// An open that is in the middle of giving up an oplock is left alone. Handing it a new
		// one would put it back into holding without ending the break, and whoever was waiting
		// for that break would wait for a channel that is never closed.
		op.mu.Lock()
		granted := op.oplockState == smb2.OplockNone
		if granted {
			op.oplockLevel = requested
			op.oplockState = smb2.OplockHeld
		}
		op.mu.Unlock()
		s.oplockMu.Unlock()

		if granted {
			return requested
		}

		return smb2.OPLOCK_LEVEL_NONE
	}

	_, notify := startBreaks(others)
	s.oplockMu.Unlock()

	// The breaks are sent without waiting for them. This runs on the goroutine that serves the
	// connection, which must not be held up; and an oplock granted this late was granted while
	// the file stood free, so its holder cannot be sitting on anything the create should have
	// seen. All that matters is that the holder is told, which starting the break does.
	go func() {
		for _, other := range notify {
			s.sendOplockBreak(other)
		}
	}()

	return smb2.OPLOCK_LEVEL_NONE
}

// acknowledgeOplockBreak takes the answer of a client to a break that is in flight and returns
// the status to reply with.
func (op *open) acknowledgeOplockBreak(level uint8) uint32 {
	op.mu.Lock()
	state := op.oplockState
	held := op.oplockLevel
	op.mu.Unlock()

	// Nothing is being broken, so there is nothing to acknowledge.
	if state != smb2.OplockBreaking {
		return smb2.STATUS_INVALID_DEVICE_STATE
	}

	// A lease is never granted, so it can never be given up either.
	if level == smb2.OPLOCK_LEVEL_LEASE {
		op.completeOplockBreak()
		return smb2.STATUS_INVALID_PARAMETER
	}

	// A client may only answer with a level below the one it held.
	var valid bool
	switch held {
	case smb2.OPLOCK_LEVEL_BATCH:
		valid = level == smb2.OPLOCK_LEVEL_EXCLUSIVE || level == smb2.OPLOCK_LEVEL_II || level == smb2.OPLOCK_LEVEL_NONE
	case smb2.OPLOCK_LEVEL_EXCLUSIVE:
		valid = level == smb2.OPLOCK_LEVEL_II || level == smb2.OPLOCK_LEVEL_NONE
	case smb2.OPLOCK_LEVEL_II:
		valid = level == smb2.OPLOCK_LEVEL_NONE
	}

	op.completeOplockBreak()

	if !valid {
		return smb2.STATUS_INVALID_OPLOCK_PROTOCOL
	}

	return smb2.STATUS_OK
}
