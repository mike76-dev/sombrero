package main

import (
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

var (
	rwh = uint32(smb2.SMB2_LEASE_READ_CACHING | smb2.SMB2_LEASE_HANDLE_CACHING | smb2.SMB2_LEASE_WRITE_CACHING)
	rh  = uint32(smb2.SMB2_LEASE_READ_CACHING | smb2.SMB2_LEASE_HANDLE_CACHING)

	aliceKey = [16]byte{0xa1, 0x1c, 0xe0}
	bobKey   = [16]byte{0xb0, 0xb0, 0xb0}
)

// newLeasedOpen builds an open already holding a lease on the file, as a create would have left
// it, together with the connection carrying it and what its client would have received.
func newLeasedOpen(t *testing.T, s *server, sh *share, path string, key [16]byte, state uint32) (*open, *lease, *connection, chan []byte) {
	t.Helper()

	op, c, sent := newOplockOpen(t, s, sh, path)

	l, matches := s.leaseFor([16]byte(c.clientGuid), smb2.LeaseRequest{LeaseKey: key, Version: 2}, path)
	if !matches {
		t.Fatalf("the lease key is already in use for another file")
	}
	l.join(s.leaseTableFor(l.clientGuid), op, state)

	return op, l, c, sent
}

func TestLeaseStartBreak(t *testing.T) {
	t.Run("a lease that holds nothing has nothing to give up", func(t *testing.T) {
		l := &lease{state: smb2.SMB2_LEASE_NONE, opens: make(map[uint64]*open)}
		if ch, started := l.startBreak(smb2.SMB2_LEASE_NONE); ch != nil || started {
			t.Error("a break was started on a lease that promises nothing")
		}
	})

	t.Run("a lease already at the state asked for is left alone", func(t *testing.T) {
		l := &lease{state: rh, opens: make(map[uint64]*open)}
		if ch, started := l.startBreak(rh); ch != nil || started {
			t.Error("a break was started on a lease that is already where it should be")
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.breaking {
			t.Error("the lease was left breaking")
		}
	})

	t.Run("a break already in flight is waited for rather than sent twice", func(t *testing.T) {
		l := &lease{state: rwh, opens: make(map[uint64]*open)}

		first, started := l.startBreak(smb2.SMB2_LEASE_NONE)
		if first == nil || !started {
			t.Fatal("the first break was not started")
		}

		// A second create arriving while the first break is outstanding waits on the same
		// break instead of telling the client twice about it.
		second, started := l.startBreak(smb2.SMB2_LEASE_NONE)
		if second != first {
			t.Error("the second caller was given a different break to wait on")
		}
		if started {
			t.Error("the second caller was told it had started the break")
		}

		// The epoch moves once per break, not once per caller: the client uses it to tell a
		// stale state change from a fresh one.
		l.mu.Lock()
		epoch := l.epoch
		l.mu.Unlock()
		if epoch != 1 {
			t.Errorf("epoch = %d, want 1 after a single break", epoch)
		}
	})

	t.Run("a deeper conflict escalates the break in flight", func(t *testing.T) {
		l := &lease{state: rwh, opens: make(map[uint64]*open)}

		// A create that only reads starts a break that lets the client keep its read and
		// handle caches.
		first, started := l.startBreak(rh)
		if first == nil || !started {
			t.Fatal("the first break was not started")
		}

		// A create that changes the file needs everything gone, which is further than the
		// break in flight goes: the client has to be told again, and answering the first
		// notification is no longer good enough.
		second, started := l.startBreak(smb2.SMB2_LEASE_NONE)
		if second != first {
			t.Error("the escalated break is not the one already in flight")
		}
		if !started {
			t.Error("the deeper cut was not sent to the client")
		}

		l.mu.Lock()
		to, epoch := l.breakToState, l.epoch
		l.mu.Unlock()
		if to != smb2.SMB2_LEASE_NONE {
			t.Errorf("breakToState = %#x, want SMB2_LEASE_NONE", to)
		}
		if epoch != 2 {
			t.Errorf("epoch = %d, want 2 after the second notification", epoch)
		}

		// A third conflict no deeper than the escalated break changes nothing and waits with
		// everybody else.
		third, started := l.startBreak(rh)
		if third != first {
			t.Error("the third caller was given a different break to wait on")
		}
		if started {
			t.Error("a conflict the break already covers was sent to the client again")
		}
	})
}

func TestGrantLease(t *testing.T) {
	t.Run("a client that has the file to itself is granted what it asked for", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		op, c, _ := newOplockOpen(t, s, sh, "dir/file")
		l, _ := s.leaseFor([16]byte(c.clientGuid), smb2.LeaseRequest{LeaseKey: aliceKey, Version: 2}, "dir/file")

		if got := s.grantLease(op, l, rwh, op.treeConnect, "dir/file"); got != rwh {
			t.Errorf("granted %#x, want %#x", got, rwh)
		}
		if state := l.stateNow(); state != rwh {
			t.Errorf("the lease holds %#x, want %#x", state, rwh)
		}
	})

	t.Run("another open of the same client shares the lease", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		first, l, c, sent := newLeasedOpen(t, s, sh, "dir/file", aliceKey, rwh)

		// A second open of the same client under the same key is not somebody else on the
		// file, so the lease stands and nobody is told anything.
		second, _, _ := newOplockOpen(t, s, sh, "dir/file")
		second.connection = c

		if got := s.grantLease(second, l, rwh, second.treeConnect, "dir/file"); got != rwh {
			t.Errorf("granted %#x to a second open of the same lease, want %#x", got, rwh)
		}

		select {
		case <-sent:
			t.Error("a client was told to break its own lease")
		case <-time.After(100 * time.Millisecond):
		}

		if l.stateNow() != rwh {
			t.Error("the lease was cut back when its own client opened the file again")
		}
		_ = first
	})

	t.Run("an open that finds another client gets nothing and breaks it", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		_, held, _, sent := newLeasedOpen(t, s, sh, "dir/file", aliceKey, rwh)

		// Another client appears on the file after the lease was granted, which is the race the
		// grant has to close: the newcomer gets nothing, and the holder is told.
		latecomer, c, _ := newOplockOpen(t, s, sh, "dir/file")
		mine, _ := s.leaseFor([16]byte(c.clientGuid), smb2.LeaseRequest{LeaseKey: bobKey, Version: 2}, "dir/file")

		if got := s.grantLease(latecomer, mine, rwh, latecomer.treeConnect, "dir/file"); got != smb2.SMB2_LEASE_NONE {
			t.Errorf("granted %#x to a client sharing the file, want none", got)
		}

		buf := recvBreak(t, sent)
		if !isLeaseBreak(buf) {
			t.Error("the holder was sent an oplock break rather than a lease break")
		}
		if key := brokenLeaseKey(buf); key != aliceKey {
			t.Errorf("the break names % x, want the holder's key % x", key, aliceKey)
		}

		held.mu.Lock()
		defer held.mu.Unlock()
		if !held.breaking {
			t.Error("the holder was not left breaking")
		}
	})
}

