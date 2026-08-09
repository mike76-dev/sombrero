package main

import (
	"sync"
	"testing"

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
