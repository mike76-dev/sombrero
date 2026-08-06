package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
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

// TestIntegrationLeaseIsRefusedWhenItWasNotOffered holds the granting of leases to what the server
// said at negotiate time. A client only asks for a lease because the server advertised
// SMB2_GLOBAL_CAP_LEASING, and [MS-SMB2] 3.3.5.9 has a server that does not support leasing ignore
// the lease create context — so a connection that never offered leases must not hand one out. The
// two answers are the same answer, and this is what keeps them so.
func TestIntegrationLeaseIsRefusedWhenItWasNotOffered(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	cl := h.dial("alice")

	// The connection the harness builds carries what a 3.1.1 negotiate settles, leasing included,
	// so it is taken away again here: this is the 2.0.2 client, or a server built without leases.
	cl.conn.serverCapabilities &^= smb2.GLOBAL_CAP_LEASING

	buf, _ := cl.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}

	// The oplock level of the response is what says whether a lease was granted: a lease answers
	// with SMB2_OPLOCK_LEVEL_LEASE, and anything else means the context was passed over.
	if level := createdOplockLevel(buf); level == smb2.OPLOCK_LEVEL_LEASE {
		t.Error("a lease was granted over a connection that never offered leases")
	}

	file := h.srv.globalOpenTable[openIDOf(createdFileID(buf))]
	if file == nil {
		t.Fatal("the create left no open behind it")
	}
	file.mu.Lock()
	held := file.lease
	file.mu.Unlock()
	if held != nil {
		t.Error("the open holds a lease over a connection that never offered leases")
	}
}

// TestIntegrationAnUnuploadedFileSurvivesTheTreeConnect is what a client sees after the connection
// it created a file over has gone. A file created and not yet written to has no object behind it —
// the Sia network takes nothing empty — so the server is the only thing that knows it is there. That
// state used to be kept on the tree connect, so a client whose connection dropped came back to a
// share where the file it had just made could not be opened at all: "the file couldn't be found",
// for a file the client had created seconds earlier and never closed the share on.
//
// The state belongs to the share now, under the workgroup whose namespace the file is in, so the
// file is there for the next tree connect as it was for the last.
func TestIntegrationAnUnuploadedFileSurvivesTheTreeConnect(t *testing.T) {
	h := newSMBTest(t)

	first := h.dial("alice")
	created, _ := first.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the file was answered with %#x", status)
	}
	if _, err := first.closeHandle(createdFileID(created)); err != nil {
		t.Fatalf("closing the handle failed: %v", err)
	}

	// The connection goes, and with it the session and the tree connect the file was made on.
	first.goesAway()

	// The client comes back — a new connection, a new session, a new tree connect, the same
	// workgroup — and asks for the file it made.
	again := h.dial("alice")
	opened, _ := again.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(opened).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the file over a new tree connect was answered with %#x, want it found", status)
	}

	// And it is the same file: the state the first tree connect left is the state this open is on.
	file := h.srv.globalOpenTable[openIDOf(createdFileID(opened))]
	if file == nil {
		t.Fatal("the open has nothing behind it")
	}
	if fs, found := again.tc.persistedFile("notes.txt"); !found || fs != file.file {
		t.Error("the open is not on the state the share is holding for the file")
	}

	// A listing over the new tree connect shows it too, which is what an explorer window asks for.
	shown := again.tc.persistedObjects(func(path string) bool { return path == "notes.txt" })
	if len(shown) != 1 {
		t.Errorf("the listing carried %d entries for the file, want it there", len(shown))
	}
}

