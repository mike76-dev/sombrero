package main

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestCloseConnectionTwiceIsHarmless is the connection that more than one thing notices has gone.
// A connection is torn down by whoever gets to it first, and the callers do not know of each
// other: the reading loop finds the socket gone, the periodic sweep finds the connection idle,
// and a ban takes down every connection from the host at once.
func TestCloseConnectionTwiceIsHarmless(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	// A request the server has answered with an interim response and gone off to work on, which
	// is what puts a stop channel on the connection in the first place.
	_, stop := cl.waiting(1, 42, smb2.SMB2_CHANGE_NOTIFY)

	h.srv.closeConnection(cl.conn)

	if !cancelled(stop) {
		t.Fatal("the work behind the request was not told to stop when the connection went")
	}

	cl.conn.mu.Lock()
	kept := len(cl.conn.stopChans)
	cl.conn.mu.Unlock()
	if kept != 0 {
		t.Errorf("the connection kept %d stop channel(s) after being closed, all of them closed already", kept)
	}

	// The second caller finds the work already told to stop and nothing left to close.
	h.srv.closeConnection(cl.conn)
}

// TestCloseConnectionRacesItself is the same two callers arriving at once rather than one after
// the other, which is how they arrive in practice.
func TestCloseConnectionRacesItself(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	for i := range 8 {
		cl.waiting(uint64(i+1), uint64(i+1), smb2.SMB2_CHANGE_NOTIFY)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.srv.closeConnection(cl.conn)
		}()
	}
	wg.Wait()
}

// TestAPanicOnAConnectionTakesOnlyThatConnection is the parsing bug that got through. Every
// goroutine carrying a connection works on what a peer sent, and a panic on one of them is
// unrecovered by default: the process goes, and with it every other connection and every open they
// held. What happens instead is what happens to a connection whose socket is lost - that one is
// torn down, and the rest carry on.
func TestAPanicOnAConnectionTakesOnlyThatConnection(t *testing.T) {
	h := newSMBTest(t)
	victim := h.dial("alice")
	bystander := h.dial("bob")

	// A request the server has gone off to work on, which is what the teardown has to tell to stop.
	_, stop := victim.waiting(1, 42, smb2.SMB2_CHANGE_NOTIFY)

	// One of the goroutines that carry a connection, panicking the way a field nobody measured
	// against the message would make it panic. Nothing recovers it but the connection itself, so a
	// panic that got past this takes the test binary with it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer victim.conn.recoverConnection("testing")
		panic("a field nobody checked the length of")
	}()
	<-done

	if !cancelled(stop) {
		t.Error("the work behind the request was not told to stop when the connection panicked")
	}

	h.srv.mu.Lock()
	_, kept := h.srv.connectionList[victim.conn.clientName]
	_, carriedOn := h.srv.connectionList[bystander.conn.clientName]
	h.srv.mu.Unlock()

	if kept {
		t.Error("the connection that panicked was left in the connection list")
	}
	if !carriedOn {
		t.Error("the connection that panicked took another connection with it")
	}

	// The socket is what puts the peer out of reach, so it is shut whatever else happened.
	if _, err := victim.conn.conn.Write([]byte{0}); err == nil {
		t.Error("the socket of the connection that panicked is still open")
	}
}

// TestAMessageThatDoesNotDecryptIsRefused is the transform header over ciphertext nothing can
// open. The decryption hands back nothing when it fails, and the only thing measured against that
// was the size the sender named: a sender naming zero was answered by a nil message carried on into
// the request path and read there as an SMB2 header.
func TestAMessageThatDoesNotDecryptIsRefused(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").encrypting()

	for _, size := range []uint32{0, 64} {
		msg := make([]byte, smb2.SMB2TransformHeaderSize+64)
		hdr := smb2.Header(msg)
		hdr.SetProtocolID(smb2.PROTOCOL_SMB2_ENCRYPTED)
		hdr.SetEncryptionFlags(1)
		hdr.SetTransformSessionID(cl.ss.sessionID)
		hdr.SetOriginalMessageSize(size)

		if err := cl.conn.acceptRequest(msg); err == nil {
			t.Errorf("a message that does not decrypt, naming %d bytes of plaintext, was accepted", size)
		}
	}
}

