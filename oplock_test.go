package main

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// oplockTestClient counts the fake clients built for a test, so that each gets a name of its
// own: the channel list of a session is keyed by the name of the connection.
var oplockTestClient int

// newOplockOpen builds an open on the given file, together with the session and the connection
// that carry it. Everything the break path reaches for is in place: the connection accepts
// whatever is sent to it, and the session lists it as a channel. The returned channel is what
// the client would have received.
func newOplockOpen(t *testing.T, s *server, sh *share, path string) (*open, *connection, chan []byte) {
	t.Helper()

	oplockTestClient++
	sent := make(chan []byte, 8)

	// Each fake client gets a GUID of its own, so that the lease paths, which key everything
	// off it, tell them apart.
	var guid [16]byte
	guid[0] = byte(oplockTestClient)

	c := &connection{
		server:           s,
		clientGuid:       guid[:],
		clientName:       fmt.Sprintf("client-%d", oplockTestClient),
		negotiateDialect: smb2.SMB_DIALECT_311,
		writeChan:        sent,
		closeChan:        make(chan struct{}),
	}

	ss := &session{
		sessionID:   uint64(oplockTestClient),
		state:       sessionValid,
		openTable:   make(map[uint64]*open),
		channelList: map[string]*channel{c.clientName: {connection: c}},
		connection:  c,
	}

	op := &open{
		fileID:        uint64(oplockTestClient) * 10,
		durableFileID: uint64(oplockTestClient) * 20,
		session:       ss,
		treeConnect:   &treeConnect{share: sh},
		connection:    c,
		pathName:      path,
	}
	op.cond = sync.NewCond(&op.mu)

	ss.openTable[op.fileID] = op
	s.globalOpenTable[op.durableFileID] = op
	s.connectionList[c.clientName] = c

	return op, c, sent
}

// newCachingServer builds a server with the tables the oplock and lease paths reach for, and
// nothing else.
func newCachingServer() *server {
	return &server{
		globalOpenTable:      make(map[uint64]*open),
		globalLeaseTableList: make(map[[16]byte]*leaseTable),
		connectionList:       make(map[string]*connection),
	}
}

// recvBreak takes the break notification the server sent to a client, failing the test if none
// arrives. The notification travels on a goroutine of its own, so it cannot simply be read.
func recvBreak(t *testing.T, sent chan []byte) []byte {
	t.Helper()

	select {
	case buf := <-sent:
		return buf
	case <-time.After(5 * time.Second):
		t.Fatal("no oplock break notification was sent")
		return nil
	}
}

func TestOplockEligible(t *testing.T) {
	files := &treeConnect{share: &share{name: "files"}}
	pipes := &treeConnect{share: &share{name: "ipc$"}}

	tests := []struct {
		name      string
		requested uint8
		tc        *treeConnect
		isDir     bool
		want      bool
	}{
		{name: "a batch oplock on a file", requested: smb2.OPLOCK_LEVEL_BATCH, tc: files, want: true},
		{name: "an exclusive oplock on a file", requested: smb2.OPLOCK_LEVEL_EXCLUSIVE, tc: files, want: true},
		{
			// Level II would have to be broken whenever anybody writes, and the server only
			// breaks on a create, so granting one would leave stale data cached.
			name: "level II is never granted", requested: smb2.OPLOCK_LEVEL_II, tc: files, want: false,
		},
		{name: "a lease is never granted", requested: smb2.OPLOCK_LEVEL_LEASE, tc: files, want: false},
		{name: "asking for nothing gets nothing", requested: smb2.OPLOCK_LEVEL_NONE, tc: files, want: false},
		{name: "a named pipe has nothing to cache", requested: smb2.OPLOCK_LEVEL_BATCH, tc: pipes, want: false},
		{name: "a directory is only ever leased", requested: smb2.OPLOCK_LEVEL_BATCH, tc: files, isDir: true, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := oplockEligible(test.requested, test.tc, test.isDir); got != test.want {
				t.Errorf("oplockEligible = %v, want %v", got, test.want)
			}
		})
	}
}

