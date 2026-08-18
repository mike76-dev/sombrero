package main

import (
	"encoding/binary"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestFlushChecksTheHandleItIsGiven is what an SMB2_FLUSH is answered with. The lookup of the open
// was made only to decide whether there was anything to wait for, so a flush that named no open at
// all was still answered with a success. [MS-SMB2] 3.3.5.11 refuses that one with
// STATUS_FILE_CLOSED, and refuses a handle without write access with STATUS_ACCESS_DENIED; a
// client told its writes are safe when the server never had a handle to make them safe through has
// been given a promise nobody kept.
func TestFlushChecksTheHandleItIsGiven(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	buf, err := cl.flushHandle(fid)
	if err != nil {
		t.Fatalf("the flush failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("a flush of an open handle was answered %#x, want it served", status)
	}

	// The volatile half of a handle that exists, with a persistent half that belongs to nothing.
	forged := make([]byte, 16)
	copy(forged, fid)
	binary.LittleEndian.PutUint64(forged[8:], binary.LittleEndian.Uint64(fid[8:])^0xdeadbeef)

	buf, err = cl.flushHandle(forged)
	if err != nil {
		t.Fatalf("the flush failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("a flush of a mismatched handle was answered %#x, want STATUS_FILE_CLOSED", status)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	buf, err = cl.flushHandle(fid)
	if err != nil {
		t.Fatalf("the flush failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("a flush of a closed handle was answered %#x, want STATUS_FILE_CLOSED", status)
	}

	// A handle with no write access has nothing of its own to make safe. The access an open is
	// granted comes from the tree connect, so that is where the writing is taken away.
	cl.tc.maximalAccess = readAccess

	reading, err := cl.createAccessing("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, nil)
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}

	buf, err = cl.flushHandle(createdFileID(reading))
	if err != nil {
		t.Fatalf("the flush failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_ACCESS_DENIED {
		t.Errorf("a flush of a read-only handle was answered %#x, want STATUS_ACCESS_DENIED", status)
	}
}