// TestABackendThatPanicsOnAReadIsAnswered is the download that comes apart rather than returning.
// It runs on a goroutine of its own, with the read that asked for it waiting on the chunk it was
// filling: a panic there took the whole server, and had it not, it would have left every reader of
// that chunk waiting for a fill that was never going to come.
func TestABackendThatPanicsOnAReadIsAnswered(t *testing.T) {
	h := newSMBTest(t)
	h.files.putData("notes.txt", []byte("another test"))
	h.files.panicOnReads()

	cl := h.dial("alice")
	handle, _ := cl.create("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	read, err := cl.readOver(createdFileID(handle), 12, smb2.SMB2_CHANNEL_NONE)
	if err != nil {
		t.Fatalf("the read was never answered: %v", err)
	}
	if status := smb2.Header(read).Status(); status != smb2.STATUS_DATA_ERROR {
		t.Errorf("the read was answered with %#x, want the I/O error %#x", status, smb2.STATUS_DATA_ERROR)
	}

	// And the server is still there for everybody else, which is the whole of what containment means.
	other := h.dial("bob")
	if _, err := other.createErr("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN); err != nil {
		t.Fatalf("the server stopped serving after the backend came apart: %v", err)
	}
}

// TestABackendThatPanicsOnAPartIsAnswered is the same for the way out. A part goes to the backend
// long after the write it came from was answered, so a panic on the way has nobody left to tell:
// unrecovered it took the server, and the close that waits on the part would have waited for good.
func TestABackendThatPanicsOnAPartIsAnswered(t *testing.T) {
	h := newSMBTest(t)

	cl := h.dial("alice")
	cl.tc.maxUploadSize = 1024

	handle, _ := cl.create("big.bin", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	fid := createdFileID(handle)

	h.files.panicOnWrites()

	// Two parts' worth, so that a part is sent while the handle is still open.
	if _, err := cl.write(fid, 0, make([]byte, 2048)); err != nil {
		t.Fatalf("the write was never answered: %v", err)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close was never answered: %v", err)
	}

	other := h.dial("bob")
	if _, err := other.createErr("notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN); err != nil {
		t.Fatalf("the server stopped serving after the backend came apart: %v", err)
	}
}

// TestAPeerThatHasGoneEndsTheConnection is the client that closes its socket. The end of a stream is
// the end of it — a read that finds one finds it again every time it is asked — so a loop that took
// it for something to wait out sat there at ten reads a second, holding the connection, its sessions
// and its opens, until the sweep came round minutes later and shut the socket under it.
func TestAPeerThatHasGoneEndsTheConnection(t *testing.T) {
	h := newSMBTest(t)

	peer, sock := net.Pipe()
	c := h.srv.newConnection(sock)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.readLoop("10.0.0.1")
	}()

	peer.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the read loop was still going long after the peer had gone")
	}

	h.srv.mu.Lock()
	_, kept := h.srv.connectionList[c.clientName]
	h.srv.mu.Unlock()
	if kept {
		t.Error("the connection was left in the connection list after the peer had gone")
	}
}

// TestWritingToAClosedConnectionGivesUp is the response nobody is left to send. The sender stops
// when the connection is torn down and the sending queue has no room, so a response handed over
// afterwards was waited on for as long as the process lived - stranding the dispatcher, the reading
// loop or a directory watch, and with them the connection, its sessions and everything they hold.
func TestWritingToAClosedConnectionGivesUp(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	h.srv.closeConnection(cl.conn)

	// A full sending queue with nothing draining it, which is where the teardown leaves one.
	for range cap(cl.sent) {
		cl.sent <- nil
	}

	resp := smb2.NewErrorResponse(request(t, echoRequest(0, cl.ss.sessionID, cl.tc.treeID)),
		smb2.STATUS_CANCELLED, 0, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.writeResponse(cl.conn, cl.ss, resp)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writing to a closed connection is still waiting for a sender that has stopped")
	}
}
