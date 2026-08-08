package main

import (
	"bytes"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// A file goes to the backend as one multipart upload, which takes the bytes in the order they are to
// be stored. A client that writes them in some other order — every client with more than one write
// outstanding at a time — leaves the upload holding what it cannot take yet, and a file that could
// not be joined up at the close used to be refused outright: the client was told its copy had failed
// after sending the whole of it.

// TestIntegrationAnOverlappingWriteOutOfOrderIsStillJoinedUp is the queued write that begins behind
// where the buffering has reached. Writes are worked on one goroutine apiece, so two the client sent
// in order can be taken in either order, and the second may be queued before the first has been taken
// in.
func TestIntegrationAnOverlappingWriteOutOfOrderIsStillJoinedUp(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// The second write is sent first and overlaps the first: it begins at 4 while nothing has been
	// taken in yet, so it is queued, and what fills the gap in front of it reaches past where it
	// begins.
	if _, err := cl.write(fid, 4, []byte("45678901")); err != nil {
		t.Fatalf("the write ahead failed: %v", err)
	}
	if _, err := cl.write(fid, 0, []byte("012345")); err != nil {
		t.Fatalf("the write behind failed: %v", err)
	}

	closed, err := cl.closeHandle(fid)
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close was answered with %#x, want the file stored", status)
	}

	if got := string(h.files.dataOf("notes.txt")); got != "012345678901" {
		t.Errorf("the store holds %q, want the file as it was written", got)
	}
}

// TestIntegrationAFileWithAHoleInItIsStoredAsZeros is the file the client never wrote the whole of.
// Nothing in flight and a gap still queued means the client wrote a file with nothing in that part of
// it, which on any file system reads as zeros. There are no holes to be had in an object on either
// backend, so the zeros are written: refusing the file loses everything the client did send.
func TestIntegrationAFileWithAHoleInItIsStoredAsZeros(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	handle, _ := cl.create("sparse.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	head := bytes.Repeat([]byte("h"), 16)
	tail := bytes.Repeat([]byte("t"), 16)
	if _, err := cl.write(fid, 0, head); err != nil {
		t.Fatalf("the write of the head failed: %v", err)
	}

	// Nothing is ever written between the two, which is the hole.
	if _, err := cl.write(fid, 48, tail); err != nil {
		t.Fatalf("the write of the tail failed: %v", err)
	}

	closed, err := cl.closeHandle(fid)
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close was answered with %#x, want the file stored", status)
	}

	want := append(append(append([]byte{}, head...), make([]byte, 32)...), tail...)
	if got := h.files.dataOf("sparse.bin"); !bytes.Equal(got, want) {
		t.Errorf("the store holds %d bytes %q, want the %d written with the hole as zeros",
			len(got), got, len(want))
	}
}

// TestIntegrationAHoleThatIsFilledBeforeTheCloseIsNotZeroed is the ordinary case the one above must
// not spoil: a gap is only the client's business until the writes stop arriving. Filled in time, the
// file is what was written and nothing is zeroed.
func TestIntegrationAHoleThatIsFilledBeforeTheCloseIsNotZeroed(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	if _, err := cl.write(fid, 8, []byte("89ab")); err != nil {
		t.Fatalf("the write ahead failed: %v", err)
	}
	if _, err := cl.write(fid, 0, []byte("01234567")); err != nil {
		t.Fatalf("the write behind failed: %v", err)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	if got := string(h.files.dataOf("notes.txt")); got != "0123456789ab" {
		t.Errorf("the store holds %q, want the file as it was written", got)
	}
}
