package main

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestIntegrationARefusedCreateMakesNothing is the create that asks for extended attributes, which
// this server does not support. The refusal has to leave the share as it found it: an open that
// outlives the create it was made for holds the file until the session goes, and a path left
// persisted is a file the client is told it never got and can open all the same.
func TestIntegrationARefusedCreateMakesNothing(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	ctx := createContext(smb2.CREATE_EA_BUFFER, make([]byte, 8))
	buf, err := cl.createWith("ea.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE, ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_EAS_NOT_SUPPORTED {
		t.Fatalf("Status = %#x, want %#x", status, smb2.STATUS_EAS_NOT_SUPPORTED)
	}

	h.srv.mu.Lock()
	opens := len(h.srv.globalOpenTable)
	h.srv.mu.Unlock()
	if opens != 0 {
		t.Errorf("%d open(s) left behind by a create that was refused", opens)
	}

	cl.ss.mu.Lock()
	held := len(cl.ss.openTable)
	cl.ss.mu.Unlock()
	if held != 0 {
		t.Errorf("the session is holding %d handle(s) for a create that was refused", held)
	}

	cl.tc.share.mu.Lock()
	persisted := len(cl.tc.share.persisted)
	cl.tc.share.mu.Unlock()
	if persisted != 0 {
		t.Errorf("%d file(s) left on the share by a create that was refused", persisted)
	}

	// And the file the client was told it did not get is not there to open.
	again, err := cl.createErr("ea.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if err != nil {
		t.Fatalf("opening the file afterwards: %v", err)
	}
	if status := smb2.Header(again).Status(); status != smb2.STATUS_OBJECT_NAME_NOT_FOUND {
		t.Errorf("opening the file the refused create was asked for answered %#x, want it absent", status)
	}
}

func TestIntegrationCreateGrantsOplock(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	buf, async := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)

	// Nobody else has the file, so there is nothing to break and nothing to wait for.
	if async {
		t.Error("a create with no oplock to break was answered asynchronously")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("Status = %#x, want %#x", status, smb2.STATUS_OK)
	}
	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Errorf("OplockLevel = %#x, want %#x", level, smb2.OPLOCK_LEVEL_BATCH)
	}
}

func TestIntegrationCreateWithoutOplockRequest(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	buf, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("OplockLevel = %#x, want none: an oplock was granted to a client that never asked", level)
	}

	// A client that holds no oplock is never told about anybody else opening the file.
	bob := h.dial("bob")
	if _, async := bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN); async {
		t.Error("a create was made to wait although no oplock was outstanding")
	}
	alice.quiet(100*time.Millisecond, "a break was sent for an oplock that was never granted")
}

func TestIntegrationSecondCreateBreaksOplock(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("alice was granted %#x rather than a batch oplock", level)
	}

	bob := h.dial("bob")

	// Bob opens the file to overwrite it, which is a create that changes the file by itself, so
	// alice has to give up everything rather than come down to a read cache.
	//
	// Alice answers her break as soon as she is told, which is what lets bob's create through.
	// She has to do it from a goroutine of her own: bob's create does not come back until she
	// has, which is the whole point of the exercise.
	type answer struct {
		note []byte
		resp []byte
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
		a.resp, a.err = alice.ackBreak(brokenFileID(a.note), smb2.OPLOCK_LEVEL_NONE)
		answered <- a
	}()

	buf, async := bob.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OVERWRITE)

	a := <-answered
	if a.note == nil {
		t.Fatal("alice was never told to give up her oplock")
	}
	if a.err != nil {
		t.Fatalf("alice could not acknowledge the break: %v", a.err)
	}

	// The break names the open alice holds, and asks for nothing back.
	if cmd := smb2.Header(a.note).Command(); cmd != smb2.SMB2_OPLOCK_BREAK {
		t.Errorf("alice was sent command %#x, want an oplock break", cmd)
	}
	if mid := smb2.Header(a.note).MessageID(); mid != smb2.OplockBreakUnsolicitedMessageID {
		t.Errorf("the break carries message ID %#x, want the unsolicited one", mid)
	}
	if fid := brokenFileID(a.note); !bytes.Equal(fid, createdFileID(held)) {
		t.Errorf("the break names % x, want alice's open % x", fid, createdFileID(held))
	}
	if level := a.note[smb2.SMB2HeaderSize+2]; level != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("the break offers to keep level %#x, want none", level)
	}

	// The acknowledgment is answered, and tells alice what she has left.
	if status := smb2.Header(a.resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("the acknowledgment was refused with %#x", status)
	}
	if level := a.resp[smb2.SMB2HeaderSize+2]; level != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("alice was left holding %#x after the break, want none", level)
	}

	// Bob's create could not be answered until the break was over. He asked for a batch oplock
	// and cannot have one, because alice still has the file open, but with nobody holding more
	// than a read cache he is given one of those.
	if !async {
		t.Error("bob's create was answered without waiting for the break")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}
	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_II {
		t.Errorf("bob was granted %#x while alice had the file open, want level II", level)
	}
}

