package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A file goes to the backend as a multipart upload, in parts of the size that backend takes them in.
// What the client sends and what goes up are not the same thing in the same order: the parts travel
// on their own, land in whatever order the network gives them, and are put together at the end.

// filling writes enough through the handle to fill n parts of the given size.
func filling(t *testing.T, cl *testClient, fid []byte, part uint64, n int) {
	t.Helper()

	block := bytes.Repeat([]byte("s"), int(part))
	for i := range n {
		if _, err := cl.write(fid, uint64(i)*part, block); err != nil {
			t.Fatalf("the write of block %d failed: %v", i, err)
		}
	}
}

// TestIntegrationAWriteIsAnsweredBeforeItsPartReachesTheStore is the freeze. The part was sent by the
// write that filled it, so the client was left waiting on that write for as long as the backend took
// over it — a minute at a time on a large file, which is long enough for the client to give up.
func TestIntegrationAWriteIsAnsweredBeforeItsPartReachesTheStore(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// Nothing reaches the store until the test says so.
	release := h.files.holdParts(0)

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		filling(t, cl, fid, 1024, 2)
	}()

	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("the writes were not answered while their parts were still on their way to the store")
	}

	// And the file is stored once the parts land, with the close reporting it.
	release()
	closed, err := cl.closeHandle(fid)
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the close was answered with %#x, want the file stored", status)
	}
	if got := uint64(len(h.files.dataOf("big.bin"))); got != 2048 {
		t.Errorf("the store holds %d bytes, want the 2048 written", got)
	}
}

// TestIntegrationAPartThatFailsIsReportedAtTheClose is where a part that went wrong is answered for.
// The write it came from was answered when its bytes were taken in, so there is nobody left to tell:
// the close is the last word on whether the file was stored, and it has to carry the failure.
func TestIntegrationAPartThatFailsIsReportedAtTheClose(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	h.files.failParts(errors.New("the host would not take the sector"))

	// The write itself is answered: the part had not been tried when the bytes were taken in.
	answer, err := cl.write(fid, 0, bytes.Repeat([]byte("s"), 1024))
	if err != nil {
		t.Fatalf("the write failed outright: %v", err)
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the write was answered with %#x, want the bytes taken in", status)
	}

	closed, err := cl.closeHandle(fid)
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_UNEXPECTED_NETWORK_ERROR {
		t.Errorf("the close was answered with %#x, want the file reported unstored", status)
	}
	if h.files.has("big.bin") {
		t.Error("the store holds a file whose parts never got there")
	}
}

// TestIntegrationThePartsArePutTogetherInOrder is what the backend needs of them. Each part goes to
// the store on its own now, so they land in whatever order the network gives them, and the list the
// backend is handed to put them together has to be in the order they make the file up in.
func TestIntegrationThePartsArePutTogetherInOrder(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	// The first part is held while the ones behind it land, so that they land out of order.
	release := h.files.holdParts(1)
	filling(t, cl, fid, 1024, 4)
	time.Sleep(50 * time.Millisecond)
	release()

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	parts := h.files.storedParts()
	if !slices.IsSorted(parts) {
		t.Errorf("the backend was given the parts as %v, want them in the order they make the file up in", parts)
	}
	if len(parts) != 4 {
		t.Errorf("the backend was given %d part(s) %v, want the 4 the file was written in", len(parts), parts)
	}
}

// TestPartsInFlightBoundsTheMemoryNotTheCount is the shape of the bound. A part is 4 MiB on one
// backend and a slab of tens of MiB on the other, and it is the memory that has to be bounded, so the
// same budget buys many small parts and few large ones — but never fewer than it takes to keep the
// line busy, and never so many that a single transfer can run the server out of memory.
func TestPartsInFlightBoundsTheMemoryNotTheCount(t *testing.T) {
	for _, c := range []struct {
		part uint64
		want int
	}{
		{part: 4 << 20, want: 16}, // renterd: a sector apiece
		{part: 40 << 20, want: 4}, // indexd: ten shards of a sector
		{part: 64 << 20, want: 4}, // as large as the budget: the floor holds
		{part: 0, want: 4},        // unknown: the floor holds
		{part: 1 << 20, want: 16}, // small enough to hit the ceiling
	} {
		if got := partsInFlight(c.part); got != c.want {
			t.Errorf("parts of %s go %d at a time, want %d", traceBytes(c.part), got, c.want)
		}
	}

	// Whatever the part size, what may be in flight is within the budget or is the floor.
	for part := uint64(1 << 16); part < 1<<30; part <<= 1 {
		n := partsInFlight(part)
		if n > minPartsInFlight && uint64(n)*part > partsInFlightBudget {
			t.Errorf("parts of %s go %d at a time, which is %s in flight, over the budget of %s",
				traceBytes(part), n, traceBytes(uint64(n)*part), traceBytes(partsInFlightBudget))
		}
	}
}

