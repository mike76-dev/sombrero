package main

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A client that is told to give up an oplock or a lease has to say that it has. A client that
// says nothing at all cannot be allowed to hold the file for ever, so the promise is withdrawn
// once the acknowledgment timer runs out and whoever was waiting carries on.
//
// This is the case where the client is still there and still reachable - it simply does not
// answer. It is what separates the timer from the other two ways a break ends: the holder going
// away, and the break never being deliverable in the first place. Both of those release the file
// at once, so the wait is the only thing that tells them apart, and each test below asserts both
// that the notification arrived and that the waiting really happened.

// breakTimeout is short enough to keep the tests quick and long enough that a break released at
// once is unmistakably faster.
const breakTimeout = 300 * time.Millisecond

func TestIntegrationOplockGoesWhenTheClientNeverAnswers(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	timeout := h.impatient(breakTimeout)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("alice was granted %#x rather than a batch oplock", level)
	}

	h.srv.mu.Lock()
	aliceOpen := h.srv.globalOpenTable[binary.LittleEndian.Uint64(createdFileID(held)[8:16])]
	h.srv.mu.Unlock()
	if aliceOpen == nil {
		t.Fatal("the open alice was granted is not in the global table")
	}

	// Alice hears the break and says nothing. Her connection stays up throughout, so an
	// undeliverable break is not what is being measured here.
	arrived := make(chan []byte, 1)
	go func() {
		select {
		case note := <-alice.sent:
			arrived <- note
		case <-time.After(20 * time.Second):
			arrived <- nil
		}
	}()

	bob := h.dial("bob")

	start := time.Now()
	buf, async := bob.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	waited := time.Since(start)

	note := <-arrived
	if note == nil {
		t.Fatal("the break never reached alice, so nothing was waiting on her answer")
	}
	if fid := brokenFileID(note); !bytes.Equal(fid, createdFileID(held)) {
		t.Errorf("the break names % x, want alice's open % x", fid, createdFileID(held))
	}

	if !async {
		t.Error("bob's create did not wait for the break at all")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}

	// The lower bound is the point of the test: a break given up on, or ended by the holder
	// going away, would have let bob through immediately.
	if waited < timeout {
		t.Errorf("bob waited only %v, want at least the %v alice had to answer in", waited, timeout)
	}
	// The upper bound catches the timer running on the 35-second constant rather than on what
	// the server was told to use.
	if waited > 5*timeout {
		t.Errorf("bob waited %v, far longer than the %v alice had to answer in", waited, timeout)
	}

	aliceOpen.mu.Lock()
	defer aliceOpen.mu.Unlock()
	if aliceOpen.oplockState != smb2.OplockNone || aliceOpen.oplockLevel != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("the open that never answered is still in state %d holding %#x",
			aliceOpen.oplockState, aliceOpen.oplockLevel)
	}
	if aliceOpen.oplockBreak != nil {
		t.Error("the break outlived the timer that ended it")
	}
}

func TestIntegrationLeaseGoesWhenTheClientNeverAnswers(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	timeout := h.impatient(breakTimeout)

	alice := h.dial("alice")
	held, _ := alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, _ := createdLeaseState(held); state != rwh {
		t.Fatalf("alice was granted %#x rather than a full lease", state)
	}

	l := h.srv.findLease([16]byte(alice.conn.clientGuid), aliceKey)
	if l == nil {
		t.Fatal("alice holds no lease")
	}

	arrived := make(chan []byte, 1)
	go func() {
		select {
		case note := <-alice.sent:
			arrived <- note
		case <-time.After(20 * time.Second):
			arrived <- nil
		}
	}()

	bob := h.dial("bob")

	start := time.Now()
	buf, async := bob.createLeased("dir/file", bobKey, rwh, 2, smb2.FILE_OPEN)
	waited := time.Since(start)

	note := <-arrived
	if note == nil {
		t.Fatal("the break never reached alice, so nothing was waiting on her answer")
	}
	if !isLeaseBreak(note) {
		t.Errorf("what arrived was not a lease break")
	}

	if !async {
		t.Error("bob's create did not wait for the break at all")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}

	if waited < timeout {
		t.Errorf("bob waited only %v, want at least the %v alice had to answer in", waited, timeout)
	}
	if waited > 5*timeout {
		t.Errorf("bob waited %v, far longer than the %v alice had to answer in", waited, timeout)
	}

	if state := l.stateNow(); state != smb2.SMB2_LEASE_NONE {
		t.Errorf("the lease of a client that never answered holds %#x, want none", state)
	}
}
