package main

import (
	"encoding/binary"
	"testing"

	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
)

// A client is remembered across the connections it dials, by the GUID it names itself with, and the
// dialect it settled on is remembered with it. The dialect is the client's to choose once: every
// later connection of the same client has to arrive on the same one, because a client that speaks
// two dialects at once is either confused or is not the client it says it is. Both are answered by
// throwing the session away rather than serving it ([MS-SMB2] 3.3.5.5.3).

// negotiated brings up a connection that has finished negotiating and holds no session yet, which
// is where a client stands when it sends the first leg of a session setup.
func (h *smbTest) negotiated(name string, guid [16]byte, dialect uint16) *connection {
	h.t.Helper()

	c := h.newTestConnection(name)
	c.clientGuid = guid[:]
	c.negotiateDialect = dialect
	c.dialect = dialectName(dialect)
	c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

	h.srv.mu.Lock()
	h.srv.connectionList[c.clientName] = c
	h.srv.mu.Unlock()

	return c
}

// authenticateOver runs a whole session setup over the connection: the client says who it is, the
// server challenges, and the client answers. What comes back is the answer to the second leg, which
// is where the client is told it has a session - or that it is not getting one.
func (h *smbTest) authenticateOver(c *connection, nc ntlmClient) smb2.GenericResponse {
	h.t.Helper()

	send := func(mid, sid uint64, token []byte) smb2.GenericResponse {
		resp, _, err := c.processRequest(request(h.t, tokenRequest(mid, sid, 0, token)))
		if err != nil {
			h.t.Fatalf("the server gave up on the session setup: %v", err)
		}

		return resp
	}

	challenge := send(1, 0, nc.negotiate())
	if status := challenge.Header().Status(); status != smb2.STATUS_MORE_PROCESSING_REQUIRED {
		h.t.Fatalf("the first leg was answered with %#x, want the exchange to go on", status)
	}

	// The challenge to answer travels in the security buffer of the response, and the session it
	// opened in the header - which is what the client names on the leg that follows.
	buf := challenge.Encode()
	off := binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize+4 : smb2.SMB2HeaderSize+6])
	length := binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize+6 : smb2.SMB2HeaderSize+8])

	return send(2, challenge.Header().SessionID(), nc.authenticate(h.t, buf[off:off+length]))
}

// clientRecord is what the server remembers about the client with the given GUID, or nil if it has
// never seen it.
func (h *smbTest) clientRecord(guid [16]byte) *smbClient {
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()

	return h.srv.globalClientTable[guid]
}

// live reports whether the session is one the server would still serve: known to it, and running on
// the connection it was set up over.
func (h *smbTest) live(c *connection, sid uint64) bool {
	h.srv.mu.Lock()
	_, known := h.srv.globalSessionTable[sid]
	h.srv.mu.Unlock()

	c.mu.Lock()
	_, on := c.sessionTable[sid]
	c.mu.Unlock()

	return known && on
}

// TestSessionSetupRemembersTheDialectOfTheClient is the first connection of a client. Nothing is
// refused here - what matters is that the dialect it authenticated on is written down, because
// every later connection of the same client is held against it.
func TestSessionSetupRemembersTheDialectOfTheClient(t *testing.T) {
	h := newSMBTest(t)
	nc := h.withPassword("carol", "hunter2")

	guid := [16]byte{0xc1}
	c := h.negotiated("carol-first", guid, smb2.SMB_DIALECT_311)

	resp := h.authenticateOver(c, nc)
	if status := resp.Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the session setup was answered with %#x, want the client let in", status)
	}
	if !h.live(c, resp.Header().SessionID()) {
		t.Fatal("the client authenticated but has no session to show for it")
	}

	cl := h.clientRecord(guid)
	if cl == nil {
		t.Fatal("the client that authenticated was not written down")
	}
	if cl.dialect != smb2.SMB_DIALECT_311 {
		t.Errorf("the client is written down as speaking %#x, want the dialect it negotiated", cl.dialect)
	}
}