// TestIntegrationEncryptedLeaseBreakNamesItsSession is the rule that decides what happens on the
// wire when two rules disagree.
//
// [MS-SMB2] 3.3.4.7 has a lease break notification carry a SessionId of zero: a lease belongs to a
// client rather than to a session, and the lease key alone says what is meant. [MS-SMB2] 3.2.5.1.1
// has a client disconnect the connection when a decrypted message names a different session than the
// transform header it arrived in — and zero is a different session. A client cannot be told to give
// up a lease at all if the telling costs it the connection, so what is encrypted names the session
// it is encrypted under.
func TestIntegrationEncryptedLeaseBreakNamesItsSession(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice").encrypting()
	alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

	// Somebody else opening the file is what takes the lease away.
	h.impatient(50 * time.Millisecond)
	bob := h.dial("bob")
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		bob.createErr("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE)
	}()
	defer func() {
		<-opened
	}()

	sealed := alice.recv(10 * time.Second)
	if id := smb2.Header(sealed).ProtocolID(); id != smb2.PROTOCOL_SMB2_ENCRYPTED {
		t.Fatalf("the break went out under protocol %#x, want it encrypted", id)
	}

	// The transform header names the session, because that is what the client finds the key by.
	if got := smb2.Header(sealed).TransformSessionID(); got != alice.ss.sessionID {
		t.Errorf("the transform header names session %#x, want %#x", got, alice.ss.sessionID)
	}

	// And so must the message inside it, or the client drops the connection instead of reading it.
	note := alice.decrypted(sealed)
	if !isLeaseBreak(note) {
		t.Fatal("what came apart is not a lease break")
	}
	if got := smb2.Header(note).SessionID(); got != alice.ss.sessionID {
		t.Errorf("the lease break names session %#x, want the %#x the transform header names",
			got, alice.ss.sessionID)
	}
}

