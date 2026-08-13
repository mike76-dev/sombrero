package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha512"
	"encoding/asn1"
	"encoding/binary"
	"strings"
	"sync"
	"testing"

	"github.com/mike76-dev/sombrero/kdf"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/spnego"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"golang.org/x/crypto/md4"
)

// ntlmSignature opens every NTLM message, and the message type behind it is what tells the legs
// of an exchange apart.
var ntlmSignature = []byte("NTLMSSP\x00")

// ntlmMessage builds the opening bytes of an NTLM message of the given type. What the binding
// reads out of a token is only which leg it belongs to, so nothing behind the type matters here.
func ntlmMessage(typ uint32, size int) []byte {
	msg := make([]byte, max(size, 12))
	copy(msg[:8], ntlmSignature)
	binary.LittleEndian.PutUint32(msg[8:12], typ)

	return msg
}

// tokenRequest builds the bytes of a session setup request carrying an authentication token,
// which trails the fixed part of the body and is pointed at from within it.
func tokenRequest(mid, sid uint64, flags uint8, token []byte) []byte {
	msg := sessionSetupRequest(mid, sid, flags)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[12:14], uint16(len(msg)))
	binary.LittleEndian.PutUint16(body[14:16], uint16(len(token)))

	return append(msg, token...)
}

// bindingRequest builds the bytes of a session setup request that binds a session to the
// connection it arrives on, carrying the given authentication token.
func bindingRequest(mid, sid uint64, token []byte) []byte {
	return tokenRequest(mid, sid, smb2.SESSION_FLAG_BINDING, token)
}

// binding turns a message into the session setup request the binding path works with.
func binding(t *testing.T, msg []byte) smb2.SessionSetupRequest {
	t.Helper()

	return smb2.SessionSetupRequest{Request: *request(t, msg)}
}

// joining brings up a second connection of the same client, as a connection carrying a binding
// request stands: negotiated exactly like the one the session lives on, and not yet a channel of
// anything.
func (h *smbTest) joining(cl *testClient) *connection {
	h.t.Helper()

	c := h.newTestConnection("joining-" + cl.conn.clientName)
	c.clientGuid = cl.conn.clientGuid
	c.negotiateDialect = cl.conn.negotiateDialect
	c.dialect = cl.conn.dialect
	c.preauthIntegrityHashID = smb2.SHA_512
	c.preauthIntegrityHashValue = bytes.Repeat([]byte{0x3c}, 64)

	return c
}

// TestPrepareBindingAcceptsASecondConnection is the request that has everything it needs: the
// session is live, the connection belongs to the same client, speaks the same dialect, and the
// request is signed.
func TestPrepareBindingAcceptsASecondConnection(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)

	msg := signed(t, bindingRequest(1, cl.ss.sessionID, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)

	ss, status := c.prepareBinding(binding(t, msg))
	if status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}
	if ss != cl.ss {
		t.Fatal("the binding is not against the session the request named")
	}

	// The exchange gets a hash of its own, started off from where this connection stood at the
	// end of its negotiation.
	c.mu.Lock()
	pss, found := c.preauthSessionTable[cl.ss.sessionID]
	c.mu.Unlock()
	if !found {
		t.Fatal("the exchange was not given a preauthentication hash")
	}
	if !bytes.Equal(pss.preauthIntegrityHashValue, c.preauthIntegrityHashValue) {
		t.Fatal("the hash of the exchange did not start off from the hash of the connection")
	}
}

