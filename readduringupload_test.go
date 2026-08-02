package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestReadDuringUploadIsServedFromTheUploadBuffer is the client reading a file it is in the middle
// of writing.
//
// A file being uploaded is not an object in the store: what has gone up so far are the parts of a
// multipart upload, and the store has nothing to hand back until that upload is completed, which
// happens when the file is closed. The read used to go to the store all the same, which answered
// that the object did not exist - "couldn't fetch object: object not found" - and the client was
// told the read failed on the device.
//
// The data the client is asking about is the data the client just sent, and the upload is still
// holding it, so that is what answers the read.
func TestReadDuringUploadIsServedFromTheUploadBuffer(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	created := cl.createWithOptions("clip.mp4", smb2.FILE_CREATE, 0)
	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]

	data := bytes.Repeat([]byte("abcdefgh"), 128) // 1 KiB
	if err := file.write(0, data); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	got, err := file.read(0, uint64(len(data)))
	if err != nil {
		t.Fatalf("the read of a file being uploaded failed: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("the read gave back something other than the data that was written")
	}

	// A range inside the file rather than the whole of it, which is the shape of the read a
	// client does before rounding out a write that does not sit on a block boundary.
	got, err = file.read(64, 128)
	if err != nil {
		t.Fatalf("the read of part of a file being uploaded failed: %v", err)
	}
	if !bytes.Equal(got, data[64:192]) {
		t.Error("the partial read gave back the wrong bytes")
	}
}

// TestReadOfWhatTheUploadNoLongerHoldsIsNotAStoreFailure is the range that has already gone up as
// a part of the multipart upload and is no longer in memory. Nothing can answer it until the file
// is closed, and that is a property of the upload rather than a failure of the store: it is told
// apart so that the read path does not report the store as broken over it.
func TestReadOfWhatTheUploadNoLongerHoldsIsNotAStoreFailure(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	created := cl.createWithOptions("clip.mp4", smb2.FILE_CREATE, 0)
	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]

	data := bytes.Repeat([]byte("x"), 1024)
	if err := file.write(0, data); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	// The buffer is made to look like one whose earlier part has already been sent, which is what
	// it looks like once a file large enough to fill a part has gone through it.
	file.mu.Lock()
	u := file.pendingUpload
	file.mu.Unlock()

	u.mu.Lock()
	u.buf = u.buf[512:]
	u.bufOffset = 512
	u.mu.Unlock()

	if _, err := file.read(0, 256); !errors.Is(err, errNotUploaded) {
		t.Errorf("the read of a range the upload no longer holds failed with %v, want it told apart", err)
	}

	// What the upload does still hold is answered as before.
	got, err := file.read(512, 256)
	if err != nil {
		t.Fatalf("the read of a range the upload still holds failed: %v", err)
	}
	if !bytes.Equal(got, data[512:768]) {
		t.Error("the read gave back the wrong bytes")
	}
}
