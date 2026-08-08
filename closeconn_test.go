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