// TestSessionSetupTakesAClientBackOnTheSameDialect is the second connection of a client that has
// not changed its mind. It is the control on the refusal below: without it, a server that turned
// every returning client away would pass that test just as well.
func TestSessionSetupTakesAClientBackOnTheSameDialect(t *testing.T) {
	h := newSMBTest(t)
	nc := h.withPassword("carol", "hunter2")

	guid := [16]byte{0xc2}
	first := h.negotiated("carol-first", guid, smb2.SMB_DIALECT_311)
	if status := h.authenticateOver(first, nc).Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first session setup was answered with %#x, want the client let in", status)
	}

	second := h.negotiated("carol-second", guid, smb2.SMB_DIALECT_311)

	resp := h.authenticateOver(second, nc)
	if status := resp.Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second session setup was answered with %#x, want the client let in again", status)
	}
	if !h.live(second, resp.Header().SessionID()) {
		t.Fatal("the returning client authenticated but has no session on its second connection")
	}
}

// TestSessionSetupRefusesAClientThatChangesDialect is the same client coming back on a dialect
// other than the one it is remembered by. The session it was in the middle of setting up is thrown
// away and the client is told so, rather than being served on two dialects at once.
func TestSessionSetupRefusesAClientThatChangesDialect(t *testing.T) {
	h := newSMBTest(t)
	nc := h.withPassword("carol", "hunter2")

	guid := [16]byte{0xc3}
	first := h.negotiated("carol-first", guid, smb2.SMB_DIALECT_311)
	if status := h.authenticateOver(first, nc).Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first session setup was answered with %#x, want the client let in", status)
	}

	// The same client, by GUID, on a dialect it never negotiated before.
	second := h.negotiated("carol-second", guid, smb2.SMB_DIALECT_302)

	resp := h.authenticateOver(second, nc)
	if status := resp.Header().Status(); status != smb2.STATUS_USER_SESSION_DELETED {
		t.Fatalf("the session setup was answered with %#x, want the client turned away", status)
	}

	// The session the exchange had opened is gone: the client authenticated, so one had been
	// created and would go on standing had the refusal not taken it back down.
	second.mu.Lock()
	left := len(second.sessionTable)
	second.mu.Unlock()
	if left != 0 {
		t.Errorf("the refused connection is still carrying %d session(s)", left)
	}

	h.srv.mu.Lock()
	known := len(h.srv.globalSessionTable)
	h.srv.mu.Unlock()
	if known != 1 {
		t.Errorf("the server knows of %d sessions, want only the one the first connection holds", known)
	}

	// And the client is still the one it was: a refused connection must not be what the next one
	// is held against, or a client could walk its way onto any dialect a connection at a time.
	if cl := h.clientRecord(guid); cl == nil || cl.dialect != smb2.SMB_DIALECT_311 {
		t.Error("the refused dialect was written down as the dialect of the client")
	}
}

// TestSessionSetupDoesNotRememberOlderClients is the dialect family the rule is not written for.
// Before 3.x there are no channels to bind and no client to hold a dialect against, so nothing is
// written down - and a client of that family is never refused for having changed it.
func TestSessionSetupDoesNotRememberOlderClients(t *testing.T) {
	h := newSMBTest(t)
	nc := h.withPassword("carol", "hunter2")

	guid := [16]byte{0xc4}
	c := h.negotiated("carol-21", guid, smb2.SMB_DIALECT_21)

	if status := h.authenticateOver(c, nc).Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the session setup was answered with %#x, want the client let in", status)
	}
	if h.clientRecord(guid) != nil {
		t.Fatal("a client from before the 3.x dialects was written down")
	}

	// Coming back on another dialect of the same family is nothing to hold against it.
	again := h.negotiated("carol-202", guid, smb2.SMB_DIALECT_202)
	if status := h.authenticateOver(again, nc).Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the second session setup was answered with %#x, want the client let in", status)
	}
}
