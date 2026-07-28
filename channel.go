package main

import (
	"bytes"
	"crypto/sha512"
	"log"
	"strings"

	"github.com/mike76-dev/sombrero/kdf"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/spnego"
)

// channel represents a cross-session connection channel in SMB2.
// It is used to support multi-channel connections, which allow
// multiple network connections to be used for a single SMB2 session.
type channel struct {
	signingKey []byte
	connection *connection
}

// bindChannel inserts the connection into the channel list of the session, or updates
// the signing key of the channel if the connection is already bound to the session.
// The key of a channel is derived from the authentication that established it: for the
// connection the session was created on, it is the signing key of the session itself.
func (ss *session) bindChannel(c *connection, signingKey []byte) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	ch, found := ss.channelList[c.clientName]
	if !found || ch.connection != c {
		ch = &channel{connection: c}
		ss.channelList[c.clientName] = ch
	}

	ch.signingKey = signingKey
}

// addChannel inserts a channel for the connection into the channel list of the session,
// unless the connection already has one there. The new channel starts out without a signing
// key: the key is derived from the authentication that established the channel and is set
// once that authentication has been processed.
func (ss *session) addChannel(c *connection) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if ch, found := ss.channelList[c.clientName]; found && ch.connection == c {
		return
	}

	ss.channelList[c.clientName] = &channel{connection: c}
}

// unbindChannel removes the connection from the channel list of the session.
func (ss *session) unbindChannel(c *connection) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if ch, found := ss.channelList[c.clientName]; found && ch.connection == c {
		delete(ss.channelList, c.clientName)
	}
}

// channel looks up the channel of the session that the given connection belongs to.
// It returns nil if the connection isn't bound to the session.
func (ss *session) channel(c *connection) *channel {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	// The channel list is keyed by the name of the connection, but a name may be reused
	// by a later connection, so the connection itself decides whether the channel matches.
	ch, found := ss.channelList[c.clientName]
	if !found || ch.connection != c {
		return nil
	}

	return ch
}

// selectConnection returns the connection to send a message about the open over. In the SMB
// 3.x dialect family the session may be served by several channels and the message may go
// over any of them, so the one the client is waiting on is preferred, and another live one
// is taken if that channel has gone away. The dialects without channels have nothing to
// choose from: the message goes over the connection the open was established on.
func (op *open) selectConnection(preferred *connection) *connection {
	// The connection of the open is read before the session is locked, and the copy is used
	// from there on: the open may be handed to another connection while this runs, and the
	// two locks must never be held at the same time.
	op.mu.Lock()
	own := op.connection
	op.mu.Unlock()

	if !smb2.Is3X(own.negotiateDialect) {
		return own
	}

	ss := op.session
	ss.mu.Lock()
	defer ss.mu.Unlock()

	// The channel list is keyed by the name of the connection, but a name may be reused by
	// a later connection, so the connection itself decides whether the channel matches.
	isChannel := func(c *connection) bool {
		ch, found := ss.channelList[c.clientName]
		return found && ch.connection == c
	}

	if preferred != nil && isChannel(preferred) {
		return preferred
	}

	// The connection the open belongs to is the next best thing: it keeps the choice stable
	// for as long as that channel lives.
	if isChannel(own) {
		return own
	}

	for _, ch := range ss.channelList {
		return ch.connection
	}

	// The session has no channels left. There is nothing to send over, but the caller is
	// given the connection of the open rather than nothing at all, so that the send fails
	// on a closed connection instead of a nil pointer.
	return own
}

// preauthSession represents a PreauthSession object. While a session is being bound to a
// new connection, the preauthentication integrity hash of the exchange cannot be kept in
// the session itself, because several connections may be binding to it at the same time
// and each of them arrives at a different hash. The connection keeps one of these per
// session being bound instead, and the signing key of the new channel is derived from it.
type preauthSession struct {
	sessionID                 uint64
	preauthIntegrityHashValue []byte
}