// TestPrepareBindingRefusals walks the reasons a connection is not allowed to join a session.
// Each of them is answered with a status of its own, which is what the client is told.
func TestPrepareBindingRefusals(t *testing.T) {
	for _, tt := range []struct {
		name string
		// setUp bends the session or the joining connection out of shape, and returns the message
		// to send if it wants one other than the signed request against the live session.
		setUp func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte
		want  uint32
	}{
		{
			name: "the session is not there",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				return signed(t, bindingRequest(1, cl.ss.sessionID+1000, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
			},
			want: smb2.STATUS_USER_SESSION_DELETED,
		},
		{
			name: "the connection speaks another dialect",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				c.negotiateDialect = smb2.SMB_DIALECT_302
				return nil
			},
			want: smb2.STATUS_INVALID_PARAMETER,
		},
		{
			name: "the request is not signed",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				return bindingRequest(1, cl.ss.sessionID, nil)
			},
			want: smb2.STATUS_INVALID_PARAMETER,
		},
		{
			name: "the connection belongs to another client",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				c.clientGuid = bytes.Repeat([]byte{0xee}, 16)
				return nil
			},
			want: smb2.STATUS_USER_SESSION_DELETED,
		},
		{
			name: "the session is still authenticating",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				cl.ss.state = sessionInProgress
				return nil
			},
			want: smb2.STATUS_REQUEST_NOT_ACCEPTED,
		},
		{
			name: "the session has expired",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				cl.ss.state = sessionExpired
				return nil
			},
			want: smb2.STATUS_NETWORK_SESSION_EXPIRED,
		},
		{
			name: "the session is anonymous",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				cl.ss.isAnonymous = true
				return nil
			},
			want: smb2.STATUS_NOT_SUPPORTED,
		},
		{
			name: "the session belongs to a guest",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				cl.ss.isGuest = true
				return nil
			},
			want: smb2.STATUS_NOT_SUPPORTED,
		},
		{
			name: "the session already runs on this connection",
			setUp: func(t *testing.T, h *smbTest, cl *testClient, c *connection) []byte {
				c.sessionTable[cl.ss.sessionID] = cl.ss
				return nil
			},
			want: smb2.STATUS_REQUEST_NOT_ACCEPTED,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			cl := h.dial("alice").signing()
			c := h.joining(cl)

			msg := tt.setUp(t, h, cl, c)
			if msg == nil {
				msg = signed(t, bindingRequest(1, cl.ss.sessionID, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
			}

			ss, status := c.prepareBinding(binding(t, msg))
			if status != tt.want {
				t.Fatalf("the server answered %#x, want %#x", status, tt.want)
			}
			if ss != nil {
				t.Fatal("the server handed back a session although it refused the binding")
			}

			// Nothing of the exchange is left behind on a request that did not get through.
			c.mu.Lock()
			_, found := c.preauthSessionTable[cl.ss.sessionID]
			c.mu.Unlock()
			if found {
				t.Error("a refused binding left a preauthentication hash behind")
			}
		})
	}
}

// TestPrepareBindingKeepsTheHashOfTheExchange is the second request of the same exchange. The
// hash covers everything the exchange has carried so far, so a later request must not start it
// over.
func TestPrepareBindingKeepsTheHashOfTheExchange(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)

	msg := signed(t, bindingRequest(1, cl.ss.sessionID, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	if _, status := c.prepareBinding(binding(t, msg)); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}

	c.updatePreauthHash(cl.ss.sessionID, msg)

	c.mu.Lock()
	moved := bytes.Clone(c.preauthSessionTable[cl.ss.sessionID].preauthIntegrityHashValue)
	c.mu.Unlock()

	if _, status := c.prepareBinding(binding(t, msg)); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x on the second request, want it to take the binding", status)
	}

	c.mu.Lock()
	now := c.preauthSessionTable[cl.ss.sessionID].preauthIntegrityHashValue
	c.mu.Unlock()

	if !bytes.Equal(now, moved) {
		t.Fatal("the second request of the exchange started the hash over")
	}
}

// TestPrepareBindingWithoutAHashOnOlderDialects is the dialect family that binds without a
// preauthentication hash: there is nothing to derive the key of the new channel from but the
// authentication itself.
func TestPrepareBindingWithoutAHashOnOlderDialects(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing().speaking(smb2.SMB_DIALECT_302)
	c := h.joining(cl)

	msg := signed(t, bindingRequest(1, cl.ss.sessionID, nil), cl.ss.signingKey, smb2.SMB_DIALECT_302, 0)
	if _, status := c.prepareBinding(binding(t, msg)); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}

	c.mu.Lock()
	_, found := c.preauthSessionTable[cl.ss.sessionID]
	c.mu.Unlock()
	if found {
		t.Fatal("a dialect that has no preauthentication hash was given one")
	}
}

