package main

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

var replayGuid = [16]byte{0x5e, 0x9a, 0x11, 0x22, 0x33}

func TestIntegrationReplayedCreateReturnsTheSameOpen(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	first, _ := alice.createDurable("dir/file", replayGuid, false)
	if status := smb2.Header(first).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first create failed with %#x", status)
	}

	h.srv.mu.Lock()
	opens := len(h.srv.globalOpenTable)
	h.srv.mu.Unlock()

	// The answer never reached the client, so it sends the same create again marked as a
	// replay. It must get the handle it already has rather than a second one on the file.
	second, _ := alice.createDurable("dir/file", replayGuid, true)
	if status := smb2.Header(second).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the replay failed with %#x", status)
	}

	if !bytes.Equal(createdFileID(second), createdFileID(first)) {
		t.Errorf("the replay returned % x, want the open the first create made % x",
			createdFileID(second), createdFileID(first))
	}

	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.globalOpenTable) != opens {
		t.Errorf("the file is open %d time(s), want %d: the replay opened it again",
			len(h.srv.globalOpenTable), opens)
	}
}

func TestIntegrationReplayReportsTheRunningTimeout(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	first, _ := alice.createDurable("dir/file", replayGuid, false)
	granted, found := createdDurableTimeout(first)
	if !found {
		t.Fatal("the first create was granted no durable handle")
	}

	// The clock started with the create being replayed, so the replay reports what is left of
	// the same grant. Asking for something else is not a way to have the handle kept longer:
	// a fresh grant would answer with the ninety seconds asked for here.
	second, _ := alice.createDurableFor("dir/file", replayGuid, true, 90_000)
	replayed, found := createdDurableTimeout(second)
	if !found {
		t.Fatal("the replay was answered without a durable handle context")
	}
	if replayed != granted {
		t.Errorf("the replay reports a timeout of %d ms, want the running %d ms", replayed, granted)
	}
	if !bytes.Equal(createdFileID(second), createdFileID(first)) {
		t.Error("the replay was answered from a different open")
	}
}

func TestIntegrationCreateGuidReusedWithoutReplayFlag(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	h.files.put("dir/other", 1024)

	alice := h.dial("alice")
	alice.createDurable("dir/file", replayGuid, false)

	// The same GUID without the replay flag is not a retry: the client still owes an open for
	// it, so the second create is refused rather than quietly making another handle.
	second, _ := alice.createDurable("dir/file", replayGuid, false)
	if status := smb2.Header(second).Status(); status != smb2.STATUS_DUPLICATE_OBJECTID {
		t.Errorf("Status = %#x, want %#x", status, smb2.STATUS_DUPLICATE_OBJECTID)
	}

	// The GUID identifies the create, not the file, so reusing it on another file is refused
	// just the same.
	other, _ := alice.createDurable("dir/other", replayGuid, false)
	if status := smb2.Header(other).Status(); status != smb2.STATUS_DUPLICATE_OBJECTID {
		t.Errorf("Status = %#x on another file, want %#x", status, smb2.STATUS_DUPLICATE_OBJECTID)
	}
}

func TestIntegrationUsingTheHandleEndsTheReplayWindow(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	first, _ := alice.createDurable("dir/file", replayGuid, false)

	h.srv.mu.Lock()
	op := h.srv.globalOpenTable[binary.LittleEndian.Uint64(createdFileID(first)[8:16])]
	h.srv.mu.Unlock()
	if op == nil {
		t.Fatal("the open the create made is not in the global table")
	}

	op.mu.Lock()
	eligible := op.isReplayEligible
	op.mu.Unlock()
	if !eligible {
		t.Fatal("a durable create left no room for a replay")
	}

	// Any command that names the handle means the client got its answer, so the create behind
	// it is no longer something that can be replayed.
	if _, err := alice.closeHandle(createdFileID(first)); err != nil {
		t.Fatalf("alice could not close the handle: %v", err)
	}

	op.mu.Lock()
	defer op.mu.Unlock()
	if op.isReplayEligible {
		t.Error("the create is still open to replay after the handle was used")
	}
}

func TestIntegrationReplayFromAnotherUserIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createDurable("dir/file", replayGuid, false)

	// A create GUID is only ever looked for among the opens of the client that sent it, so
	// another machine claiming the same GUID finds nothing and simply opens the file itself.
	elsewhere := h.dial("bob")
	other, _ := elsewhere.createDurable("dir/file", replayGuid, true)
	if status := smb2.Header(other).Status(); status != smb2.STATUS_OK {
		t.Errorf("a replay from another client gave %#x, want an ordinary create", status)
	}
	if bytes.Equal(createdFileID(other), createdFileID(held)) {
		t.Error("a replay from another client was handed the open of the first one")
	}

	// On the same machine the GUID does find the open, and there it is the user behind the
	// session that decides who may claim it.
	bob := h.dialAs("bob", [16]byte(alice.conn.clientGuid))
	buf, _ := bob.createDurable("dir/file", replayGuid, true)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_ACCESS_DENIED {
		t.Errorf("Status = %#x, want %#x", status, smb2.STATUS_ACCESS_DENIED)
	}
}

func TestIntegrationReplayFromAnotherSessionIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.createDurable("dir/file", replayGuid, false)

	// The same user on a second session of the same client is a different holder as far as the
	// handle is concerned.
	again := h.dialAs("alice", [16]byte(alice.conn.clientGuid))
	buf, _ := again.createDurable("dir/file", replayGuid, true)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_DUPLICATE_OBJECTID {
		t.Errorf("Status = %#x, want %#x", status, smb2.STATUS_DUPLICATE_OBJECTID)
	}
}

func TestIntegrationReclaimAndRequestTogetherAreRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")

	// Claiming a handle and asking for a new one in the same create is a contradiction.
	both := chainContexts(durableContext(replayGuid, 30_000), reconnectContext(1, 2, replayGuid))

	alice.mid++
	resp, err := alice.send(createRequest(alice.mid, alice.ss.sessionID, alice.tc.treeID,
		"dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, both))
	if err != nil {
		t.Fatalf("the create was not answered: %v", err)
	}
	if status := resp.Header().Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("Status = %#x, want %#x", status, smb2.STATUS_INVALID_PARAMETER)
	}
}

// TestAReplayThatRacesTheLeaseOfItsCreateIsAnswered pins a replay between the moment it reads the
// lease of the open it answers for and the moment it builds the answer, and gives the open a lease
// in between. A replay carrying no lease context of its own has nothing to build a lease context
// out of, so an answer built from a second look at the open - one that finds a lease the first look
// did not - is an answer built out of a request that was never made.
func TestAReplayThatRacesTheLeaseOfItsCreateIsAnswered(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	first, _ := alice.createDurable("dir/file", replayGuid, false)
	if status := smb2.Header(first).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first create failed with %#x", status)
	}

	h.srv.mu.Lock()
	op := h.srv.globalOpenTable[openIDOf(createdFileID(first))]
	h.srv.mu.Unlock()
	if op == nil {
		t.Fatal("the create made no open for a replay to answer for")
	}

	// The create the client sends again when the answer never reached it, carrying no lease
	// context. It is built here rather than in the goroutine below, which must not touch the
	// test at all.
	alice.mid++
	msg := createRequest(alice.mid, alice.ss.sessionID, alice.tc.treeID, "dir/file",
		smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, durableContext(replayGuid, 30_000))
	smb2.Header(msg).SetFlag(smb2.FLAGS_REPLAY_OPERATION)
	reqs, err := smb2.GetRequests(msg, 0, false)
	if err != nil {
		t.Fatalf("the replay did not parse as a request: %v", err)
	}

	// Reading the file is the first thing the answer does, so holding it here stops the replay
	// with the checks behind it and the answer still ahead.
	op.file.mu.Lock()

	type answer struct {
		resp smb2.GenericResponse
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		resp, _, err := alice.conn.processRequest(reqs[0])
		done <- answer{resp, err}
	}()

	// Long enough for the replay to reach the file and stop there. Too short only lets it past
	// the lease before there is one, which is the case that was never in question.
	time.Sleep(50 * time.Millisecond)

	l := &lease{
		leaseKey:   [16]byte{0x7a},
		clientGuid: [16]byte(alice.conn.clientGuid),
		fileName:   "dir/file",
		epoch:      1,
		opens:      make(map[uint64]*open),
	}
	l.join(h.srv.leaseTableFor(l.clientGuid), op, smb2.SMB2_LEASE_READ_CACHING)

	op.file.mu.Unlock()

	got := <-done
	if got.err != nil {
		t.Fatalf("the server gave up on the replay: %v", got.err)
	}
	if status := got.resp.Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the replay was answered %#x, want the open it asked for", status)
	}
	if _, found := createdContext(got.resp.Encode(), smb2.CREATE_REQUEST_LEASE); found {
		t.Error("the replay was answered with a lease it never asked for")
	}
}

// TestAReplayOverAnotherShareIsRefused sends the replay over a tree connect to a share other than
// the one the create was made on. The open belongs to the share it was made on, as a durable handle
// taken up again does, so the GUID is one the client is reusing rather than replaying.
func TestAReplayOverAnotherShareIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	first, _ := alice.createDurable("dir/file", replayGuid, false)
	if status := smb2.Header(first).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first create failed with %#x", status)
	}

	// A second share, connected on the same session as the first: everything the replay is
	// checked against holds except the share it arrives over.
	other := &share{
		name:            "other",
		connectSecurity: h.share.connectSecurity,
		fileSecurity:    h.share.fileSecurity,
	}
	h.srv.shareList[other.name] = other
	tc := newTreeConnectState(2, alice.ss, other, shareAccess)
	alice.ss.mu.Lock()
	alice.ss.treeConnectTable[tc.treeID] = tc
	alice.ss.mu.Unlock()

	alice.mid++
	msg := createRequest(alice.mid, alice.ss.sessionID, tc.treeID, "dir/file",
		smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, durableContext(replayGuid, 30_000))
	smb2.Header(msg).SetFlag(smb2.FLAGS_REPLAY_OPERATION)

	resp, err := alice.send(msg)
	if err != nil {
		t.Fatalf("the server gave up on the replay: %v", err)
	}

	if status := resp.Header().Status(); status != smb2.STATUS_DUPLICATE_OBJECTID {
		t.Errorf("the replay was answered %#x, want the handle kept to the share it was made on", status)
	}
}
