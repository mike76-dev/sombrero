package main

import (
	"log"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// leaseBreakTimeout is how long the server waits for a client to acknowledge that it has cut
// its lease back. Whoever wants the file carries on once it runs out, as with an oplock.
const leaseBreakTimeout = 35 * time.Second

// lease is a Lease object: the promise the server makes to one client about one file. Unlike
// an oplock, which belongs to a single open, a lease is shared by every open the client has on
// the file under the same key, and none of those opens breaks the others.
type lease struct {
	leaseKey   [16]byte
	clientGuid [16]byte
	fileName   string

	state        uint32
	breakToState uint32
	breaking     bool
	epoch        uint16

	// fileDeleteOnClose records that the file the lease covers is to be deleted once the last
	// handle on it goes. A lease key is otherwise tied to the file it was first used on for as
	// long as the lease lives; this is the one thing that frees it.
	fileDeleteOnClose bool

	// opens are the opens sharing the lease, keyed by durable file ID. The lease is worth
	// nothing once the last of them is gone.
	opens map[uint64]*open

	// breakDone is open while the client is being told to cut the lease back, and is closed
	// once it has answered, the wait has run out, or the last open has gone.
	breakDone chan struct{}

	mu sync.Mutex
}

// leaseTable is the set of leases held by one client, keyed by the lease key the client chose.
type leaseTable struct {
	leases map[[16]byte]*lease
	mu     sync.Mutex
}

// leaseStates is every bit a lease may be made of.
const leaseStates = smb2.SMB2_LEASE_READ_CACHING | smb2.SMB2_LEASE_HANDLE_CACHING |
	smb2.SMB2_LEASE_WRITE_CACHING

// leaseGrantable reports whether a lease state is one the server is willing to promise.
func leaseGrantable(requested uint32) bool {
	return requested&leaseStates != 0
}

// leaseBreakTarget returns the state a lease is to be cut back to. An open that only wants to
// read the file needs the write cache of the holder gone and no more; anything that changes the
// file needs everything gone, because the holder may be caching both the data and the handle.
func leaseBreakTarget(current uint32, sharedOK bool) uint32 {
	if sharedOK {
		return current &^ smb2.SMB2_LEASE_WRITE_CACHING
	}

	return smb2.SMB2_LEASE_NONE
}

// leaseTableFor returns the lease table of the client, making one if this is the first lease it
// has asked for.
func (s *server) leaseTableFor(guid [16]byte) *leaseTable {
	s.mu.Lock()
	defer s.mu.Unlock()

	lt, found := s.globalLeaseTableList[guid]
	if !found {
		lt = &leaseTable{leases: make(map[[16]byte]*lease)}
		s.globalLeaseTableList[guid] = lt
	}

	return lt
}

// findLease returns the lease the client holds under the given key, if it holds one.
func (s *server) findLease(guid, key [16]byte) *lease {
	s.mu.Lock()
	lt, found := s.globalLeaseTableList[guid]
	s.mu.Unlock()

	if !found {
		return nil
	}

	lt.mu.Lock()
	defer lt.mu.Unlock()

	return lt.leases[key]
}

// leaseFor returns the lease the client holds under the key it named, making one if it holds
// none yet. It returns false if the key is already in use for another file, which is the one
// thing a client is not allowed to do with a lease key.
func (s *server) leaseFor(guid [16]byte, req smb2.LeaseRequest, path string) (*lease, bool) {
	lt := s.leaseTableFor(guid)

	lt.mu.Lock()
	defer lt.mu.Unlock()

	l, found := lt.leases[req.LeaseKey]
	if found {
		l.mu.Lock()

		// A file on its way out is the exception to a key naming one file: the client may take
		// the key it was using out on something else, since what it named is about to be gone
		// ([MS-SMB2] 3.3.5.9.8, 3.3.5.9.11). Without this a client that deletes a file and opens
		// another under the same key is refused until it thinks to pick a new one.
		//
		// The lease follows the key to the new file. The spec says only that the request is not
		// to be refused, leaving the lease pointing at a name it no longer covers; moving it
		// keeps the name honest, and ties the key to the new file as it was to the old.
		if l.fileName != path && l.fileDeleteOnClose {
			l.fileName = path
			l.fileDeleteOnClose = false
		}

		matches := l.fileName == path
		l.mu.Unlock()

		return l, matches
	}

	l = &lease{
		leaseKey:   req.LeaseKey,
		clientGuid: guid,
		fileName:   path,
		epoch:      1,
		opens:      make(map[uint64]*open),
	}
	lt.leases[req.LeaseKey] = l

	return l, true
}

// join adds the open to the lease, and hands the open its share of it. The table is the one the
// lease lives in: a lease that lost its last open a moment ago has been removed from it, so
// joining puts the lease back, and takes the lock of the table ahead of the lock of the lease
// the way every path through the table does. Without that, an open racing a close under the
// same key could come out holding a lease the table no longer knows about, whose breaks could
// then never be acknowledged.
func (l *lease) join(lt *leaseTable, op *open, state uint32) {
	lt.mu.Lock()
	if _, found := lt.leases[l.leaseKey]; !found {
		lt.leases[l.leaseKey] = l
	}

	l.mu.Lock()
	l.state = state
	l.opens[op.durableFileID] = op
	l.mu.Unlock()
	lt.mu.Unlock()

	op.mu.Lock()
	op.lease = l
	op.mu.Unlock()
}

// leaveLease takes the open out of the lease it shared. A lease that has lost its last open is
// worth nothing and gives up whatever it promised, releasing anybody waiting on a break of it;
// it is also removed from the table of its client, so that the key it was taken out under is
// free for another file and the table does not grow with every key the client ever used.
func (op *open) leaveLease() {
	op.mu.Lock()
	l := op.lease
	op.lease = nil
	c := op.connection
	op.mu.Unlock()

	if l == nil {
		return
	}

	// The table is locked ahead of the lease, in the order every path through the table takes
	// the two, so that the departure of the last open and a create reusing the key cannot
	// interleave halfway.
	lt := c.server.leaseTableFor(l.clientGuid)
	lt.mu.Lock()
	defer lt.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.opens, op.durableFileID)
	if len(l.opens) > 0 {
		return
	}

	l.state = smb2.SMB2_LEASE_NONE
	if l.breaking {
		l.breaking = false
		close(l.breakDone)
		l.breakDone = nil
	}

	if lt.leases[l.leaseKey] == l {
		delete(lt.leases, l.leaseKey)
	}
}