// TestBindingToken walks the shapes an authentication token arrives in. Which leg of the exchange
// the request belongs to is read out of the NTLM message, because a binding cannot be told apart
// by the session table the way an ordinary session setup can.
func TestBindingToken(t *testing.T) {
	negotiate := ntlmMessage(1, 32)
	authenticate := ntlmMessage(3, 64)

	wrappedInit := func(t *testing.T, token []byte) []byte {
		t.Helper()
		buf, err := spnego.EncodeNegTokenInit([]asn1.ObjectIdentifier{spnego.NlmpOid}, token)
		if err != nil {
			t.Fatalf("could not wrap the token: %v", err)
		}
		return buf
	}
	wrappedResp := func(t *testing.T, token []byte) []byte {
		t.Helper()
		buf, err := spnego.EncodeNegTokenResp(1, spnego.NlmpOid, token, nil)
		if err != nil {
			t.Fatalf("could not wrap the token: %v", err)
		}
		return buf
	}

	for _, tt := range []struct {
		name string
		buf  func(t *testing.T) []byte
		want []byte
		leg2 bool
	}{
		{
			name: "a bare negotiate",
			buf:  func(t *testing.T) []byte { return negotiate },
			want: negotiate,
		},
		{
			name: "a bare authenticate",
			buf:  func(t *testing.T) []byte { return authenticate },
			want: authenticate,
			leg2: true,
		},
		{
			name: "a negotiate wrapped as an init token",
			buf:  func(t *testing.T) []byte { return wrappedInit(t, negotiate) },
			want: negotiate,
		},
		{
			name: "an authenticate wrapped as a response token",
			buf:  func(t *testing.T) []byte { return wrappedResp(t, authenticate) },
			want: authenticate,
			leg2: true,
		},
		{
			name: "a negotiate wrapped as a response token",
			buf:  func(t *testing.T) []byte { return wrappedResp(t, negotiate) },
			want: negotiate,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			token, authenticate := bindingToken(tt.buf(t))
			if !bytes.Equal(token, tt.want) {
				t.Error("the token did not come out of its wrapping")
			}
			if authenticate != tt.leg2 {
				t.Errorf("the token was read as leg two = %v, want %v", authenticate, tt.leg2)
			}
		})
	}
}

// TestUpdatePreauthHash is the hash of the exchange moving on. Each message of the exchange is
// rolled into it, so that the key derived at the end stands for everything that was said.
func TestUpdatePreauthHash(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)

	sid := cl.ss.sessionID
	start := bytes.Clone(c.preauthIntegrityHashValue)

	msg := signed(t, bindingRequest(1, sid, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	if _, status := c.prepareBinding(binding(t, msg)); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}

	c.updatePreauthHash(sid, msg)

	want := sha512.Sum512(append(bytes.Clone(start), msg...))

	c.mu.Lock()
	got := bytes.Clone(c.preauthSessionTable[sid].preauthIntegrityHashValue)
	c.mu.Unlock()

	if !bytes.Equal(got, want[:]) {
		t.Fatal("the hash of the exchange is not the one over what the exchange has carried")
	}

	// A second message carries on from where the first left off rather than starting over.
	second := signed(t, bindingRequest(2, sid, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	c.updatePreauthHash(sid, second)

	next := sha512.Sum512(append(bytes.Clone(want[:]), second...))

	c.mu.Lock()
	got = bytes.Clone(c.preauthSessionTable[sid].preauthIntegrityHashValue)
	c.mu.Unlock()

	if !bytes.Equal(got, next[:]) {
		t.Fatal("the hash did not carry on from where it stood")
	}
}

// TestUpdatePreauthHashWithoutAnExchange is the message that belongs to no binding this
// connection is running. There is nothing to roll it into.
func TestUpdatePreauthHashWithoutAnExchange(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)

	c.updatePreauthHash(cl.ss.sessionID, []byte("a message"))

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.preauthSessionTable) != 0 {
		t.Fatal("a message with no exchange behind it made one")
	}
}