func TestOpensOn(t *testing.T) {
	sh := &share{name: "files"}
	other := &share{name: "other"}
	s := newCachingServer()

	op, _, _ := newOplockOpen(t, s, sh, "dir/file")
	same, _, _ := newOplockOpen(t, s, sh, "dir/file")
	elsewhere, _, _ := newOplockOpen(t, s, sh, "dir/other")

	// The same path on another share is a different file, however alike the two look.
	crossShare, _, _ := newOplockOpen(t, s, other, "dir/file")

	found := s.opensOn(sh, "dir/file", op)
	if len(found) != 1 || found[0] != same {
		t.Fatalf("opensOn found %d open(s), want only the other open on the same file", len(found))
	}

	for _, absent := range []*open{op, elsewhere, crossShare} {
		for _, f := range found {
			if f == absent {
				t.Error("opensOn returned an open that is not on the file asked about")
			}
		}
	}
}

func TestGrantOplock(t *testing.T) {
	t.Run("an open that has the file to itself is granted what it asked for", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()
		op, _, _ := newOplockOpen(t, s, sh, "dir/file")

		if got := s.grantOplock(op, smb2.OPLOCK_LEVEL_BATCH, op.treeConnect, "dir/file"); got != smb2.OPLOCK_LEVEL_BATCH {
			t.Errorf("granted %#x, want %#x", got, smb2.OPLOCK_LEVEL_BATCH)
		}

		op.mu.Lock()
		defer op.mu.Unlock()
		if op.oplockLevel != smb2.OPLOCK_LEVEL_BATCH || op.oplockState != smb2.OplockHeld {
			t.Errorf("level = %#x, state = %d, want %#x and held", op.oplockLevel, op.oplockState, smb2.OPLOCK_LEVEL_BATCH)
		}
	})

	t.Run("an open that finds company gets nothing and breaks whoever holds one", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		holder, _, sent := newOplockOpen(t, s, sh, "dir/file")
		s.grantOplock(holder, smb2.OPLOCK_LEVEL_BATCH, holder.treeConnect, "dir/file")

		// A second open appears on the file after the first was granted its oplock, which is
		// the race the grant has to close: the newcomer gets nothing, and the holder is told.
		latecomer, _, _ := newOplockOpen(t, s, sh, "dir/file")
		if got := s.grantOplock(latecomer, smb2.OPLOCK_LEVEL_BATCH, latecomer.treeConnect, "dir/file"); got != smb2.OPLOCK_LEVEL_NONE {
			t.Errorf("granted %#x to an open sharing the file, want none", got)
		}

		buf := recvBreak(t, sent)
		if cmd := smb2.Header(buf).Command(); cmd != smb2.SMB2_OPLOCK_BREAK {
			t.Errorf("Command = %#x, want %#x", cmd, smb2.SMB2_OPLOCK_BREAK)
		}
		if level := buf[smb2.SMB2HeaderSize+2]; level != smb2.OPLOCK_LEVEL_NONE {
			t.Errorf("the break asked for level %#x, want none", level)
		}
		if fid := buf[smb2.SMB2HeaderSize+8 : smb2.SMB2HeaderSize+24]; !bytes.Equal(fid, holder.id()) {
			t.Errorf("the break names % x, want % x", fid, holder.id())
		}

		holder.mu.Lock()
		defer holder.mu.Unlock()
		if holder.oplockState != smb2.OplockBreaking {
			t.Errorf("the holder is in state %d, want breaking", holder.oplockState)
		}
	})

	t.Run("an open in the middle of a break is not handed a new oplock", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		// A file that was created earlier in the session is opened again while the break on it
		// is still outstanding: the same open comes back, and it must not be put back into
		// holding, or the break would never end and whoever waits for it would wait forever.
		op, _, _ := newOplockOpen(t, s, sh, "dir/file")
		s.grantOplock(op, smb2.OPLOCK_LEVEL_BATCH, op.treeConnect, "dir/file")
		wait, _ := op.startOplockBreak()

		if got := s.grantOplock(op, smb2.OPLOCK_LEVEL_BATCH, op.treeConnect, "dir/file"); got != smb2.OPLOCK_LEVEL_NONE {
			t.Errorf("granted %#x to an open that is breaking, want none", got)
		}

		op.mu.Lock()
		state := op.oplockState
		op.mu.Unlock()
		if state != smb2.OplockBreaking {
			t.Fatalf("the open is in state %d, want breaking", state)
		}

		op.completeOplockBreak()
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
			t.Fatal("the break could no longer be completed")
		}
	})

	t.Run("opens on different files do not disturb each other", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		first, _, sent := newOplockOpen(t, s, sh, "dir/file")
		s.grantOplock(first, smb2.OPLOCK_LEVEL_BATCH, first.treeConnect, "dir/file")

		second, _, _ := newOplockOpen(t, s, sh, "dir/other")
		if got := s.grantOplock(second, smb2.OPLOCK_LEVEL_BATCH, second.treeConnect, "dir/other"); got != smb2.OPLOCK_LEVEL_BATCH {
			t.Errorf("granted %#x for a file nobody else has open, want %#x", got, smb2.OPLOCK_LEVEL_BATCH)
		}

		select {
		case <-sent:
			t.Error("an open on another file broke the oplock")
		case <-time.After(100 * time.Millisecond):
		}
	})
}

