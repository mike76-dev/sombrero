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
	sessionExpired
)

var (
	errSessionNotFound = errors.New("session not found")
)

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
	expirationTime            time.Time
	connection                *connection
	creationTime              time.Time
	idleTime                  time.Time
	userName                  string
	workgroup                 string
	encryptData               bool
	signingKey                []byte
	encryptionKey             []byte
	decryptionKey             []byte
	applicationKey            []byte
	preauthIntegrityHashValue []byte
	channelList               map[string]*channel

	mu sync.Mutex
}

// registerSession creates a new Session object and registers it with the SMB server.
func (s *server) registerSession(connection *connection, req smb2.SessionSetupRequest) (*session, bool, error) {
	var ss *session
	var found bool
	if req.Header().SessionID() == 0 { // A new session
		sid := make([]byte, 8)
		frand.Read(sid)
		ss = &session{
			sessionID:        binary.LittleEndian.Uint64(sid),
			connection:       connection,
			state:            sessionInProgress,
			creationTime:     time.Now(),
			idleTime:         time.Now(),
			openTable:        make(map[uint64]*open),
			treeConnectTable: make(map[uint32]*treeConnect),
			channelList:      make(map[string]*channel),
		}
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
func (s *server) deregisterSession(connection *connection, sid uint64) (*session, error) {
	ss, found := connection.sessionTable[sid]
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

	for tid := range ss.treeConnectTable {
		ss.closeTreeConnect(tid)
	}

	connection.mu.Lock()
	delete(connection.sessionTable, sid)
	connection.mu.Unlock()

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

	switch ss.connection.negotiateDialect {
	case smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21:
	case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
		ss.signingKey = kdf.Kdf(ss.sessionKey, []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00"))
		ss.applicationKey = kdf.Kdf(ss.sessionKey, []byte("SMB2APP\x00"), []byte("SmbRpc\x00"))
		ss.encryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMB2AESCCM\x00"), []byte("ServerOut\x00"))
		ss.decryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMB2AESCCM\x00"), []byte("ServerIn \x00"))
	case smb2.SMB_DIALECT_311:
		ss.signingKey = kdf.Kdf(ss.sessionKey, []byte("SMBSigningKey\x00"), ss.preauthIntegrityHashValue)
		ss.applicationKey = kdf.Kdf(ss.sessionKey, []byte("SMBAppKey\x00"), ss.preauthIntegrityHashValue)
		ss.encryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMBS2CCipherKey\x00"), ss.preauthIntegrityHashValue)
		ss.decryptionKey = kdf.Kdf(ss.sessionKey, []byte("SMBC2SCipherKey\x00"), ss.preauthIntegrityHashValue)
	}

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
	ss.expirationTime = time.Now().Add(100 * 365 * 24 * time.Hour) // Impossibly long
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
	if ch == nil {
		// The connection isn't bound to the session, which shouldn't happen for a valid
		// session. Fall back to the session key, so that the client at least has a chance
		// of accepting the response.
		if c.server.debug {
			log.Printf("Connection %s is not a channel of session %d", c.clientName, ss.sessionID)
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

// validateRequest returns true if the request is correctly signed by the client.
func (ss *session) validateRequest(req *smb2.Request) bool {
	if !req.Header().IsFlagSet(smb2.FLAGS_SIGNED) || req.IsEncrypted() {
		return true
	}

	signature := req.Header().Signature()
	req.Header().WipeSignature()
	var verifier hash.Hash
	switch ss.connection.negotiateDialect {
	case smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21:
		verifier = hmac.New(sha256.New, ss.sessionKey)
	case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
		ciph, err := aes.NewCipher(ss.signingKey)
		if err != nil {
			log.Printf("Error creating cipher for verifying signature: %v", err)
			return false
		}
		verifier = cmac.New(ciph)
	case smb2.SMB_DIALECT_311:
		switch ss.connection.signingAlgorithmID {
		case smb2.AES_CMAC:
			ciph, err := aes.NewCipher(ss.signingKey)
			if err != nil {
				log.Printf("Error creating cipher for verifying signature: %v", err)
				return false
			}
			verifier = cmac.New(ciph)
		case smb2.AES_GMAC:
			nonce := make([]byte, 12)
			binary.LittleEndian.PutUint64(nonce[:8], req.Header().MessageID())
			if req.Header().Command() == smb2.SMB2_CANCEL {
				nonce[8] |= 2
			}
			var err error
			verifier, err = gmac.New(ss.signingKey, nonce)
			if err != nil {
				log.Printf("Error creating cipher for verifying signature: %v", err)
				return false
			}
		default:
			ciph, err := aes.NewCipher(ss.signingKey)
			if err != nil {
				log.Printf("Error creating cipher for verifying signature: %v", err)
				return false
			}
			verifier = cmac.New(ciph)
		}
	}
	verifier.Reset()
	verifier.Write(req.Header())
	sum := verifier.Sum(nil)
	return bytes.Equal(signature, sum[:16])
}

// encrypt uses the encryption key to encrypt the SMB message.
func (ss *session) encrypt(buf []byte) []byte {
	ciph, err := aes.NewCipher(ss.encryptionKey)
	if err != nil {
		log.Printf("Error creating cipher for encryption: %v", err)
		return nil
	}
	var encrypter cipher.AEAD
	switch ss.connection.negotiateDialect {
	case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
		encrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
	case smb2.SMB_DIALECT_311:
		switch ss.connection.cipherID {
		case smb2.AES_128_CCM:
			encrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
		case smb2.AES_128_GCM:
			encrypter, err = cipher.NewGCMWithNonceSize(ciph, 12)
		}
	}
	if err != nil {
		log.Printf("Error creating encrypter: %v", err)
		return nil
	}
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

// decrypt uses the decryption key to decrypt the SMB message.
func (ss *session) decrypt(buf []byte) []byte {
	input := append(buf[smb2.SMB2TransformHeaderSize:], smb2.Header(buf).EncryptionSignature()...)
	ciph, err := aes.NewCipher(ss.decryptionKey)
	if err != nil {
		log.Printf("Error creating cipher for decryption: %v", err)
		return nil
	}
	var decrypter cipher.AEAD
	switch ss.connection.negotiateDialect {
	case smb2.SMB_DIALECT_30, smb2.SMB_DIALECT_302:
		decrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
	case smb2.SMB_DIALECT_311:
		switch ss.connection.cipherID {
		case smb2.AES_128_CCM:
			decrypter, err = ccm.NewCCMWithNonceAndTagSizes(ciph, 11, 16)
		case smb2.AES_128_GCM:
			decrypter, err = cipher.NewGCMWithNonceSize(ciph, 12)
		}
	}
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
