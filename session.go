package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"hash"
	"log"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/internal/ccm"
	"github.com/mike76-dev/sombrero/internal/cmac"
	"github.com/mike76-dev/sombrero/internal/gmac"
	"github.com/mike76-dev/sombrero/kdf"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
	"lukechampine.com/frand"
)

const (
	sessionInProgress int = iota
	sessionValid

	// sessionExpired is reachable only under an authentication mechanism that hands back an
	// expiry time. Session.ExpirationTime is whatever GSS returns when the session is set up,
	// and infinity when it returns nothing ([MS-SMB2] 3.3.5.5.3); NTLM returns nothing, and NTLM is
	// the only mechanism this server offers. So no session here ever expires, and the code that
	// takes an expired one back through authentication is dormant rather than dead: it is what
	// a Kerberos-authenticated session, whose ticket does have a lifetime, would need.
	sessionExpired
)

var (
	errSessionNotFound = errors.New("session not found")
	errNoSigningKey    = errors.New("no signing key available")
	errUnsignedRequest = errors.New("request not signed on a session that requires signing")
	errNoCipher        = errors.New("no cipher negotiated")
)

// isSetupCommand reports whether the command is one of the two that bring a session up, which are
// the commands a signing requirement cannot yet be held against.
func isSetupCommand(command uint16) bool {
	return command == smb2.SMB2_NEGOTIATE || command == smb2.SMB2_SESSION_SETUP
}

// session represents a Session object.
type session struct {
	sessionID                 uint64
	state                     int
	securityContext           ntlm.SecurityContext
	isAnonymous               bool
	isGuest                   bool
	sessionKey                []byte
	signingRequired           bool
	openTable                 map[uint64]*open
	treeConnectTable          map[uint32]*treeConnect
	connection                *connection
	idleTime                  time.Time
	userName                  string
	workgroup                 string
	encryptData               bool
	signingKey                []byte
	encryptionKey             []byte
	decryptionKey             []byte
	preauthIntegrityHashValue []byte
	channelList               map[string]*channel

	mu sync.Mutex
}

// newSessionState returns a Session object as it stands at the start of a session setup, with
// its tables in place and nobody authenticated yet. It is the half of registerSession the tests
// share; the same reasoning applies as for newServerState.
func newSessionState(sid uint64, c *connection) *session {
	return &session{
		sessionID:        sid,
		connection:       c,
		state:            sessionInProgress,
		idleTime:         time.Now(),
		openTable:        make(map[uint64]*open),
		treeConnectTable: make(map[uint32]*treeConnect),
		channelList:      make(map[string]*channel),
	}
}

// registerSession creates a new Session object and registers it with the SMB server.
func (s *server) registerSession(connection *connection, req smb2.SessionSetupRequest) (*session, bool, error) {
	var ss *session
	var found bool
	if req.Header().SessionID() == 0 { // A new session
		sid := make([]byte, 8)
		frand.Read(sid)
		ss = newSessionState(binary.LittleEndian.Uint64(sid), connection)
		connection.mu.Lock()
		connection.sessionTable[ss.sessionID] = ss
		connection.mu.Unlock()
		s.mu.Lock()
		s.globalSessionTable[ss.sessionID] = ss
		s.stats.SOpens++
		s.mu.Unlock()

		if connection.negotiateDialect == smb2.SMB_DIALECT_311 {
			ss.preauthIntegrityHashValue = bytes.Clone(ss.connection.preauthIntegrityHashValue)
			switch ss.connection.preauthIntegrityHashID {
			case smb2.SHA_512:
				h := sha512.New()
				h.Write(ss.preauthIntegrityHashValue)
				h.Write(req.Header())
				ss.preauthIntegrityHashValue = h.Sum(ss.preauthIntegrityHashValue[:0])
			}
		}
	} else { // There is already a session with this ID, reactivate it
		connection.mu.Lock()
		ss, found = connection.sessionTable[req.Header().SessionID()]
		connection.mu.Unlock()
		if !found {
			return nil, false, errSessionNotFound
		}
		if ss.state == sessionExpired {
			ss.state = sessionInProgress
			ss.securityContext = ntlm.SecurityContext{}
		}
	}
	return ss, found, nil
}

