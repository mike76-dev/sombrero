package main

import (
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
