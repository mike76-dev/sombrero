package main

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

var testCreateGuid = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

func TestGrantDurability(t *testing.T) {
	tests := []struct {
		name string

		// What the client asks for, in milliseconds.
		requested uint32

		want time.Duration
	}{
		{
			name:      "a request of its own is honoured",
			requested: 30_000,
			want:      30 * time.Second,
		},
		{
			// A client that expresses no preference leaves the choice to the server.
			name:      "no request at all falls back to the default",
			requested: 0,
			want:      defaultDurableTimeout,
		},
		{
			// An open held for the asking would keep its memory and its unfinished upload
			// for as long as the client cared to name.
			name:      "more than the maximum is capped",
			requested: uint32(maxDurableTimeout/time.Millisecond) + 60_000,
			want:      maxDurableTimeout,
		},
		{
			name:      "exactly the maximum is left alone",
			requested: uint32(maxDurableTimeout / time.Millisecond),
			want:      maxDurableTimeout,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			op := &open{file: &fileState{}}
			granted := op.grantDurability(smb2.DurableHandleRequestV2{
				Timeout:    test.requested,
				CreateGuid: testCreateGuid,
			})

			// The value handed back to the client is in milliseconds and has to agree with
			// the one the server will actually hold the open for.
			if want := uint32(test.want / time.Millisecond); granted != want {
				t.Errorf("granted timeout = %d ms, want %d ms", granted, want)
			}
			if op.durableTimeout != test.want {
				t.Errorf("Open.durableTimeout = %v, want %v", op.durableTimeout, test.want)
			}
			if !op.isDurable {
				t.Error("the open was not marked durable")
			}
			if op.createGuid != testCreateGuid {
				t.Errorf("createGuid = % x, want % x", op.createGuid, testCreateGuid)
			}
		})
	}
}

func TestOrphanDurableOpens(t *testing.T) {
	durable := &open{
		file:      &fileState{},
		fileID:    1,
		isDurable: true,
		// A cached chunk is worth a lot of memory and nothing of what the open achieved.
		buffer:     map[uint64]*readChunk{0: {}},
		cacheOrder: []uint64{0},
	}
	ordinary := &open{file: &fileState{}, fileID: 2}

	ss := &session{openTable: map[uint64]*open{1: durable, 2: ordinary}}

	if n := ss.orphanDurableOpens(); n != 1 {
		t.Errorf("orphaned %d open(s), want 1", n)
	}

	// The durable open leaves the session so that tearing the session down afterwards
	// walks past it; the ordinary one stays behind to be closed with the session.
	if _, found := ss.openTable[1]; found {
		t.Error("the durable open was left in the open table of the session")
	}
	if _, found := ss.openTable[2]; !found {
		t.Error("the ordinary open was taken out of the open table of the session")
	}

	if durable.disconnectTime.IsZero() {
		t.Error("the durable open was not stamped with a disconnect time")
	}
	if len(durable.buffer) != 0 || durable.cacheOrder != nil {
		t.Error("the read cache of the durable open was not dropped")
	}

	// An ordinary open that got a disconnect time would be picked up by neither the reclaim
	// path nor the sweeper, and would sit in the global table forever.
	if !ordinary.disconnectTime.IsZero() {
		t.Error("the ordinary open was stamped with a disconnect time")
	}
}

// newReclaimable builds an orphaned durable open belonging to the given user on the given
// share, together with the server that holds it.
func newReclaimable(t *testing.T, userName, workgroup string, sh *share) (*connection, *open) {
	t.Helper()

	owner := &session{userName: userName, workgroup: workgroup}
	op := &open{
		file:           &fileState{},
		fileID:         10,
		durableFileID:  20,
		session:        owner,
		treeConnect:    &treeConnect{share: sh},
		isDurable:      true,
		createGuid:     testCreateGuid,
		durableTimeout: defaultDurableTimeout,
		disconnectTime: time.Now(),
	}

	s := &server{globalOpenTable: map[uint64]*open{op.durableFileID: op}}

	return &connection{server: s}, op
}

