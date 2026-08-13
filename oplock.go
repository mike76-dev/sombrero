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
func oplockEligible(requested uint8, tc *treeConnect, isDir bool) bool {
	switch requested {
	case smb2.OPLOCK_LEVEL_II, smb2.OPLOCK_LEVEL_EXCLUSIVE, smb2.OPLOCK_LEVEL_BATCH:
	default:
		return false
	}

	// A named pipe has nothing behind it worth caching, and a directory can only be cached
	// through a lease, which the server does not grant.
	return tc.share.name != "ipc$" && !isDir
}

// oplockBreakTarget returns the level an oplock is to be cut back to. An open that only wants
// to read the file needs the write cache of the holder gone and no more; anything that changes
// the file needs the read cache gone as well.
func oplockBreakTarget(sharedOK bool) uint8 {
	if sharedOK {
		return smb2.OPLOCK_LEVEL_II
	}

	return smb2.OPLOCK_LEVEL_NONE
}

// createChangesFile reports whether the create itself changes the file it opens, by emptying it
// or by marking it to be deleted.
func createChangesFile(cr smb2.CreateRequest) bool {
	switch cr.CreateDisposition() {
	case smb2.FILE_SUPERSEDE, smb2.FILE_OVERWRITE, smb2.FILE_OVERWRITE_IF:
		return true
	}

	return cr.CreateOptions()&smb2.FILE_DELETE_ON_CLOSE > 0
}

// startOplockBreak moves an open from holding its oplock to giving it up. It returns the
// channel that is closed once the break is over, and whether this call is the one that started
// it: a break that is already in flight is waited for rather than sent twice.
func (op *open) startOplockBreak(to uint8) (chan struct{}, bool) {
	op.mu.Lock()
	defer op.mu.Unlock()

	switch op.oplockState {
	case smb2.OplockHeld:
		// A level that promises as much as the one already held takes nothing away, so there
		// is nothing to tell the client about.
		if to >= op.oplockLevel {
			return nil, false
		}

		op.oplockState = smb2.OplockBreaking
		op.oplockBreakTo = to
		op.oplockBreakSeq++
		op.oplockBreak = make(chan struct{})
		return op.oplockBreak, true

	case smb2.OplockBreaking:
		if to >= op.oplockBreakTo {
			return op.oplockBreak, false
		}

		// A second conflict wants the oplock cut back further than the break already in flight,
		// so the client is told again and answering the first one is no longer good enough.
		op.oplockBreakTo = to
		return op.oplockBreak, true
	}

	return nil, false
}

// completeOplockBreak ends a break that is in flight, leaving the client holding the level
// given, and releases whoever was waiting for it. Only the first call has any effect: the
// acknowledgment of the client, the expiry of the timer and the death of the open all race to
// end the same break.
func (op *open) completeOplockBreak(level uint8) bool {
	op.mu.Lock()
	defer op.mu.Unlock()

	return op.finishOplockBreak(level)
}

// expireOplockBreak is completeOplockBreak for the wait that was started along with a break: it
// ends that break and no other. A break that has been acknowledged and a later one that has taken
// its place are two different breaks, and the wait that belonged to the first has nothing to say
// about the second - the client is owed the whole of its time to answer the notification it was
// sent, not what is left of the time granted for one it already answered.
func (op *open) expireOplockBreak(seq uint64, level uint8) bool {
	op.mu.Lock()
	defer op.mu.Unlock()

	if op.oplockBreakSeq != seq {
		return false
	}

	return op.finishOplockBreak(level)
}

// finishOplockBreak ends the break in flight and releases whoever was waiting for it, reporting
// whether there was one to end. op.mu must be held.
func (op *open) finishOplockBreak(level uint8) bool {
	if op.oplockState != smb2.OplockBreaking {
		return false
	}

	op.oplockLevel = level
	if level == smb2.OPLOCK_LEVEL_NONE {
		op.oplockState = smb2.OplockNone
	} else {
		op.oplockState = smb2.OplockHeld
	}

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
	to := op.oplockBreakTo
	held := op.oplockLevel
	seq := op.oplockBreakSeq
	op.mu.Unlock()

	notification := smb2.NewOplockBreakNotification(to, fid, ss.sessionID)

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
		op.completeOplockBreak(smb2.OPLOCK_LEVEL_NONE)
		return
	}

	// A client holding a level II oplock has nothing to answer with: the only level below it is
	// none, so there is no question how the transition was made and the break is over as soon
	// as the client has been told.
	if held == smb2.OPLOCK_LEVEL_II {
		op.completeOplockBreak(smb2.OPLOCK_LEVEL_NONE)
		return
	}

	time.AfterFunc(s.oplockBreakTimeout, func() {
		// A client that never answered keeps nothing: the file has been waiting on it.
		if op.expireOplockBreak(seq, smb2.OPLOCK_LEVEL_NONE) && s.debug {
			log.Printf("Oplock break on %s was not acknowledged in time", path)
		}
	})
}