func TestAcknowledgeOplockBreak(t *testing.T) {
	tests := []struct {
		name string

		held     uint8
		breaking bool

		// What the client answers with.
		ack uint8

		want uint32
	}{
		{name: "a batch oplock given up entirely", held: smb2.OPLOCK_LEVEL_BATCH, breaking: true, ack: smb2.OPLOCK_LEVEL_NONE, want: smb2.STATUS_OK},
		{name: "a batch oplock dropped to level II", held: smb2.OPLOCK_LEVEL_BATCH, breaking: true, ack: smb2.OPLOCK_LEVEL_II, want: smb2.STATUS_OK},
		{name: "a batch oplock dropped to exclusive", held: smb2.OPLOCK_LEVEL_BATCH, breaking: true, ack: smb2.OPLOCK_LEVEL_EXCLUSIVE, want: smb2.STATUS_OK},
		{name: "an exclusive oplock given up entirely", held: smb2.OPLOCK_LEVEL_EXCLUSIVE, breaking: true, ack: smb2.OPLOCK_LEVEL_NONE, want: smb2.STATUS_OK},
		{
			// Exclusive is not below exclusive: a client may only answer with a lower level.
			name: "an exclusive oplock cannot be kept", held: smb2.OPLOCK_LEVEL_EXCLUSIVE, breaking: true,
			ack: smb2.OPLOCK_LEVEL_EXCLUSIVE, want: smb2.STATUS_INVALID_OPLOCK_PROTOCOL,
		},
		{
			name: "a lease is never held and so never given up", held: smb2.OPLOCK_LEVEL_BATCH, breaking: true,
			ack: smb2.OPLOCK_LEVEL_LEASE, want: smb2.STATUS_INVALID_PARAMETER,
		},
		{
			// Nothing was broken, so the acknowledgment answers a break that never happened.
			name: "an unsolicited acknowledgment", held: smb2.OPLOCK_LEVEL_BATCH, breaking: false,
			ack: smb2.OPLOCK_LEVEL_NONE, want: smb2.STATUS_INVALID_DEVICE_STATE,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			op := &open{oplockLevel: test.held, oplockState: smb2.OplockHeld}
			op.cond = sync.NewCond(&op.mu)

			var wait chan struct{}
			if test.breaking {
				wait, _ = op.startOplockBreak()
			}

			if got := op.acknowledgeOplockBreak(test.ack); got != test.want {
				t.Errorf("status = %#x, want %#x", got, test.want)
			}

			if !test.breaking {
				// An acknowledgment that was refused for answering nothing must leave the
				// oplock where it was, or a client could talk its way out of one.
				op.mu.Lock()
				defer op.mu.Unlock()
				if op.oplockState != smb2.OplockHeld || op.oplockLevel != test.held {
					t.Error("an unsolicited acknowledgment took the oplock away")
				}
				return
			}

			// However the client answered, and whether or not the answer was accepted, the
			// oplock is given up in full: the server grants no level below exclusive, so there
			// is no level it could leave the client with.
			select {
			case <-wait:
			case <-time.After(5 * time.Second):
				t.Fatal("whoever was waiting for the break was never released")
			}

			op.mu.Lock()
			defer op.mu.Unlock()
			if op.oplockState != smb2.OplockNone || op.oplockLevel != smb2.OPLOCK_LEVEL_NONE {
				t.Errorf("level = %#x, state = %d, want none and none", op.oplockLevel, op.oplockState)
			}
			if op.oplockBreak != nil {
				t.Error("the break outlived its acknowledgment")
			}
		})
	}
}

