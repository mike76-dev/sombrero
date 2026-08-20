package main

import (
	"bytes"
	"log"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// createWithOptions opens a file with particular create options, which is what carries the
// request to take the file away when the handle goes.
func (cl *testClient) createWithOptions(name string, disposition, options uint32) []byte {
	cl.h.t.Helper()

	cl.mid++
	msg := createRequestWithOptions(cl.mid, cl.ss.sessionID, cl.tc.treeID, name,
		smb2.OPLOCK_LEVEL_NONE, disposition, shareAccess, options, nil)

	resp, err := cl.send(msg)
	if err != nil {
		cl.h.t.Fatalf("create of %s: %v", name, err)
	}

	buf := resp.Encode()
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		cl.h.t.Fatalf("create of %s was answered with %#x", name, status)
	}

	return buf
}

// TestCreateOptionsDoNotOutliveTheHandleThatCarriedThem is the file that a handle deletes without
// ever having been asked to.
func TestCreateOptionsDoNotOutliveTheHandleThatCarriedThem(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	// The destination of a copy, opened the way a client opens one it may have to abandon.
	cl.createWithOptions("clip.mp4", smb2.FILE_CREATE, smb2.FILE_DELETE_ON_CLOSE)

	// The same name opened again, this time by a handle that asks for nothing of the sort. The
	// file is not in the store yet, so this is the open the tree connect kept aside.
	second := cl.createWithOptions("clip.mp4", smb2.FILE_OPEN_IF, 0)

	// The data goes up while that handle is held, which is what puts the file in the store.
	h.files.put("clip.mp4", 1<<20)

	if _, err := cl.closeHandle(createdFileID(second)); err != nil {
		t.Fatalf("the close of the handle failed: %v", err)
	}

	if !h.files.has("clip.mp4") {
		t.Error("closing a handle that never asked for the file to be deleted deleted it")
	}
}

// TestDeleteOnCloseStillDeletes is the other side of it: the flag still does what it says for the
// handle that carried it, so that the options are refreshed rather than dropped.
func TestDeleteOnCloseStillDeletes(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	created := cl.createWithOptions("clip.mp4", smb2.FILE_CREATE, smb2.FILE_DELETE_ON_CLOSE)
	h.files.put("clip.mp4", 1<<20)

	if _, err := cl.closeHandle(createdFileID(created)); err != nil {
		t.Fatalf("the close of the handle failed: %v", err)
	}

	if h.files.has("clip.mp4") {
		t.Error("the handle that asked for the file to be deleted when it closed left it behind")
	}
}

// TestDeletingAFileThatWasNeverUploadedIsNotAnError is the file created and closed again with
// nothing written to it in between. Nothing empty can be uploaded, so such a file lives only as
// the entry the tree connect keeps under its name, and the backend has nothing to delete when the
// handle goes with delete-on-close set.
func TestDeletingAFileThatWasNeverUploadedIsNotAnError(t *testing.T) {
	var out bytes.Buffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := newSMBTest(t)
	cl := h.dial("alice")

	created := cl.createWithOptions("clip.mp4", smb2.FILE_CREATE, smb2.FILE_DELETE_ON_CLOSE)

	buf, err := cl.closeHandle(createdFileID(created))
	if err != nil {
		t.Fatalf("the close of the handle failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close was answered with %#x", status)
	}

	if logged := out.String(); strings.Contains(logged, "Error deleting object") {
		t.Errorf("the log reports a failure over a file that was never in the store: %s", strings.TrimSpace(logged))
	}
}

// TestDeleteThatFailsIsStillReported holds the line above to only the answer it is meant to let
// through: a backend that could not do the deletion for any other reason is still complained about.
func TestDeleteThatFailsIsStillReported(t *testing.T) {
	var out bytes.Buffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := newSMBTest(t)
	cl := h.dial("alice")

	created := cl.createWithOptions("clip.mp4", smb2.FILE_CREATE, smb2.FILE_DELETE_ON_CLOSE)
	h.files.put("clip.mp4", 1<<20)
	h.files.failDeletion(errNoObject)

	if _, err := cl.closeHandle(createdFileID(created)); err != nil {
		t.Fatalf("the close of the handle failed: %v", err)
	}

	if logged := out.String(); !strings.Contains(logged, "Error deleting object") {
		t.Error("a deletion the backend refused went unreported")
	}
}