// startBreaks moves every open that holds an oplock to breaking, and returns what it takes to
// finish the job: the channels to wait on, and the opens that still have to be told.
// s.cachingMu must be held, so that the opens cannot change hands while they are collected.
func startBreaks(opens []*open, to uint8) (waits []chan struct{}, notify []*open) {
	for _, op := range opens {
		ch, started := op.startOplockBreak(to)
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

// asker is who a create is, for deciding whose promises stand: the client it comes from, the lease
// it named if that lease is already held, and whether it named a lease key at all.
type asker struct {
	guid  [16]byte
	own   *lease
	keyed bool
}

// askedBy is the asker a create on this connection stands for.
func (c *connection) askedBy(own *lease, lr *smb2.LeaseRequest) asker {
	by := asker{own: own, keyed: lr != nil}
	if len(c.clientGuid) == 16 {
		by.guid = [16]byte(c.clientGuid)
	}

	return by
}

// covers reports whether the promise held by this lease is one the asker already has: its own lease,
// or a lease of its own client at a time when it named no key to tell views apart by.
func (by asker) covers(l *lease) bool {
	if l == by.own {
		return true
	}

	if by.keyed || by.guid == ([16]byte{}) {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	return l.clientGuid == by.guid
}

// sameCacheView reports whether the open belongs to the client that is asking.
func sameCacheView(op *open, guid [16]byte) bool {
	if guid == ([16]byte{}) {
		return false
	}

	op.mu.Lock()
	defer op.mu.Unlock()

	return op.clientGuid == guid
}

// holdersIn sorts the opens of a file by is who is asking, whose own promises are left standing.
func holdersIn(opens []*open, by asker) (oplocks []*open, leases []*lease) {
	seen := make(map[*lease]struct{})
	for _, op := range opens {
		op.mu.Lock()
		l := op.lease
		held := op.oplockState != smb2.OplockNone
		op.mu.Unlock()

		if l != nil {
			_, found := seen[l]
			if by.covers(l) || found {
				continue
			}

			// A lease that has already been given up promises nothing, however many opens are
			// still attached to it. Only one that still holds something, or is in the middle of
			// letting go of it, stands in anybody's way.
			l.mu.Lock()
			active := l.state != smb2.SMB2_LEASE_NONE || l.breaking
			l.mu.Unlock()

			if active {
				seen[l] = struct{}{}
				leases = append(leases, l)
			}
			continue
		}

		if held && !sameCacheView(op, by.guid) {
			oplocks = append(oplocks, op)
		}
	}

	return
}

// holdersOn is holdersIn over the opens of a file.
func (s *server) holdersOn(sh *share, path string, except *open, by asker) ([]*open, []*lease) {
	return holdersIn(s.opensOn(sh, path, except), by)
}

// opensOutside returns the opens that do not belong to the given lease. A lease may be granted
// while the client that asks for it already has the file open under the same lease, but not
// while anybody else has.
func opensOutside(opens []*open, own *lease) []*open {
	var outside []*open
	for _, op := range opens {
		op.mu.Lock()
		mine := own != nil && op.lease == own
		op.mu.Unlock()
		if !mine {
			outside = append(outside, op)
		}
	}

	return outside
}

// exclusiveHeld reports whether anything among these holders promises more than a read cache.
// Nothing exclusive may be promised while one stands, and nothing at all while one is being
// given up.
func exclusiveHeld(oplocks []*open, leases []*lease) bool {
	for _, op := range oplocks {
		op.mu.Lock()
		exclusive := op.oplockLevel > smb2.OPLOCK_LEVEL_II || op.oplockState == smb2.OplockBreaking
		op.mu.Unlock()
		if exclusive {
			return true
		}
	}

	for _, l := range leases {
		l.mu.Lock()
		exclusive := l.state&smb2.SMB2_LEASE_WRITE_CACHING > 0 || l.breaking
		l.mu.Unlock()
		if exclusive {
			return true
		}
	}

	return false
}

// breakHoldersOn cuts back every promise made on a file and waits for the clients that owe an
// answer. It is what a create has to do before it may look at a file: the holder of an
// exclusive oplock or a write-caching lease may be sitting on writes it has not sent yet.
// sharedOK says the create only wants to read, in which case the holders keep their read
// caches and give up only what lets them write.
// It must not be called from the goroutine that serves a connection.
func (s *server) breakHoldersOn(sh *share, path string, except *open, by asker, sharedOK bool) {
	waits, notify, leaseNotify := s.startHolderBreaks(sh, path, except, by, sharedOK)

	for _, op := range notify {
		s.sendOplockBreak(op)
	}
	for _, l := range leaseNotify {
		s.sendLeaseBreak(l)
	}
	for _, ch := range waits {
		<-ch
	}
}

// startHolderBreaks cuts back every promise on a file and returns what it takes to finish the
// job: the channels to wait on, and the holders that still have to be told.
func (s *server) startHolderBreaks(sh *share, path string, except *open, by asker, sharedOK bool) (waits []chan struct{}, notify []*open, leaseNotify []*lease) {
	s.cachingMu.Lock()
	defer s.cachingMu.Unlock()

	oplocks, leases := s.holdersOn(sh, path, except, by)
	waits, notify = startBreaks(oplocks, oplockBreakTarget(sharedOK))
	leaseWaits, leaseNotify := startLeaseBreaks(leases, sharedOK)

	return append(waits, leaseWaits...), notify, leaseNotify
}

// tellHoldersOn cuts back the promises on a file without waiting for anybody. It is what an
// operation does when nothing it found has to be acknowledged, and it must be used wherever the
// goroutine serving a connection would otherwise be held up.
func (s *server) tellHoldersOn(sh *share, path string, except *open, by asker, sharedOK bool) {
	_, notify, leaseNotify := s.startHolderBreaks(sh, path, except, by, sharedOK)
	if len(notify) == 0 && len(leaseNotify) == 0 {
		return
	}

	go func() {
		defer recoverGoroutine("sending the breaks")

		for _, op := range notify {
			s.sendOplockBreak(op)
		}
		for _, l := range leaseNotify {
			s.sendLeaseBreak(l)
		}
	}()
}

// needsBreakWait reports whether a create would have to wait for a break before it may look at
// the file. Only a promise that has to be acknowledged is worth waiting for: a read cache has
// no level below it to argue about, so its holder is told and that is the end of it.
func (s *server) needsBreakWait(sh *share, path string, except *open, by asker, sharedOK bool) bool {
	oplocks, leases := s.holdersOn(sh, path, except, by)

	to := oplockBreakTarget(sharedOK)
	for _, op := range oplocks {
		op.mu.Lock()
		wait := op.oplockLevel > smb2.OPLOCK_LEVEL_II && to < op.oplockLevel
		op.mu.Unlock()
		if wait {
			return true
		}
	}

	for _, l := range leases {
		l.mu.Lock()
		wait := l.state != smb2.SMB2_LEASE_READ_CACHING && l.state&^leaseBreakTarget(l.state, sharedOK) != 0
		l.mu.Unlock()
		if wait {
			return true
		}
	}

	return false
}

// hasHoldersOn reports whether anybody holds, or is in the middle of giving up, an oplock or a
// lease on a file.
func (s *server) hasHoldersOn(sh *share, path string, except *open, by asker) bool {
	oplocks, leases := s.holdersOn(sh, path, except, by)
	return len(oplocks) > 0 || len(leases) > 0
}

// breakForChange cuts back every promise on a file that would let a client go on serving data
// the change about to be made has rendered stale. It is what a write, a truncation, a rename or
// a delete has to do.
func (s *server) breakForChange(op *open) {
	op.mu.Lock()
	sh := op.treeConnect.share
	path := op.pathName
	own := op.lease
	op.mu.Unlock()

	s.tellHoldersOn(sh, path, op, asker{own: own}, false)
}

// grantOplock gives the open the oplock it asked for, and returns the level granted. An
// exclusive oplock is only granted to an open that has the file to itself, which is what makes
// a create the only thing that can conflict with one.
func (s *server) grantOplock(op *open, requested uint8, tc *treeConnect, path string) uint8 {
	s.cachingMu.Lock()

	// An open in the middle of giving up an oplock is left alone. Handing it a new one would
	// put it back into holding without ending the break, and whoever was waiting for that
	// break would wait for a channel that is never closed.
	op.mu.Lock()
	busy := op.oplockState != smb2.OplockNone
	op.mu.Unlock()
	if busy {
		s.cachingMu.Unlock()
		return smb2.OPLOCK_LEVEL_NONE
	}

	// The opens of the client that is asking are in the same view of the file as the open being
	// granted, so they neither stand in the way of the promise nor are broken for it.
	op.mu.Lock()
	guid := op.clientGuid
	op.mu.Unlock()

	others := s.opensOn(tc.share, path, op)
	var elsewhere []*open
	for _, other := range others {
		if !sameCacheView(other, guid) {
			elsewhere = append(elsewhere, other)
		}
	}

	oplocks, leases := holdersIn(elsewhere, asker{guid: guid})

	// The file is free, so it may be promised in full. Otherwise nothing exclusive can be
	// promised, but a read cache still can, as long as nobody else is holding more than one.
	granted := requested
	if len(elsewhere) > 0 {
		if exclusiveHeld(oplocks, leases) {
			granted = smb2.OPLOCK_LEVEL_NONE
		} else {
			granted = smb2.OPLOCK_LEVEL_II
		}
	}

	if granted != smb2.OPLOCK_LEVEL_NONE {
		op.mu.Lock()
		op.oplockLevel = granted
		op.oplockState = smb2.OplockHeld
		op.mu.Unlock()
		s.cachingMu.Unlock()

		return granted
	}

	_, notify := startBreaks(oplocks, smb2.OPLOCK_LEVEL_NONE)
	_, leaseNotify := startLeaseBreaks(leases, false)
	s.cachingMu.Unlock()

	// The breaks are sent without waiting for them. This runs on the goroutine that serves the
	// connection, which must not be held up; and a promise made this late was made while the
	// file stood free, so its holder cannot be sitting on anything the create should have seen.
	// All that matters is that the holder is told, which starting the break does.
	go func() {
		defer recoverGoroutine("sending the breaks")

		for _, other := range notify {
			s.sendOplockBreak(other)
		}
		for _, other := range leaseNotify {
			s.sendLeaseBreak(other)
		}
	}()

	return smb2.OPLOCK_LEVEL_NONE
}

// acknowledgeOplockBreak takes the answer of a client to a break that is in flight and returns
// the status to reply with.
func (op *open) acknowledgeOplockBreak(level uint8) (uint32, uint8) {
	op.mu.Lock()
	state := op.oplockState
	held := op.oplockLevel
	to := op.oplockBreakTo
	op.mu.Unlock()

	// Nothing is being broken, so there is nothing to acknowledge.
	if state != smb2.OplockBreaking {
		return smb2.STATUS_INVALID_DEVICE_STATE, smb2.OPLOCK_LEVEL_NONE
	}

	// A lease is never held through an oplock, so it can never be given up as one.
	if level == smb2.OPLOCK_LEVEL_LEASE {
		op.completeOplockBreak(smb2.OPLOCK_LEVEL_NONE)
		return smb2.STATUS_INVALID_PARAMETER, smb2.OPLOCK_LEVEL_NONE
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

	if !valid {
		op.completeOplockBreak(smb2.OPLOCK_LEVEL_NONE)
		return smb2.STATUS_INVALID_OPLOCK_PROTOCOL, smb2.OPLOCK_LEVEL_NONE
	}

	// A conflict that arrived while the break was in flight may have cut the oplock back
	// further than the client is answering about. The break stands, and the client is expected
	// to answer the notification that named the lower level.
	if level > to {
		return smb2.STATUS_REQUEST_NOT_ACCEPTED, smb2.OPLOCK_LEVEL_NONE
	}

	op.completeOplockBreak(level)

	return smb2.STATUS_OK, level
}