// deregisterSession destroys the Session object and closes all associated tree connections.
func (s *server) deregisterSession(conn *connection, sid uint64) (*session, error) {
	// The session is taken off the connection here rather than only at the end, under the lock
	// that guards that table. Read without it, the table races the very same read from a second
	// teardown of the connection - the reading loop and the periodic sweep both come through
	// here - and both of them would go on to tear the one session down twice over.
	conn.mu.Lock()
	ss, found := conn.sessionTable[sid]
	if found {
		delete(conn.sessionTable, sid)
	}
	conn.mu.Unlock()

	if !found {
		return nil, errSessionNotFound
	}

	ss.mu.Lock()
	for _, op := range ss.openTable {
		s.mu.Lock()
		delete(ss.connection.server.globalOpenTable, op.durableFileID)
		s.mu.Unlock()
		op.cancel()
	}
	ss.mu.Unlock()

	// The tree connects are named first and closed after: closing one takes the lock of the
	// session and deletes the entry, so a walk of the table itself would be reading it while a
	// tree disconnect arriving over another channel writes to it.
	ss.mu.Lock()
	tids := make([]uint32, 0, len(ss.treeConnectTable))
	for tid := range ss.treeConnectTable {
		tids = append(tids, tid)
	}
	ss.mu.Unlock()

	for _, tid := range tids {
		ss.closeTreeConnect(tid)
	}

	// The session may be served by more than one connection, so it has to go from all of
	// them, not only from the one that the request arrived on. The channels are collected
	// first and cleared out, so that none of the connection locks is taken while the lock
	// of the session is held.
	ss.mu.Lock()
	connections := make([]*connection, 0, len(ss.channelList)+1)
	for _, ch := range ss.channelList {
		connections = append(connections, ch.connection)
	}
	ss.channelList = make(map[string]*channel)
	ss.mu.Unlock()

	// The dialects without channels leave the list empty, and the connection the request
	// came in on is the only one to clear. Clearing it twice does no harm.
	connections = append(connections, conn)
	for _, c := range connections {
		c.mu.Lock()
		delete(c.sessionTable, sid)
		c.mu.Unlock()
	}

	s.mu.Lock()
	delete(s.globalSessionTable, sid)
	s.stats.SOpens--
	s.mu.Unlock()

	return ss, nil
}

// finalize finalizes the session after successfully authenticating the user.
func (ss *session) finalize(req smb2.SessionSetupRequest) {
	ss.securityContext = ss.connection.ntlmServer.Session().GetSecurityContext()
	ss.userName = ss.connection.ntlmServer.Session().User()
	ss.workgroup = ss.connection.ntlmServer.Session().Domain()
	if ss.userName == "" {
		ss.isAnonymous = true
	}
	if ss.userName == "guest" {
		ss.isGuest = true
	}
	ss.signingRequired = (req.SecurityMode()&smb2.NEGOTIATE_SIGNING_REQUIRED > 0) && !ss.isAnonymous && !ss.isGuest && ss.connection.shouldSign

	if ss.connection.negotiateDialect == smb2.SMB_DIALECT_311 {
		switch ss.connection.preauthIntegrityHashID {
		case smb2.SHA_512:
			h := sha512.New()
			h.Write(ss.preauthIntegrityHashValue)
			h.Write(req.Header())
			ss.preauthIntegrityHashValue = h.Sum(ss.preauthIntegrityHashValue[:0])
		}
	}

	ss.sessionKey = ss.connection.ntlmServer.Session().SessionKey()
	ss.encryptData = ss.connection.server.encryptData

	if ss.connection.server.debug {
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, ss.sessionID)
		log.Printf("Session ID: %x\n", buf)
		log.Printf("Session key: %x\n", ss.sessionKey)
	}

	ss.deriveKeys()

	// The connection the session is established on becomes its first channel, and the
	// signing key of that channel is the signing key of the session.
	if smb2.Is3X(ss.connection.negotiateDialect) {
		ss.bindChannel(ss.connection, ss.signingKey)
	}

	ss.connection.mu.Lock()
	defer ss.connection.mu.Unlock()

	if req.PreviousSessionID() != 0 { // This session replaces another one; delete the previous one
		pss, found := ss.connection.sessionTable[req.PreviousSessionID()]
		if found && ss.securityContext.UserRID == pss.securityContext.UserRID && ss.sessionID != req.PreviousSessionID() {
			delete(ss.connection.sessionTable, req.PreviousSessionID())
			ss.connection.server.mu.Lock()
			delete(ss.connection.server.globalSessionTable, req.PreviousSessionID())
			ss.connection.server.mu.Unlock()
		}
	}

	ss.state = sessionValid
}