// TestUpdatePreauthHashOnAnOlderDialect is the dialect family that keeps no such hash. The
// message is let by without anything happening to it.
func TestUpdatePreauthHashOnAnOlderDialect(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)

	sid := cl.ss.sessionID
	c.preauthSessionTable[sid] = &preauthSession{preauthIntegrityHashValue: []byte("as it stood")}
	c.negotiateDialect = smb2.SMB_DIALECT_302

	c.updatePreauthHash(sid, []byte("a message"))

	if got := string(c.preauthSessionTable[sid].preauthIntegrityHashValue); got != "as it stood" {
		t.Fatalf("the hash moved to %q on a dialect that keeps none", got)
	}
}

// TestTakePreauthHash is the end of the exchange: the hash is handed over to derive the key of
// the new channel with, and the exchange is forgotten.
func TestTakePreauthHash(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)

	sid := cl.ss.sessionID
	msg := signed(t, bindingRequest(1, sid, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	if _, status := c.prepareBinding(binding(t, msg)); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}
	c.updatePreauthHash(sid, msg)

	c.mu.Lock()
	want := bytes.Clone(c.preauthSessionTable[sid].preauthIntegrityHashValue)
	c.mu.Unlock()

	if got := c.takePreauthHash(sid); !bytes.Equal(got, want) {
		t.Fatal("what was handed over is not the hash of the exchange")
	}

	if got := c.takePreauthHash(sid); got != nil {
		t.Fatal("the exchange was still there after it had been taken")
	}
}

// TestAddChannel is the connection joining the channel list of a session. The channel starts out
// without a signing key: the key comes from the authentication that established it, which has not
// been worked through yet.
func TestAddChannel(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)

	cl.ss.addChannel(c)

	ch := cl.ss.channel(c)
	if ch == nil {
		t.Fatal("the connection did not become a channel of the session")
	}
	if len(ch.signingKey) != 0 {
		t.Fatal("the new channel was given a signing key before it had authenticated")
	}
}

// TestAddChannelLeavesAStandingChannelAlone is the connection that is already a channel. Its key
// is the one it authenticated with, and adding it again must not take that away.
func TestAddChannelLeavesAStandingChannelAlone(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	key := cl.keyed(0x11)

	cl.ss.addChannel(cl.conn)

	if ch := cl.ss.channel(cl.conn); !bytes.Equal(ch.signingKey, key) {
		t.Fatal("the channel lost the key it had authenticated with")
	}
}

// TestAddChannelReplacesAConnectionOfTheSameName is the name coming round again on a connection
// of its own. The channel list is keyed by name, and the channel belongs to whichever connection
// holds the name now, without the key of the old one.
func TestAddChannelReplacesAConnectionOfTheSameName(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	cl.keyed(0x11)

	again := h.newTestConnection(cl.conn.clientName)
	cl.ss.addChannel(again)

	ch := cl.ss.channel(again)
	if ch == nil {
		t.Fatal("the connection that holds the name now is not a channel of the session")
	}
	if len(ch.signingKey) != 0 {
		t.Fatal("the new connection took over the key of the old one")
	}
	if cl.ss.channel(cl.conn) != nil {
		t.Fatal("the connection that lost the name is still a channel of the session")
	}
}