func TestBreakOplocksOn(t *testing.T) {
	t.Run("a break the client answers", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		holder, _, sent := newOplockOpen(t, s, sh, "dir/file")
		s.grantOplock(holder, smb2.OPLOCK_LEVEL_BATCH, holder.treeConnect, "dir/file")

		// The client answers as soon as it is told, which is what releases the create that is
		// waiting for the file.
		go func() {
			recvBreak(t, sent)
			holder.acknowledgeOplockBreak(smb2.OPLOCK_LEVEL_NONE)
		}()

		done := make(chan struct{})
		go func() {
			s.breakHoldersOn(sh, "dir/file", nil, nil)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the wait for the break outlived the acknowledgment")
		}

		holder.mu.Lock()
		defer holder.mu.Unlock()
		if holder.oplockState != smb2.OplockNone {
			t.Errorf("the holder is in state %d, want none", holder.oplockState)
		}
	})

	t.Run("a client that cannot be reached loses the oplock at once", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		holder, c, _ := newOplockOpen(t, s, sh, "dir/file")
		s.grantOplock(holder, smb2.OPLOCK_LEVEL_BATCH, holder.treeConnect, "dir/file")

		// The connection is gone, so the notification can be delivered nowhere. Waiting out
		// the acknowledgment timer for a client that will never answer would hold the file
		// hostage for the length of it.
		close(c.closeChan)

		done := make(chan struct{})
		go func() {
			s.breakHoldersOn(sh, "dir/file", nil, nil)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the wait outlived a client that could not be reached")
		}

		holder.mu.Lock()
		defer holder.mu.Unlock()
		if holder.oplockState != smb2.OplockNone || holder.oplockLevel != smb2.OPLOCK_LEVEL_NONE {
			t.Error("the oplock of an unreachable client was left in place")
		}
	})

	t.Run("nothing to break returns without a word to anybody", func(t *testing.T) {
		sh := &share{name: "files"}
		s := newCachingServer()

		op, _, sent := newOplockOpen(t, s, sh, "dir/file")

		if s.hasHoldersOn(sh, "dir/file", nil, nil) {
			t.Error("a file nobody holds an oplock on was reported as held")
		}

		s.breakHoldersOn(sh, "dir/file", nil, nil)

		select {
		case <-sent:
			t.Error("a break was sent for an oplock that was never granted")
		case <-time.After(100 * time.Millisecond):
		}

		// The create that follows still gets its oplock: finding nothing to break must not
		// leave the open in a state that refuses one.
		if got := s.grantOplock(op, smb2.OPLOCK_LEVEL_BATCH, op.treeConnect, "dir/file"); got != smb2.OPLOCK_LEVEL_BATCH {
			t.Errorf("granted %#x, want %#x", got, smb2.OPLOCK_LEVEL_BATCH)
		}
	})
}

func TestReleaseOplockFreesWaiters(t *testing.T) {
	sh := &share{name: "files"}
	s := newCachingServer()

	holder, _, sent := newOplockOpen(t, s, sh, "dir/file")
	s.grantOplock(holder, smb2.OPLOCK_LEVEL_BATCH, holder.treeConnect, "dir/file")

	done := make(chan struct{})
	go func() {
		s.breakHoldersOn(sh, "dir/file", nil, nil)
		close(done)
	}()

	recvBreak(t, sent)

	// The client never answers; instead its open dies, which is what happens when the session
	// is torn down or the handle is closed while the break is outstanding. The create that is
	// waiting must carry on straight away rather than sit out the acknowledgment timer.
	holder.releaseOplock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait outlived the open it was waiting for")
	}

	if s.hasHoldersOn(sh, "dir/file", nil, nil) {
		t.Error("the oplock survived the open that held it")
	}
}
