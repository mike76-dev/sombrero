package main

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
)

// TestTreeConnectAnswersWithTheStatusTheSpecNames checks what a refused tree connect is told.
// [MS-SMB2] 3.3.5.7 names a status for each way it can be refused, and they are not
// interchangeable: a client that is told SHARE_UNAVAILABLE where the spec says BAD_NETWORK_NAME
// keeps a name it should have given up on, and one told INVALID_PARAMETER where the spec says
// REQUEST_NOT_ACCEPTED treats a share that is merely busy as a share it asked for wrongly.
func TestTreeConnectAnswersWithTheStatusTheSpecNames(t *testing.T) {
	for _, tt := range []struct {
		what  string
		path  string
		setUp func(h *smbTest)
		want  uint32
	}{
		{
			what: "a share that does not exist",
			path: `\\SERVER\nosuch`,
			want: smb2.STATUS_BAD_NETWORK_NAME,
		},
		{
			what: "a path with no share name in it",
			path: `\\SERVER\`,
			want: smb2.STATUS_INVALID_PARAMETER,
		},
		{
			what: "a share already at its limit of uses",
			path: `\\SERVER\files`,
			setUp: func(h *smbTest) {
				h.restrictTo("alice")
				h.share.currentUses = maxShareUses
			},
			want: smb2.STATUS_REQUEST_NOT_ACCEPTED,
		},
		{
			// The order the two checks are weighed in, which 3.3.5.7 puts the access check first
			// in: a user with no business on the share is told so whether it is busy or not.
			what: "a share at its limit that the user has no access to anyway",
			path: `\\SERVER\files`,
			setUp: func(h *smbTest) {
				h.restrictTo("bob")
				h.share.currentUses = maxShareUses
			},
			want: smb2.STATUS_ACCESS_DENIED,
		},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)
			if tt.setUp != nil {
				tt.setUp(h)
			}

			// Below 3.1.1, where an unsigned tree connect is answered rather than a reason to drop
			// the connection.
			cl := h.dial("alice").speaking(smb2.SMB_DIALECT_302)

			resp, _, err := cl.conn.processRequest(request(t,
				treeConnectRequest(0, cl.ss.sessionID, tt.path)))
			if err != nil {
				t.Fatalf("the tree connect was not answered: %v", err)
			}
			if status := resp.Header().Status(); status != tt.want {
				t.Errorf("the tree connect was answered %#x, want %#x", status, tt.want)
			}
		})
	}
}

// TestTreeConnectGuestNeedsAGuestShare verifies that a session that logged in
// without a password reaches only the shares that offer guest access, whatever
// the policies of its workgroup grant it, and that IPC$ is not one of them:
// [MS-SMB2] 3.3.5.9 restricts the anonymous session on a pipe, not the guest.
func TestTreeConnectGuestNeedsAGuestShare(t *testing.T) {
	for _, tt := range []struct {
		what  string
		path  string
		allow bool
		want  uint32
	}{
		{"a share that does not take guests", `\\SERVER\files`, false, smb2.STATUS_ACCESS_DENIED},
		{"a share that takes guests", `\\SERVER\files`, true, smb2.STATUS_OK},
		{"IPC$, which every session reaches", `\\SERVER\ipc$`, false, smb2.STATUS_OK},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)

			// The policies grant the user the share, so the guest flag is the
			// only thing left to decide it.
			h.restrictTo("alice")
			h.share.allowGuest = tt.allow

			cl := h.dial("alice").speaking(smb2.SMB_DIALECT_302)
			cl.ss.isGuest = true

			resp, _, err := cl.conn.processRequest(request(t,
				treeConnectRequest(0, cl.ss.sessionID, tt.path)))
			if err != nil {
				t.Fatalf("the tree connect was not answered: %v", err)
			}
			if status := resp.Header().Status(); status != tt.want {
				t.Errorf("the tree connect was answered %#x, want %#x", status, tt.want)
			}
		})
	}
}

// TestTreeConnectAnonymousNeedsAnAnonymousShare verifies that a session that
// presented no credentials reaches only the shares that offer anonymous
// access, and that what it is granted there is the same everywhere: the
// policies of the share have nothing to say about it.
func TestTreeConnectAnonymousNeedsAnAnonymousShare(t *testing.T) {
	for _, tt := range []struct {
		what  string
		allow bool
		want  uint32
	}{
		{"a share that does not take anonymous sessions", false, smb2.STATUS_ACCESS_DENIED},
		{"a share that does", true, smb2.STATUS_OK},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)

			// The policies of the session's own user grant it the share, so
			// the anonymous flag is the only thing left to decide it.
			h.restrictTo("alice")
			h.share.allowAnonymous = tt.allow
			h.share.publicDir = "Drop"

			// As a share of a server that encrypts what it can.
			h.share.encryptData = true

			cl := h.dial("alice").speaking(smb2.SMB_DIALECT_302).anonymously()

			resp, _, err := cl.conn.processRequest(request(t,
				treeConnectRequest(0, cl.ss.sessionID, `\\SERVER\files`)))
			if err != nil {
				t.Fatalf("the tree connect was not answered: %v", err)
			}
			if status := resp.Header().Status(); status != tt.want {
				t.Fatalf("the tree connect was answered %#x, want %#x", status, tt.want)
			}

			if tt.want != smb2.STATUS_OK {
				return
			}

			// The tree must not come back demanding encryption: this session
			// holds no key, so a share that insists on it leaves the client
			// nothing it may send, and it gives up without asking anything.
			flags := binary.LittleEndian.Uint32(resp.Encode()[smb2.SMB2HeaderSize+4 : smb2.SMB2HeaderSize+8])
			if flags&smb2.SHAREFLAG_ENCRYPT_DATA != 0 {
				t.Errorf("the tree was reported as encrypted to a session with no key, flags %#x", flags)
			}

			// What the client is told it holds over the public folder, which is
			// everything: clients ask for DELETE on opens they may never delete
			// anything through, and one identity owns every file in there
			// anyway.
			access := binary.LittleEndian.Uint32(resp.Encode()[smb2.SMB2HeaderSize+12 : smb2.SMB2HeaderSize+16])
			for _, want := range []struct {
				bit  uint32
				what string
			}{
				{smb2.FILE_READ_DATA, "read"},
				{smb2.FILE_WRITE_DATA, "write"},
				{smb2.DELETE, "delete"},
			} {
				if access&want.bit == 0 {
					t.Errorf("want the public folder open to %s, got %#x", want.what, access)
				}
			}
		})
	}
}

// TestAnonymousIsRootedAtThePublicFolder verifies the confinement: what an
// anonymous client calls the root of the share is the public folder, so every
// name it can utter lands inside that folder and nothing it can say leads out
// of it. The files of the share proper are not merely hidden from it — they
// have no name it can reach them by.
func TestAnonymousIsRootedAtThePublicFolder(t *testing.T) {
	h := newSMBTest(t)
	h.restrictTo("alice")
	h.share.allowAnonymous = true
	h.share.publicDir = "Drop"

	// The identity such a session is bound to, which the server puts in place
	// at startup when it is configured to admit them.
	if _, err := h.srv.store.EnsureAnonymous(); err != nil {
		t.Fatalf("EnsureAnonymous: %v", err)
	}

	// A file of the share proper, and one already in the public folder.
	h.files.put("secret.txt", 100)
	h.files.putDir("Drop")
	h.files.putData("Drop/left.txt", []byte("dropped earlier"))

	cl := h.dial("alice").anonymously()

	// The name the client uses is the name inside the folder.
	if path := cl.tc.confine("hello.txt"); path != "Drop/hello.txt" {
		t.Errorf("want the create rooted at the folder, got %q", path)
	}
	if path := cl.tc.confine(""); path != "Drop" {
		t.Errorf("want the root of the share to be the folder, got %q", path)
	}

	// The share's own files are out of reach: the name that would find one
	// resolves inside the folder instead.
	if path := cl.tc.confine("secret.txt"); path != "Drop/secret.txt" {
		t.Errorf("want the name confined, got %q", path)
	}

	// And a session with a user behind it is left alone.
	other := h.dial("alice")
	if path := other.tc.confine("secret.txt"); path != "secret.txt" {
		t.Errorf("want an ordinary session unconfined, got %q", path)
	}

	// The create path is where that reaches the client: opening "left.txt"
	// finds the file that only exists inside the folder, and opening
	// "secret.txt" finds nothing, because the share's own file is not what
	// that name resolves to any more.
	resp, _ := cl.create("left.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Errorf("opening a file of the public folder was answered %#x", status)
	}
	resp, _ = cl.create("secret.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OBJECT_NAME_NOT_FOUND {
		t.Errorf("opening a file outside the folder was answered %#x, want it not found", status)
	}
}

// TestConfinementSurvivesTheNamesAClientCanSend verifies that nothing a client
// may put in a path escapes the public folder. The traversal it would take is
// refused before this by validPath, which is what the confinement leans on.
func TestConfinementSurvivesTheNamesAClientCanSend(t *testing.T) {
	h := newSMBTest(t)
	h.share.allowAnonymous = true
	h.share.publicDir = "Drop"

	cl := h.dial("alice").anonymously()

	for _, name := range []string{"../secret.txt", "a/../../secret.txt", "/secret.txt", "./secret.txt"} {
		if validPath(name) {
			t.Errorf("%q was let through as a path, which the confinement relies on refusing", name)
		}
	}

	// What is left after that check cannot lead out of the folder.
	for _, name := range []string{"a/b/c.txt", "sub/file"} {
		if path := cl.tc.confine(name); path != "Drop/"+name {
			t.Errorf("want %q under the folder, got %q", name, path)
		}
	}
}

// anonymousLogin is the client half of a login with no credentials at all: the
// AUTHENTICATE message carries no user, no domain and no response.
type anonymousLogin struct{}

func (anonymousLogin) negotiate() []byte {
	return ntlmClient{}.negotiate()
}

func (anonymousLogin) authenticate(t *testing.T, _ []byte) []byte {
	t.Helper()

	msg := ntlmMessage(3, 64)
	binary.LittleEndian.PutUint32(msg[60:64], ntlm.NTLMSSP_NEGOTIATE_UNICODE|ntlm.NTLMSSP_NEGOTIATE_SIGN)
	return msg
}

// TestASessionWithoutAKeyIsNotHeldToOne verifies that neither an anonymous nor
// a guest session is put into signing or encryption. Neither holds a key the
// client and the server can agree on, so a server that turns either on hands
// out a session that cannot carry a single request: with encryption on, every
// message it then sends is refused for being encrypted, which is how this was
// found.
func TestASessionWithoutAKeyIsNotHeldToOne(t *testing.T) {
	for _, tt := range []struct {
		what  string
		login func(h *smbTest) ntlmLogin
	}{
		{"an anonymous session", func(*smbTest) ntlmLogin { return anonymousLogin{} }},
		{"a guest session", func(h *smbTest) ntlmLogin { return h.withPassword("guest", "") }},
	} {
		t.Run(tt.what, func(t *testing.T) {
			h := newSMBTest(t)

			// A server that encrypts what it can, which is what put the
			// session into encryption whatever it was.
			h.srv.encryptData = true

			c := h.negotiated("anon", [16]byte{9}, smb2.SMB_DIALECT_311)
			c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store, true)
			c.cipherID = smb2.AES_128_GCM
			c.clientCapabilities |= smb2.GLOBAL_CAP_ENCRYPTION

			resp := h.authenticateOver(c, tt.login(h))
			if status := resp.Header().Status(); status != smb2.STATUS_OK {
				t.Fatalf("the session setup was answered %#x, want it established", status)
			}

			c.mu.Lock()
			ss := c.sessionTable[resp.Header().SessionID()]
			c.mu.Unlock()
			if ss == nil {
				t.Fatal("the session was not registered on the connection")
			}

			if ss.signingRequired {
				t.Error("a session with no key of its own was required to sign")
			}
			if ss.encryptData {
				t.Error("a session with no key of its own was put into encryption")
			}

			// And the client is not told to encrypt either.
			flags := binary.LittleEndian.Uint16(resp.Encode()[smb2.SMB2HeaderSize+2 : smb2.SMB2HeaderSize+4])
			if flags&smb2.SESSION_FLAG_ENCRYPT_DATA != 0 {
				t.Errorf("the client was told to encrypt, flags %#x", flags)
			}
		})
	}
}

// TestUpdateShareTakesEffectOnTheRunningServer verifies that turning anonymous
// access on reaches the share the server is serving with. It holds a copy of
// what it was registered with, so without this a change would wait for the next
// restart, which is how a share that looks enabled is still refusing clients.
func TestUpdateShareTakesEffectOnTheRunningServer(t *testing.T) {
	h := newSMBTest(t)
	h.restrictTo("alice")
	if _, err := h.srv.store.EnsureAnonymous(); err != nil {
		t.Fatalf("EnsureAnonymous: %v", err)
	}

	connect := func() uint32 {
		t.Helper()

		cl := h.dial("alice").speaking(smb2.SMB_DIALECT_302).anonymously()
		resp, _, err := cl.conn.processRequest(request(t,
			treeConnectRequest(0, cl.ss.sessionID, `\\SERVER\files`)))
		if err != nil {
			t.Fatalf("the tree connect was not answered: %v", err)
		}
		return resp.Header().Status()
	}

	if status := connect(); status != smb2.STATUS_ACCESS_DENIED {
		t.Fatalf("before the change the tree connect was answered %#x, want it refused", status)
	}

	if err := h.srv.UpdateShare(stores.Share{
		Name:           h.share.name,
		AllowAnonymous: true,
		PublicDir:      "Drop",
	}); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}

	if status := connect(); status != smb2.STATUS_OK {
		t.Fatalf("after the change the tree connect was answered %#x, want it served", status)
	}
}

// publicDirStore records what the server asks it to make the public folder of,
// and can refuse, so that a tree connect can be held to both.
type publicDirStore struct {
	stores.Store
	asked []string
	err   error
}

func (p *publicDirStore) EnsurePublicDir(share, name string) error {
	p.asked = append(p.asked, share+"/"+name)
	return p.err
}

// TestAnonymousOnIndexdMakesThePublicFolder verifies that the folder an
// anonymous session is confined to is made under the reserved identity when
// such a session arrives, and that a share whose folder cannot be made that way
// takes no anonymous sessions at all: it would let one in with nothing it could
// see or write.
func TestAnonymousOnIndexdMakesThePublicFolder(t *testing.T) {
	h := newSMBTest(t)
	h.restrictTo("alice")
	h.share.allowAnonymous = true
	h.share.publicDir = "Drop"
	h.share.backend = "indexd"
	h.share.indexdConns = map[string]*indexdConn{
		stores.AnonymousWorkgroup.String(): {client: h.files},
	}

	store := &publicDirStore{Store: h.srv.store}
	h.srv.store = store

	connect := func() uint32 {
		t.Helper()

		cl := h.dial("alice").speaking(smb2.SMB_DIALECT_302).anonymously()
		resp, _, err := cl.conn.processRequest(request(t,
			treeConnectRequest(0, cl.ss.sessionID, `\\SERVER\files`)))
		if err != nil {
			t.Fatalf("the tree connect was not answered: %v", err)
		}
		return resp.Header().Status()
	}

	if status := connect(); status != smb2.STATUS_OK {
		t.Fatalf("the tree connect was answered %#x, want it served", status)
	}
	if len(store.asked) != 1 || store.asked[0] != "files/Drop" {
		t.Fatalf("want the public folder of the share made, got %v", store.asked)
	}

	// A folder that cannot be made — somebody else's, say — keeps the session
	// out rather than letting it in blind.
	store.err = errors.New("belongs to another workgroup")
	if status := connect(); status != smb2.STATUS_SHARE_UNAVAILABLE {
		t.Fatalf("the tree connect was answered %#x, want the share unavailable", status)
	}
}

// TestAnonymousOnIndexdNeedsTheReservedConnection verifies that an indexd share
// takes anonymous sessions only once the reserved workgroup has a connection of
// its own. What such a session drops is pinned under that connection's app key,
// so without it there is nothing to write to, and a client is told the share is
// unavailable rather than being let in to fail later.
func TestAnonymousOnIndexdNeedsTheReservedConnection(t *testing.T) {
	h := newSMBTest(t)
	h.restrictTo("alice")
	h.share.allowAnonymous = true
	h.share.publicDir = "Drop"

	// A share of the backend that pins per workgroup, with no connections yet.
	h.share.backend = "indexd"
	h.share.indexdConns = make(map[string]*indexdConn)

	connect := func() uint32 {
		t.Helper()

		cl := h.dial("alice").speaking(smb2.SMB_DIALECT_302).anonymously()
		resp, _, err := cl.conn.processRequest(request(t,
			treeConnectRequest(0, cl.ss.sessionID, `\\SERVER\files`)))
		if err != nil {
			t.Fatalf("the tree connect was not answered: %v", err)
		}
		return resp.Header().Status()
	}

	if status := connect(); status != smb2.STATUS_SHARE_UNAVAILABLE {
		t.Fatalf("without the connection the tree connect was answered %#x, want the share unavailable", status)
	}

	// The reserved workgroup connects like any other, through the same flow and
	// under an app key of its own.
	h.share.indexdConns[stores.AnonymousWorkgroup.String()] = &indexdConn{client: h.files}

	if status := connect(); status != smb2.STATUS_OK {
		t.Fatalf("with the connection the tree connect was answered %#x, want it served", status)
	}
}