// setLeaseDeleteOnClose records on the lease of the open, if it holds one, whether the file is
// to be deleted when the last handle on it goes. What a pending deletion buys the client is the
// freedom to use the same lease key for another file; calling the deletion off takes that back,
// since the file is staying and the key is its own again.
func (op *open) setLeaseDeleteOnClose(pending bool) {
	op.mu.Lock()
	l := op.lease
	op.mu.Unlock()

	if l == nil {
		return
	}

	l.mu.Lock()
	l.fileDeleteOnClose = pending
	l.mu.Unlock()
}

// renameLease points the lease of the open, if it holds one, at the new name of the file it
// covers. A file that has just been renamed is not on its way out, so the key is tied to it
// again ([MS-SMB2] 3.3.5.21.1).
func (op *open) renameLease(path string) {
	op.mu.Lock()
	l := op.lease
	op.mu.Unlock()

	if l == nil {
		return
	}

	l.mu.Lock()
	l.fileName = path
	l.fileDeleteOnClose = false
	l.mu.Unlock()
}

// releaseCaching takes away whatever the open was allowed to cache, be it an oplock of its own
// or a share of a lease, and releases anybody waiting for either to be given up.
func (op *open) releaseCaching() {
	op.releaseOplock()
	op.leaveLease()
}

// stateNow returns what the lease currently promises.
func (l *lease) stateNow() uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.state
}

// currentEpoch returns the epoch of the lease, which the client uses to tell a stale lease
// state change from a fresh one.
func (l *lease) currentEpoch() uint16 {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.epoch
}

// leaseRequest returns the lease the client is asking for with this create, or nil if it is
// asking for none. A lease context means nothing unless the client says it wants a lease, and
// only a client that named a GUID at negotiate time can hold one.
//
// It also means nothing on a connection the server never offered leases on. [MS-SMB2] 3.3.5.9: a
// server that does not support leasing ignores the lease create context, and 2.0.2 is the dialect
// that has no leases in it - so what is advertised at negotiate time and what is granted here are
// the same answer.
func (c *connection) leaseRequest(cr smb2.CreateRequest, contexts map[uint32][]byte) *smb2.LeaseRequest {
	if c.serverCapabilities&smb2.GLOBAL_CAP_LEASING == 0 {
		return nil
	}

	if cr.RequestedOplockLevel() != smb2.OPLOCK_LEVEL_LEASE || len(c.clientGuid) != 16 {
		return nil
	}

	ctx, found := contexts[smb2.CREATE_REQUEST_LEASE]
	if !found {
		return nil
	}

	lr, ok := smb2.ParseLeaseRequest(ctx)
	if !ok {
		return nil
	}

	return &lr
}

