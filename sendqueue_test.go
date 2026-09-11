package main

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestSendQueueOrder checks small messages go before file data, and each lane stays FIFO.
func TestSendQueueOrder(t *testing.T) {
	q := newSendQueue()
	if msg := q.pop(); msg != nil {
		t.Fatalf("an empty queue gave %q", msg)
	}

	q.push([]byte("read 1"), true)
	q.push([]byte("read 2"), true)
	q.push([]byte("listing"), false)
	q.push([]byte("read 3"), true)
	q.push([]byte("echo"), false)

	var got []string
	for msg := q.pop(); msg != nil; msg = q.pop() {
		got = append(got, string(msg))
	}

	want := []string{"listing", "echo", "read 1", "read 2", "read 3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sent %v, want %v", got, want)
	}

	select {
	case <-q.ready:
	default:
		t.Error("queueing did not wake the sender")
	}
}

// TestFileDataLane checks only successful reads count as file data.
func TestFileDataLane(t *testing.T) {
	resp := func(command uint16, status uint32) smb2.GenericResponse {
		msg := echoRequest(1, 1, 1)
		smb2.Header(msg).SetCommand(command)
		reqs, err := smb2.GetRequests(msg, 0, false)
		if err != nil {
			t.Fatalf("the request did not parse: %v", err)
		}
		return smb2.NewErrorResponse(reqs[0], status, 0, nil)
	}

	for _, tt := range []struct {
		name    string
		command uint16
		status  uint32
		bulk    bool
	}{
		{"a read that succeeded", smb2.SMB2_READ, smb2.STATUS_OK, true},
		{"the interim response of a read", smb2.SMB2_READ, smb2.STATUS_PENDING, false},
		{"a read past the end", smb2.SMB2_READ, smb2.STATUS_END_OF_FILE, false},
		{"a listing", smb2.SMB2_QUERY_DIRECTORY, smb2.STATUS_OK, false},
		{"a write", smb2.SMB2_WRITE, smb2.STATUS_OK, false},
	} {
		if got := carriesFileData(resp(tt.command, tt.status)); got != tt.bulk {
			t.Errorf("%s: file data %v, want %v", tt.name, got, tt.bulk)
		}
	}
}

// dialPiped is dial with the real dispatcher and sender, writing into a pipe the test reads.
// A pipe only moves data as fast as the test reads it, which makes it a deliberately slow link.
func (h *smbTest) dialPiped(user string) (*testClient, net.Conn) {
	h.t.Helper()

	cl := h.connectAs(user, nextClientGUID())
	server, client := net.Pipe()

	c := cl.conn
	c.conn = server
	go c.sendResponses()
	go c.processRequests()

	h.t.Cleanup(func() {
		c.once.Do(func() { close(c.closeChan) })
		server.Close()
		client.Close()
	})

	return cl, client
}

// enqueue queues a request for the dispatcher, as the reading loop does, without waiting.
func (cl *testClient) enqueue(msg []byte) {
	cl.h.t.Helper()

	reqs, err := smb2.GetRequests(msg, 0, false)
	if err != nil {
		cl.h.t.Fatalf("the message did not parse as a request: %v", err)
	}

	c := cl.conn
	c.mu.Lock()
	c.requestList[reqs[0].Header().MessageID()] = reqs[0]
	c.mu.Unlock()
	c.wake()
}

// readWire reads one message off the client end of the pipe, as framed on the wire.
func readWire(t *testing.T, conn net.Conn) []byte {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}

	var frame [4]byte
	if _, err := io.ReadFull(conn, frame[:]); err != nil {
		t.Fatalf("reading the frame: %v", err)
	}
	msg := make([]byte, binary.BigEndian.Uint32(frame[:]))
	if _, err := io.ReadFull(conn, msg); err != nil {
		t.Fatalf("reading the message: %v", err)
	}

	return msg
}

// TestSmallResponsesDoNotWaitBehindReads checks a small response waits behind at most the read already
// on the wire, not every queued one (sha256sum in one shell, tab completion in another).
func TestSmallResponsesDoNotWaitBehindReads(t *testing.T) {
	const (
		fileSize = 24 << 20
		readSize = 4 << 20
		reads    = 4
	)

	h := newSMBTest(t)
	h.files.putData("movie.mp4", make([]byte, fileSize))

	cl, wire := h.dialPiped("alice")
	sid, tid := cl.ss.sessionID, cl.tc.treeID

	// One read fills the cache, so the reads below are answered inside the dispatcher.
	cl.enqueue(createRequestSharing(1, sid, tid, "movie.mp4", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, readAccess, 0, everySharing, nil))
	created := readWire(t, wire)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the open was answered %#x", status)
	}
	fid := createdFileID(created)

	cl.enqueue(readRequest(2, sid, tid, fid, 0, 64<<10))
	for {
		if smb2.Header(readWire(t, wire)).Status() != smb2.STATUS_PENDING {
			break
		}
	}

	// The reads, then the one small request behind them.
	const echoMID = 3 + reads
	for i := 0; i < reads; i++ {
		cl.enqueue(readRequest(uint64(3+i), sid, tid, fid, uint64(i)*readSize, readSize))
	}
	cl.enqueue(echoRequest(echoMID, sid, tid))

	// Let the server process everything it can before the test starts reading.
	time.Sleep(500 * time.Millisecond)

	var order []uint64
	for len(order) < reads+1 {
		msg := readWire(t, wire)
		hdr := smb2.Header(msg)
		if status := hdr.Status(); status != smb2.STATUS_OK {
			t.Fatalf("message %d was answered %#x", hdr.MessageID(), status)
		}
		order = append(order, hdr.MessageID())
	}

	// Only the read already on the wire when the echo was answered may go before it.
	for i, mid := range order {
		if mid == echoMID {
			if i > 1 {
				t.Errorf("the echo came out after %d reads (order %v); want it behind the one read on the wire at most", i, order)
			}
			return
		}
	}
	t.Fatalf("the echo never came out: %v", order)
}
