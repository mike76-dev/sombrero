package main

import (
	"encoding/binary"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestLockOnAHandleThatIsGoneIsRefused is the lock that names no open. Nothing is locked here, and
// nothing has to be, but [MS-SMB2] 3.3.5.14 still has the handle looked up and answers the request
// with STATUS_FILE_CLOSED when it names none. A success in its place tells the client that a range
// of a file it no longer holds is now its own, which is the one answer that cannot be true.
func TestLockOnAHandleThatIsGoneIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	// A handle that is open is locked over, so that what follows is refused for the handle rather
	// than for the lock.
	buf, err := cl.lockRange(fid, 0, 512)
	if err != nil {
		t.Fatalf("the lock failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("a lock over an open handle was answered %#x, want it granted", status)
	}

	// The volatile half of a handle that exists, with a persistent half that belongs to nothing.
	forged := make([]byte, 16)
	copy(forged, fid)
	binary.LittleEndian.PutUint64(forged[8:], binary.LittleEndian.Uint64(fid[8:])^0xdeadbeef)

	buf, err = cl.lockRange(forged, 0, 512)
	if err != nil {
		t.Fatalf("the lock failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("a lock on a mismatched handle was answered %#x, want STATUS_FILE_CLOSED", status)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	buf, err = cl.lockRange(fid, 0, 512)
	if err != nil {
		t.Fatalf("the lock failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("a lock on a closed handle was answered %#x, want STATUS_FILE_CLOSED", status)
	}
}
