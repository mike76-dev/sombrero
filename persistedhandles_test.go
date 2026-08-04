package main

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A file that has been created but not yet uploaded is remembered by its state, because the backend
// has no object to find it by. The state is shared; the opens on it are not. These are the tests of
// that division: what one handle writes another handle sees, and what one handle is promised the
// other handle can break.

// TestIntegrationTwoHandlesOnAnUnuploadedFileAreTwoOpens is the division itself. Two creates over a
// file that exists only as its state used to be answered with one open, so a client held two handles
// that named the same file ID - which the protocol does not allow, and which left the server unable
// to tell the two apart in anything it did afterwards.
func TestIntegrationTwoHandlesOnAnUnuploadedFileAreTwoOpens(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	first, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(first).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the file was answered with %#x", status)
	}

	second, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(second).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the file again was answered with %#x", status)
	}

	if bytes.Equal(createdFileID(first), createdFileID(second)) {
		t.Errorf("both handles were given the file ID %x, want one apiece",
			createdFileID(first))
	}

	// Both are opens on the file, so whoever asks what is holding it is told about the two of them.
	if got := len(h.srv.opensOn(h.share, "notes.txt", nil)); got != 2 {
		t.Errorf("%d open(s) on the file, want the two handles", got)
	}

	// And they are opens on the same file: the state is what they share.
	one := h.srv.globalOpenTable[openIDOf(createdFileID(first))]
	two := h.srv.globalOpenTable[openIDOf(createdFileID(second))]
	if one == nil || two == nil {
		t.Fatal("one of the handles has no open behind it")
	}
	if one == two {
		t.Error("the two handles are the same open")
	}
	if one.file != two.file {
		t.Error("the two opens are on the same file and do not share its state")
	}
}

// TestIntegrationWriteThroughOneHandleIsSeenThroughAnother is what the sharing is for. The size of a
// file that is being written lives with the file and not with the handle that is writing it, so a
// client asking after the file through another of its handles - which is what an editor does while
// it saves - is told how large the file is now.
func TestIntegrationWriteThroughOneHandleIsSeenThroughAnother(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	writing, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(writing).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the file was answered with %#x", status)
	}
	watching, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(watching).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the file again was answered with %#x", status)
	}

	data := bytes.Repeat([]byte("sombrero "), 64)
	if _, err := cl.write(createdFileID(writing), 0, data); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	// FileStandardInformation carries the end of the file, which is what a client reads a size out
	// of. It is asked for through the handle that wrote nothing.
	info := queriedInfo(t, cl.queryInfo(createdFileID(watching), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != uint64(len(data)) {
		t.Errorf("the second handle says the file is %d bytes long, want the %d just written through the first",
			got, len(data))
	}
}

// TestIntegrationSecondOpenByTheSameClientLeavesItsOplockAlone is the hang this exemption was made
// to end, and the reason a client is entitled to say nothing.
//
// An oplock is a promise to a client's view of a file, not to one handle on it, so a second open by
// that same client is inside the promise: nothing it has cached has gone stale. [MS-FSA] carries this
// as the oplock key, "a GUID value that identifies multiple handles belonging to the same client
// cache view", and a client will not answer a break of its own oplock provoked by its own open - it
// is waiting for that open to be answered. The server waited for the acknowledgment, the client
// waited for the create, and only the acknowledgment timer broke the tie: thirty-five seconds for
// every open of a file the client already had open, which is what an editor does to a file it is
// about to save.
func TestIntegrationSecondOpenByTheSameClientLeavesItsOplockAlone(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	cl := h.dial("alice")
	held, _ := cl.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("the create was granted %#x rather than a batch oplock", level)
	}

	// The second open is answered there and then: there is nothing to wait for, so it is not held
	// back behind an interim response the way a create that has to break somebody's promise is.
	again, async := cl.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if async {
		t.Error("the second open was held back for a break, want it answered at once")
	}
	if status := smb2.Header(again).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second open was answered with %#x", status)
	}
	if bytes.Equal(createdFileID(again), createdFileID(held)) {
		t.Error("the second open was answered with the handle that holds the oplock")
	}

	// And nothing was sent about the oplock at all.
	cl.quiet(200*time.Millisecond, "a break of the client's own oplock")

	first := h.srv.globalOpenTable[openIDOf(createdFileID(held))]
	if first == nil {
		t.Fatal("the handle that holds the oplock has no open behind it")
	}
	first.mu.Lock()
	level, state := first.oplockLevel, first.oplockState
	first.mu.Unlock()
	if level != smb2.OPLOCK_LEVEL_BATCH || state != smb2.OplockHeld {
		t.Errorf("the oplock is at level %#x in state %d, want the batch oplock still held", level, state)
	}
}

