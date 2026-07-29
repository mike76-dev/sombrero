package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// Level II oplocks and read-caching leases are the shared promise: several clients may hold one
// at once, and what breaks them is not another client opening the file but somebody changing it.

func TestIntegrationReadingCreateDowngradesExclusiveOplock(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("alice was granted %#x rather than a batch oplock", level)
	}

	bob := h.dial("bob")

	// Alice keeps her read cache: bob only means to read the file, so all she has to give up is
	// what let her hold writes back. She answers by coming down to level II.
	type answer struct {
		note []byte
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
		_, a.err = alice.ackBreak(brokenFileID(a.note), smb2.OPLOCK_LEVEL_II)
		answered <- a
	}()

	buf, _ := bob.createReading("dir/file", smb2.OPLOCK_LEVEL_II)

	a := <-answered
	if a.note == nil {
		t.Fatal("alice was never told to come down from her batch oplock")
	}
	if a.err != nil {
		t.Fatalf("alice could not acknowledge the break: %v", a.err)
	}

	// The break asks for level II rather than for nothing, which is what lets alice keep the
	// read cache she has built up.
	if level := a.note[smb2.SMB2HeaderSize+2]; level != smb2.OPLOCK_LEVEL_II {
		t.Errorf("the break asked alice down to %#x, want level II", level)
	}
	if !bytes.Equal(brokenFileID(a.note), createdFileID(held)) {
		t.Error("the break names an open other than alice's")
	}

	// Both of them end up holding a read cache, which is the point of the level existing.
	h.srv.mu.Lock()
	aliceOpen := h.srv.globalOpenTable[openIDOf(createdFileID(held))]
	h.srv.mu.Unlock()
	if aliceOpen == nil {
		t.Fatal("alice's open is not in the global table")
	}

	aliceOpen.mu.Lock()
	level, state := aliceOpen.oplockLevel, aliceOpen.oplockState
	aliceOpen.mu.Unlock()
	if level != smb2.OPLOCK_LEVEL_II || state != smb2.OplockHeld {
		t.Errorf("alice is left with %#x in state %d, want level II held", level, state)
	}
	if got := createdOplockLevel(buf); got != smb2.OPLOCK_LEVEL_II {
		t.Errorf("bob was granted %#x, want level II", got)
	}
}

func TestIntegrationReadingCreateLeavesSharedHoldersAlone(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.createReading("dir/file", smb2.OPLOCK_LEVEL_II)

	// Two readers on the same file have nothing to take from each other, so neither is told
	// anything and neither create has to wait.
	bob := h.dial("bob")
	buf, async := bob.createReading("dir/file", smb2.OPLOCK_LEVEL_II)

	if async {
		t.Error("a reader was made to wait although nothing had to be given up")
	}
	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_II {
		t.Errorf("the second reader was granted %#x, want level II", level)
	}
	alice.quiet(200*time.Millisecond, "a reader was told to give up its read cache for another reader")
}

func TestIntegrationWriteBreaksSharedOplock(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createReading("dir/file", smb2.OPLOCK_LEVEL_II)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_II {
		t.Fatalf("alice was granted %#x rather than level II", level)
	}

	// Bob opens the file to write to it, which alice's read cache survives: nothing has changed
	// yet. It is the write itself that makes what she has cached wrong.
	bob := h.dial("bob")
	bobHeld, _ := bob.create("dir/file", smb2.OPLOCK_LEVEL_II, smb2.FILE_OPEN)
	alice.quiet(200*time.Millisecond, "opening the file for writing broke a read cache before anything was written")

	if _, err := bob.write(createdFileID(bobHeld), 0, []byte("hello")); err != nil {
		t.Fatalf("bob could not write: %v", err)
	}

	// A read cache has no level below it to argue about, so alice is told and that is the end
	// of it: no acknowledgment, and nothing waits for one.
	note := alice.recv(10 * time.Second)
	if level := note[smb2.SMB2HeaderSize+2]; level != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("the break asked alice down to %#x, want none", level)
	}
	if !bytes.Equal(brokenFileID(note), createdFileID(held)) {
		t.Error("the break names an open other than alice's")
	}

	h.srv.mu.Lock()
	aliceOpen := h.srv.globalOpenTable[openIDOf(createdFileID(held))]
	h.srv.mu.Unlock()

	// The break is over the moment it has been sent, so alice holds nothing without ever
	// having answered.
	deadline := time.Now().Add(5 * time.Second)
	for {
		aliceOpen.mu.Lock()
		state := aliceOpen.oplockState
		aliceOpen.mu.Unlock()
		if state == smb2.OplockNone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alice is still in state %d, want none", state)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Bob keeps his own read cache: he knows what he wrote.
	h.srv.mu.Lock()
	bobOpen := h.srv.globalOpenTable[openIDOf(createdFileID(bobHeld))]
	h.srv.mu.Unlock()

	bobOpen.mu.Lock()
	defer bobOpen.mu.Unlock()
	if bobOpen.oplockLevel != smb2.OPLOCK_LEVEL_II {
		t.Errorf("the writer was left with %#x, want its own level II", bobOpen.oplockLevel)
	}
}