// prepareBinding runs the checks that a request binding an existing session to this
// connection as a new channel has to pass. It returns the session being bound, or the
// status to fail the request with. The caller must already have established that the
// connection is able to carry a channel at all.
func (c *connection) prepareBinding(ssr smb2.SessionSetupRequest) (*session, uint32) {
	// The session lives on another connection, so it is looked up in the global table
	// rather than in the session table of this one.
	sid := ssr.Header().SessionID()
	c.server.mu.Lock()
	ss, found := c.server.globalSessionTable[sid]
	c.server.mu.Unlock()
	if !found {
		return nil, smb2.STATUS_USER_SESSION_DELETED
	}

	// A channel has to be negotiated exactly like the session it joins, otherwise the two
	// connections would disagree about how to sign and encrypt the traffic of the session.
	if c.negotiateDialect != ss.connection.negotiateDialect {
		return nil, smb2.STATUS_INVALID_PARAMETER
	}

	// The request has to be signed: its signature is the only proof that the client asking
	// for the channel is the one that owns the session.
	if !ssr.Header().IsFlagSet(smb2.FLAGS_SIGNED) {
		return nil, smb2.STATUS_INVALID_PARAMETER
	}

	// Channels of one session all have to belong to the same client.
	if !bytes.Equal(c.clientGuid, ss.connection.clientGuid) {
		return nil, smb2.STATUS_USER_SESSION_DELETED
	}

	switch ss.state {
	case sessionInProgress:
		// The session is still being authenticated on its first connection, so there is
		// nothing to bind to yet.
		return nil, smb2.STATUS_REQUEST_NOT_ACCEPTED
	case sessionExpired:
		return nil, smb2.STATUS_NETWORK_SESSION_EXPIRED
	}

	// An anonymous or a guest session is not authenticated well enough to be extended
	// onto another connection.
	if ss.isAnonymous || ss.isGuest {
		return nil, smb2.STATUS_NOT_SUPPORTED
	}

	// The session already runs on this connection; a second channel needs a second
	// connection.
	c.mu.Lock()
	_, bound := c.sessionTable[sid]
	c.mu.Unlock()
	if bound {
		return nil, smb2.STATUS_REQUEST_NOT_ACCEPTED
	}

	// The binding exchange has a preauthentication integrity hash of its own, which starts
	// off from the hash of this connection at the end of the negotiation. It is created on
	// the first request of the exchange and reused by the ones that follow.
	if c.negotiateDialect == smb2.SMB_DIALECT_311 {
		c.mu.Lock()
		if _, found := c.preauthSessionTable[sid]; !found {
			c.preauthSessionTable[sid] = &preauthSession{
				sessionID:                 sid,
				preauthIntegrityHashValue: bytes.Clone(c.preauthIntegrityHashValue),
			}
		}
		c.mu.Unlock()
	}

	return ss, smb2.STATUS_OK
}

// bindingToken unwraps the authentication token of a session setup request and reports
// whether it carries the second leg of the exchange. A binding exchange cannot be told
// apart by the session table the way an ordinary session setup is, because the session
// only joins the table of this connection once the binding has completed, so the leg is
// decided by the type of the NTLM message the client sent.
func bindingToken(buf []byte) (token []byte, authenticate bool) {
	if resp, err := spnego.DecodeNegTokenResp(buf); err == nil {
		return resp.ResponseToken, ntlm.IsAuthenticate(resp.ResponseToken)
	}

	if init, err := spnego.DecodeNegTokenInit(buf); err == nil {
		return init.MechToken, ntlm.IsAuthenticate(init.MechToken)
	}

	// The token may not be wrapped in SPNEGO at all; fall back to the raw bytes.
	return buf, ntlm.IsAuthenticate(buf)
}