// TestIntegrationAWriteBehindWhatIsStoredIsRefused is the one write this server cannot carry out. The
// parts of a multipart upload cannot be taken back, so a write over bytes that have gone up already
// cannot be honoured — and answering it as though it had been would leave the client with a file that
// is not the one it wrote and no way of finding out. It is told instead, and the upload is called off.
func TestIntegrationAWriteBehindWhatIsStoredIsRefused(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// Two parts' worth, so that the first has been sent and is beyond recall.
	if _, err := cl.write(fid, 0, bytes.Repeat([]byte("h"), 1024)); err != nil {
		t.Fatalf("the first write failed: %v", err)
	}
	if _, err := cl.write(fid, 1024, bytes.Repeat([]byte("t"), 1024)); err != nil {
		t.Fatalf("the second write failed: %v", err)
	}

	again, err := cl.write(fid, 0, bytes.Repeat([]byte("z"), 1024))
	if err != nil {
		t.Fatalf("the write over what was stored failed outright: %v", err)
	}
	if status := smb2.Header(again).Status(); status != smb2.STATUS_DATA_ERROR {
		t.Errorf("the write over what was stored was answered with %#x, want the client told", status)
	}

	// And the upload is called off with it, rather than left to store a file the client did not write.
	file := h.srv.globalOpenTable[openIDOf(fid)]
	if file == nil {
		t.Fatal("the handle has no open behind it")
	}
	if file.file.uploadNow() != nil {
		t.Error("the upload is still running after a write it could not carry out")
	}
	if h.files.has("big.bin") {
		t.Error("the store holds a file that was never finished")
	}
}

// TestIntegrationTheFrontOfAFileBeingWrittenCanBeRead is the copy macOS gave up on. A client reads
// back the file it is writing — it sniffs the type of what it has copied, or something on the machine
// makes a thumbnail of it — and what it reads is the front. By then the front has gone to the store as
// a part, which cannot be read until the upload is complete, so the read was answered with an error;
// and a client that cannot read the file it is copying abandons the copy.
func TestIntegrationTheFrontOfAFileBeingWrittenCanBeRead(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// The front of the file, and then enough behind it that the front is several parts back.
	front := bytes.Repeat([]byte("ftypisom"), 128)
	if _, err := cl.write(fid, 0, front); err != nil {
		t.Fatalf("the write of the front failed: %v", err)
	}
	block := bytes.Repeat([]byte("s"), 1024)
	for i := 1; i <= 8; i++ {
		if _, err := cl.write(fid, uint64(i)*1024, block); err != nil {
			t.Fatalf("the write of block %d failed: %v", i, err)
		}
	}

	file := h.srv.globalOpenTable[openIDOf(fid)]
	if file == nil || file.file.uploadNow() == nil {
		t.Fatal("the file is not being written, so there is nothing to read against")
	}

	// The read the client makes: a few bytes of the front, while the upload runs.
	read, err := cl.readOver(fid, uint32(len(front)), smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read failed outright: %v", err)
	}
	if got := string(readData(t, read)); got != string(front) {
		t.Errorf("the front of the file reads as %q, want %q", got, front)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}
}