func TestGrantOplockBreaksLease(t *testing.T) {
	sh := &share{name: "files"}
	s := newCachingServer()

	_, held, _, sent := newLeasedOpen(t, s, sh, "dir/file", aliceKey, rwh)

	// An oplock and a lease are different ways of promising the same thing, so an open asking
	// for one has to break the other.
	latecomer, _, _ := newOplockOpen(t, s, sh, "dir/file")
	if got := s.grantOplock(latecomer, smb2.OPLOCK_LEVEL_BATCH, latecomer.treeConnect, "dir/file"); got != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("granted %#x on a file held under a lease, want none", got)
	}

	buf := recvBreak(t, sent)
	if !isLeaseBreak(buf) {
		t.Error("the lease holder was sent an oplock break rather than a lease break")
	}

	held.mu.Lock()
	defer held.mu.Unlock()
	if !held.breaking {
		t.Error("the lease was not broken by an open asking for an oplock")
	}
}

func TestSendLeaseBreakToUnreachableClient(t *testing.T) {
	sh := &share{name: "files"}
	s := newCachingServer()

	_, l, c, _ := newLeasedOpen(t, s, sh, "dir/file", aliceKey, rwh)

	// The connection is gone, so the notification can be delivered nowhere. Waiting out the
	// acknowledgment timer for a client that will never answer would hold the file hostage for
	// the length of it.
	close(c.closeChan)

	wait, started := l.startBreak(smb2.SMB2_LEASE_NONE)
	if !started {
		t.Fatal("the break was not started")
	}

	done := make(chan struct{})
	go func() {
		s.sendLeaseBreak(l)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the send outlived a client that could not be reached")
	}

	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("whoever was waiting for the break was never released")
	}

	if l.stateNow() != smb2.SMB2_LEASE_NONE {
		t.Error("the lease of an unreachable client was left in place")
	}
}

