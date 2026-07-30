package main

import (
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// Marking a file for deletion does not delete it: it says what is to happen when the last handle
// on it goes. Until then the client may change its mind, which is what DeleteFile set to zero
// says ([MS-FSCC] 2.4.11).

func TestIntegrationDeletionMarkDeletesOnClose(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	if _, err := alice.markForDeletion(createdFileID(held)); err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}

	// Nothing has happened to the file yet: the handle is still open.
	if !h.files.has("dir/file") {
		t.Error("the file was deleted while a handle on it was still open")
	}

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if h.files.has("dir/file") {
		t.Error("the file was still there after the last handle on it was closed")
	}
}

func TestIntegrationDeletionCanBeCalledOff(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	if _, err := alice.markForDeletion(createdFileID(held)); err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}

	resp, err := alice.keepFile(createdFileID(held))
	if err != nil {
		t.Fatalf("calling the deletion off did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("calling the deletion off returned %#x", status)
	}

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if !h.files.has("dir/file") {
		t.Error("the file was deleted although the client had called the deletion off")
	}
}

// A create that takes the file out to delete it is subject to the same change of mind.
func TestIntegrationDeleteOnCloseCreateCanBeCalledOff(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeasedWithOptions("dir/file", aliceKey, rwh, smb2.FILE_DELETE_ON_CLOSE)

	if _, err := alice.keepFile(createdFileID(held)); err != nil {
		t.Fatalf("calling the deletion off did not come back: %v", err)
	}
	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}

	if !h.files.has("dir/file") {
		t.Error("the file was deleted although the client had called the deletion off")
	}
}

// The lease key follows the file rather than the request that named it: a file that is staying
// after all keeps its key to itself.
func TestIntegrationCalledOffDeletionTiesTheKeyBack(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/one", 1024)
	h.files.put("dir/two", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeasedWithOptions("dir/one", aliceKey, rwh, smb2.FILE_DELETE_ON_CLOSE)
	if state, found := createdLeaseState(held); !found || state == smb2.SMB2_LEASE_NONE {
		t.Fatalf("alice was granted %#x, want a lease", state)
	}

	if _, err := alice.keepFile(createdFileID(held)); err != nil {
		t.Fatalf("calling the deletion off did not come back: %v", err)
	}

	buf, _ := alice.createLeased("dir/two", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("the key was still free after the deletion was called off: got %#x, want invalid parameter", status)
	}
}

// DeleteFile is a boolean, so the file goes on anything other than zero rather than on one
// exactly.
func TestIntegrationDeletionMarkTakesAnyNonZero(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	resp, err := alice.setInfo(createdFileID(held), smb2.FileDispositionInformation, []byte{2})
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the deletion mark returned %#x", status)
	}

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if h.files.has("dir/file") {
		t.Error("the file was still there although the client had asked for it to go")
	}
}

// The disposition is a single byte, which the server reads without checking for. What keeps that
// safe is the buffer of an SMB2_SET_INFO being required to hold something at all, well before
// the info class is looked at - so this pins the check the disposition relies on rather than
// anything the disposition does itself.
func TestIntegrationEmptyDispositionIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	resp, err := alice.setInfo(createdFileID(held), smb2.FileDispositionInformation, nil)
	if err != nil {
		t.Fatalf("the request did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("an empty disposition returned %#x, want invalid parameter", status)
	}

	// The file is untouched either way.
	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if !h.files.has("dir/file") {
		t.Error("a request the server refused deleted the file anyway")
	}
}