func TestIntegrationReadingCreateDowngradesLease(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, _ := createdLeaseState(held); state != rwh {
		t.Fatalf("alice was granted %#x rather than a full lease", state)
	}

	bob := h.dial("bob")

	answered := make(chan []byte, 1)
	go func() {
		select {
		case note := <-alice.sent:
			alice.ackLeaseBreak(brokenLeaseKey(note), rh)
			answered <- note
		case <-time.After(20 * time.Second):
			answered <- nil
		}
	}()

	buf, _ := bob.createLeasedReading("dir/file", bobKey, rwh)

	note := <-answered
	if note == nil {
		t.Fatal("alice was never told to cut her lease back")
	}

	// The break takes the write cache and leaves the rest, so alice goes on caching reads and
	// the handle she has open.
	current, granted, ackRequired := brokenLeaseStates(note)
	if current != rwh {
		t.Errorf("CurrentLeaseState = %#x, want %#x", current, rwh)
	}
	if granted != rh {
		t.Errorf("NewLeaseState = %#x, want %#x", granted, rh)
	}
	if !ackRequired {
		t.Error("a break that leaves the client something to cache was sent without asking for an acknowledgment")
	}

	l := h.srv.findLease([16]byte(alice.conn.clientGuid), aliceKey)
	if l == nil {
		t.Fatal("alice holds no lease")
	}
	if state := l.stateNow(); state != rh {
		t.Errorf("alice is left holding %#x, want %#x", state, rh)
	}
	if state, _ := createdLeaseState(buf); state != rh {
		t.Errorf("bob was granted %#x, want %#x", state, rh)
	}
}

func TestIntegrationWriteBreaksSharedLease(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.createLeasedReading("dir/file", aliceKey, rh)

	l := h.srv.findLease([16]byte(alice.conn.clientGuid), aliceKey)
	if l == nil || l.stateNow() != rh {
		t.Fatalf("alice does not hold a read and handle caching lease")
	}

	bob := h.dial("bob")
	bobHeld, _ := bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	alice.quiet(200*time.Millisecond, "opening the file for writing broke a lease before anything was written")

	if _, err := bob.write(createdFileID(bobHeld), 0, []byte("hello")); err != nil {
		t.Fatalf("bob could not write: %v", err)
	}

	note := alice.recv(10 * time.Second)
	if !isLeaseBreak(note) {
		t.Error("alice was sent an oplock break rather than a lease break")
	}

	// Everything goes: the handle alice was caching now points at a file whose contents she has
	// no idea about.
	_, granted, ackRequired := brokenLeaseStates(note)
	if granted != smb2.SMB2_LEASE_NONE {
		t.Errorf("NewLeaseState = %#x, want none", granted)
	}

	// Handle caching has to be given up in so many words. The client may be holding the file
	// open for an application that has already closed it, and the server has to know that it
	// really has let go before the lease is written off.
	if !ackRequired {
		t.Fatal("a break of a handle-caching lease was sent without asking for an acknowledgment")
	}
	if l.stateNow() != rh {
		t.Error("the lease was written off before the client had answered")
	}

	if _, err := alice.ackLeaseBreak(brokenLeaseKey(note), smb2.SMB2_LEASE_NONE); err != nil {
		t.Fatalf("alice could not acknowledge the break: %v", err)
	}
	if state := l.stateNow(); state != smb2.SMB2_LEASE_NONE {
		t.Errorf("alice is still holding %#x, want none", state)
	}
}

func TestIntegrationWriteBreaksReadOnlyLeaseWithoutAnAnswer(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.createLeasedReading("dir/file", aliceKey, smb2.SMB2_LEASE_READ_CACHING)

	l := h.srv.findLease([16]byte(alice.conn.clientGuid), aliceKey)
	if l == nil || l.stateNow() != smb2.SMB2_LEASE_READ_CACHING {
		t.Fatalf("alice does not hold a read caching lease")
	}

	bob := h.dial("bob")
	bobHeld, _ := bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if _, err := bob.write(createdFileID(bobHeld), 0, []byte("hello")); err != nil {
		t.Fatalf("bob could not write: %v", err)
	}

	note := alice.recv(10 * time.Second)

	// A lease that only promises a read cache has nothing below it to argue about, so the
	// client is told and the break is over without an answer.
	_, _, ackRequired := brokenLeaseStates(note)
	if ackRequired {
		t.Error("a break of a read-only lease asked for an acknowledgment")
	}

	deadline := time.Now().Add(5 * time.Second)
	for l.stateNow() != smb2.SMB2_LEASE_NONE {
		if time.Now().After(deadline) {
			t.Fatalf("alice is still holding %#x, want none", l.stateNow())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIntegrationWriteWithoutAccessBreaksNothing(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.createReading("dir/file", smb2.OPLOCK_LEVEL_II)

	// Bob may only read this share, so the write through his handle is refused. Nothing changes,
	// and alice has no reason to hear about it.
	//
	// The access of a handle comes from the share rather than from what the create asked for,
	// so it is the tree connect that has to be read-only here.
	bob := h.dial("bob")
	bob.tc.maximalAccess = readAccess
	bobHeld, _ := bob.createReading("dir/file", smb2.OPLOCK_LEVEL_NONE)

	buf, err := bob.write(createdFileID(bobHeld), 0, []byte("hello"))
	if err != nil {
		t.Fatalf("the write was not answered: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_ACCESS_DENIED {
		t.Fatalf("the write returned %#x, want access denied", status)
	}

	alice.quiet(200*time.Millisecond, "a write that was refused broke a read cache anyway")
}