func TestIntegrationLeaseGranted(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run("version "+string(rune('0'+version)), func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("dir/file", 1024)

			alice := h.dial("alice")
			buf, async := alice.createLeased("dir/file", aliceKey, rwh, version, smb2.FILE_OPEN)

			if async {
				t.Error("a create with no lease to break was answered asynchronously")
			}
			if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
				t.Fatalf("Status = %#x, want %#x", status, smb2.STATUS_OK)
			}

			// A granted lease is announced by the oplock level, and the state comes back in a
			// context of its own.
			if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_LEASE {
				t.Errorf("OplockLevel = %#x, want %#x", level, smb2.OPLOCK_LEVEL_LEASE)
			}
			state, found := createdLeaseState(buf)
			if !found {
				t.Fatal("the response carried no lease context")
			}
			if state != rwh {
				t.Errorf("LeaseState = %#x, want %#x", state, rwh)
			}
		})
	}
}

func TestIntegrationLeaseStatesGranted(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	h.files.put("dir/other", 1024)

	alice := h.dial("alice")

	// Read and handle caching without write caching is the shared promise, and is granted on
	// its own terms: several clients may hold one at once.
	buf, _ := alice.createLeased("dir/file", aliceKey, rh, 2, smb2.FILE_OPEN)
	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_LEASE {
		t.Errorf("OplockLevel = %#x, want %#x", level, smb2.OPLOCK_LEVEL_LEASE)
	}
	if state, _ := createdLeaseState(buf); state != rh {
		t.Errorf("LeaseState = %#x, want %#x", state, rh)
	}

	// A client that asks for nothing gets nothing, and is told so rather than being left to
	// guess from the absence of a context.
	empty, _ := alice.createLeased("dir/other", bobKey, smb2.SMB2_LEASE_NONE, 2, smb2.FILE_OPEN)
	if level := createdOplockLevel(empty); level != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("OplockLevel = %#x, want none", level)
	}
	state, found := createdLeaseState(empty)
	if !found {
		t.Fatal("a client that asked for a lease was given no answer about one")
	}
	if state != smb2.SMB2_LEASE_NONE {
		t.Errorf("LeaseState = %#x, want none", state)
	}
}

func TestIntegrationSameClientSharesLease(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	first, _ := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, _ := createdLeaseState(first); state != rwh {
		t.Fatalf("the first open was granted %#x rather than a full lease", state)
	}

	// The same client opens the same file again under the same key. This is what a lease is
	// for: the two opens share it, so there is nothing to break and nothing to wait for, and
	// the second open keeps the same promise as the first.
	second, async := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	if async {
		t.Error("a client was made to wait for a break of its own lease")
	}
	if level := createdOplockLevel(second); level != smb2.OPLOCK_LEVEL_LEASE {
		t.Errorf("OplockLevel = %#x, want %#x", level, smb2.OPLOCK_LEVEL_LEASE)
	}
	if state, _ := createdLeaseState(second); state != rwh {
		t.Errorf("the second open was granted %#x, want the lease it already held", state)
	}

	alice.quiet(200*time.Millisecond, "a client was told to break its own lease")

	if !h.srv.hasHoldersOn(h.share, "dir/file", nil, asker{}) {
		t.Error("the lease was given up when the client opened the file a second time")
	}
}