// bindSession runs the exchange that adds this connection to an existing session as another
// channel. It mirrors an ordinary session setup: the client authenticates once more, over
// the new connection, as the user that the session belongs to.
func (c *connection) bindSession(ss *session, ssr smb2.SessionSetupRequest) (smb2.GenericResponse, *session, error) {
	sid := ss.sessionID
	token, authenticate := bindingToken(ssr.SecurityBuffer())

	// The binding exchange carries a preauthentication integrity hash of its own, and the
	// request is a part of it whichever leg it belongs to.
	c.updatePreauthHash(sid, ssr.Header())

	if !authenticate { // The first leg; answer with a challenge
		challenge, err := c.ntlmServer.Challenge(token)
		if err != nil {
			log.Println("Couldn't generate CHALLENGE for a binding session:", err)
			return nil, nil, err
		}

		if !bytes.Equal(token, ssr.SecurityBuffer()) { // The request was wrapped in SPNEGO
			challenge, err = spnego.EncodeNegTokenResp(0x01, spnego.NlmpOid, challenge, nil)
			if err != nil {
				log.Println("Couldn't generate CHALLENGE token for a binding session:", err)
				return nil, nil, err
			}
		}

		// The response is signed with the signing key of the session, which is the only key
		// the two connections share until the binding completes. It joins the hash of the
		// exchange once it has been signed, which happens on the way out.
		resp := &smb2.SessionSetupResponse{}
		resp.FromRequest(ssr)
		resp.Generate(sid, 0, challenge, false)
		resp.Header().SetCreditResponse(1) // Only one credit while the process is incomplete

		return resp, ss, nil
	}

	// The second leg; the client has to prove that it is the owner of the session.
	if err := c.ntlmServer.Authenticate(token); err != nil {
		c.server.mu.Lock()
		c.server.stats.PwErrors++
		c.server.mu.Unlock()
		resp := smb2.NewErrorResponse(ssr, smb2.STATUS_NO_SUCH_USER, 0, nil)
		return resp, nil, nil
	}

	// A channel may only be added to the session of the very same user: a successful
	// authentication as somebody else is no reason to hand out the session.
	if !strings.EqualFold(c.ntlmServer.Session().User(), ss.userName) {
		resp := smb2.NewErrorResponse(ssr, smb2.STATUS_NOT_SUPPORTED, 0, nil)
		return resp, nil, nil
	}

	// The binding has succeeded: the session starts serving this connection as well, and
	// gains a channel for it. The channel has no signing key yet.
	c.mu.Lock()
	c.sessionTable[sid] = ss
	c.mu.Unlock()
	ss.addChannel(c)

	// The signing key of the new channel is derived from the authentication that has just
	// taken place on this connection, so it differs from the key of every other channel of
	// the session. In the 3.1.1 dialect the context of the derivation is the hash of the
	// binding exchange, which by now covers everything up to and including this request.
	// The hash has served its purpose and is dropped along with the rest of the exchange.
	preauthHash := c.takePreauthHash(sid)
	var signingKey []byte
	switch c.negotiateDialect {
	case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
		signingKey = kdf.Kdf(c.ntlmServer.Session().SessionKey(), []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00"))
	case smb2.SMB_DIALECT_311:
		signingKey = kdf.Kdf(c.ntlmServer.Session().SessionKey(), []byte("SMBSigningKey\x00"), preauthHash)
	}
	ss.bindChannel(c, signingKey)

	// The final response of the exchange is signed with the key just derived, which is what
	// makes it verifiable to the client: the flags are left clear, because binding changes
	// nothing about how the session as a whole is protected.
	resp := &smb2.SessionSetupResponse{}
	resp.FromRequest(ssr)
	resp.Generate(sid, 0, spnego.FinalNegTokenResp, true)

	return resp, ss, nil
}

// updatePreauthHash rolls the message into the preauthentication integrity hash of the
// binding exchange that the connection runs for the given session. The hash of a binding
// exchange is kept per connection, so that several connections may bind to one session at
// the same time without disturbing each other's key derivation.
func (c *connection) updatePreauthHash(sid uint64, msg []byte) {
	if c.negotiateDialect != smb2.SMB_DIALECT_311 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	pss, found := c.preauthSessionTable[sid]
	if !found {
		return
	}

	switch c.preauthIntegrityHashID {
	case smb2.SHA_512:
		h := sha512.New()
		h.Write(pss.preauthIntegrityHashValue)
		h.Write(msg)
		pss.preauthIntegrityHashValue = h.Sum(pss.preauthIntegrityHashValue[:0])
	}
}

// takePreauthHash returns the preauthentication integrity hash of the binding exchange that
// the connection ran for the given session, and forgets it: the exchange is over, and the
// hash has served its purpose of deriving the signing key of the new channel.
func (c *connection) takePreauthHash(sid uint64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	pss, found := c.preauthSessionTable[sid]
	delete(c.preauthSessionTable, sid)
	if !found {
		return nil
	}

	return pss.preauthIntegrityHashValue
}