// TestIntegrationSecondOpenByAnotherClientStillBreaks is the other side of the exemption. A client
// opening a file somebody else holds an oplock on is outside that promise, so the promise has to go -
// which is the whole reason the mechanism exists.
func TestIntegrationSecondOpenByAnotherClientStillBreaks(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("alice was granted %#x rather than a batch oplock", level)
	}

	bob := h.dial("bob")

	type answer struct {
		note []byte
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		var a answer
		select {
		case a.note = <-alice.sent:
		case <-time.After(20 * time.Second):
			answered <- a
			return
		}
		_, a.err = alice.ackBreak(brokenFileID(a.note), smb2.OPLOCK_LEVEL_NONE)
		answered <- a
	}()

	if _, err := bob.createErr("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN); err != nil {
		t.Fatalf("bob could not open the file: %v", err)
	}

	a := <-answered
	if a.note == nil {
		t.Fatal("alice was never told to give up her oplock")
	}
	if a.err != nil {
		t.Fatalf("alice could not acknowledge the break: %v", a.err)
	}
	if !bytes.Equal(brokenFileID(a.note), createdFileID(held)) {
		t.Errorf("the break named the file ID %x, want alice's %x", brokenFileID(a.note), createdFileID(held))
	}
}

// TestIntegrationChangeBreaksTheClientsOtherHandle is the line between an open and a change. A second
// open by the same client leaves its oplock alone, because opening a file changes nothing about it. A
// write, a rename or a delete does change it, and what the client has cached through its other
// handles is stale whoever made it so — so those handles are told, the client's own included.
//
// A client that is not told goes on holding a cached handle on a file that has been changed or
// deleted, and says it has files open on the share when it is asked to let the share go.
func TestIntegrationChangeBreaksTheClientsOtherHandle(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	cl := h.dial("alice")

	// The handle that caches the file, and a second handle of the same client to change it through.
	// The second open leaves the first one's oplock standing, which is what the exemption is for.
	held, _ := cl.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("the first handle was granted %#x rather than a batch oplock", level)
	}

	writing, async := cl.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if async {
		t.Fatal("the second open was held back for a break of the client's own oplock")
	}
	if status := smb2.Header(writing).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second open was answered with %#x", status)
	}

	// The write is what makes the first handle's cache stale. The break of that cache is started
	// while the write is being answered, so by the time the answer is in hand the promise is
	// already going.
	written, err := cl.write(createdFileID(writing), 0, []byte("sombrero"))
	if err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	first := h.srv.globalOpenTable[openIDOf(createdFileID(held))]
	if first == nil {
		t.Fatal("the handle holding the cache has no open behind it")
	}
	first.mu.Lock()
	state := first.oplockState
	first.mu.Unlock()
	if state == smb2.OplockHeld {
		t.Error("the write left the other handle's oplock standing, so its cache is stale and it does not know")
	}

	// And the client is told. The break and the answer to the write travel over the one connection,
	// and the harness may take either of them off it first, so the break is looked for in both what
	// the write returned and what was left behind.
	msgs := [][]byte{written}
	for len(msgs) < 4 {
		select {
		case msg := <-cl.sent:
			msgs = append(msgs, msg)
			continue
		case <-time.After(2 * time.Second):
		}
		break
	}

	var note []byte
	for _, msg := range msgs {
		if smb2.Header(msg).Command() == smb2.SMB2_OPLOCK_BREAK {
			note = msg
		}
	}
	if note == nil {
		t.Fatal("the handle holding the cache was never told that the file had changed under it")
	}
	if !bytes.Equal(brokenFileID(note), createdFileID(held)) {
		t.Errorf("the break named the file ID %x, want the %x of the handle holding the cache",
			brokenFileID(note), createdFileID(held))
	}
}
