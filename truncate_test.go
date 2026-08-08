package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// Setting the end of a file is how a client cuts it short: an editor saving a shorter document does
// it before it writes, and expects what was beyond the new end to be gone.

// endOfFile sets the end of the file the handle is on.
func (cl *testClient) endOfFile(fid []byte, eof uint64) ([]byte, error) {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, eof)

	return cl.setInfo(fid, smb2.FileEndOfFileInformation, buf)
}

// TestIntegrationCuttingAStoredFileShortReachesTheStore is the truncation nothing was written after.
// The store cannot shorten an object, so what is left of the file is written out again: left holding
// the longer one, the store would answer the next create with the old length and the bytes behind it,
// however short the file had been made to look while it was open.
func TestIntegrationCuttingAStoredFileShortReachesTheStore(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the open was answered with %#x", status)
	}

	set, err := cl.endOfFile(createdFileID(handle), 4)
	if err != nil {
		t.Fatalf("setting the end of the file failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_OK {
		t.Fatalf("setting the end of the file was answered with %#x", status)
	}

	// The file is four bytes long from here on, through this handle and every other.
	info := queriedInfo(t, cl.queryInfo(createdFileID(handle), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != 4 {
		t.Errorf("the file is %d bytes long, want the 4 it was cut down to", got)
	}

	read, err := cl.readOver(createdFileID(handle), 64, smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read failed outright: %v", err)
	}
	if got := string(readData(t, read)); got != "anot" {
		t.Errorf("the file reads as %q, want what is left of it", got)
	}

	if _, err := cl.closeHandle(createdFileID(handle)); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	// And the store holds what is left of it, so the next client to open the file finds it that way.
	if got := string(h.files.dataOf("notes.txt")); got != "anot" {
		t.Errorf("the store holds %q, want what is left of the file", got)
	}
}

// TestIntegrationCuttingAStoredFileToNothingTakesTheObjectAway is the truncation to zero, which is
// what a client sends before writing a file anew. Nothing empty can be stored on either backend, so
// the file becomes what an empty file is on this server: one with no object behind it, known by its
// state alone - which is what keeps it in the listings and openable.
func TestIntegrationCuttingAStoredFileToNothingTakesTheObjectAway(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the open was answered with %#x", status)
	}

	set, err := cl.endOfFile(createdFileID(handle), 0)
	if err != nil {
		t.Fatalf("setting the end of the file failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_OK {
		t.Fatalf("setting the end of the file was answered with %#x", status)
	}

	if h.files.has("notes.txt") {
		t.Error("the store still holds an object for the file that was cut down to nothing")
	}

	if _, err := cl.closeHandle(createdFileID(handle)); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	// The file is still there, and still empty: the state is the whole of it now, so it is the state
	// that has to outlive the handle.
	reopened, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(reopened).Status(); status != smb2.STATUS_OK {
		t.Fatalf("reopening the file was answered with %#x, want the empty file still there", status)
	}
	info := queriedInfo(t, cl.queryInfo(createdFileID(reopened), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != 0 {
		t.Errorf("the file is %d bytes long, want the empty file it was cut down to", got)
	}
}

// TestIntegrationCuttingShortAFileBeingWrittenCutsTheUpload is the truncation of a file that has not
// been stored yet. The bytes are in the upload, so that is where the new end has to reach: stored as
// they stood, the file would end up longer than the client made it.
func TestIntegrationCuttingShortAFileBeingWrittenCutsTheUpload(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}

	if _, err := cl.write(createdFileID(handle), 0, []byte("another test")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	set, err := cl.endOfFile(createdFileID(handle), 4)
	if err != nil {
		t.Fatalf("setting the end of the file failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_OK {
		t.Fatalf("setting the end of the file was answered with %#x", status)
	}

	// No second upload was needed for it: the file was already going up as one.
	if got := h.files.uploadsOf("notes.txt"); got != 1 {
		t.Errorf("%d uploads were started for the file, want the one the write began", got)
	}

	if _, err := cl.closeHandle(createdFileID(handle)); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	if got := string(h.files.dataOf("notes.txt")); got != "anot" {
		t.Errorf("the store holds %q, want the file as it was cut down to", got)
	}
}

// TestIntegrationCuttingShortBehindWhatIsStoredIsRefused is the one truncation this server cannot
// carry out. The parts of a multipart upload cannot be taken back, so a file cut short at a point
// that has already gone up cannot be made to end there, and saying so is the only honest answer:
// told its truncation succeeded, a client would go on to write a file that ends up longer than it is.
func TestIntegrationCuttingShortBehindWhatIsStoredIsRefused(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")

	// The parts of an upload are the size the backend takes, which is what decides how much of a
	// file is still in reach. A part small enough to fill is what puts the earlier bytes out of it.
	cl.tc.maxUploadSize = 64

	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}

	if _, err := cl.write(createdFileID(handle), 0, bytes.Repeat([]byte("s"), 128)); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	set, err := cl.endOfFile(createdFileID(handle), 32)
	if err != nil {
		t.Fatalf("setting the end of the file failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_DATA_ERROR {
		t.Errorf("setting the end of the file was answered with %#x, want the truncation refused", status)
	}

	// What was refused was not half done: the file is what it was.
	info := queriedInfo(t, cl.queryInfo(createdFileID(handle), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != 128 {
		t.Errorf("the file is %d bytes long, want the 128 that were written to it", got)
	}
}

// TestIntegrationSettingTheEndBeyondTheFileIsTheAllocation is the other direction, which is not a
// change to the contents at all. Neither backend can put a hole in an object without writing the
// whole of it out again, so an end beyond what the file holds is taken as the space it is going to
// need - which is what a client that preallocates before writing is asking for. Taken as the length,
// it would leave the file claiming bytes that nothing can answer a read for.
func TestIntegrationSettingTheEndBeyondTheFileIsTheAllocation(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the open was answered with %#x", status)
	}

	set, err := cl.endOfFile(createdFileID(handle), 4096)
	if err != nil {
		t.Fatalf("setting the end of the file failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_OK {
		t.Fatalf("setting the end of the file was answered with %#x", status)
	}

	info := queriedInfo(t, cl.queryInfo(createdFileID(handle), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[:8]); got != 4096 {
		t.Errorf("the file is allocated %d bytes, want the 4096 that were asked for", got)
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != 12 {
		t.Errorf("the file is %d bytes long, want the 12 it holds", got)
	}

	// And nothing was uploaded over it: the file is untouched until something is written.
	if got := h.files.uploadsOf("notes.txt"); got != 0 {
		t.Errorf("%d uploads were started for the file, want none", got)
	}
	if got := string(h.files.dataOf("notes.txt")); got != "another test" {
		t.Errorf("the store holds %q, want the file as it was", got)
	}
}

// TestIntegrationSettingTheEndOfFileNeedsEightBytes is the request that carries no number.
func TestIntegrationSettingTheEndOfFileNeedsEightBytes(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the open was answered with %#x", status)
	}

	set, err := cl.setInfo(createdFileID(handle), smb2.FileEndOfFileInformation, []byte{4})
	if err != nil {
		t.Fatalf("the request failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("the request was answered with %#x, want it turned away", status)
	}
}

// A file on its way out is not stored on its way out. These are the tests of a close that both
// deletes the file and has an upload of it pending, which is what a client that writes or cuts a file
// short and then deletes it leaves behind.

// TestIntegrationDeletingOnCloseStoresNothingFirst is the upload that was finished on the way to the
// deletion. It was work that could only be undone, and one more thing that could fail on a close: a
// client told its close failed keeps the handle, and a handle it will not let go of is what stands
// between it and disconnecting the share.
func TestIntegrationDeletingOnCloseStoresNothingFirst(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}

	if _, err := cl.write(createdFileID(handle), 0, []byte("another test")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	// The store will not put an upload together from here on, which is what makes the point: the
	// close must not be finishing one.
	h.files.failFinishingUploads(errors.New("the store is out of sync"))

	if _, err := cl.markForDeletion(createdFileID(handle)); err != nil {
		t.Fatalf("marking the file for deletion failed: %v", err)
	}

	closed, err := cl.closeHandle(createdFileID(handle))
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close was answered with %#x, want the handle let go of", status)
	}

	if h.files.has("notes.txt") {
		t.Error("the store holds the file that was deleted on close")
	}
	if _, found := cl.tc.persistedFile("notes.txt"); found {
		t.Error("the deleted file is still known to the share")
	}
}

// TestIntegrationASaveAfterADeletionPutsTheFileBack is the rule the two of them settle by: whoever
// saves last is what the file is. A client deleting a file another client has open takes the file
// away, and the save that comes after it puts the file back exactly as that client wrote it - the
// upload is the file's, and it is the other client's close that stores it.
func TestIntegrationASaveAfterADeletionPutsTheFileBack(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("what was there before"))

	writer, remover := h.dial("alice"), h.dial("alice")
	writing, _ := writer.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(writing).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the writer's open was answered with %#x", status)
	}
	deleting, _ := remover.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(deleting).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the remover's open was answered with %#x", status)
	}

	// The one client saves - the bytes are in the upload until its handle closes - and the other
	// deletes the file in the meantime.
	if _, err := writer.write(createdFileID(writing), 0, []byte("another test")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	if _, err := remover.markForDeletion(createdFileID(deleting)); err != nil {
		t.Fatalf("marking the file for deletion failed: %v", err)
	}
	removed, err := remover.closeHandle(createdFileID(deleting))
	if err != nil {
		t.Fatalf("the remover's close failed outright: %v", err)
	}
	if status := smb2.Header(removed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the remover's close was answered with %#x", status)
	}

	// The deletion is what has happened so far, so the file is gone.
	if h.files.has("notes.txt") {
		t.Error("the store still holds the file that was deleted")
	}

	// And then the save lands, which is the last word on the file.
	saved, err := writer.closeHandle(createdFileID(writing))
	if err != nil {
		t.Fatalf("the writer's close failed outright: %v", err)
	}
	if status := smb2.Header(saved).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the writer's close was answered with %#x, want the file stored", status)
	}

	if !h.files.has("notes.txt") {
		t.Fatal("the file the writer saved is not in the store")
	}
	if got := string(h.files.dataOf("notes.txt")); got != "another test" {
		t.Errorf("the store holds %q, want the file exactly as it was saved", got)
	}
}

// A client cuts a file short while it is being written, which is what a copy that has lost its place
// does: it rolls the file back to where it last knows it stood and carries on from there. The bytes may
// have gone to the store by then, and a part cannot be taken back - but a part left out of the
// completion was never part of the file, so a new end that falls where a part ends can be honoured.

// TestIntegrationCuttingBackToAPartBoundaryLeavesThePartsOut is that rollback. Refused, it costs the
// client the whole transfer: it is told its truncation failed and has nowhere to go from there.
func TestIntegrationCuttingBackToAPartBoundaryLeavesThePartsOut(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// Four parts, so that the file can be cut back to the end of the second.
	for i := range 4 {
		block := bytes.Repeat([]byte{byte('a' + i)}, 1024)
		if _, err := cl.write(fid, uint64(i)*1024, block); err != nil {
			t.Fatalf("the write of block %d failed: %v", i, err)
		}
	}

	set, err := cl.endOfFile(fid, 2048)
	if err != nil {
		t.Fatalf("cutting the file back failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_OK {
		t.Fatalf("cutting the file back was answered with %#x, want it carried out", status)
	}

	// The client carries on from where it rolled back to.
	if _, err := cl.write(fid, 2048, bytes.Repeat([]byte("z"), 1024)); err != nil {
		t.Fatalf("the write after the rollback failed: %v", err)
	}

	closed, err := cl.closeHandle(fid)
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close was answered with %#x, want the file stored", status)
	}

	// The file is the two parts that were kept and what was written after the rollback. The parts
	// that were left out are no part of it.
	want := append(bytes.Repeat([]byte("a"), 1024), bytes.Repeat([]byte("b"), 1024)...)
	want = append(want, bytes.Repeat([]byte("z"), 1024)...)
	if got := h.files.dataOf("big.bin"); !bytes.Equal(got, want) {
		t.Errorf("the store holds %d bytes %.8q…, want %d bytes %.8q…", len(got), got, len(want), want)
	}
}

// TestIntegrationEmptyingAFileBeingWrittenCallsOffTheUpload is the other end of it: a client that cuts
// the file to nothing while writing it. There is nothing to keep and nothing to store, so the upload is
// called off and the file is the empty one it was cut down to — which the client may then write anew.
func TestIntegrationEmptyingAFileBeingWrittenCallsOffTheUpload(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	for i := range 3 {
		if _, err := cl.write(fid, uint64(i)*1024, bytes.Repeat([]byte("s"), 1024)); err != nil {
			t.Fatalf("the write of block %d failed: %v", i, err)
		}
	}

	set, err := cl.endOfFile(fid, 0)
	if err != nil {
		t.Fatalf("emptying the file failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_OK {
		t.Fatalf("emptying the file was answered with %#x, want it carried out", status)
	}

	file := h.srv.globalOpenTable[openIDOf(fid)]
	if file == nil {
		t.Fatal("the handle has no open behind it")
	}
	if file.file.uploadNow() != nil {
		t.Error("the upload is still running after the file was emptied")
	}
	if got := file.file.sizeNow(); got != 0 {
		t.Errorf("the file is %d bytes, want the empty file it was cut down to", got)
	}

	// And the client writes it anew, as one that has started over does.
	if _, err := cl.write(fid, 0, []byte("starting over")); err != nil {
		t.Fatalf("the write after emptying the file failed: %v", err)
	}
	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}
	if got := string(h.files.dataOf("big.bin")); got != "starting over" {
		t.Errorf("the store holds %q, want what was written after the file was emptied", got)
	}
}

// TestIntegrationARollbackIntoTheLastPartIsHonoured is the rewind a copy does when it loses its
// place.
func TestIntegrationARollbackIntoTheLastPartIsHonoured(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 4096

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// Seven parts, the last of which has gone to the store.
	for i := range 7 {
		block := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		if _, err := cl.write(fid, uint64(i)*4096, block); err != nil {
			t.Fatalf("the write of block %d failed: %v", i, err)
		}
	}

	// Back by a quarter of the last part, which is where the client's rollback lands.
	const back = 7*4096 - 1024
	set, err := cl.endOfFile(fid, back)
	if err != nil {
		t.Fatalf("rolling the file back failed outright: %v", err)
	}
	if status := smb2.Header(set).Status(); status != smb2.STATUS_OK {
		t.Fatalf("rolling the file back was answered with %#x, want it carried out", status)
	}

	// And the client carries on from where it rolled back to, as it does.
	if _, err := cl.write(fid, back, bytes.Repeat([]byte("z"), 2048)); err != nil {
		t.Fatalf("the write after the rollback failed: %v", err)
	}

	closed, err := cl.closeHandle(fid)
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close was answered with %#x, want the file stored", status)
	}

	// The file is the six parts that stood, the three quarters of the seventh that were kept, and
	// what was written over the rest.
	var want []byte
	for i := range 6 {
		want = append(want, bytes.Repeat([]byte{byte('a' + i)}, 4096)...)
	}
	want = append(want, bytes.Repeat([]byte("g"), 3072)...)
	want = append(want, bytes.Repeat([]byte("z"), 2048)...)

	if got := h.files.dataOf("big.bin"); !bytes.Equal(got, want) {
		t.Errorf("the store holds %d bytes, want %d; first difference at %d", len(got), len(want), firstDifference(got, want))
	}
}

// firstDifference is where two runs of bytes stop agreeing, for a message that says something.
func firstDifference(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	return min(len(a), len(b))
}