// TestBindSessionAnswersTheFirstLegWithAChallenge is the opening request of a binding exchange.
// The client has said who it is; what comes back is the challenge it has to answer, and the
// session is not joined by anybody yet.
func TestBindSessionAnswersTheFirstLegWithAChallenge(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)
	c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

	sid := cl.ss.sessionID
	msg := signed(t, bindingRequest(1, sid, ntlmMessage(1, 32)), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	ssr := binding(t, msg)

	if _, status := c.prepareBinding(ssr); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}

	resp, ss, err := c.bindSession(cl.ss, ssr)
	if err != nil {
		t.Fatalf("the server gave up on the binding: %v", err)
	}
	if ss != cl.ss {
		t.Fatal("the first leg did not carry on with the session it was about")
	}
	if status := resp.Header().Status(); status != smb2.STATUS_MORE_PROCESSING_REQUIRED {
		t.Fatalf("the response carries %#x, want the exchange to go on", status)
	}
	// The credits granted sit in the field a request asks for them in, so that is what reads them.
	if credits := resp.Header().CreditRequest(); credits != 1 {
		t.Fatalf("the response grants %d credits, want one while the exchange is unfinished", credits)
	}

	// Nothing is bound until the client has answered the challenge.
	c.mu.Lock()
	_, bound := c.sessionTable[sid]
	c.mu.Unlock()
	if bound {
		t.Fatal("the session was joined before the client had authenticated")
	}
	if cl.ss.channel(c) != nil {
		t.Fatal("the connection became a channel before the client had authenticated")
	}

	// The request went into the hash of the exchange, which the key of the new channel will be
	// derived from at the end.
	c.mu.Lock()
	moved := !bytes.Equal(c.preauthSessionTable[sid].preauthIntegrityHashValue, c.preauthIntegrityHashValue)
	c.mu.Unlock()
	if !moved {
		t.Fatal("the request of the first leg was left out of the hash of the exchange")
	}
}

// TestBindSessionRefusesAnAuthenticationThatFails is the second leg from somebody who cannot
// prove they are the owner of the session. The session stays where it was.
func TestBindSessionRefusesAnAuthenticationThatFails(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)
	c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

	sid := cl.ss.sessionID

	// An authenticate message the server cannot make anything of is the position of anybody who
	// does not hold the password.
	msg := signed(t, bindingRequest(1, sid, ntlmMessage(3, 64)), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	ssr := binding(t, msg)

	if _, status := c.prepareBinding(ssr); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}

	resp, ss, err := c.bindSession(cl.ss, ssr)
	if err != nil {
		t.Fatalf("the server gave up on the binding: %v", err)
	}
	if ss != nil {
		t.Fatal("the server handed back a session although the client did not authenticate")
	}
	if status := resp.Header().Status(); status != smb2.STATUS_NO_SUCH_USER {
		t.Fatalf("the response carries %#x, want the client turned away", status)
	}

	c.mu.Lock()
	_, bound := c.sessionTable[sid]
	c.mu.Unlock()
	if bound {
		t.Fatal("the session was joined by a client that did not authenticate")
	}
	if cl.ss.channel(c) != nil {
		t.Fatal("a client that did not authenticate became a channel of the session")
	}
}