// TestIntegrationFlushWaitsForEveryHandlesWrites is the upload belonging to the file rather than to
// the handle that started it.
//
// Two handles on one file write through the one upload, so a handle that finalizes it has to wait for
// the writes going into it through every other handle as well. Waiting only for its own left the
// finalize running against a buffer with a hole in it, which comes back as "non-contiguous pending
// write data": the upload is thrown away, and a file that a client saved through two handles — which
// is how an office application saves, through a temporary file it then renames — is lost.
func TestIntegrationFlushWaitsForEveryHandlesWrites(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	first, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(first).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	second, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(second).Status(); status != smb2.STATUS_OK {
		t.Fatalf("opening the file again was answered with %#x", status)
	}

	one := h.srv.globalOpenTable[openIDOf(createdFileID(first))]
	two := h.srv.globalOpenTable[openIDOf(createdFileID(second))]
	if one == nil || two == nil || one.file != two.file {
		t.Fatal("the two handles are not two opens on one file")
	}

	// A real write through the first handle, so that there is an upload for the flush to finalize.
	if _, err := cl.write(createdFileID(first), 0, []byte("sombrero")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	if one.file.uploadNow() == nil {
		t.Fatal("the write left no upload for the flush to wait on")
	}

	// And a second write on its way in through that handle, counted as the write path counts it.
	one.file.beginWrite()

	// The second handle finalizes the upload. It must not get as far as the flush while that write
	// is outstanding, so this is expected to sit still until the write lands.
	flushed := make(chan error, 1)
	go func() {
		flushed <- two.flush()
	}()

	select {
	case err := <-flushed:
		t.Fatalf("the flush ran with a write of another handle still in flight (%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Once the write has landed, the flush goes ahead and finalizes the upload.
	one.file.endWrite()

	select {
	case err := <-flushed:
		if err != nil {
			t.Errorf("the flush failed once the write had landed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the flush never came back after the write had landed")
	}
}

// TestIntegrationKeylessOpenLeavesTheClientsLeaseAlone is the lease side of the cache view, and the
// second half of the deadlock the oplock exemption fixed.
//
// A client holding a lease on a file and opening that file again — through an ordinary create, with no
// lease key on it — is asking about the file it already has, so its lease stands. Breaking it deadlocks
// the two ends: the client will not answer a break until the create that provoked it is answered, and
// the server will not answer the create until the break is answered. That cost 35 seconds an open, and
// the acknowledgment arrived the moment the server gave up waiting for it.
func TestIntegrationKeylessOpenLeavesTheClientsLeaseAlone(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	cl := h.dial("alice")
	held, _ := cl.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, found := createdLeaseState(held); !found || state != rwh {
		t.Fatalf("the lease was granted %#x (found %v) rather than read, write and handle caching", state, found)
	}

	// The same client opens the file again, naming no lease key.
	again, async := cl.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if async {
		t.Error("the second open was held back for a break of the client's own lease")
	}
	if status := smb2.Header(again).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second open was answered with %#x", status)
	}

	cl.quiet(200*time.Millisecond, "a break of the client's own lease")

	// The lease still promises everything it did.
	l := h.srv.findLease([16]byte(cl.conn.clientGuid), aliceKey)
	if l == nil {
		t.Fatal("the lease is gone")
	}
	if state := l.stateNow(); state != rwh {
		t.Errorf("the lease is at %#x, want the %#x it was granted", state, rwh)
	}
}

// TestIntegrationKeyedOpenBreaksTheClientsOtherLease is the line the exemption stops at. Two lease
// keys are two views of the file, even in one client, and a client keeps them apart by key: a create
// naming a second key has to take the first view's write caching away, or the client would be caching
// writes in one view that the other cannot see.
func TestIntegrationKeyedOpenBreaksTheClientsOtherLease(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	h.impatient(50 * time.Millisecond)

	cl := h.dial("alice")
	held, _ := cl.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, found := createdLeaseState(held); !found || state != rwh {
		t.Fatalf("the lease was granted %#x (found %v) rather than read, write and handle caching", state, found)
	}

	// The same client, a second lease key: another view of the file, which the first has to make
	// room for. The create waits for the break, so it is sent from a goroutine of its own.
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		cl.createWith("dir/file", smb2.OPLOCK_LEVEL_LEASE, smb2.FILE_OPEN, leaseContext(bobKey, rwh, 2))
	}()
	defer func() {
		<-opened
	}()

	note := cl.recv(10 * time.Second)
	if !isLeaseBreak(note) {
		t.Fatalf("what arrived was command %d, want the break of the first lease",
			smb2.Header(note).Command())
	}
	if key := brokenLeaseKey(note); key != aliceKey {
		t.Errorf("the break names the lease key %x, want the first view's %x", key, aliceKey)
	}
}

// TestIntegrationAStoreThatFailsIsNotANameThatIsGone is what a client is told when the store will not
// do the work. A Sia node that is out of sync refuses to make a directory, and answering that the
// name was not found has the client tell whoever asked that the folder they are creating no longer
// exists — which is untrue, unhelpful, and sends them looking for a file system problem.
func TestIntegrationAStoreThatFailsIsNotANameThatIsGone(t *testing.T) {
	h := newSMBTest(t)
	h.files.failDirectories(errors.New("consensus is not synced"))

	cl := h.dial("alice")
	// The create is sent by hand: the helper for directories fails the test on any status but
	// success, and a status is exactly what this is about.
	cl.mid++
	resp, err := cl.send(createRequestWithOptions(cl.mid, cl.ss.sessionID, cl.tc.treeID, "New folder",
		smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN_IF, writeAccess, smb2.FILE_DIRECTORY_FILE, nil))
	if err != nil {
		t.Fatalf("the create failed outright: %v", err)
	}

	status := resp.Header().Status()
	if status == smb2.STATUS_OBJECT_NAME_NOT_FOUND {
		t.Error("the store failing was answered as the name not being found")
	}
	if status != smb2.STATUS_UNEXPECTED_NETWORK_ERROR {
		t.Errorf("the create was answered with %#x, want an error naming the store", status)
	}
}

// TestIntegrationACloseThatCannotStoreTheFileSaysSo is the write that never reached the store. A
// close is what finishes an upload, so a close whose upload fails is a file that was not saved: a
// client told the close succeeded believes what it wrote is on the share, and the bytes are gone with
// nothing to say so.
func TestIntegrationACloseThatCannotStoreTheFileSaysSo(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	created, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}

	if _, err := cl.write(createdFileID(created), 0, []byte("sombrero")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	// The store takes the parts and will not put them together.
	h.files.failFinishingUploads(errors.New("consensus is not synced"))

	closed, err := cl.closeHandle(createdFileID(created))
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status == smb2.STATUS_OK {
		t.Error("the close of a file that was never stored was answered with success")
	} else if status != smb2.STATUS_UNEXPECTED_NETWORK_ERROR {
		t.Errorf("the close was answered with %#x, want an error naming the store", status)
	}
}

// TestIntegrationFlushWaitsForTheWritesItCoversAndStoresNothing is the contract a flush carries here.
//
// Storing on a flush is not open to this server: a file goes to the backend as one multipart upload,
// and a finished upload cannot be added to, so committing on every flush would mean uploading the whole
// file again on the next write. What a flush does mean is that everything sent so far has been taken
// in — a client that pipelines a write and a flush must not be told the flush is done while the write
// is still on its way — and the upload is left running for the close to finish.
func TestIntegrationFlushWaitsForTheWritesItCoversAndStoresNothing(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	created, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}
	if _, err := cl.write(createdFileID(created), 0, []byte("sombrero")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]
	if file == nil {
		t.Fatal("the create left no open behind it")
	}

	// A write on its way in, counted as the write path counts it.
	file.file.beginWrite()

	flushed := make(chan []byte, 1)
	go func() {
		resp, err := cl.flushHandle(createdFileID(created))
		if err != nil {
			flushed <- nil
			return
		}
		flushed <- resp
	}()

	select {
	case <-flushed:
		t.Fatal("the flush was answered with a write still on its way in")
	case <-time.After(300 * time.Millisecond):
	}

	file.file.endWrite()

	select {
	case resp := <-flushed:
		if resp == nil {
			t.Fatal("the flush failed")
		}
		if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
			t.Errorf("the flush was answered with %#x", status)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the flush never came back after the write had landed")
	}

	// And it stored nothing: the upload is still there for the close to finish.
	if file.file.uploadNow() == nil {
		t.Error("the flush finished the upload, which would mean rewriting the file on the next write")
	}
}

// TestIntegrationRenameStoresWhatIsStillBeingWritten is a rename that arrives before the close.
//
// The bytes of a file being written are in the upload buffer, not in the store, and a rename moves
// what the store holds — so renaming while the upload is pending asked the backend to move an object
// that was not there, and the client was told its rename failed on a file it had just written. The
// upload is finished under the name it was started with first, and the rename moves what it stored.
func TestIntegrationRenameStoresWhatIsStillBeingWritten(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	created, _ := cl.create("draft.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}

	data := bytes.Repeat([]byte("sombrero "), 64)
	if _, err := cl.write(createdFileID(created), 0, data); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]
	if file == nil {
		t.Fatal("the create left no open behind it")
	}
	if file.file.uploadNow() == nil {
		t.Fatal("the write left no upload pending, so the rename has nothing to finish")
	}

	// The rename comes while the upload is still pending, which is the case this is about.
	renamed, err := cl.rename(createdFileID(created), "final.txt")
	if err != nil {
		t.Fatalf("the rename failed outright: %v", err)
	}
	if status := smb2.Header(renamed).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the rename was answered with %#x, want the file moved", status)
	}

	// What the store holds is the file under its new name, with everything that was written to it.
	if _, err := h.files.Object(context.Background(), stores.Account{}, "draft.txt"); err == nil {
		t.Error("the store still holds the file under the name it was written under")
	}
	oi, err := h.files.Object(context.Background(), stores.Account{}, "final.txt")
	if err != nil {
		t.Fatalf("the store does not hold the file under its new name: %v", err)
	}
	if oi.Size != uint64(len(data)) {
		t.Errorf("the stored file is %d bytes, want the %d that were written", oi.Size, len(data))
	}

	// And the upload is done with, rather than left pending under a name nothing points at.
	if file.file.uploadNow() != nil {
		t.Error("the upload is still pending after the rename")
	}
}