func TestIntegrationAnotherClientBreaksLease(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, _ := createdLeaseState(held); state != rwh {
		t.Fatalf("alice was granted %#x rather than a full lease", state)
	}

	bob := h.dial("bob")

	// Bob opens the file to overwrite it, which is a create that changes the file by itself, so
	// alice has to give up the whole lease rather than come down to a read cache.
	type answer struct {
		note []byte
		resp []byte
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		var a answer
		select {
		case a.note = <-alice.sent:
		case <-time.After(20 * time.Second):
			answered <- a
			return
		}
		a.resp, a.err = alice.ackLeaseBreak(brokenLeaseKey(a.note), smb2.SMB2_LEASE_NONE)
		answered <- a
	}()

	buf, async := bob.createLeased("dir/file", bobKey, rwh, 2, smb2.FILE_OVERWRITE)

	a := <-answered
	if a.note == nil {
		t.Fatal("alice was never told to give up her lease")
	}
	if a.err != nil {
		t.Fatalf("alice could not acknowledge the break: %v", a.err)
	}

	// The break is about a lease, names alice's key, and asks her to confirm before it takes
	// effect. It names no session: a lease belongs to the client, not to one of its sessions.
	if !isLeaseBreak(a.note) {
		t.Error("alice was sent an oplock break rather than a lease break")
	}
	if key := brokenLeaseKey(a.note); key != aliceKey {
		t.Errorf("the break names % x, want alice's key % x", key, aliceKey)
	}
	if sid := smb2.Header(a.note).SessionID(); sid != 0 {
		t.Errorf("the break names session %#x, want none", sid)
	}

	current, granted, ackRequired := brokenLeaseStates(a.note)
	if current != rwh {
		t.Errorf("CurrentLeaseState = %#x, want %#x", current, rwh)
	}
	if granted != smb2.SMB2_LEASE_NONE {
		t.Errorf("NewLeaseState = %#x, want none", granted)
	}
	if !ackRequired {
		t.Error("a break of a write-caching lease was sent without asking for an acknowledgment")
	}

	if status := smb2.Header(a.resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("the acknowledgment was refused with %#x", status)
	}

	// Bob's create could not be answered until the break was over. He asked for the full lease
	// and cannot have the write cache, because alice still has the file open, but with nobody
	// holding one he keeps the read and handle caches.
	if !async {
		t.Error("bob's create was answered without waiting for the break")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}
	if state, _ := createdLeaseState(buf); state != rh {
		t.Errorf("bob was granted %#x while alice had the file open, want %#x", state, rh)
	}
}

func TestIntegrationLeaseAndOplockBreakEachOther(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	// Alice holds a lease; bob asks for an oplock on the same file. The two promises are made
	// in different ways but mean the same thing, so neither may stand while the other does.
	alice := h.dial("alice")
	alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	bob := h.dial("bob")

	answered := make(chan []byte, 1)
	go func() {
		select {
		case note := <-alice.sent:
			alice.ackLeaseBreak(brokenLeaseKey(note), smb2.SMB2_LEASE_NONE)
			answered <- note
		case <-time.After(20 * time.Second):
			answered <- nil
		}
	}()

	buf, async := bob.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)

	note := <-answered
	if note == nil {
		t.Fatal("a create asking for an oplock did not break the lease on the file")
	}
	if !isLeaseBreak(note) {
		t.Error("alice was sent an oplock break rather than a lease break")
	}
	if !async {
		t.Error("bob's create was answered without waiting for the lease break")
	}
	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_II {
		t.Errorf("bob was granted %#x while alice had the file open, want level II", level)
	}
}

func TestIntegrationLeaseKeyBoundToOneFile(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	h.files.put("dir/other", 1024)

	alice := h.dial("alice")
	alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	// The same key on another file is the one thing a client may not do with a lease key: the
	// key is what the server matches a break acknowledgment against.
	buf, _ := alice.createLeased("dir/other", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("Status = %#x, want %#x", status, smb2.STATUS_INVALID_PARAMETER)
	}

	// The refusal must leave nothing behind: the file it was refused on stays free.
	if h.srv.hasHoldersOn(h.share, "dir/other", nil, asker{}) {
		t.Error("a refused create left a promise on the file")
	}
}

func TestIntegrationLeaseOutlivesOneOfItsOpens(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	first, _ := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	second, _ := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	l := h.srv.findLease([16]byte(alice.conn.clientGuid), aliceKey)
	if l == nil {
		t.Fatal("alice holds no lease")
	}

	// Closing one of the two handles leaves the lease standing: the client is still caching
	// the file through the other one.
	if _, err := alice.closeHandle(createdFileID(first)); err != nil {
		t.Fatalf("alice could not close the first handle: %v", err)
	}
	if state := l.stateNow(); state != rwh {
		t.Errorf("the lease holds %#x after one of two handles closed, want %#x", state, rwh)
	}
	if !h.srv.hasHoldersOn(h.share, "dir/file", nil, asker{}) {
		t.Error("the lease was given up while an open still shared it")
	}

	// Closing the last one leaves nothing to cache through, so the lease promises nothing.
	if _, err := alice.closeHandle(createdFileID(second)); err != nil {
		t.Fatalf("alice could not close the second handle: %v", err)
	}
	if state := l.stateNow(); state != smb2.SMB2_LEASE_NONE {
		t.Errorf("the lease holds %#x after its last handle closed, want none", state)
	}
	if h.srv.hasHoldersOn(h.share, "dir/file", nil, asker{}) {
		t.Error("the lease outlived the last open that shared it")
	}
}