func TestBindSessionDropsTheHashOfAFailedExchange(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()
	c := h.joining(cl)
	c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

	sid := cl.ss.sessionID

	msg := signed(t, bindingRequest(1, sid, ntlmMessage(3, 64)), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	ssr := binding(t, msg)

	if _, status := c.prepareBinding(ssr); status != smb2.STATUS_OK {
		t.Fatalf("the server answered %#x, want it to take the binding", status)
	}

	if _, _, err := c.bindSession(cl.ss, ssr); err != nil {
		t.Fatalf("the server gave up on the binding: %v", err)
	}

	// The exchange failed, so its hash has to go with it. Kept, it would be picked up by the
	// next attempt, whose key derivation would then fold the failed exchange in and disagree
	// with the client, which starts its count afresh from the hash of the connection.
	c.mu.Lock()
	_, kept := c.preauthSessionTable[sid]
	c.mu.Unlock()
	if kept {
		t.Fatal("the hash of the failed exchange was kept for the next attempt")
	}
}

// ntHashOf is the hash of a password as the store keeps it, which is the one thing in an NTLM
// exchange that the client has to know beforehand.
func ntHashOf(password string) []byte {
	h := md4.New()
	h.Write(utils.EncodeStringToBytes(password))

	return h.Sum(nil)
}

// ntlmClient is the client half of an NTLMv2 exchange. The ntlm package keeps its own client to
// itself, so the computation is written out here — which suits the tests, because everything in
// it but the hash is something the client sees or chooses, and working it out separately is what
// makes the exchange a real one rather than the server agreeing with itself.
type ntlmClient struct {
	user      string
	workgroup string
	ntHash    []byte
}

// withPassword puts an account with a password behind a user, and returns the client that can
// authenticate as them. The harness adds its users without one, which is all a test that never
// authenticates needs; a binding has to go through the real exchange.
func (h *smbTest) withPassword(user, password string) ntlmClient {
	h.t.Helper()

	// The store hashes the password itself and keeps only the hash, so the password is what goes
	// in; the client works the same hash out for its side of the exchange.
	if err := h.srv.store.AddAccount(stores.Account{Username: user, Password: password, Workgroup: h.workgroup}); err != nil {
		h.t.Fatalf("could not add the account of %s: %v", user, err)
	}

	return ntlmClient{user: user, workgroup: h.workgroup, ntHash: ntHashOf(password)}
}

// negotiate is the opening message of the exchange. Every flag is asked for, and what comes back
// in the challenge is what the server was willing to agree to.
func (nc ntlmClient) negotiate() []byte {
	msg := ntlmMessage(1, 32)
	binary.LittleEndian.PutUint32(msg[12:16], ^uint32(0))

	return msg
}

// authenticate answers the challenge of the server, proving that the client holds the hash of the
// password without ever sending it.
func (nc ntlmClient) authenticate(t *testing.T, cmsg []byte) []byte {
	t.Helper()

	// The exchange runs under the flags the server settled on. The key exchange and the version
	// field are dropped: neither carries anything a binding needs, and both would only add fields
	// to fill in.
	flags := binary.LittleEndian.Uint32(cmsg[20:24]) &^ ntlm.NTLMSSP_NEGOTIATE_KEY_EXCH &^ ntlm.NTLMSSP_NEGOTIATE_VERSION

	serverChallenge := cmsg[24:32]
	tiLen := binary.LittleEndian.Uint16(cmsg[40:42])
	tiOff := binary.LittleEndian.Uint32(cmsg[44:48])
	targetInfo := cmsg[tiOff : tiOff+uint32(tiLen)]

	userBytes := utils.EncodeStringToBytes(nc.user)
	domainBytes := utils.EncodeStringToBytes(nc.workgroup)

	// NTOWFv2: the hash of the password, taken over the name it belongs to, so that a response
	// cannot be carried across to another user or another workgroup.
	owf := hmac.New(md5.New, nc.ntHash)
	owf.Write(utils.EncodeStringToBytes(strings.ToUpper(nc.user)))
	owf.Write(domainBytes)

	// The response is the proof, followed by the client's own half of the challenge that the
	// proof was taken over — the server needs that half to work the same thing out. The timestamp
	// and the client challenge are left at zero: nothing here reads them back.
	ntResp := make([]byte, 16+28+len(targetInfo))
	clientChallenge := ntResp[16:]
	clientChallenge[0] = 1
	clientChallenge[1] = 1
	copy(clientChallenge[28:], targetInfo)

	proof := hmac.New(md5.New, owf.Sum(nil))
	proof.Write(serverChallenge)
	proof.Write(clientChallenge)
	proof.Sum(ntResp[:0])

	// The fixed part of the message is followed by the three fields it points into. There is no
	// MIC and no session key to carry, so they lie end to end.
	const fixed = 64
	amsg := ntlmMessage(3, fixed+len(ntResp)+len(domainBytes)+len(userBytes))
	binary.LittleEndian.PutUint32(amsg[60:64], flags)

	off := fixed
	put := func(field int, b []byte) {
		binary.LittleEndian.PutUint16(amsg[field:field+2], uint16(len(b)))
		binary.LittleEndian.PutUint16(amsg[field+2:field+4], uint16(len(b)))
		binary.LittleEndian.PutUint32(amsg[field+4:field+8], uint32(off))
		copy(amsg[off:], b)
		off += len(b)
	}
	put(20, ntResp)      // NtChallengeResponseFields
	put(28, domainBytes) // DomainNameFields
	put(36, userBytes)   // UserNameFields

	return amsg
}

// bindOver runs a whole binding exchange over the connection: the client says who it is, the
// server challenges, and the client answers. It returns the two requests it sent, which are what
// the hash of the exchange is made of, and what the server made of the second one.
func (h *smbTest) bindOver(c *connection, ss *session, nc ntlmClient, dialect uint16) (legs [][]byte, resp smb2.GenericResponse, bound *session) {
	h.t.Helper()

	send := func(mid uint64, token []byte) ([]byte, smb2.GenericResponse, *session) {
		msg := signed(h.t, bindingRequest(mid, ss.sessionID, token), ss.signingKey, dialect, 0)
		ssr := binding(h.t, msg)

		got, status := c.prepareBinding(ssr)
		if status != smb2.STATUS_OK {
			h.t.Fatalf("the server answered %#x, want it to take the binding", status)
		}

		resp, bound, err := c.bindSession(got, ssr)
		if err != nil {
			h.t.Fatalf("the server gave up on the binding: %v", err)
		}

		return msg, resp, bound
	}

	first, challenge, _ := send(1, nc.negotiate())

	// The challenge to answer travels in the security buffer of the response.
	buf := challenge.Encode()
	off := binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize+4 : smb2.SMB2HeaderSize+6])
	length := binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize+6 : smb2.SMB2HeaderSize+8])

	second, resp, bound := send(2, nc.authenticate(h.t, buf[off:off+length]))

	return [][]byte{first, second}, resp, bound
}