// deriveKeys works the signing, application and encryption keys out of the session key, by the
// method the dialect calls for. The dialects before 3.0 derive nothing: they sign with the
// session key itself and cannot encrypt at all.
func (ss *session) deriveKeys() {
	switch ss.connection.negotiateDialect {
	case smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21:
	case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
		ss.signingKey = kdf.Kdf(ss.sessionKey, []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00"))
		ss.encryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMB2AESCCM\x00"), []byte("ServerOut\x00"))
		ss.decryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMB2AESCCM\x00"), []byte("ServerIn \x00"))
	case smb2.SMB_DIALECT_311:
		ss.signingKey = kdf.Kdf(ss.sessionKey, []byte("SMBSigningKey\x00"), ss.preauthIntegrityHashValue)
		ss.encryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMBS2CCipherKey\x00"), ss.preauthIntegrityHashValue)
		ss.decryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMBC2SCipherKey\x00"), ss.preauthIntegrityHashValue)
	}
}

// signingKeyFor returns the key to sign the response with. In the SMB 3.x dialect family, an
// SMB2_SESSION_SETUP response that doesn't carry STATUS_SUCCESS is signed with the signing
// key of the session: it is sent while the channel is still being established, so the
// channel key isn't the one the client expects yet. Every other response is signed with the
// signing key of the channel that the sending connection belongs to.
func (ss *session) signingKeyFor(c *connection, header smb2.Header) []byte {
	if !smb2.Is3X(c.negotiateDialect) {
		return ss.signingKey
	}

	if header.Command() == smb2.SMB2_SESSION_SETUP && header.Status() != smb2.STATUS_OK {
		return ss.signingKey
	}

	ch := ss.channel(c)
	if ch == nil || len(ch.signingKey) == 0 {
		// Either the connection isn't bound to the session, or the channel it is bound to
		// hasn't been given a key yet. Neither should happen for a valid session. Fall back
		// to the signing key of the session, so that a well-formed response goes out rather
		// than one that claims to be signed but isn't.
		if c.server.debug {
			log.Printf("Connection %s has no signing key for session %d", c.clientName, ss.sessionID)
		}
		return ss.signingKey
	}

	return ch.signingKey
}

// sign signs each response in the message with the key that the connection it is sent on
// requires.
func (ss *session) sign(buf []byte, c *connection) {
	var off uint32
	var zero [16]byte
	for {
		next := binary.LittleEndian.Uint32(buf[off+20 : off+24])
		flags := binary.LittleEndian.Uint32(buf[off+16 : off+20])
		binary.LittleEndian.PutUint32(buf[off+16:off+20], flags|smb2.FLAGS_SIGNED)
		copy(buf[off+48:off+64], zero[:])
		key := ss.signingKeyFor(c, smb2.Header(buf[off:]))
		var signer hash.Hash
		switch c.negotiateDialect {
		case smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21:
			signer = hmac.New(sha256.New, ss.sessionKey)
		case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
			ciph, err := aes.NewCipher(key)
			if err != nil {
				log.Printf("Error creating cipher for signing: %v", err)
				return
			}
			signer = cmac.New(ciph)
		case smb2.SMB_DIALECT_311:
			switch c.signingAlgorithmID {
			case smb2.AES_CMAC:
				ciph, err := aes.NewCipher(key)
				if err != nil {
					log.Printf("Error creating cipher for signing: %v", err)
					return
				}
				signer = cmac.New(ciph)
			case smb2.AES_GMAC:
				nonce := make([]byte, 12)
				binary.LittleEndian.PutUint64(nonce[:8], smb2.Header(buf[off:]).MessageID())
				nonce[8] |= 1 // Server-side
				if smb2.Header(buf[off:]).Command() == smb2.SMB2_CANCEL {
					nonce[8] |= 2
				}
				var err error
				signer, err = gmac.New(key, nonce)
				if err != nil {
					log.Printf("Error creating cipher for signing: %v", err)
					return
				}
			default:
				ciph, err := aes.NewCipher(key)
				if err != nil {
					log.Printf("Error creating cipher for signing: %v", err)
					return
				}
				signer = cmac.New(ciph)
			}
		}
		signer.Reset()
		if next == 0 { // Last response in the chain
			signer.Write(buf[off:])
		} else {
			signer.Write(buf[off : off+next])
		}
		copy(buf[off+48:off+64], signer.Sum(nil))
		off += next
		if next == 0 {
			break
		}
	}
}

// isSessionBinding returns true if the request binds a session to a new channel.
func isSessionBinding(req *smb2.Request) bool {
	if req.Header().Command() != smb2.SMB2_SESSION_SETUP {
		return false
	}

	ssr := smb2.SessionSetupRequest{Request: *req}
	return ssr.Flags()&smb2.SESSION_FLAG_BINDING > 0
}

// verificationKeyFor returns the key to verify the signature of the request received on the
// given connection with, or nil if that key is not available. In the SMB 3.x dialect family,
// a request that binds a session to a new channel is signed with the signing key of the
// session, because the channel it establishes has no key of its own yet; every other request
// is signed with the key of the channel it arrives on. Older dialects have no channels and
// sign with the session key.
func (ss *session) verificationKeyFor(c *connection, req *smb2.Request) []byte {
	if !smb2.Is3X(c.negotiateDialect) {
		return ss.sessionKey
	}

	if isSessionBinding(req) {
		return ss.signingKey
	}

	ch := ss.channel(c)
	if ch == nil {
		return nil
	}

	return ch.signingKey
}

// validateRequest verifies the signature of the request received on the connection. It returns
// nil if the request is signed correctly, errNoSigningKey if the key needed to verify it is
// not available, errUnsignedRequest if the session demands a signature and the request carries
// none, and errInvalidSignature if the signature doesn't match.
func (ss *session) validateRequest(req *smb2.Request, c *connection) error {
	if req.IsEncrypted() {
		// Encryption stands in for signing: the transform header is authenticated, so what
		// arrived under it is as much beyond tampering as a signature would make it.
		return nil
	}

	if !req.Header().IsFlagSet(smb2.FLAGS_SIGNED) {
		// A session that requires signing gets nothing served unsigned ([MS-SMB2] 3.3.5.2.9).
		// Verifying a signature that is offered is not the same as demanding one: without this
		// an attacker who can put packets on the wire needs no key to be obeyed, since dropping
		// the flag was enough to skip the check altogether.
		//
		// The two commands of the setup itself are the exception. A NEGOTIATE precedes every
		// session, and a SESSION_SETUP is how a session that has expired is authenticated again;
		// neither can be held to the requirement that authenticating establishes. A binding
		// request is made to carry a signature by the binding path itself.
		if ss.signingRequired && !isSetupCommand(req.Header().Command()) {
			return errUnsignedRequest
		}

		return nil
	}

	key := ss.verificationKeyFor(c, req)
	if len(key) == 0 {
		return errNoSigningKey
	}

	signature := req.Header().Signature()
	req.Header().WipeSignature()
	var verifier hash.Hash
	switch c.negotiateDialect {
	case smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21:
		verifier = hmac.New(sha256.New, key)
	case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
		ciph, err := aes.NewCipher(key)
		if err != nil {
			log.Printf("Error creating cipher for verifying signature: %v", err)
			return errInvalidSignature
		}
		verifier = cmac.New(ciph)
	case smb2.SMB_DIALECT_311:
		switch c.signingAlgorithmID {
		case smb2.AES_CMAC:
			ciph, err := aes.NewCipher(key)
			if err != nil {
				log.Printf("Error creating cipher for verifying signature: %v", err)
				return errInvalidSignature
			}
			verifier = cmac.New(ciph)
		case smb2.AES_GMAC:
			nonce := make([]byte, 12)
			binary.LittleEndian.PutUint64(nonce[:8], req.Header().MessageID())
			if req.Header().Command() == smb2.SMB2_CANCEL {
				nonce[8] |= 2
			}
			var err error
			verifier, err = gmac.New(key, nonce)
			if err != nil {
				log.Printf("Error creating cipher for verifying signature: %v", err)
				return errInvalidSignature
			}
		default:
			ciph, err := aes.NewCipher(key)
			if err != nil {
				log.Printf("Error creating cipher for verifying signature: %v", err)
				return errInvalidSignature
			}
			verifier = cmac.New(ciph)
		}
	}
	verifier.Reset()
	verifier.Write(req.Header())
	sum := verifier.Sum(nil)
	if !bytes.Equal(signature, sum[:16]) {
		return errInvalidSignature
	}

	return nil
}

// aead returns the cipher that protects the traffic of the session over the given
// connection, keyed with the key provided. The keys belong to the session and are shared by
// all of its channels, but the algorithm is negotiated by each connection separately, so a
// message has to be protected with the cipher of the channel it travels over rather than
// with the one the session was established under.
func (ss *session) aead(key []byte, c *connection) (cipher.AEAD, error) {
	ciph, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// The cipher of the connection, whichever dialect settled it: 3.1.1 agrees one in a negotiate
	// context, and the dialects before it have only ever had AES-128-CCM, which the negotiate
	// writes down here all the same so that one field answers for both.
	switch c.cipherID {
	case smb2.AES_128_CCM:
		return ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
	case smb2.AES_128_GCM:
		return cipher.NewGCMWithNonceSize(ciph, 12)
	}

	return nil, errNoCipher
}

// encrypt uses the encryption key to encrypt the SMB message.
func (ss *session) encrypt(buf []byte, c *connection) []byte {
	encrypter, err := ss.aead(ss.encryptionKey, c)
	if err != nil {
		log.Printf("Error creating encrypter: %v", err)
		return nil
	}
	// Whatever is encrypted has to name the session it is encrypted under. [MS-SMB2] 3.2.5.1.1: a
	// client that decrypts a message whose SMB2 header names a different session than the transform
	// header disconnects the connection, and a session of zero is a different session.
	//
	// A lease break notification is the message this is for. [MS-SMB2] 3.3.4.7 has the server set
	// its SessionId to zero - a lease belongs to a client rather than to a session - and that is
	// what goes out over a connection that does not encrypt. Encrypted, the two rules cannot both
	// be kept, and the client's is the one that decides what happens on the wire: it drops the
	// connection the moment it reads a lease break, and a client that could never be told to give
	// up a lease is worse off than one told in a header it can make sense of.
	stampSessionID(buf, ss.sessionID)

	nonce := make([]byte, encrypter.NonceSize())
	frand.Read(nonce)
	output := smb2.Header(make([]byte, smb2.SMB2TransformHeaderSize+len(buf)+16))
	output.SetProtocolID(smb2.PROTOCOL_SMB2_ENCRYPTED)
	output.SetNonce(nonce)
	output.SetOriginalMessageSize(uint32(len(buf)))
	output.SetEncryptionFlags(1)
	output.SetTransformSessionID(ss.sessionID)
	encrypter.Seal(output[:smb2.SMB2TransformHeaderSize], nonce, buf, output.AssociatedData())
	output.SetEncryptionSignature(output[len(output)-16:])
	output = output[:len(output)-16]
	return output
}

// stampSessionID names the session in every SMB2 header of the message that names none. A message
// built for a session says so already; one built for a client, as a lease break is, says nothing,
// and that is what a peer will not read once it is encrypted.
func stampSessionID(msg []byte, sessionID uint64) {
	var off uint32
	for {
		if uint64(off)+smb2.SMB2HeaderSize > uint64(len(msg)) {
			return
		}

		h := smb2.Header(msg[off:])
		if h.SessionID() == 0 {
			h.SetSessionID(sessionID)
		}

		next := h.NextCommand()
		if next == 0 {
			return
		}
		off += next
	}
}

// decrypt uses the decryption key to decrypt the SMB message.
func (ss *session) decrypt(buf []byte, c *connection) []byte {
	input := append(buf[smb2.SMB2TransformHeaderSize:], smb2.Header(buf).EncryptionSignature()...)
	decrypter, err := ss.aead(ss.decryptionKey, c)
	if err != nil {
		log.Printf("Error creating decrypter: %v", err)
		return nil
	}
	msg, err := decrypter.Open(input[:0], smb2.Header(buf).Nonce()[:decrypter.NonceSize()], input, smb2.Header(buf).AssociatedData())
	if err != nil {
		log.Printf("Decryption error at session %d: %v", ss.sessionID, err)
		return nil
	}
	return msg
}