func TestIntegrationAccessDeniedLeavesOplockAlone(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	// Only alice may have the file.
	h.restrictTo("alice")

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("alice was granted %#x rather than a batch oplock", level)
	}

	bob := h.dial("bob")
	buf, async := bob.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)

	if status := smb2.Header(buf).Status(); status != smb2.STATUS_ACCESS_DENIED {
		t.Errorf("bob's create returned %#x, want access denied", status)
	}

	// A client that may not have the file has no business making its holder give it up, and a
	// create that is going to be refused has nothing to wait for.
	if async {
		t.Error("a create that was refused was made to wait for a break first")
	}
	alice.quiet(200*time.Millisecond, "a client with no access to the file broke the oplock on it")

	if !h.srv.hasHoldersOn(h.share, "dir/file", nil, asker{}) {
		t.Error("the oplock was given up although the create that would have broken it was refused")
	}
}

func TestIntegrationOplockSurvivesAnotherFile(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	h.files.put("dir/other", 1024)

	alice := h.dial("alice")
	alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)

	bob := h.dial("bob")
	buf, async := bob.create("dir/other", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)

	if async {
		t.Error("a create on another file was made to wait for a break")
	}
	if level := createdOplockLevel(buf); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Errorf("bob was granted %#x on a file nobody else had open, want a batch oplock", level)
	}
	alice.quiet(100*time.Millisecond, "a create on another file broke the oplock")
}

// breakEnding drives a break to its end by some means other than an acknowledgment, and
// measures how long the create that was waiting for it had to wait. The oplock has to be given
// up wherever the open goes, or the create sits out the whole acknowledgment timer for a client
// that is never going to answer.
func breakEnding(t *testing.T, end func(h *smbTest, alice *testClient, held []byte)) {
	t.Helper()

	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("alice was granted %#x rather than a batch oplock", level)
	}

	// The open alice was given, so that it can be inspected once she is gone. Bob takes her
	// place on the file, so asking who holds an oplock on it afterwards answers about him.
	h.srv.mu.Lock()
	aliceOpen := h.srv.globalOpenTable[binary.LittleEndian.Uint64(createdFileID(held)[8:16])]
	h.srv.mu.Unlock()
	if aliceOpen == nil {
		t.Fatal("the open alice was granted is not in the global table")
	}

	bob := h.dial("bob")

	// Alice is told to give up the oplock and never answers.
	told := make(chan struct{})
	go func() {
		<-alice.sent
		end(h, alice, held)
		close(told)
	}()

	start := time.Now()
	buf, async := bob.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	waited := time.Since(start)

	<-told

	if !async {
		t.Error("bob's create did not wait for the break at all")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}

	// The acknowledgment timer is 35 seconds. Anything approaching it means the break was left
	// to expire rather than ended with the open that held it.
	if waited > oplockBreakTimeout/4 {
		t.Errorf("bob waited %v for a holder that had gone, want the break to end with the open", waited)
	}

	aliceOpen.mu.Lock()
	defer aliceOpen.mu.Unlock()
	if aliceOpen.oplockState != smb2.OplockNone || aliceOpen.oplockLevel != smb2.OPLOCK_LEVEL_NONE {
		t.Errorf("the open that went away is still in state %d holding %#x", aliceOpen.oplockState, aliceOpen.oplockLevel)
	}
	if aliceOpen.oplockBreak != nil {
		t.Error("the break outlived the open it was sent for")
	}
}

func TestIntegrationBreakEndsWhenSessionDies(t *testing.T) {
	// Losing the connection tears the session down. Alice is gone and will never answer.
	breakEnding(t, func(h *smbTest, alice *testClient, _ []byte) {
		h.srv.deregisterSession(alice.conn, alice.ss.sessionID)
	})
}

func TestIntegrationBreakEndsWhenHandleIsClosed(t *testing.T) {
	// Closing the handle rather than acknowledging is what a client holding a batch oplock
	// does when it was caching the handle and has no further use for the file.
	breakEnding(t, func(_ *smbTest, alice *testClient, held []byte) {
		if _, err := alice.closeHandle(createdFileID(held)); err != nil {
			t.Errorf("alice could not close the handle: %v", err)
		}
	})
}