// TestEmptyingAFileIsAnsweredWithoutWaitingForTheStore is the abort answered at leisure. A client that
// gives up on a copy empties the file, and the parts still on their way are of a file that is being
// thrown away — so there is nothing to wait for. Waited for anyway, the answer takes as long as the
// backend does, which on a backend that has slowed to a crawl was seven seconds for a client that had
// already stopped listening.
func TestEmptyingAFileIsAnsweredWithoutWaitingForTheStore(t *testing.T) {
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

	// The store takes the parts and never comes back, as one that has stalled does.
	release := h.files.holdParts(0)
	defer release()

	if _, err := cl.write(fid, 3072, bytes.Repeat([]byte("s"), 1024)); err != nil {
		t.Fatalf("the write into the stalled store failed: %v", err)
	}

	answered := make(chan uint32, 1)
	go func() {
		set, err := cl.endOfFile(fid, 0)
		if err != nil {
			answered <- 0xffffffff
			return
		}
		answered <- smb2.Header(set).Status()
	}()

	select {
	case status := <-answered:
		if status != smb2.STATUS_OK {
			t.Errorf("emptying the file was answered with %#x", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("emptying the file waited on a store that had stalled")
	}
}

// TestWritesStopOnceAPartHasFailed is the transfer that was over before it ended. A part goes to the
// backend long after the write it came from was answered, so a backend that has stopped taking them
// is found out in the middle of a file — and the upload is completed from the parts it was given, so
// one missing part loses the file whatever else is written. The client used to go on sending it all
// the same, and was told at the close, having transferred hundreds of megabytes for nothing.
func TestWritesStopOnceAPartHasFailed(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	h.files.failParts(errors.New("consensus is not synced"))

	// The write that fills the first part is answered: the part had not been tried yet.
	if _, err := cl.write(fid, 0, bytes.Repeat([]byte("s"), 1024)); err != nil {
		t.Fatalf("the first write failed: %v", err)
	}

	// The part fails on its way up, and the next write is told rather than taken.
	deadline := time.Now().Add(2 * time.Second)
	var status uint32
	var taken int
	for offset := uint64(1024); time.Now().Before(deadline); offset += 1024 {
		answer, err := cl.write(fid, offset, bytes.Repeat([]byte("s"), 1024))
		if err != nil {
			t.Fatalf("the write at %d failed outright: %v", offset, err)
		}
		status = smb2.Header(answer).Status()
		if status != smb2.STATUS_OK {
			break
		}
		taken++
		time.Sleep(10 * time.Millisecond)
	}

	if taken > 4 {
		t.Errorf("%d further writes were taken after a part had failed, for a file that cannot be stored", taken)
	}

	if status == smb2.STATUS_OK {
		t.Error("the writes were still being taken after a part had failed, for a file that cannot be stored")
	}
}

// TestIntegrationTheFrontOfAFileIsReadableWellIntoTheUpload is how far back a client looks.
func TestIntegrationTheFrontOfAFileIsReadableWellIntoTheUpload(t *testing.T) {
	// The window shrunk, so that the shape can be tested without writing out the whole of it.
	was := uploadHeadKept
	uploadHeadKept = 64 * 1024
	t.Cleanup(func() { uploadHeadKept = was })

	h := newSMBTest(t)
	cl := h.dial("alice")
	cl.tc.maxUploadSize = 16 * 1024

	handle, _ := cl.create("clip.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(handle).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	fid := createdFileID(handle)

	// A file whose every block says where it came from, so that a read can be checked.
	block := func(i int) []byte {
		b := bytes.Repeat([]byte{byte('a' + i%26)}, 4096)
		copy(b, []byte(fmt.Sprintf("block %d", i)))
		return b
	}

	for i := range 64 {
		if _, err := cl.write(fid, uint64(i)*4096, block(i)); err != nil {
			t.Fatalf("the write of block %d failed: %v", i, err)
		}
	}

	// A read well behind the buffer but inside what is kept of the front, which is where the client
	// goes looking while the upload runs.
	const at = 40 * 1024
	read, err := cl.readOver(fid, 4096, smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read of the front failed outright: %v", err)
	}
	if status := smb2.Header(read).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the read of the front was answered with %#x", status)
	}

	// And one further in, at an offset the front no longer covers but the part that went last does.
	file := h.srv.globalOpenTable[openIDOf(fid)]
	if file == nil {
		t.Fatal("the file has no open behind it")
	}
	if u := file.file.uploadNow(); u == nil {
		t.Fatal("the file is not being written")
	}

	got, err := file.read(at, 4096)
	if err != nil {
		t.Fatalf("the read at %d failed: %v", at, err)
	}
	if want := block(at / 4096); !bytes.Equal(got, want) {
		t.Errorf("the read at %d gave back %.12q, want %.12q", at, got, want)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}
}

// TestTheLastPartGoesBeforeTheOthersAreWaitedFor is the freeze a moment from the end. A file is not
// stored until every part has landed, so the finish waits — and it used to send the last of the file
// only once that wait was over, adding one more trip to the backend to the end of all the others. On a
// link where a part takes seconds, that is a copy that looks finished and then sits there.
func TestTheLastPartGoesBeforeTheOthersAreWaitedFor(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 4096

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	// Two full parts and a piece of a third, which is what the finish has to send.
	for i := range 2 {
		if _, err := cl.write(fid, uint64(i)*4096, bytes.Repeat([]byte{byte('a' + i)}, 4096)); err != nil {
			t.Fatalf("the write of block %d failed: %v", i, err)
		}
	}
	if _, err := cl.write(fid, 8192, bytes.Repeat([]byte("z"), 1024)); err != nil {
		t.Fatalf("the write of the tail failed: %v", err)
	}

	// Every part is held, and the order they were handed over in is what the test is about: the
	// last one has to be among them before the finish starts waiting.
	release := h.files.holdParts(0)

	done := make(chan error, 1)
	go func() {
		_, err := cl.closeHandle(fid)
		done <- err
	}()

	// The last part reaches the store while the others are still held, which is only possible if it
	// was handed over before the wait.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.files.mu.Lock()
		sent := len(h.files.partsWritten)
		h.files.mu.Unlock()
		if sent == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	h.files.mu.Lock()
	sent := append([]int(nil), h.files.partsWritten...)
	h.files.mu.Unlock()
	if len(sent) != 3 {
		t.Errorf("%d part(s) had been handed over while the store was held %v, want all three including the last", len(sent), sent)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	want := append(bytes.Repeat([]byte("a"), 4096), bytes.Repeat([]byte("b"), 4096)...)
	want = append(want, bytes.Repeat([]byte("z"), 1024)...)
	if got := h.files.dataOf("big.bin"); !bytes.Equal(got, want) {
		t.Errorf("the store holds %d bytes, want %d", len(got), len(want))
	}
}
