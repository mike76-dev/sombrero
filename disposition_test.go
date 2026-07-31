package main

import (
	"errors"
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

// A directory is only marked for deletion if it is empty ([MS-FSCC] 2.4.11): what would otherwise
// happen on the close is the silent loss of everything inside it. The check belongs to the
// marking rather than to the close, so it is the marking that is refused.

func TestIntegrationFullDirectoryIsNotMarkedForDeletion(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held := alice.openDir("dir")

	resp, err := alice.markForDeletion(createdFileID(held))
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_DIRECTORY_NOT_EMPTY {
		t.Errorf("marking a directory holding a file returned %#x, want directory not empty", status)
	}

	// The refusal has to be the end of it. A mark that took effect anyway would take the
	// directory and the file with it on the close.
	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if !h.files.has("dir") {
		t.Error("the directory was deleted although the mark had been refused")
	}
	if !h.files.has("dir/file") {
		t.Error("the file inside the directory went with it")
	}
}

// The positive control: the same request against a directory with nothing in it goes through, so
// that the refusal above is known to come from the contents rather than from the request.
func TestIntegrationEmptyDirectoryIsDeletedOnClose(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")

	alice := h.dial("alice")
	held := alice.openDir("dir")

	resp, err := alice.markForDeletion(createdFileID(held))
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("marking an empty directory returned %#x", status)
	}

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if h.files.has("dir") {
		t.Error("the directory was still there after the last handle on it was closed")
	}
}

// A directory whose name merely starts another one's is not inside it, so it does not keep it
// from going. This is the boundary the trailing slash on the path draws.
func TestIntegrationSiblingDirectoryDoesNotCountAsContents(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")
	h.files.put("dirty", 1024)

	alice := h.dial("alice")
	held := alice.openDir("dir")

	resp, err := alice.markForDeletion(createdFileID(held))
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("marking a directory with only a like-named sibling returned %#x", status)
	}
}

// The emptiness of a directory cannot be guessed at. A store that will not say is answered with a
// failure rather than with a deletion that may be taking files with it.
func TestIntegrationUnreadableDirectoryIsNotMarkedForDeletion(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")

	alice := h.dial("alice")
	held := alice.openDir("dir")

	h.files.failEmptiness(errors.New("the store is not answering"))

	resp, err := alice.markForDeletion(createdFileID(held))
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_NETWORK_NAME_DELETED {
		t.Errorf("marking a directory the store would not read returned %#x, want network name deleted", status)
	}

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if !h.files.has("dir") {
		t.Error("the directory went although the server never learned whether it was empty")
	}
}

// Refusing the mark is not enough on its own. The deletion happens at the close, and what was
// true when it was asked for need not still be true by then: the mark may have been given while
// the directory was empty, and the create options are a second way to ask that never passes the
// marking at all. So the close checks again, and quietly declines rather than orphaning whatever
// is inside.

func TestIntegrationFullDirectorySurvivesADeleteOnCloseCreate(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")
	h.files.put("dir/file", 1024)

	// The create carries the deletion in its options, so no mark is ever sent and nothing refuses
	// it up front. The create is not the place to say no: a local file system takes it and settles
	// the question at the close.
	alice := h.dial("alice")
	held := alice.openDirWithOptions("dir", smb2.FILE_DELETE_ON_CLOSE)

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}

	if !h.files.has("dir") {
		t.Error("the directory was deleted although it was not empty")
	}
	if !h.files.has("dir/file") {
		t.Error("the file inside the directory was orphaned")
	}
}

func TestIntegrationDirectoryThatFillsUpIsNotDeleted(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")

	alice := h.dial("alice")
	held := alice.openDir("dir")

	// The mark is given while there is nothing to stop it.
	resp, err := alice.markForDeletion(createdFileID(held))
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("marking an empty directory returned %#x", status)
	}

	// Something lands in the directory before the handle goes, which is what makes the answer
	// given at the marking stale.
	h.files.put("dir/file", 1024)

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}

	if !h.files.has("dir") {
		t.Error("the directory was deleted although it had filled up since the mark")
	}
	if !h.files.has("dir/file") {
		t.Error("the file put into the directory was orphaned")
	}
}

// The positive control for the create-options route: an empty directory asked for the same way
// does go, so the two above are known to turn on the contents rather than on the route.
func TestIntegrationEmptyDirectoryGoesOnADeleteOnCloseCreate(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")

	alice := h.dial("alice")
	held := alice.openDirWithOptions("dir", smb2.FILE_DELETE_ON_CLOSE)

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}

	if h.files.has("dir") {
		t.Error("the directory was still there after the handle asking for it to go was closed")
	}
}

// A store that will not say whether the directory is empty leaves it where it is, for the same
// reason as at the marking: there is no deleting it on a guess.
func TestIntegrationUnreadableDirectoryIsNotDeletedOnClose(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("dir")

	alice := h.dial("alice")
	held := alice.openDirWithOptions("dir", smb2.FILE_DELETE_ON_CLOSE)

	h.files.failEmptiness(errors.New("the store is not answering"))

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}

	if !h.files.has("dir") {
		t.Error("the directory went although the server never learned whether it was empty")
	}
}

// A file is deleted on the close without any of this, however much is in the store beside it.
// Failing the emptiness check shows the file never goes near it.
func TestIntegrationFileGoesOnCloseWithoutTheEmptinessCheck(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	if _, err := alice.markForDeletion(createdFileID(held)); err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}

	h.files.failEmptiness(errors.New("the store is not answering"))

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}

	if h.files.has("dir/file") {
		t.Error("the file was still there after the last handle on it was closed")
	}
}

// Only a directory is held to any of this. A file is never asked about, which is shown by marking
// one while the check is failing: a file that went through is a file the check was never put to.
func TestIntegrationFileIsMarkedWithoutTheEmptinessCheck(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	h.files.failEmptiness(errors.New("the store is not answering"))

	resp, err := alice.markForDeletion(createdFileID(held))
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("marking a file returned %#x, want the emptiness check not to have been reached", status)
	}

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close did not come back: %v", err)
	}
	if h.files.has("dir/file") {
		t.Error("the file was still there after the last handle on it was closed")
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