// TestValidPath is the names a request may act on. The backends key their objects by string, so
// ".." is a segment like any other down there rather than a walk up a tree — which is exactly why
// it has to be turned away here, before the name reaches them ([MS-SMB2] 3.3.5.9).
func TestValidPath(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		// The share root, which a client opens to ask about the volume.
		{"", true},

		{"file", true},
		{"dir/file", true},
		{"dir/sub/file", true},
		{".hidden", true},
		{"a..b", true}, // dots inside a name are part of it
		{"...", true},  // and a name of nothing but dots is still a name
		{"file.", true},

		// Walking out of the share, at every position.
		{"..", false},
		{"../file", false},
		{"dir/../../file", false},
		{"dir/..", false},

		// Naming the same file twice over.
		{".", false},
		{"./file", false},
		{"dir/./file", false},

		// Absolute, and empty components.
		{"/file", false},
		{"dir//file", false},
		{"dir/", false},
	} {
		if got := validPath(tt.path); got != tt.want {
			t.Errorf("validPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// TestIntegrationACreateCannotWalkOutOfTheShare is the name reaching the dispatcher. Nothing looked
// at the components of a create's path: it went from the wire to the backend as a key.
func TestIntegrationACreateCannotWalkOutOfTheShare(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	for _, path := range []string{`..\secrets`, `dir\..\..\secrets`, `\absolute`, `.\file`} {
		buf, err := cl.createErr(path, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN_IF)
		if err != nil {
			t.Fatalf("create of %q: %v", path, err)
		}
		if status := smb2.Header(buf).Status(); status != smb2.STATUS_INVALID_PARAMETER {
			t.Errorf("a create of %q was answered %#x, want STATUS_INVALID_PARAMETER", path, status)
		}
	}

	// Nothing was made on the way to refusing them.
	cl.tc.share.mu.Lock()
	persisted := len(cl.tc.share.persisted)
	cl.tc.share.mu.Unlock()
	if persisted != 0 {
		t.Errorf("%d file(s) left on the share by creates that were refused", persisted)
	}
}

// TestIntegrationARenameCannotWalkOutOfTheShare is the same name arriving the other way. A rename
// names the file it is moving to, and that name was joined to nothing and passed straight on.
func TestIntegrationARenameCannotWalkOutOfTheShare(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	for _, name := range []string{`..\stolen`, `dir\..\..\stolen`, `\absolute`, ``} {
		buf, err := cl.rename(fid, name)
		if err != nil {
			t.Fatalf("rename to %q: %v", name, err)
		}
		if status := smb2.Header(buf).Status(); status != smb2.STATUS_INVALID_PARAMETER {
			t.Errorf("a rename to %q was answered %#x, want STATUS_INVALID_PARAMETER", name, status)
		}
	}
}

// TestIntegrationAnAnonymousSessionOpensNoPipe is the session with nobody behind it reaching for
// the IPC$ share. [MS-SMB2] 3.3.5.9 lets an anonymous caller open only those pipes that allow one,
// and this server offers none; the check was missing altogether, and what stood in for it was the
// account lookup failing, which answers that the session is gone rather than that the caller may
// not have the pipe.
func TestIntegrationAnAnonymousSessionOpensNoPipe(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	// One of the two dialects whose tree connect asks for no signature of its own.
	cl.conn.negotiateDialect = smb2.SMB_DIALECT_302
	cl.conn.dialect = dialectName(cl.conn.negotiateDialect)

	resp, _, err := cl.conn.processRequest(request(t,
		treeConnectRequest(0, cl.ss.sessionID, `\\SERVER\IPC$`)))
	if err != nil {
		t.Fatalf("the tree connect to IPC$ failed: %v", err)
	}
	if status := resp.Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the tree connect to IPC$ was answered %#x", status)
	}
	tid := resp.Header().TreeID()

	// The flag alone, which is the condition the rule is written against. An anonymous session
	// authenticated for real also carries an empty user name, and the account lookup further down
	// the create path happens to refuse that - but it refuses it as a session that is gone rather
	// than as a caller without the right, and it counts nothing. The flag is what decides here.
	cl.ss.isAnonymous = true

	before := h.srv.Stats().PermErrors

	for _, pipe := range []string{"srvsvc", "lsarpc", "mdssvc"} {
		created, _, err := cl.conn.processRequest(request(t, createRequest(1, cl.ss.sessionID, tid,
			pipe, smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, nil)))
		if err != nil {
			t.Fatalf("opening %s: %v", pipe, err)
		}
		if status := created.Header().Status(); status != smb2.STATUS_ACCESS_DENIED {
			t.Errorf("an anonymous session opening %s was answered %#x, want STATUS_ACCESS_DENIED",
				pipe, status)
		}
	}

	// A refusal on grounds of permission is counted as one.
	if got := h.srv.Stats().PermErrors - before; got != 3 {
		t.Errorf("the refusals raised the permission error count by %d, want 3", got)
	}
}