func TestReclaimDurableOpen(t *testing.T) {
	sh := &share{name: "files"}
	otherShare := &share{name: "other"}

	// The request that succeeds; each case below spoils exactly one thing about it.
	valid := smb2.DurableHandleReconnectV2{FileID: 10, DurableID: 20, CreateGuid: testCreateGuid}

	tests := []struct {
		name string
		rec  smb2.DurableHandleReconnectV2

		// Who is asking, and through which share.
		userName  string
		workgroup string
		share     *share

		// Applied to the open before the reclaim is attempted.
		prepare func(op *open)

		want bool
	}{
		{
			name: "the client that created it gets it back",
			rec:  valid, userName: "alice", workgroup: "wg", share: sh,
			want: true,
		},
		{
			name:     "an unknown durable ID is not found",
			rec:      smb2.DurableHandleReconnectV2{FileID: 10, DurableID: 999, CreateGuid: testCreateGuid},
			userName: "alice", workgroup: "wg", share: sh,
			want: false,
		},
		{
			// The file ID travels in the clear on every request that uses the handle, so
			// the GUID is the only thing proving who is asking.
			name:     "a wrong create GUID is refused",
			rec:      smb2.DurableHandleReconnectV2{FileID: 10, DurableID: 20, CreateGuid: [16]byte{9, 9, 9}},
			userName: "alice", workgroup: "wg", share: sh,
			want: false,
		},
		{
			name:     "a wrong file ID is refused",
			rec:      smb2.DurableHandleReconnectV2{FileID: 11, DurableID: 20, CreateGuid: testCreateGuid},
			userName: "alice", workgroup: "wg", share: sh,
			want: false,
		},
		{
			name: "another user may not take the handle",
			rec:  valid, userName: "bob", workgroup: "wg", share: sh,
			want: false,
		},
		{
			name: "the same name in another workgroup may not take the handle",
			rec:  valid, userName: "alice", workgroup: "elsewhere", share: sh,
			want: false,
		},
		{
			name: "the handle does not cross over to another share",
			rec:  valid, userName: "alice", workgroup: "wg", share: otherShare,
			want: false,
		},
		{
			name: "an open still in use is not up for reclaiming",
			rec:  valid, userName: "alice", workgroup: "wg", share: sh,
			prepare: func(op *open) { op.disconnectTime = time.Time{} },
			want:    false,
		},
		{
			// The sweeper takes durability away before it cancels an expired open, which
			// is what stops a reclaim racing it.
			name: "an open the sweeper has claimed is refused",
			rec:  valid, userName: "alice", workgroup: "wg", share: sh,
			prepare: func(op *open) { op.isDurable = false },
			want:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, op := newReclaimable(t, "alice", "wg", sh)
			if test.prepare != nil {
				test.prepare(op)
			}

			ss := &session{userName: test.userName, workgroup: test.workgroup, openTable: make(map[uint64]*open)}
			tc := &treeConnect{share: test.share}

			got := c.reclaimDurableOpen(test.rec, ss, tc)
			if (got != nil) != test.want {
				t.Fatalf("reclaimed = %v, want %v", got != nil, test.want)
			}

			if !test.want {
				// A refused reclaim must leave the open exactly where it was, or the next
				// attempt by the rightful owner would find it altered.
				if len(ss.openTable) != 0 {
					t.Error("a refused reclaim put the open into the session anyway")
				}
				if op.session == ss {
					t.Error("a refused reclaim handed the open to the asking session")
				}
				return
			}

			if op.session != ss || op.treeConnect != tc || op.connection != c {
				t.Error("the open was not reattached to the session, tree connect and connection")
			}
			if !op.disconnectTime.IsZero() {
				t.Error("the open is still marked as disconnected")
			}
			if ss.openTable[op.fileID] != op {
				t.Error("the open did not reappear in the open table of the session")
			}
			if tc.openCount != 1 {
				t.Errorf("TreeConnect.openCount = %d, want 1", tc.openCount)
			}
		})
	}
}

func TestSweepDurableOpens(t *testing.T) {
	var cancelled sync.Map
	newOpen := func(dfid uint64, disconnectedFor time.Duration, durable bool) *open {
		op := &open{
			file:           &fileState{},
			durableFileID:  dfid,
			pathName:       "file",
			isDurable:      durable,
			durableTimeout: defaultDurableTimeout,
			cancel:         func() { cancelled.Store(dfid, true) },
		}
		if disconnectedFor > 0 {
			op.disconnectTime = time.Now().Add(-disconnectedFor)
		}
		return op
	}

	attached := newOpen(1, 0, true)                                  // In use, never disconnected
	waiting := newOpen(2, defaultDurableTimeout/2, true)             // Orphaned, still within its time
	expired := newOpen(3, defaultDurableTimeout+time.Second, true)   // Orphaned for too long
	ordinary := newOpen(4, defaultDurableTimeout+time.Second, false) // Not durable at all

	s := &server{globalOpenTable: map[uint64]*open{
		1: attached, 2: waiting, 3: expired, 4: ordinary,
	}}

	s.sweepDurableOpens()

	for _, keep := range []uint64{1, 2, 4} {
		if _, found := s.globalOpenTable[keep]; !found {
			t.Errorf("open %d was swept but should have been left alone", keep)
		}
		if _, gone := cancelled.Load(keep); gone {
			t.Errorf("open %d was cancelled but should have been left alone", keep)
		}
	}

	if _, found := s.globalOpenTable[3]; found {
		t.Error("the expired open was left in the global open table")
	}
	if _, gone := cancelled.Load(uint64(3)); !gone {
		t.Error("the expired open was not cancelled")
	}

	// Durability is taken away before the open is cancelled, so that a reclaim running at
	// the same moment fails its own check instead of reattaching a dead open.
	if expired.isDurable {
		t.Error("the expired open is still marked durable")
	}

	// A second sweep must find nothing left to do rather than double-cancel.
	s.sweepDurableOpens()
	if len(s.globalOpenTable) != 3 {
		t.Errorf("a repeated sweep changed the global open table: %d entries, want 3", len(s.globalOpenTable))
	}
}