// A file the store has an object for is shared exactly as one it has nothing for: the state on the
// share is what every handle on the file finds, whoever opened it. These are the tests of what the
// sharing keeps from happening — two writings of one file going up as two uploads, and a file left
// at a size the store was never given.

// readData is the bytes a read response carries.
func readData(t *testing.T, buf []byte) []byte {
	t.Helper()

	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the read was answered with %#x", status)
	}
	if len(buf) < smb2.SMB2HeaderSize+smb2.SMB2ReadResponseMinSize {
		t.Fatalf("the read answered with %d bytes, too few for a read response", len(buf))
	}

	offset := int(buf[smb2.SMB2HeaderSize+2])
	length := int(binary.LittleEndian.Uint32(buf[smb2.SMB2HeaderSize+4 : smb2.SMB2HeaderSize+8]))
	if offset+length > len(buf) {
		t.Fatalf("the read names %d bytes at offset %d of a %d byte response", length, offset, len(buf))
	}

	return buf[offset : offset+length]
}

// TestIntegrationTwoClientsOnAStoredFileShareItsState is the sharing itself, for a file that is in
// the store. It used to hold only for a file the store had nothing for: the state was asked for
// where the object lookup had failed and nowhere else, so two clients opening a file that exists got
// a state apiece and, with it, a size apiece.
func TestIntegrationTwoClientsOnAStoredFileShareItsState(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	first, second := h.dial("alice"), h.dial("alice")
	one, _ := first.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(one).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first client's open was answered with %#x", status)
	}
	two, _ := second.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(two).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second client's open was answered with %#x", status)
	}

	opOne := h.srv.globalOpenTable[openIDOf(createdFileID(one))]
	opTwo := h.srv.globalOpenTable[openIDOf(createdFileID(two))]
	if opOne == nil || opTwo == nil {
		t.Fatal("one of the opens is missing")
	}
	if opOne.file != opTwo.file {
		t.Fatal("the two clients have the same file open and do not share its state")
	}

	// So what one client writes is the size the other is told, as it is for two handles of one
	// client: the file is one file.
	if _, err := first.write(createdFileID(one), 0, []byte("test")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	info := queriedInfo(t, second.queryInfo(createdFileID(two), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != 4 {
		t.Errorf("the second client says the file is %d bytes long, want the 4 the first has written", got)
	}
}

// TestIntegrationTwoClientsWritingAStoredFileShareOneUpload is the corruption the sharing was
// found by. A file goes to the backend as one multipart upload, and the upload belongs to the file:
// two clients writing one file used to start one upload each, both to the same path, and the store
// kept whichever was finished last. Everything the other client wrote was gone, and until then each
// of them read the object the store still held cut off at a length of its own.
func TestIntegrationTwoClientsWritingAStoredFileShareOneUpload(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("0123456789ab"))

	first, second := h.dial("alice"), h.dial("alice")
	one, _ := first.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(one).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first client's open was answered with %#x", status)
	}
	two, _ := second.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(two).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second client's open was answered with %#x", status)
	}

	// Both write before either closes, which is what makes it one writing of the file rather than
	// two in a row.
	if _, err := second.write(createdFileID(two), 0, []byte("another test")); err != nil {
		t.Fatalf("the second client's write failed: %v", err)
	}
	if _, err := first.write(createdFileID(one), 0, []byte("test")); err != nil {
		t.Fatalf("the first client's write failed: %v", err)
	}

	if got := h.files.uploadsOf("notes.txt"); got != 1 {
		t.Errorf("%d uploads were started for the file, want the one it goes to the backend as", got)
	}

	if _, err := first.closeHandle(createdFileID(one)); err != nil {
		t.Fatalf("the first client's close failed: %v", err)
	}
	if _, err := second.closeHandle(createdFileID(two)); err != nil {
		t.Fatalf("the second client's close failed: %v", err)
	}

	// The file the store holds is the file as both of them left it: the longer write, with the
	// shorter one over the front of it. Neither client's bytes went nowhere.
	if got := string(h.files.dataOf("notes.txt")); got != "testher test" {
		t.Errorf("the store holds %q, want both writings of the file", got)
	}
}