// leaseBreakEnding drives a lease break to its end by some means other than an acknowledgment,
// and measures how long the create that was waiting for it had to wait. The lease has to be
// given up wherever its opens go, or the create sits out the whole acknowledgment timer for a
// client that is never going to answer.
func leaseBreakEnding(t *testing.T, end func(h *smbTest, alice *testClient, held []byte)) {
	t.Helper()

	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, _ := createdLeaseState(held); state != rwh {
		t.Fatalf("alice was granted %#x rather than a full lease", state)
	}

	l := h.srv.findLease([16]byte(alice.conn.clientGuid), aliceKey)
	if l == nil {
		t.Fatal("alice holds no lease")
	}

	bob := h.dial("bob")

	// Alice is told to give up the lease and never answers.
	told := make(chan struct{})
	go func() {
		<-alice.sent
		end(h, alice, held)
		close(told)
	}()

	start := time.Now()
	buf, async := bob.createLeased("dir/file", bobKey, rwh, 2, smb2.FILE_OPEN)
	waited := time.Since(start)

	<-told

	if !async {
		t.Error("bob's create did not wait for the break at all")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}
	if waited > leaseBreakTimeout/4 {
		t.Errorf("bob waited %v for a holder that had gone, want the break to end with the opens", waited)
	}
	if state := l.stateNow(); state != smb2.SMB2_LEASE_NONE {
		t.Errorf("the lease of a client that went away holds %#x, want none", state)
	}
}

func TestIntegrationLeaseBreakEndsWhenHandleIsClosed(t *testing.T) {
	// Closing the handle rather than acknowledging is what a client holding a handle-caching
	// lease does when it was keeping the file open and has no further use for it.
	leaseBreakEnding(t, func(_ *smbTest, alice *testClient, held []byte) {
		if _, err := alice.closeHandle(createdFileID(held)); err != nil {
			t.Errorf("alice could not close the handle: %v", err)
		}
	})
}

func TestIntegrationLeaseBreakEndsWhenSessionDies(t *testing.T) {
	// Losing the connection tears the session down. Alice is gone and will never answer.
	leaseBreakEnding(t, func(h *smbTest, alice *testClient, _ []byte) {
		h.srv.deregisterSession(alice.conn, alice.ss.sessionID)
	})
}

func TestIntegrationLeaseBreakAcknowledgment(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	// Nothing is being broken yet, so there is nothing to acknowledge.
	resp, err := alice.ackLeaseBreak(aliceKey, smb2.SMB2_LEASE_NONE)
	if err != nil {
		t.Fatalf("the acknowledgment was not answered: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_UNSUCCESSFUL {
		t.Errorf("an unsolicited acknowledgment gave %#x, want %#x", status, smb2.STATUS_UNSUCCESSFUL)
	}

	// A key nobody holds a lease under is not a lease at all.
	resp, err = alice.ackLeaseBreak(bobKey, smb2.SMB2_LEASE_NONE)
	if err != nil {
		t.Fatalf("the acknowledgment was not answered: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OBJECT_NAME_NOT_FOUND {
		t.Errorf("an unknown lease key gave %#x, want %#x", status, smb2.STATUS_OBJECT_NAME_NOT_FOUND)
	}

	// A client may not keep more than the break left it.
	l := h.srv.findLease([16]byte(alice.conn.clientGuid), aliceKey)
	if l == nil {
		t.Fatal("alice holds no lease")
	}
	if _, started := l.startBreak(smb2.SMB2_LEASE_NONE); !started {
		t.Fatal("the lease could not be broken")
	}

	resp, err = alice.ackLeaseBreak(aliceKey, rwh)
	if err != nil {
		t.Fatalf("the acknowledgment was not answered: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_REQUEST_NOT_ACCEPTED {
		t.Errorf("keeping more than was offered gave %#x, want %#x", status, smb2.STATUS_REQUEST_NOT_ACCEPTED)
	}

	// The break is still outstanding after a refused acknowledgment, and a proper one ends it.
	resp, err = alice.ackLeaseBreak(aliceKey, smb2.SMB2_LEASE_NONE)
	if err != nil {
		t.Fatalf("the acknowledgment was not answered: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("a proper acknowledgment gave %#x, want success", status)
	}
	if h.srv.hasHoldersOn(h.share, "dir/file", nil, asker{}) {
		t.Error("the lease survived the break it acknowledged")
	}
}
