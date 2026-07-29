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

func TestIntegrationLeaseNeedsWriteCaching(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")

	// Read and handle caching without write caching is the shared promise: several clients may
	// hold it at once and it has to be broken on every write, which the server does not do.
	buf, _ := alice.createLeased("dir/file", aliceKey, rh, 2, smb2.FILE_OPEN)

	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("OplockLevel = %#x, want none", level)
	}

	// The client is still answered, so that it learns it was given nothing rather than being
	// left to guess from the absence of a context.
	state, found := createdLeaseState(buf)
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

	if !h.srv.hasHoldersOn(h.share, "dir/file", nil, nil) {
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

	buf, async := bob.createLeased("dir/file", bobKey, rwh, 2, smb2.FILE_OPEN)

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

	// Bob's create could not be answered until the break was over, and gets no lease of its
	// own: alice still has the file open.
	if !async {
		t.Error("bob's create was answered without waiting for the break")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}
	if state, _ := createdLeaseState(buf); state != smb2.SMB2_LEASE_NONE {
		t.Errorf("bob was granted %#x while alice had the file open, want none", state)
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
	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("bob was granted %#x while alice had the file open, want none", level)
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
	if h.srv.hasHoldersOn(h.share, "dir/other", nil, nil) {
		t.Error("a refused create left a promise on the file")
	}
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
	if h.srv.hasHoldersOn(h.share, "dir/file", nil, nil) {
		t.Error("the lease survived the break it acknowledged")
	}
}
