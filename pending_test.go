package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// Nothing the client is waiting on is left unanswered. A request the server has taken and never
// answers is one the client counts as outstanding on the connection for as long as it holds it, which
// is what stands between the client and letting the share go - and the client has no way of knowing
// the work behind it came to nothing.

// TestIntegrationAReadWhoseHandleGoesIsStillAnswered is the read that was worked out and thrown
// away. A handle may be closed while a read on it is still being served, and the answer was dropped
// on the floor: the client was left waiting for a response to a request the server had finished with.
func TestIntegrationAReadWhoseHandleGoesIsStillAnswered(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the open was answered with %#x", status)
	}

	// The read is held up in the store, so the close below reaches the handle while the read on it
	// is still being worked on.
	release := h.files.holdReads()

	fid := createdFileID(handle)
	cl.mid++
	interim, err := cl.send(readRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, 0, 12))
	if err != nil {
		t.Fatalf("the read failed outright: %v", err)
	}
	if status := interim.Header().Status(); status != smb2.STATUS_PENDING {
		t.Fatalf("the read was answered with %#x, want it taken on and worked out", status)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	release()

	// The read is answered, and with the one thing there is to say about it.
	answer := cl.recv(2 * time.Second)
	if answer == nil {
		t.Fatal("nothing came back for the read whose handle had gone")
	}
	if got := smb2.Header(answer).Command(); got != smb2.SMB2_READ {
		t.Fatalf("what came back is a response to command %d, want the read", got)
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("the read was answered with %#x, want the handle reported gone", status)
	}
}

// TestIntegrationTheInterimComesBeforeTheAnswer is the message a client cannot place. A command that
// is worked on in the background is answered twice — an interim response saying it is pending, then
// the answer — and both travel over the one queue, so the order of them is settled the moment the
// interim is put on it. The work may finish before that happens: then the client is handed the answer
// to a request it has never been told is pending, and the interim arrives afterwards for a request it
// has already finished with.
func TestIntegrationTheInterimComesBeforeTheAnswer(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	created, _ := cl.create("clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(created)

	// The write is taken on and worked out while the interim is still in hand: whatever the work
	// does, it may not answer before the interim has been queued.
	cl.mid++
	msg := writeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, 0, bytes.Repeat([]byte("s"), 4096))
	reqs, err := smb2.GetRequests(msg, 0, 0, false)
	if err != nil {
		t.Fatalf("the write did not parse: %v", err)
	}

	interim, _, err := cl.conn.processRequest(reqs[0])
	if err != nil {
		t.Fatalf("the write failed outright: %v", err)
	}
	if status := interim.Header().Status(); status != smb2.STATUS_PENDING {
		t.Fatalf("the write was answered with %#x, want it taken on", status)
	}

	// Nothing may go out before the interim is queued, however long the work takes to finish.
	cl.quiet(200*time.Millisecond, "the answer to a write before the client was told it was pending")

	// Once it is, the answer follows.
	cl.conn.interimQueued(interim.Header().MessageID())

	answer := cl.recv(2 * time.Second)
	if answer == nil {
		t.Fatal("nothing came back for the write once the interim had gone")
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_OK {
		t.Errorf("the write was answered with %#x", status)
	}
}