// startBreak moves the lease to being cut back to the given state, and hands back the channel
// that is closed once the break is over, together with whether this call is the one that
// started it: a break already in flight is waited for rather than sent twice.
func (l *lease) startBreak(to uint32) (chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// A state that keeps everything the lease already holds takes nothing away, so there is
	// nothing to tell the client about.
	if l.state == smb2.SMB2_LEASE_NONE || l.state&^to == 0 {
		return nil, false
	}
	if l.breaking {
		// A target that takes nothing beyond what the break in flight is already taking is
		// covered by that break, and is simply waited for. Otherwise a second conflict wants
		// the lease cut back further, so the client is told again, at a fresh epoch, and
		// answering the first notification is no longer good enough — as with an oplock.
		if l.breakToState&^to == 0 {
			return l.breakDone, false
		}

		l.breakToState &= to
		l.epoch++
		return l.breakDone, true
	}

	l.breaking = true
	l.breakToState = to
	l.epoch++
	l.breakDone = make(chan struct{})

	return l.breakDone, true
}

// completeBreak ends a break that is in flight and releases whoever was waiting for it. Only
// the first call has any effect: the acknowledgment of the client, the expiry of the wait and
// the departure of the last open all race to end the same break.
func (l *lease) completeBreak(state uint32) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.breaking {
		return false
	}

	l.state = state
	l.breaking = false
	close(l.breakDone)
	l.breakDone = nil

	return true
}

// clientConnections lists the connections a client is reachable over. A lease belongs to the
// client rather than to any one of its sessions, so a break may travel over any of them.
func (s *server) clientConnections(guid [16]byte) []*connection {
	s.mu.Lock()
	defer s.mu.Unlock()

	var conns []*connection
	for _, c := range s.connectionList {
		if len(c.clientGuid) == 16 && [16]byte(c.clientGuid) == guid {
			conns = append(conns, c)
		}
	}

	return conns
}

// sendLeaseBreak tells the client to cut its lease back, and starts the clock on the
// acknowledgment. A client that cannot be reached on any of its connections loses the lease
// straight away: it cannot be caching on the strength of a promise the server has no way of
// withdrawing.
func (s *server) sendLeaseBreak(l *lease) {
	l.mu.Lock()
	key, guid := l.leaseKey, l.clientGuid
	current, to, epoch := l.state, l.breakToState, l.epoch
	path := l.fileName
	opens := make([]*open, 0, len(l.opens))
	for _, op := range l.opens {
		opens = append(opens, op)
	}
	l.mu.Unlock()

	// The session is only needed to encrypt the message where the client asked for encryption;
	// the notification itself names no session at all.
	var ss *session
	for _, op := range opens {
		op.mu.Lock()
		ss = op.session
		op.mu.Unlock()
		if ss != nil {
			break
		}
	}

	// A lease that keeps nothing back has nothing left to acknowledge with, so the client is
	// told and the break is over. Anything else has to be confirmed before it takes effect.
	ackRequired := current != smb2.SMB2_LEASE_READ_CACHING
	notification := smb2.NewLeaseBreakNotification(key, current, to, epoch, ackRequired)

	var sent bool
	for _, conn := range s.clientConnections(guid) {
		if s.trySendResponse(conn, ss, notification) {
			sent = true
			break
		}
	}

	if !sent {
		if s.debug {
			log.Printf("Lease break on %s could not be delivered, revoking it", path)
		}
		l.completeBreak(smb2.SMB2_LEASE_NONE)
		return
	}

	if !ackRequired {
		l.completeBreak(to)
		return
	}

	time.AfterFunc(s.leaseBreakTimeout, func() {
		if l.completeBreak(smb2.SMB2_LEASE_NONE) && s.debug {
			log.Printf("Lease break on %s was not acknowledged in time", path)
		}
	})
}

