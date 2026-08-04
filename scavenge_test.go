package main

import (
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A connection that nobody authenticates over is of no use to anybody: nothing can be asked of
// it, and it still counts against the limit on how many connections one address may hold. After
// a while the server drops it (3.3.6.3).

// connectedAt registers a bare connection that has been open since the given time, carrying no
// sessions until the test puts one on it.
func (h *smbTest) connectedAt(name string, since time.Time) *connection {
	c := h.newTestConnection(name)
	c.creationTime = since

	h.srv.mu.Lock()
	h.srv.connectionList[name] = c
	h.srv.mu.Unlock()

	return c
}

// stillConnected reports whether the server still knows about the connection.
func (h *smbTest) stillConnected(name string) bool {
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()

	_, found := h.srv.connectionList[name]

	return found
}

// withSession puts a session in the given state on the connection, as a session setup that got
// that far would have left it.
func withSession(c *connection, id uint64, state int) {
	ss := newSessionState(id, c)
	ss.state = state

	c.mu.Lock()
	c.sessionTable[id] = ss
	c.mu.Unlock()
}

func TestScavengeDropsConnectionNobodyAuthenticatedOver(t *testing.T) {
	long := time.Now().Add(-connectionScavengeTimeout - time.Second)

	cases := map[string]func(c *connection){
		// Nothing at all: the client opened a socket and went quiet before negotiating.
		"nothing was negotiated": func(*connection) {},

		// A dialect was agreed and the client stopped there.
		"a dialect and no session": func(c *connection) {
			c.negotiateDialect = smb2.SMB_DIALECT_311
			c.dialect = dialectName(smb2.SMB_DIALECT_311)
		},

		// A session setup was begun and never finished, so nobody is authenticated even though
		// the table is not empty.
		"a session setup left half done": func(c *connection) {
			c.negotiateDialect = smb2.SMB_DIALECT_311
			c.dialect = dialectName(smb2.SMB_DIALECT_311)
			withSession(c, 1, sessionInProgress)
		},
	}

	for name, setUp := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSMBTest(t)
			c := h.connectedAt("quiet", long)
			setUp(c)

			h.srv.scavengeConnections()

			if h.stillConnected("quiet") {
				t.Error("the connection was kept although nobody had authenticated over it")
			}
		})
	}
}

func TestScavengeKeepsConnectionThatAuthenticated(t *testing.T) {
	long := time.Now().Add(-connectionScavengeTimeout - time.Second)

	cases := map[string]int{
		"a session in use": sessionValid,

		// A session whose authentication has run out has still been authenticated once, and the
		// client may take it back through a session setup rather than starting over.
		"a session that expired": sessionExpired,
	}

	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSMBTest(t)
			c := h.connectedAt("busy", long)
			c.negotiateDialect = smb2.SMB_DIALECT_311
			c.dialect = dialectName(smb2.SMB_DIALECT_311)
			withSession(c, 1, state)

			h.srv.scavengeConnections()

			if !h.stillConnected("busy") {
				t.Error("a connection somebody had authenticated over was dropped")
			}
		})
	}
}

// The timeout is what makes this safe: a client that has just connected is in the middle of
// negotiating, and dropping it would make the server unusable rather than tidy.
func TestScavengeKeepsConnectionThatJustArrived(t *testing.T) {
	h := newSMBTest(t)
	h.connectedAt("arriving", time.Now())

	h.srv.scavengeConnections()

	if !h.stillConnected("arriving") {
		t.Error("a connection was dropped before it had time to negotiate")
	}
}

// A client in the middle of its work is not disturbed, however long it has been connected.
func TestScavengeLeavesAWorkingClientAlone(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.conn.creationTime = time.Now().Add(-time.Hour)

	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("alice's create failed with %#x", status)
	}

	h.srv.scavengeConnections()

	if !h.stillConnected(alice.conn.clientName) {
		t.Fatal("a client with a file open was dropped")
	}

	// And the handle it was given still works.
	resp, err := alice.write(createdFileID(held), 0, []byte("hello"))
	if err != nil {
		t.Fatalf("the write did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("a write after the sweep returned %#x", status)
	}
}

// Dropping the connection takes the opens on it with it, so whatever it was holding on a file is
// given up rather than left to be waited out.
func TestScavengeReleasesWhatTheConnectionHeld(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if !h.srv.hasHoldersOn(h.share, "dir/file", nil, nil, [16]byte{}) {
		t.Fatal("alice was not granted an oplock to begin with")
	}

	// Her session goes back to being half authenticated, as though the setup had never finished,
	// and the connection ages past the timeout.
	alice.conn.creationTime = time.Now().Add(-time.Hour)
	alice.ss.mu.Lock()
	alice.ss.state = sessionInProgress
	alice.ss.mu.Unlock()

	h.srv.scavengeConnections()

	if h.stillConnected(alice.conn.clientName) {
		t.Fatal("the connection was kept although nobody was authenticated over it")
	}
	if h.srv.hasHoldersOn(h.share, "dir/file", nil, nil, [16]byte{}) {
		t.Error("the oplock outlived the connection that held it")
	}
}