// TestIntegrationAnUploadThatCameToNothingLeavesTheStoredSize is the file that read as four
// characters of somebody else's writing.
//
// A write moves the size of the file to what the writer has reached, and the store is given none of
// it until the upload is finished. An upload that is called off - a close whose store refused it, a
// handle nobody came back for - used to leave that size behind: the store still held the object it
// always had, and a read of the file was served those bytes cut off at the length of a writing that
// never happened.
func TestIntegrationAnUploadThatCameToNothingLeavesTheStoredSize(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	// The second client holds the file open throughout, so that the state survives the close that
	// fails and the rollback is what the read is answered out of.
	first, second := h.dial("alice"), h.dial("alice")
	watching, _ := second.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(watching).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the watching open was answered with %#x", status)
	}
	writing, _ := first.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(writing).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the writing open was answered with %#x", status)
	}

	h.files.failFinishingUploads(errors.New("the store is out of sync"))
	if _, err := first.write(createdFileID(writing), 0, []byte("test")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	closed, err := first.closeHandle(createdFileID(writing))
	if err != nil {
		t.Fatalf("the close failed outright: %v", err)
	}
	if status := smb2.Header(closed).Status(); status != smb2.STATUS_UNEXPECTED_NETWORK_ERROR {
		t.Fatalf("the close was answered with %#x, want the file it could not store reported", status)
	}

	// The file is what the store holds, whole: the size of the writing that came to nothing is gone
	// with it.
	info := queriedInfo(t, second.queryInfo(createdFileID(watching), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != 12 {
		t.Errorf("the file is %d bytes long, want the 12 the store holds", got)
	}

	read, err := second.readOver(createdFileID(watching), 64, smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read failed outright: %v", err)
	}
	if got := string(readData(t, read)); got != "another test" {
		t.Errorf("the file reads as %q, want what the store holds", got)
	}
}

// TestIntegrationTheStateOfAStoredFileGoesWithTheLastHandle is the other side of keeping the state
// on the share. A file the store answers for is answered for by the store again once nobody holds it
// open: kept any longer, the state would go on telling every create what the last writer left behind.
// A file the store has nothing for stays, because the state is the only record that it exists.
func TestIntegrationTheStateOfAStoredFileGoesWithTheLastHandle(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("stored.txt", []byte("another test"))

	cl := h.dial("alice")
	stored, _ := cl.create("stored.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(stored).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the open of the stored file was answered with %#x", status)
	}
	made, _ := cl.create("made.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(made).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create was answered with %#x", status)
	}

	// While they are open, both are on the share: that is what the handles share.
	if _, found := cl.tc.persistedFile("stored.txt"); !found {
		t.Error("the file that is open is not on the share")
	}

	if _, err := cl.closeHandle(createdFileID(stored)); err != nil {
		t.Fatalf("the close of the stored file failed: %v", err)
	}
	if _, err := cl.closeHandle(createdFileID(made)); err != nil {
		t.Fatalf("the close of the created file failed: %v", err)
	}

	if _, found := cl.tc.persistedFile("stored.txt"); found {
		t.Error("the state of the stored file outlived the last handle on it")
	}
	if _, found := cl.tc.persistedFile("made.txt"); !found {
		t.Error("the file that was never uploaded is gone, and nothing else knows it exists")
	}
}

