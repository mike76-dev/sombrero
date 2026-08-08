package main

import (
	"bytes"
	"log"
	"os"
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