// A reconnect is a create like any other, so the contexts it carries are answered rather than
// ignored. The lease in particular: it was released when the connection was lost, so a client
// that asks after it is told it holds nothing, rather than left to guess.
func TestIntegrationReconnectAnswersTheContextsItCarries(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createDurable("dir/file", testCreateGuid, false)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the durable create failed with %#x", status)
	}
	fid := createdFileID(held)

	// The connection is lost, and the open is set aside for reclaiming.
	if n := alice.ss.orphanDurableOpens(); n != 1 {
		t.Fatalf("%d opens were set aside, want 1", n)
	}

	// The client comes back, claims the handle, and asks for the lease it thinks it holds.
	again := h.dial("alice")
	contexts := chainContexts(
		reconnectContext(binary.LittleEndian.Uint64(fid[:8]), binary.LittleEndian.Uint64(fid[8:16]), testCreateGuid),
		leaseContext(aliceKey, rwh, 2),
	)
	buf, err := again.createWith("dir/file", smb2.OPLOCK_LEVEL_LEASE, smb2.FILE_OPEN, contexts)
	if err != nil {
		t.Fatalf("the reconnect failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the reconnect was answered with %#x", status)
	}
	if !bytes.Equal(createdFileID(buf), fid) {
		t.Errorf("the reconnect handed back % x, want the handle % x", createdFileID(buf), fid)
	}

	data, found := createdContext(buf, smb2.CREATE_REQUEST_LEASE)
	if !found {
		t.Fatal("the client asked for a lease and was not answered")
	}
	if len(data) < 20 {
		t.Fatalf("the lease response context is %d bytes long, want at least 20", len(data))
	}
	if state := binary.LittleEndian.Uint32(data[16:20]); state != smb2.SMB2_LEASE_NONE {
		t.Errorf("the client was told it holds %#x, want SMB2_LEASE_NONE", state)
	}
}

// TestIntegrationLogoffKeepsTheDurableHandles is what becomes of a durable handle when the client
// says it is done with the session. [MS-SMB2] 3.3.5.6 detaches it and leaves it to the scavenger,
// exactly as the loss of the connection does, and closes only the handles that were never made
// durable. The logoff went straight to the teardown, which cancels every open of the session, so
// the one thing the client asked to be able to come back to was the one thing it could not.
func TestIntegrationLogoffKeepsTheDurableHandles(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)
	h.files.put("dir/other", 1024)

	alice := h.dial("alice")

	held, _ := alice.createDurable("dir/file", testCreateGuid, false)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the durable create failed with %#x", status)
	}
	fid := createdFileID(held)

	// A handle of the ordinary kind, which the logoff is to close.
	ordinary, _ := alice.create("dir/other", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(ordinary).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create failed with %#x", status)
	}
	closed := openIDOf(createdFileID(ordinary))

	buf, err := alice.logoff()
	if err != nil {
		t.Fatalf("the logoff failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the logoff was answered %#x, want it served", status)
	}

	h.srv.mu.Lock()
	_, stillThere := h.srv.globalOpenTable[closed]
	h.srv.mu.Unlock()
	if stillThere {
		t.Error("a handle that was never made durable outlived the session it was opened on")
	}

	// The same user comes back on a session of its own and claims the handle.
	again := h.dial("alice")
	buf, err = again.createWith("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN,
		reconnectContext(binary.LittleEndian.Uint64(fid[:8]), binary.LittleEndian.Uint64(fid[8:16]), testCreateGuid))
	if err != nil {
		t.Fatalf("the reconnect failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the reconnect after a logoff was answered %#x, want the handle back", status)
	}
	if !bytes.Equal(createdFileID(buf), fid) {
		t.Errorf("the reconnect handed back % x, want the handle % x", createdFileID(buf), fid)
	}
}