// TestCancelledCopyLeavesNothingBehind is the copy a client gives up on while something else on the
// machine - a scanner, a preview, the shell itself - is holding the destination open as well. The
// upload belongs to the file rather than to either handle, and the handle that carries the deletion
// is not the last one, so the upload used to be left running: the deletion took a file that was not
// on the share yet, which is nothing at all, and the other handle's close then stored the half of
// the file that had been written before the client gave up.
func TestCancelledCopyLeavesNothingBehind(t *testing.T) {
	h := newSMBTest(t)
	copier, other := h.dial("alice"), h.dial("alice")

	copying := copier.createWithOptions("clip.mp4", smb2.FILE_CREATE, 0)
	held, _ := other.create("clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second handle was answered with %#x", status)
	}

	// Half of the file goes up, and then the client cancels: the copy is marked for deletion and
	// its handle closes, while the other handle stays where it is.
	if _, err := copier.write(createdFileID(copying), 0, []byte("the first half")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	if _, err := copier.markForDeletion(createdFileID(copying)); err != nil {
		t.Fatalf("marking the copy for deletion failed: %v", err)
	}
	if _, err := copier.closeHandle(createdFileID(copying)); err != nil {
		t.Fatalf("the close of the cancelled copy failed: %v", err)
	}

	if h.files.has("clip.mp4") {
		t.Fatal("the cancelled copy is in the store")
	}

	// Whatever the handle that is still open does, the half-written file does not appear.
	if _, err := other.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close of the other handle failed: %v", err)
	}

	if h.files.has("clip.mp4") {
		t.Errorf("the cancelled copy came back as %q", string(h.files.dataOf("clip.mp4")))
	}
}

// TestAnAbandonedUploadIsNotLeftOnTheShare is the copy that was interrupted and then disconnected
// from. Nothing finishes such an upload, so it is called off when the handle is torn down and the
// bytes it was carrying are gone from the backend - but the state that stood for the file stayed on
// the share, and the state alone is what a file not yet in the store is listed out of. The client
// came back to a file that exists nowhere.
func TestAnAbandonedUploadIsNotLeftOnTheShare(t *testing.T) {
	h := newSMBTest(t)
	h.files.putDir("docs")

	cl := h.dial("alice")
	fid := createdFileID(cl.createWithOptions("docs/clip.mp4", smb2.FILE_CREATE, 0))
	if _, err := cl.write(fid, 0, []byte("the first half")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	// The client goes away without ever closing the handle, which is what the share is left with
	// when a copy is interrupted and the client disconnects.
	h.srv.closeConnection(cl.conn)

	if h.files.has("docs/clip.mp4") {
		t.Fatal("the interrupted copy is in the store")
	}

	// It is not on the share either, so the client that comes back is not shown a file that
	// nothing holds the bytes of.
	again := h.dial("alice")
	if _, found := again.tc.persistedFile("docs/clip.mp4"); found {
		t.Error("the abandoned copy is still on the share")
	}

	listing := listedNames(t, again.queryDirectory(createdFileID(again.openDir("docs")), "*"))
	if slices.Contains(listing, "clip.mp4") {
		t.Errorf("the listing carries the abandoned copy: %v", listing)
	}
}

// TestAFileMadeAndNeverWrittenSurvivesTheDisconnect holds the line above to the uploads it is meant
// for. A file created and never written to has no upload to call off and nothing of it is anywhere
// else, so it stays where it is: taking it away would lose the only record that it exists.
func TestAFileMadeAndNeverWrittenSurvivesTheDisconnect(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	h.srv.closeConnection(cl.conn)

	again := h.dial("alice")
	if _, found := again.tc.persistedFile("notes.txt"); !found {
		t.Error("the file the client made is gone, and nothing else knows it exists")
	}
}