// TestBindSessionJoinsTheSession is the binding that goes through. The client authenticates over
// the new connection as the user the session belongs to, and the connection becomes a channel of
// it with a signing key of its own — derived from this authentication, so no two channels of one
// session sign alike.
func TestBindSessionJoinsTheSession(t *testing.T) {
	for _, tt := range []struct {
		name    string
		dialect uint16
	}{
		{"3.1.1", smb2.SMB_DIALECT_311},
		{"3.0.2", smb2.SMB_DIALECT_302},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			nc := h.withPassword("carol", "hunter2")

			cl := h.dial("carol").signing().speaking(tt.dialect)
			c := h.joining(cl)
			c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

			sid := cl.ss.sessionID
			start := bytes.Clone(c.preauthIntegrityHashValue)

			legs, resp, bound := h.bindOver(c, cl.ss, nc, tt.dialect)

			if bound != cl.ss {
				t.Fatal("the binding did not end on the session it was about")
			}
			if status := resp.Header().Status(); status != smb2.STATUS_OK {
				t.Fatalf("the response carries %#x, want the binding to have gone through", status)
			}

			// The session now serves this connection as well.
			c.mu.Lock()
			joined, found := c.sessionTable[sid]
			c.mu.Unlock()
			if !found || joined != cl.ss {
				t.Fatal("the session does not run on the connection it was bound to")
			}

			ch := cl.ss.channel(c)
			if ch == nil {
				t.Fatal("the connection did not become a channel of the session")
			}

			// The key of the new channel comes from the authentication that has just taken place,
			// which in 3.1.1 is bound to the hash of the whole exchange.
			sessionKey := c.ntlmServer.Session().SessionKey()
			var want []byte
			if tt.dialect == smb2.SMB_DIALECT_311 {
				hash := start
				for _, leg := range legs {
					sum := sha512.Sum512(append(bytes.Clone(hash), leg...))
					hash = sum[:]
				}
				want = kdf.Kdf(sessionKey, []byte("SMBSigningKey\x00"), hash)
			} else {
				want = kdf.Kdf(sessionKey, []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00"))
			}

			if !bytes.Equal(ch.signingKey, want) {
				t.Fatal("the channel was not given the key derived from the authentication that established it")
			}
			if bytes.Equal(ch.signingKey, cl.ss.signingKey) {
				t.Fatal("the new channel signs with the key of the session rather than one of its own")
			}

			// The exchange is over, and what it kept is gone with it.
			c.mu.Lock()
			_, left := c.preauthSessionTable[sid]
			c.mu.Unlock()
			if left {
				t.Error("the finished exchange left its hash behind")
			}
		})
	}
}