// TestIntegrationAWriterThatDisconnectsLeavesTheStoredSize is the same file, lost the other way.
// Nothing finishes the upload of a client that goes away, so the size it reached describes a file
// that was never stored — and the file was left at it, to be read as the object the store still held
// cut off at that length.
func TestIntegrationAWriterThatDisconnectsLeavesTheStoredSize(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))

	first, second := h.dial("alice"), h.dial("alice")
	watching, _ := second.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(watching).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the watching open was answered with %#x", status)
	}
	writing, _ := first.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(writing).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the writing open was answered with %#x", status)
	}

	if _, err := first.write(createdFileID(writing), 0, []byte("test")); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	// The writer is gone before it closed the handle, so its upload is never finished. The handle
	// is not durable, so nothing is set aside for it to come back to.
	h.srv.closeConnection(first.conn)

	info := queriedInfo(t, second.queryInfo(createdFileID(watching), smb2.FileStandardInformation, 64))
	if len(info) < 16 {
		t.Fatalf("the query answered with %d bytes, too few for a standard information structure", len(info))
	}
	if got := binary.LittleEndian.Uint64(info[8:16]); got != 12 {
		t.Errorf("the file is %d bytes long, want the 12 the store holds", got)
	}

	read, err := second.readOver(createdFileID(watching), 64, smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read failed outright: %v", err)
	}
	if got := string(readData(t, read)); got != "another test" {
		t.Errorf("the file reads as %q, want what the store holds", got)
	}
}