// grantLease gives the open the lease its client asked for, and returns the state granted. A
// lease is only granted when every other open on the file is one of the client's own under the
// same key, which is what makes a create by anybody else the only thing that can conflict with
// it.
//
// As with an oplock, the decision and the granting happen under the one lock, so that two
// creates racing for the same file cannot both come out of it holding a promise. If the file
// turns out to have been opened elsewhere, nothing is granted and whoever holds a promise on it
// has to give it up.
func (s *server) grantLease(op *open, l *lease, requested uint32, tc *treeConnect, path string) uint32 {
	s.cachingMu.Lock()

	others := s.opensOn(tc.share, path, op)

	// A client asking for a lease on a file it already holds an oplock on through another handle is
	// asking about its own view of the file, so that oplock is left standing.
	op.mu.Lock()
	guid := op.clientGuid
	op.mu.Unlock()

	oplocks, leases := holdersIn(others, asker{guid: guid, own: l, keyed: true})

	// The file is the client's own, so it may be promised in full. Otherwise nothing that lets
	// the client write can be promised, but a read cache still can, as long as nobody else is
	// holding more than one.
	granted := requested & leaseStates
	if len(opensOutside(others, l)) > 0 {
		if exclusiveHeld(oplocks, leases) {
			granted = smb2.SMB2_LEASE_NONE
		} else {
			granted &^= smb2.SMB2_LEASE_WRITE_CACHING
		}
	}

	if granted != smb2.SMB2_LEASE_NONE {
		l.join(s.leaseTableFor(l.clientGuid), op, granted)
		s.cachingMu.Unlock()

		return granted
	}

	_, notify := startBreaks(oplocks, smb2.OPLOCK_LEVEL_NONE)
	_, leaseNotify := startLeaseBreaks(leases, false)
	s.cachingMu.Unlock()

	// The break is not waited for here: this runs on the goroutine that serves the connection,
	// and a promise made this late was made while the file stood free, so its holder cannot be
	// sitting on anything the create should have seen.
	go func() {
		defer recoverGoroutine("sending the breaks")

		for _, other := range notify {
			s.sendOplockBreak(other)
		}
		for _, other := range leaseNotify {
			s.sendLeaseBreak(other)
		}
	}()

	return smb2.SMB2_LEASE_NONE
}

// startLeaseBreaks cuts every lease back to nothing, and returns what it takes to finish the
// job: the channels to wait on, and the leases that still have to be told. s.cachingMu must be
// held, so that the leases cannot change hands while they are collected.
func startLeaseBreaks(leases []*lease, sharedOK bool) (waits []chan struct{}, notify []*lease) {
	for _, l := range leases {
		ch, started := l.startBreak(leaseBreakTarget(l.stateNow(), sharedOK))
		if ch == nil {
			continue
		}
		waits = append(waits, ch)
		if started {
			notify = append(notify, l)
		}
	}

	return
}

// acknowledgeLeaseBreak takes the answer of a client to a break that is in flight and returns
// the status to reply with, together with the state the client is left holding.
func (s *server) acknowledgeLeaseBreak(guid, key [16]byte, state uint32) (uint32, uint32) {
	l := s.findLease(guid, key)
	if l == nil {
		return smb2.STATUS_OBJECT_NAME_NOT_FOUND, smb2.SMB2_LEASE_NONE
	}

	l.mu.Lock()
	breaking := l.breaking
	to := l.breakToState
	opens := make([]*open, 0, len(l.opens))
	for _, op := range l.opens {
		opens = append(opens, op)
	}
	l.mu.Unlock()

	// Answering a break counts as using the handles the lease covers, so the creates that made
	// them can no longer be replayed. A lease break arrives without a FileId, so this is the
	// one path that does not go past findOpen.
	for _, op := range opens {
		s.clearReplayEligible(op)
	}

	// Nothing is being broken, so the acknowledgment answers a break that never happened.
	if !breaking {
		return smb2.STATUS_UNSUCCESSFUL, smb2.SMB2_LEASE_NONE
	}

	// The client may only keep what it was told it could keep.
	if state&^to != 0 {
		return smb2.STATUS_REQUEST_NOT_ACCEPTED, smb2.SMB2_LEASE_NONE
	}

	l.completeBreak(state)

	return smb2.STATUS_OK, state
}
