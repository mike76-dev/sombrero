package main

import (
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// A handle is on the file it was opened on, not on the name it was opened under, so it follows the
// file wherever the file goes. A client that deletes a file it still has open renames it aside first
// and goes on using the handle it kept, which is where this matters.

// TestIntegrationEveryHandleFollowsARename is the delete-while-open dance. A client that deletes a
// file it still has open renames it aside first — a macOS client to ".smbdeleteAAA…" — and goes on
// using the handle it kept. A handle is on the file and not on the name, so it has to follow: left
// pointing at the old name, its next read reaches for an object the backend has nothing under, which
// is answered as an I/O error and ends whatever the client was doing.
func TestIntegrationEveryHandleFollowsARename(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("._clip.mp4", []byte("attributes and a resource fork"))

	cl := h.dial("alice")

	// One handle reads the file; another renames it aside, as a delete does.
	reader, _ := cl.create("._clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(reader).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the read handle was answered with %#x", status)
	}
	mover, _ := cl.create("._clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(mover).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the renaming handle was answered with %#x", status)
	}

	renamed, err := cl.rename(createdFileID(mover), ".smbdeleteAAA6e181a29c3affc7b")
	if err != nil {
		t.Fatalf("the rename failed outright: %v", err)
	}
	if status := smb2.Header(renamed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the rename was answered with %#x", status)
	}

	// The handle that was kept is on the file, wherever the file has gone.
	kept := h.srv.globalOpenTable[openIDOf(createdFileID(reader))]
	if kept == nil {
		t.Fatal("the read handle has no open behind it")
	}
	kept.mu.Lock()
	where := kept.pathName
	kept.mu.Unlock()
	if where != ".smbdeleteAAA6e181a29c3affc7b" {
		t.Errorf("the handle that was kept points at %q, want it to have followed the file", where)
	}

	read, err := cl.readOver(createdFileID(reader), 64, smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read failed outright: %v", err)
	}
	if status := smb2.Header(read).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the read through the kept handle was answered with %#x, want the file it is on", status)
	}
	if got := string(readData(t, read)); got != "attributes and a resource fork" {
		t.Errorf("the kept handle reads %q, want what the file holds", got)
	}
}

// TestIntegrationADirectoryHoldsNoBytes is the share root that grew. A directory has no size of its
// own, but the key it is kept under may have one at the backend, and the root's came back as the size
// of whatever had last been written into it: six kilobytes after a .DS_Store landed, then ten, then
// fourteen. What a client makes of a directory that reports a length is its own business, and not
// something to find out the hard way.
func TestIntegrationADirectoryHoldsNoBytes(t *testing.T) {
	h := newSMBTest(t)

	// A backend that answers for the directory's key with a size, as renterd does for the root.
	h.files.putSizedDir("folder", 14336)

	cl := h.dial("alice")
	handle := cl.openDir("folder")
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the directory was answered with %#x", status)
	}

	op := h.srv.globalOpenTable[openIDOf(createdFileID(handle))]
	if op == nil {
		t.Fatal("the open of the directory is missing")
	}
	if got := op.file.sizeNow(); got != 0 {
		t.Errorf("the directory is %d bytes long, want a directory to hold none", got)
	}
}