// TestBindSessionRefusesAnotherUser is somebody who can authenticate, but not as the owner of the
// session. Proving who you are is no reason to be handed somebody else's session.
func TestBindSessionRefusesAnotherUser(t *testing.T) {
	h := newSMBTest(t)
	nc := h.withPassword("carol", "hunter2")

	// The session belongs to dave; carol is the one who turns up on the new connection.
	cl := h.dial("dave").signing()
	c := h.joining(cl)
	c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

	sid := cl.ss.sessionID
	_, resp, bound := h.bindOver(c, cl.ss, nc, smb2.SMB_DIALECT_311)

	if bound != nil {
		t.Fatal("the server handed back a session to a user it does not belong to")
	}
	if status := resp.Header().Status(); status != smb2.STATUS_NOT_SUPPORTED {
		t.Fatalf("the response carries %#x, want the client turned away", status)
	}

	c.mu.Lock()
	_, joined := c.sessionTable[sid]
	c.mu.Unlock()
	if joined {
		t.Fatal("the session was joined by a user it does not belong to")
	}
	if cl.ss.channel(c) != nil {
		t.Fatal("a user the session does not belong to became a channel of it")
	}
}

// TestBindSessionRefusesTheWrongPassword is the control on the exchange above. Without it, a
// binding that went through would only show that the server accepts what this test builds, however
// little of the password it turned out to depend on.
func TestBindSessionRefusesTheWrongPassword(t *testing.T) {
	h := newSMBTest(t)
	nc := h.withPassword("carol", "hunter2")
	nc.ntHash = ntHashOf("hunter3")

	cl := h.dial("carol").signing()
	c := h.joining(cl)
	c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

	sid := cl.ss.sessionID
	_, resp, bound := h.bindOver(c, cl.ss, nc, smb2.SMB_DIALECT_311)

	if bound != nil {
		t.Fatal("the server handed back a session to a client that did not know the password")
	}
	if status := resp.Header().Status(); status != smb2.STATUS_NO_SUCH_USER {
		t.Fatalf("the response carries %#x, want the client turned away", status)
	}

	c.mu.Lock()
	_, joined := c.sessionTable[sid]
	c.mu.Unlock()
	if joined {
		t.Fatal("the session was joined by a client that did not know the password")
	}
}

// TestABindingThatRacesTheSetupSeesAFinishedSession puts the two halves of a session setup against
// each other: one connection finishing the authentication while a second one asks to join. A
// session becomes valid with a single write, and everything the session is made of - who is behind
// it, its keys, whether it signs - is written before that. So a binding that reads the state
// through the lock the write is taken under either finds a session that is not ready yet and is
// turned away, or finds one that is ready in full. Reading it any other way is a data race, and one
// that hands a channel to a session with nobody behind it yet.
func TestABindingThatRacesTheSetupSeesAFinishedSession(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()

	// Built once and only read from here on, so that the goroutine sending it does nothing to the
	// test itself.
	msg := signed(t, bindingRequest(1, cl.ss.sessionID, nil), cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)
	req := binding(t, msg)

	for i := range 50 {
		c := h.joining(cl)

		// Back to where the session stands while its first connection is still authenticating.
		cl.ss.state = sessionInProgress

		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			ss     *session
			status uint32
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			cl.ss.activate()
		}()
		go func() {
			defer wg.Done()
			<-start
			ss, status = c.prepareBinding(req)
		}()
		close(start)
		wg.Wait()

		switch status {
		case smb2.STATUS_OK:
			if ss.userName == "" {
				t.Fatalf("round %d: the binding was accepted against a session with nobody behind it", i)
			}
		case smb2.STATUS_REQUEST_NOT_ACCEPTED: // The setup had not finished yet.
		default:
			t.Fatalf("round %d: the server answered %#x, want the binding taken or turned away", i, status)
		}
	}
}
