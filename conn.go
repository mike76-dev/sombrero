package main

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/rpc"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/spnego"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"github.com/oiweiwei/go-msrpc/msrpc/lsat/lsarpc/v0"
	"github.com/oiweiwei/go-msrpc/ndr"
	"lukechampine.com/frand"
)

const (
	// staleThreshold is how soon a connection or a session are considered stale.
	staleThreshold = 10 * time.Minute
)

var (
	errRequestNotWithinWindow        = errors.New("request out of command sequence window")
	errCommandSecuenceWindowExceeded = errors.New("command sequence window exceeded")
	errLongRequest                   = errors.New("request too long")
	errAlreadyNegotiated             = errors.New("dialect already negotiated")
	errInvalidSignature              = errors.New("invalid signature")
)

// connection represents a Connection object.
type connection struct {
	commandSequenceWindow      map[uint64]struct{}
	requestList                map[uint64]*smb2.Request
	pendingResponses           map[uint64]smb2.GenericResponse
	clientCapabilities         uint32
	negotiateDialect           uint16
	asyncCommandList           map[uint64]*smb2.Request
	dialect                    string
	shouldSign                 bool
	clientName                 string
	clientGuid                 []byte
	maxTransactSize            uint64
	maxWriteSize               uint64
	maxReadSize                uint64
	supportsMultiCredit        bool
	sessionTable               map[uint64]*session
	creationTime               time.Time
	serverCapabilities         uint32
	clientSecurityMode         uint16
	serverSecurityMode         uint16
	preauthIntegrityHashID     uint16
	preauthIntegrityHashValue  []byte
	preauthSessionTable        map[uint64]*preauthSession
	cipherID                   uint16
	clientDialects             []uint16
	compressionIDs             []uint16
	supportsChainedCompression bool
	signingAlgorithmID         uint16

	// Auxiliary fields.
	conn       net.Conn
	mu         sync.Mutex
	server     *server
	ntlmServer *ntlm.Server
	writeChan  chan []byte
	closeChan  chan struct{}
	once       sync.Once
	stopChans  map[uint64]chan struct{}

	// requestOpens maps the message ID of an in-flight request that carries a FileId
	// to the Open it refers to. It lets the outstanding request counters of the Open
	// be decremented when the response to that request is sent, no matter whether the
	// Open is still in the tables by then (an SMB2_CLOSE removes it before responding).
	requestOpens map[uint64]*open
}

// grantCredits increases the number of credits available to the client by the given number.
// Each SMB2 request consumes at least one credit.
func (c *connection) grantCredits(mid uint64, numCredits uint16) error {
	if numCredits == 0 {
		numCredits = 1 // At least one credit needs to be granted
	}
	// Find the maximal message ID that a request may come in with.
	max, _ := utils.FindMaxKey(c.commandSequenceWindow)
	if max == 0 { // Window empty or only containing zero
		max = mid
	}

	if uint64(numCredits) > math.MaxUint64-max {
		return errCommandSecuenceWindowExceeded
	}

	var i uint64
	for i = 0; i < uint64(numCredits); i++ {
		c.commandSequenceWindow[max+i+1] = struct{}{}
	}

	return nil
}

// acceptRequest processes an SMB message into one or more requests and puts them in the queue.
func (c *connection) acceptRequest(msg []byte) error {
	if uint64(len(msg)) > c.maxTransactSize+256 {
		return errLongRequest
	}

	if len(msg) < smb2.SMB2HeaderSize {
		return smb2.ErrWrongLength
	}

	// Assign a random cancel ID.
	cid := make([]byte, 8)
	frand.Read(cid)

	// Check for encryption.
	var tsid uint64
	var size uint32
	if smb2.Header(msg).ProtocolID() == smb2.PROTOCOL_SMB2_ENCRYPTED {
		if c.serverCapabilities&smb2.GLOBAL_CAP_ENCRYPTION == 0 {
			return smb2.ErrEncryptedMessage
		}
		if smb2.Header(msg).EncryptionFlags() != 1 {
			return smb2.ErrWrongFormat
		}
		tsid = smb2.Header(msg).TransformSessionID()
		size = smb2.Header(msg).OriginalMessageSize()
		c.mu.Lock()
		ss, ok := c.sessionTable[tsid]
		if !ok {
			c.mu.Unlock()
			return errSessionNotFound
		}
		c.mu.Unlock()
		if ss.isAnonymous || ss.isGuest {
			return errAccessDenied
		}
		msg = ss.decrypt(msg, c)
		if uint32(len(msg)) != size {
			return smb2.ErrWrongLength
		}
	}

	var compressed bool
	if smb2.Header(msg).ProtocolID() == smb2.PROTOCOL_SMB2_COMPRESSED {
		var err error
		msg, err = c.decompress(msg)
		if err != nil {
			log.Printf("couldn't decompress message: %v", err)
			return errDecompressionError
		}
		compressed = true
	}

	reqs, err := smb2.GetRequests(msg, binary.LittleEndian.Uint64(cid), tsid, compressed)
	if err != nil {
		return err
	}

	var ss *session
	var found bool
	for i, req := range reqs {
		if err := req.Header().Validate(); err != nil {
			return err
		}

		var mid uint64
		if req.Header().IsSmb() {
			// Once an SMB2 dialect has been negotiated, no more legacy SMB requests are allowed.
			// The protocol explicitly prohibits that; however, many cients do that nevertheless.
			if c.negotiateDialect != smb2.SMB_DIALECT_UNKNOWN || len(reqs) > 1 {
				return smb2.ErrWrongProtocol
			}
			c.mu.Lock()
			c.grantCredits(mid, 1) // Grant just one credit
			c.mu.Unlock()
		} else {
			if c.supportsMultiCredit {
				if req.Header().Command() != smb2.SMB2_READ &&
					req.Header().Command() != smb2.SMB2_WRITE &&
					req.Header().Command() != smb2.SMB2_IOCTL &&
					req.Header().Command() != smb2.SMB2_QUERY_DIRECTORY &&
					req.Header().Command() != smb2.SMB2_CHANGE_NOTIFY &&
					req.Header().Command() != smb2.SMB2_QUERY_INFO &&
					req.Header().Command() != smb2.SMB2_SET_INFO &&
					req.Len() > 68*1024 {
					return errLongRequest
				}
			} else {
				if req.Len() > 68*1024 {
					return errLongRequest
				}
			}
			mid = req.Header().MessageID()
			credits := max(req.Header().CreditCharge(), req.Header().CreditRequest()) // Grant whatever the CreditRequest is. If CreditCharge is greater, grant that much.
			if credits == 0 {                                                         // The number of credits cannot be zero
				credits = 1
			}

			c.mu.Lock()
			c.grantCredits(mid, credits)
			c.mu.Unlock()
			if req.Header().Command() == smb2.SMB2_CANCEL { // SMB2_CANCEL requests are handled separately
				if err := c.cancelRequest(req); err != nil {
					log.Printf("Couldn't cancel request %d:, %v\n", req.Header().Command(), err)
				}

				continue
			}
		}

		c.mu.Lock()
		_, ok := c.commandSequenceWindow[mid]
		if !ok {
			c.mu.Unlock()
			return errRequestNotWithinWindow
		}
		c.mu.Unlock()

		if mid == math.MaxUint64 {
			return errCommandSecuenceWindowExceeded
		}

		// If this is the first request in a chain of related requests, or if the requests are unrelated,
		// find the associated session.
		if i == 0 || req.GroupID() == 0 {
			c.mu.Lock()
			ss, found = c.sessionTable[req.Header().SessionID()]
			c.mu.Unlock()
			if !found && isSessionBinding(req) {
				// A request that binds a session to this connection is signed with the
				// signing key of that session, which is only reachable through the global
				// table: the session doesn't belong to this connection yet.
				c.server.mu.Lock()
				ss, found = c.server.globalSessionTable[req.Header().SessionID()]
				c.server.mu.Unlock()
			}
		}

		var rejectStatus uint32
		if found {
			if err := ss.validateRequest(req, c); err != nil {
				if !errors.Is(err, errNoSigningKey) {
					return err
				}
				// The key required to verify the signature is not available. The request
				// must be failed and not processed any further.
				rejectStatus = smb2.STATUS_NOT_SUPPORTED
			}
		}

		// Request processed; this message ID is not allowed anymore.
		c.mu.Lock()
		if c.negotiateDialect == smb2.SMB_DIALECT_202 || !c.supportsMultiCredit {
			delete(c.commandSequenceWindow, mid)
		} else { // Remove as many IDs as the CreditCharge field
			var count uint16
			i := mid
			m, _ := utils.FindMaxKey(c.commandSequenceWindow)
			for i < m && count < req.Header().CreditCharge() {
				if _, found := c.commandSequenceWindow[i]; found {
					delete(c.commandSequenceWindow, i)
					count++
				}
				i++
			}
		}

		if rejectStatus != 0 {
			c.mu.Unlock()
			// The response carries a copy of the request header, so the signature of the
			// client has to be cleared: the response cannot be signed, since the key to
			// sign it with is the one that couldn't be found.
			resp := smb2.NewErrorResponse(*req, rejectStatus, 0, nil)
			resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
			resp.Header().WipeSignature()
			c.server.writeResponse(c, ss, resp)
			continue
		}

		// Put request in the queue.
		c.requestList[mid] = req
		c.mu.Unlock()
	}

	return nil
}

// processRequest processes the request depending on its Command field and genertates a response.
func (c *connection) processRequest(req *smb2.Request) (smb2.GenericResponse, *session, error) {
	if req.Header().IsSmb() && req.Header().LegacyCommand() == smb2.SMB_COM_NEGOTIATE {
		// The client has sent a legacy SMB_COM_NEGOTIATE request.
		nr := smb2.NegotiateRequest{Request: *req}
		if err := nr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrDialectNotSupported) { // The client doesn't support SMB2, decline
				resp := smb2.NegotiateErrorResponse(smb2.STATUS_NOT_SUPPORTED)
				return resp, nil, nil
			}
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NegotiateErrorResponse(smb2.STATUS_INVALID_PARAMETER)
				return resp, nil, nil
			}
			log.Println("Invalid SMB_COM_NEGOTIATE request:", err)
			return nil, nil, err
		}

		// Respond with an SMB2_NEGOTIATE response.
		dialect := nr.MaxCommonDialect()
		switch dialect {
		case smb2.SMB_DIALECT_202:
			c.negotiateDialect = dialect
			c.dialect = "2.0.2"
			c.maxTransactSize = 65536
			c.maxReadSize = 65536
			c.maxWriteSize = 65536
		case smb2.SMB_DIALECT_MULTICREDIT:
			c.supportsMultiCredit = true
		}
		c.grantCredits(nr.Header().MessageID(), 1) // Grant just one credit

		resp := smb2.NewNegotiateResponse(c.server.serverGuid[:], c.ntlmServer, dialect, c.serverCapabilities, uint32(c.maxTransactSize), uint32(c.maxReadSize), uint32(c.maxWriteSize))
		return resp, nil, nil
	}

	switch req.Header().Command() {
	case smb2.SMB2_NEGOTIATE:
		if c.negotiateDialect != smb2.SMB_DIALECT_UNKNOWN { // A dialect has already been negotiated
			log.Println("Error: repeated SMB2_NEGOTIATE request received")
			return nil, nil, errAlreadyNegotiated
		}

		nr := smb2.NegotiateRequest{Request: *req}
		if err := nr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrDialectNotSupported) {
				resp := smb2.NewErrorResponse(nr, smb2.STATUS_NOT_SUPPORTED, 0, nil)
				return resp, nil, nil
			}
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(nr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_NEGOTIATE request:", err)
			return nil, nil, err
		}

		c.clientCapabilities = nr.Capabilities()
		c.clientGuid = nr.ClientGuid()
		c.clientSecurityMode = nr.SecurityMode()
		c.negotiateDialect = nr.MaxCommonDialect()
		switch c.negotiateDialect {
		case smb2.SMB_DIALECT_202:
			c.dialect = "2.0.2"
		case smb2.SMB_DIALECT_21:
			c.dialect = "2.1"
		case smb2.SMB_DIALECT_30:
			c.dialect = "3.0"
		case smb2.SMB_DIALECT_302:
			c.dialect = "3.0.2"
		case smb2.SMB_DIALECT_311:
			c.dialect = "3.1.1"
			c.clientDialects = nr.Dialects()
			c.serverCapabilities = c.serverCapabilities &^ smb2.GLOBAL_CAP_ENCRYPTION
		}

		if smb2.Is3X(c.negotiateDialect) && c.server.isMultiChannelCapable && c.clientCapabilities&smb2.GLOBAL_CAP_MULTI_CHANNEL > 0 {
			c.serverCapabilities |= smb2.GLOBAL_CAP_MULTI_CHANNEL
		}

		if nr.SecurityMode()&smb2.NEGOTIATE_SIGNING_REQUIRED > 0 {
			c.shouldSign = true
		}

		if c.negotiateDialect != smb2.SMB_DIALECT_202 {
			c.supportsMultiCredit = true
		} else {
			c.maxTransactSize = 65536
			c.maxReadSize = 65536
			c.maxWriteSize = 65536
		}

		resp := &smb2.NegotiateResponse{}
		resp.FromRequest(nr)
		resp.Generate(c.server.serverGuid[:], c.ntlmServer, c.negotiateDialect, c.serverCapabilities, uint32(c.maxTransactSize), uint32(c.maxReadSize), uint32(c.maxWriteSize))

		if c.negotiateDialect == smb2.SMB_DIALECT_311 {
			ncs := nr.NegotiateContexts()
			hashAlgos, _, err := smb2.GetPreauthIntegrityCapabilities(ncs)
			if err != nil {
				resp := smb2.NewErrorResponse(nr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			if !utils.IsOverlapped(hashAlgos, supportedHashAlgos) {
				resp := smb2.NewErrorResponse(nr, smb2.STATUS_SMB_NO_PREAUTH_INTEGRITY_HASH_OVERLAP, 0, nil)
				return resp, nil, nil
			}
			c.preauthIntegrityHashID = supportedHashAlgos[0]
			switch c.preauthIntegrityHashID {
			case smb2.SHA_512:
				c.preauthIntegrityHashValue = make([]byte, 64)
				h := sha512.New()
				h.Write(c.preauthIntegrityHashValue)
				h.Write(req.Header()) // The entire request message
				c.preauthIntegrityHashValue = h.Sum(c.preauthIntegrityHashValue[:0])
			}

			ciphers, err := smb2.GetEncryptionCapabilities(ncs)
			if err != nil {
				resp := smb2.NewErrorResponse(nr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			if ciphers != nil {
				c.cipherID = utils.FirstMatch(ciphers, supportedEncryptionAlgos)
				c.serverCapabilities |= smb2.GLOBAL_CAP_ENCRYPTION
			}

			flags, compAlgos, err := smb2.GetCompressionCapabilities(ncs)
			if err != nil {
				resp := smb2.NewErrorResponse(nr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			if utils.Equal(utils.Subset(compAlgos, supportedCompressionAlgos), compAlgos) {
				c.compressionIDs = compAlgos
			}
			if c.server.chainedCompressionSupported && flags&smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED != 0 {
				flags = smb2.COMPRESSION_CAPABILITIES_FLAG_CHAINED
				c.supportsChainedCompression = true
			} else {
				flags = smb2.COMPRESSION_CAPABILITIES_FLAG_NONE
			}

			signingAlgos, err := smb2.GetSigningCapabilities(ncs)
			if err != nil {
				resp := smb2.NewErrorResponse(nr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			if signingAlgos != nil {
				c.signingAlgorithmID = utils.FirstMatch(signingAlgos, supportedSigningAlgos)
			}

			var blobs [][]byte
			var salt [32]byte
			frand.Read(salt[:])
			blobs = append(blobs, smb2.PreauthIntegrityCapabilities(salt[:]))

			if ciphers != nil {
				blobs = append(blobs, smb2.EncryptionCapabilities(c.cipherID))
			}

			var cas []uint16
			if compAlgos != nil {
				if len(c.compressionIDs) == 0 {
					cas = append(cas, smb2.COMPRESSION_NONE)
				} else {
					cas = c.compressionIDs
				}
				blobs = append(blobs, smb2.CompressionCapabilities(flags, cas))
			}

			if len(signingAlgos) != 0 {
				blobs = append(blobs, smb2.SigningCapabilities(c.signingAlgorithmID))
			}

			resp.AddNegotiateContexts(blobs)

			switch c.preauthIntegrityHashID {
			case smb2.SHA_512:
				h := sha512.New()
				h.Write(c.preauthIntegrityHashValue)
				h.Write(resp.Encode())
				c.preauthIntegrityHashValue = h.Sum(c.preauthIntegrityHashValue[:0])
			}
		}

		return resp, nil, nil

	case smb2.SMB2_SESSION_SETUP:
		ssr := smb2.SessionSetupRequest{Request: *req}
		if err := ssr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(ssr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_SESSION_SETUP request:", err)
			return nil, nil, err
		}

		if smb2.Is3X(c.negotiateDialect) && c.server.encryptData && c.clientCapabilities&smb2.GLOBAL_CAP_ENCRYPTION == 0 {
			resp := smb2.NewErrorResponse(ssr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, nil, nil
		}

		if isSessionBinding(req) {
			// The client wants to add this connection to an existing session as another
			// channel. Only a connection that negotiated a dialect with channels on a server
			// that offers them can carry one.
			if !smb2.Is3X(c.negotiateDialect) || !c.server.isMultiChannelCapable {
				resp := smb2.NewErrorResponse(ssr, smb2.STATUS_REQUEST_NOT_ACCEPTED, 0, nil)
				return resp, nil, nil
			}

			bss, status := c.prepareBinding(ssr)
			if status != smb2.STATUS_OK {
				resp := smb2.NewErrorResponse(ssr, status, 0, nil)
				return resp, nil, nil
			}

			return c.bindSession(bss, ssr)
		}

		// Find a session or create a new one.
		ss, found, err := c.server.registerSession(c, ssr)
		if err != nil {
			if errors.Is(err, errSessionNotFound) {
				resp := smb2.NewErrorResponse(ssr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
				return resp, nil, nil
			} else {
				log.Println("Error registering session:", err)
				return nil, nil, err
			}
		}

		var token []byte
		if found { // Session found, proceed to the step 2
			authToken, err := spnego.DecodeNegTokenResp(ssr.SecurityBuffer())
			if err != nil { // It's possible that the token is not wrapped in SPNEGO; fall back to raw bytes
				authToken = &spnego.NegTokenResp{ResponseToken: ssr.SecurityBuffer()}
			}

			// Try to authenticate the user.
			// This code doesn't distinguish between different authentication errors; perhaps it should.
			if err := c.ntlmServer.Authenticate(authToken.ResponseToken); err != nil {
				c.server.deregisterSession(c, ss.sessionID)
				c.server.mu.Lock()
				c.server.stats.PwErrors++
				c.server.mu.Unlock()
				resp := smb2.NewErrorResponse(ssr, smb2.STATUS_NO_SUCH_USER, 0, nil)
				return resp, nil, nil
			}

			// User successfully authenticated.
			ss.finalize(ssr)
			if smb2.Is3X(c.negotiateDialect) && c.server.encryptData && c.serverCapabilities&smb2.GLOBAL_CAP_ENCRYPTION != 0 {
				ss.signingRequired = false
				ss.encryptData = true
			} else {
				ss.signingRequired = true
				ss.encryptData = false
			}

			ss.mu.Lock()
			ss.idleTime = time.Now()
			ss.mu.Unlock()

			token = spnego.FinalNegTokenResp
			if smb2.Is3X(c.negotiateDialect) {
				c.server.mu.Lock()
				cl, ok := c.server.globalClientTable[[16]byte(c.clientGuid)]
				c.server.mu.Unlock()
				if !ok {
					cl = &smbClient{[16]byte(c.clientGuid), c.negotiateDialect}
					c.server.mu.Lock()
					c.server.globalClientTable[[16]byte(c.clientGuid)] = cl
					c.server.mu.Unlock()
				} else if cl.dialect != c.negotiateDialect {
					c.server.deregisterSession(c, ss.sessionID)
					resp := smb2.NewErrorResponse(ssr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
					return resp, nil, nil
				}
			}
		} else { // Begin the session setup process
			negToken, err := spnego.DecodeNegTokenInit(ssr.SecurityBuffer())
			var noSpnego bool
			if err != nil { // It's possible that the token is not wrapped in SPNEGO; fall back to raw bytes
				negToken = &spnego.NegTokenInit{MechToken: ssr.SecurityBuffer()}
				noSpnego = true
			}

			// Generate a challenge.
			challenge, err := c.ntlmServer.Challenge(negToken.MechToken)
			if err != nil {
				c.server.deregisterSession(c, ss.sessionID)
				log.Println("Couldn't generate CHALLENGE:", err)
				return nil, nil, err
			}

			if noSpnego {
				token = challenge
			} else {
				token, err = spnego.EncodeNegTokenResp(0x01, spnego.NlmpOid, challenge, nil)
				if err != nil {
					c.server.deregisterSession(c, ss.sessionID)
					log.Println("Couldn't generate CHALLENGE token:", err)
					return nil, nil, err
				}
			}
		}

		var flags uint16
		if ss.state == sessionValid {
			switch strings.ToLower(ss.userName) {
			case "":
				flags = smb2.SESSION_FLAG_IS_NULL
			case "guest":
				flags = smb2.SESSION_FLAG_IS_GUEST
			default:
			}
		}
		if smb2.Is3X(c.negotiateDialect) && ss.encryptData && c.clientCapabilities&smb2.GLOBAL_CAP_ENCRYPTION != 0 {
			flags |= smb2.SESSION_FLAG_ENCRYPT_DATA
		}

		resp := &smb2.SessionSetupResponse{}
		resp.FromRequest(ssr)
		resp.Generate(ss.sessionID, flags, token, found)
		if !found {
			resp.Header().SetCreditResponse(1) // Only one credit if the process is incomplete

			if c.negotiateDialect == smb2.SMB_DIALECT_311 {
				switch ss.connection.preauthIntegrityHashID {
				case smb2.SHA_512:
					h := sha512.New()
					h.Write(ss.preauthIntegrityHashValue)
					h.Write(resp.Encode())
					ss.preauthIntegrityHashValue = h.Sum(ss.preauthIntegrityHashValue[:0])
				}
			}
		}

		return resp, ss, nil

	case smb2.SMB2_LOGOFF:
		lr := smb2.LogoffRequest{Request: *req}
		if err := lr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(lr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_LOGOFF request:", err)
			return nil, nil, err
		}

		ss, err := c.server.deregisterSession(c, req.Header().SessionID())
		if err != nil {
			if errors.Is(err, errSessionNotFound) {
				resp := smb2.NewErrorResponse(lr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
				return resp, nil, nil
			} else {
				log.Println("Error deregistering session:", err)
				return nil, nil, err
			}
		}

		resp := &smb2.LogoffResponse{}
		resp.FromRequest(lr)

		return resp, ss, nil

	case smb2.SMB2_TREE_CONNECT:
		tcr := smb2.TreeConnectRequest{Request: *req}
		if err := tcr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(tcr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_TREE_CONNECT request:", err)
			return nil, nil, err
		}

		// Validate signature or encryption.
		if c.negotiateDialect == smb2.SMB_DIALECT_311 {
			if !tcr.Header().IsFlagSet(smb2.FLAGS_SIGNED) && !tcr.IsEncrypted() {
				if c.server.debug {
					log.Println("Unsigned or unencrypted SMB2_TREE_CONNECT request")
				}
				return nil, nil, smb2.ErrWrongSecurity
			}
		}

		c.mu.Lock()
		ss, found := c.sessionTable[tcr.Header().SessionID()]
		c.mu.Unlock()
		if !found {
			resp := smb2.NewErrorResponse(tcr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		tc, err := c.newTreeConnect(ss, tcr.PathName())
		if err != nil {
			if errors.Is(err, errAccessDenied) {
				resp := smb2.NewErrorResponse(tcr, smb2.STATUS_ACCESS_DENIED, 0, nil)
				return resp, ss, nil
			} else if errors.Is(err, errNoShare) {
				resp := smb2.NewErrorResponse(tcr, smb2.STATUS_SHARE_UNAVAILABLE, 0, nil)
				return resp, ss, nil
			} else {
				resp := smb2.NewErrorResponse(tcr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}
		}

		resp := &smb2.TreeConnectResponse{}
		resp.FromRequest(tcr)
		resp.Generate(tc.treeID, uint8(tc.share.shareType), tc.maximalAccess, tc.share.encryptData, tc.share.compressData)

		return resp, ss, nil

	case smb2.SMB2_TREE_DISCONNECT:
		tdr := smb2.TreeDisconnectRequest{Request: *req}
		if err := tdr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(tdr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_TREE_DISCONNECT request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[tdr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(tdr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		if err := ss.closeTreeConnect(tdr.Header().TreeID()); err != nil {
			resp := smb2.NewErrorResponse(tdr, smb2.STATUS_NETWORK_NAME_DELETED, 0, nil)
			return resp, ss, nil
		}

		resp := &smb2.TreeDisconnectResponse{}
		resp.FromRequest(tdr)

		return resp, ss, nil

	case smb2.SMB2_CREATE:
		cr := smb2.CreateRequest{Request: *req}
		if err := cr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_CREATE request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[cr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		tc, found := ss.treeConnectTable[cr.Header().TreeID()]
		ss.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		contexts, err := cr.CreateContexts()
		if err != nil {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		// A reconnect claims a handle that already exists, so it is answered from the open
		// that was kept aside rather than by resolving the path and going to the backend.
		if ctx, found := contexts[smb2.SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2]; found {
			rec, ok := smb2.ParseDurableHandleReconnectV2(ctx)
			if !ok {
				resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}

			op := c.reclaimDurableOpen(rec, ss, tc)
			if op == nil {
				// Nothing to reclaim: the handle is gone, or it never belonged to this
				// client. The client is expected to open the file from scratch.
				resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
				return resp, ss, nil
			}

			resp := &smb2.CreateResponse{}
			resp.FromRequest(cr)
			op.mu.Lock()
			resp.Generate(
				smb2.OPLOCK_LEVEL_NONE,
				smb2.FILE_OPENED,
				op.size,
				op.allocated,
				op.lastModified,
				op.fileAttributes&smb2.FILE_ATTRIBUTE_DIRECTORY > 0,
				op.fileID,
				op.durableFileID,
				nil,
			)
			op.mu.Unlock()
			req.SetOpenID(op.id())

			return resp, ss, nil
		}

		path := strings.ReplaceAll(cr.Filename(), "\\", "/")
		if strings.HasPrefix(path, ".") { // Hidden files of any sort are not supported
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_NOT_SUPPORTED, 0, nil)
			return resp, ss, nil
		}

		co := cr.CreateOptions()
		if co&smb2.FILE_DELETE_ON_CLOSE > 0 && (tc.maximalAccess&(smb2.DELETE|smb2.GENERIC_ALL|smb2.GENERIC_EXECUTE|smb2.GENERIC_READ|smb2.GENERIC_WRITE) == 0) {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		if co&smb2.FILE_NO_INTERMEDIATE_BUFFERING > 0 {
			da := cr.DesiredAccess()
			cr.SetDesiredAccess(da &^ smb2.FILE_APPEND_DATA)
		}

		co = co &^ smb2.FILE_COMPLETE_IF_OPLOCKED
		co = co &^ smb2.FILE_SYNCHRONOUS_IO_ALERT
		co = co &^ smb2.FILE_SYNCHRONOUS_IO_NONALERT
		co = co &^ smb2.FILE_OPEN_FOR_FREE_SPACE_QUERY
		cr.SetCreateOptions(co)

		// The access rights are settled before anybody's oplock is disturbed. A client that
		// may not have the file has no business making whoever holds it give it up, and a
		// create that is going to be refused has nothing to wait for. The desired access is
		// read after the adjustment above, which is what decides it in the marginal cases.
		if tc.share.name != "ipc$" && !grantAccess(cr, tc, ss) {
			c.server.mu.Lock()
			c.server.stats.PermErrors++
			c.server.mu.Unlock()
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		// Whoever holds an oplock on this file has to give it up before the create may look at
		// the file at all, because the holder may be sitting on writes it has not sent yet.
		if !c.server.hasOplockHolders(tc.share, path, nil) {
			return c.createFile(req, cr, ss, tc, acc, contexts, path), ss, nil
		}

		// Waiting for the acknowledgment cannot happen here: a connection serves its requests
		// one at a time, and the acknowledgment being waited for may be on its way in over this
		// very connection. The create is answered with an interim response and finished on a
		// goroutine of its own.
		aid := make([]byte, 8)
		frand.Read(aid)
		asyncID := binary.LittleEndian.Uint64(aid)
		c.mu.Lock()
		c.asyncCommandList[asyncID] = req
		c.mu.Unlock()

		go func() {
			c.server.breakOplocksOn(tc.share, path, nil)

			resp := c.createFile(req, cr, ss, tc, acc, contexts, path)

			// The final response of an asynchronous command must carry the async flag and ID
			// in all cases, including errors.
			resp.Header().SetAsyncID(asyncID)
			resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)

			c.mu.Lock()
			delete(c.requestList, resp.Header().MessageID())
			delete(c.asyncCommandList, asyncID)
			c.mu.Unlock()

			c.releaseOpen(req)
			c.server.writeResponse(c, ss, resp)
		}()

		resp := smb2.NewErrorResponse(cr, smb2.STATUS_PENDING, 0, nil)
		resp.Header().SetAsyncID(asyncID)
		resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
		resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
		resp.Header()[len(resp.Header())-1] = 0x21

		return resp, ss, nil

	case smb2.SMB2_OPLOCK_BREAK:
		obr := smb2.OplockBreakRequest{Request: *req}
		if err := obr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrWrongFormat) {
				// A lease break acknowledgment, told apart by its structure size. The server
				// grants no leases, so there is nothing it could be acknowledging.
				resp := smb2.NewErrorResponse(obr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_OPLOCK_BREAK request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[obr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(obr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		id := obr.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(obr, status, 0, nil)
			return resp, ss, nil
		}

		if status := op.acknowledgeOplockBreak(obr.OplockLevel()); status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(obr, status, 0, nil)
			return resp, ss, nil
		}

		// The oplock is always given up in full, whatever level the client offered to keep, so
		// that is what the response tells it that it has left.
		resp := &smb2.OplockBreakResponse{}
		resp.FromRequest(obr)
		resp.Generate(smb2.OPLOCK_LEVEL_NONE, id)

		return resp, ss, nil

	case smb2.SMB2_CLOSE:
		cr := smb2.CloseRequest{Request: *req}
		if err := cr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_CLOSE request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[cr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		tc, found := ss.treeConnectTable[cr.Header().TreeID()]
		ss.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		id := cr.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(cr, status, 0, nil)
			return resp, ss, nil
		}

		op.mu.Lock()
		pu := op.pendingUpload
		op.mu.Unlock()
		if pu != nil { // This SMB2_CLOSE request is a sign for us to flush any active multipart upload
			if err := op.flush(); err != nil {
				op.cancelUpload()
				log.Println("Error completing write:", err)
			}
		}

		op.mu.Lock()
		co := op.createOptions
		attr := op.fileAttributes
		path := op.pathName
		op.mu.Unlock()
		if co&smb2.FILE_DELETE_ON_CLOSE > 0 { // Delete the file or directory
			tc.mu.Lock()
			delete(tc.persistedOpens, path)
			tc.mu.Unlock()
			if err := tc.client.Delete(op.ctx, acc, path, attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0); err != nil {
				log.Printf("Error deleting object %s: %v", path, err)
			}
		}

		tc.mu.Lock()
		_, found = tc.persistedOpens[path]
		tc.mu.Unlock()
		c.server.closeOpen(op, found)

		// Issue a response to each SMB2_CHANGE_NOTIFY request that the Open is associated with.
		toNotify := make(map[uint64]*smb2.Request)
		c.mu.Lock()
		for aid, r := range c.asyncCommandList {
			if r.Header().Command() == smb2.SMB2_CHANGE_NOTIFY && bytes.Equal(r.OpenID(), id) {
				toNotify[aid] = r
				delete(c.asyncCommandList, aid)
			}
		}
		c.mu.Unlock()

		for aid, r := range toNotify {
			resp := smb2.NewErrorResponse(r, smb2.STATUS_NOTIFY_CLEANUP, 0, nil)
			resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
			resp.Header().SetAsyncID(aid)
			c.releaseOpen(r)
			c.server.writeResponse(c, ss, resp)
		}

		resp := &smb2.CloseResponse{}
		resp.FromRequest(cr)
		op.mu.Lock()
		resp.Generate(op.lastModified, op.size, op.allocated, op.fileAttributes)
		op.mu.Unlock()

		return resp, ss, nil

	case smb2.SMB2_FLUSH: // We don't do anything on an SMB2_FLUSH request, only send a response
		fr := smb2.FlushRequest{Request: *req}
		if err := fr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(fr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_FLUSH request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[fr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(fr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		resp := &smb2.FlushResponse{}
		resp.FromRequest(fr)

		return resp, ss, nil

	case smb2.SMB2_READ:
		rr := smb2.ReadRequest{Request: *req}
		if err := rr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(rr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_READ request:", err)
			return nil, nil, err
		}

		if c.server.compressionSupported && len(c.compressionIDs) != 0 && rr.Flags()&smb2.READFLAG_REQUEST_COMPRESSED != 0 {
			rr.SetCompressReply(true)
		}

		c.mu.Lock()
		ss, found := c.sessionTable[rr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(rr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		if rr.Length() > uint32(c.maxReadSize) {
			resp := smb2.NewErrorResponse(rr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		id := rr.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(rr, status, 0, nil)
			return resp, ss, nil
		}

		op.mu.Lock()
		ga := op.grantedAccess
		name := op.fileName
		op.mu.Unlock()
		if ga&(smb2.FILE_READ_DATA|smb2.GENERIC_READ) == 0 {
			resp := smb2.NewErrorResponse(rr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		// A special case: some clients use the SRVSVC named pipe for writing requests to it
		// and reading responses from it. Usually, an SMB2_IOCTL request serves this purpose.
		if strings.ToLower(name) == "srvsvc" {
			if c.negotiateDialect == smb2.SMB_DIALECT_302 || c.negotiateDialect == smb2.SMB_DIALECT_311 {
				if rr.Flags()&smb2.READFLAG_READ_UNBUFFERED != 0 {
					resp := smb2.NewErrorResponse(rr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
					return resp, ss, nil
				}
			}
			op.mu.Lock()
			data := bytes.Clone(op.srvsvcData)
			op.mu.Unlock()
			if data != nil {
				ip := rpc.InboundPacket{}
				ip.Read(bytes.NewBuffer(data))

				var packet *rpc.OutboundPacket
				switch ip.Header.PacketType {
				case rpc.PACKET_TYPE_BIND:
					body := ip.Body.(*rpc.Bind)
					packet = rpc.NewBindAck(ip.Header.CallID, "\\pipe\\srvsvc", body.ContextList)

				case rpc.PACKET_TYPE_REQUEST:
					body := ip.Body.(*rpc.Request)
					switch body.OpNum {
					case rpc.NET_SHARE_ENUM_ALL:
						var request rpc.NetShareEnumAllRequest
						request.Unmarshal(ip.Payload)
						if request.Level == 1 {
							packet = rpc.NewNetShareEnumAllResponse(
								ip.Header.CallID,
								c.server.enumShares(),
								smb2.STATUS_OK,
							)
						}

					default:
					}

				default:
				}

				var buf bytes.Buffer
				packet.Write(&buf)
				resp := &smb2.ReadResponse{}
				resp.FromRequest(rr)
				resp.Generate(buf.Bytes(), rr.Padding())
				op.mu.Lock()
				op.srvsvcData = nil
				op.mu.Unlock()
				return resp, ss, nil
			}
		}

		op.mu.Lock()
		size := op.size
		op.mu.Unlock()
		if rr.Offset() >= size {
			resp := smb2.NewErrorResponse(rr, smb2.STATUS_END_OF_FILE, 0, nil)
			return resp, ss, nil
		}

		length := uint64(rr.Length())
		if rr.Offset()+length >= size {
			length = size - rr.Offset()
		}

		// If the whole range is already cached, respond right away. Going async here
		// would race the interim response against the final one: the read completes
		// in microseconds, and a client that sees the final response before the
		// interim treats it as a protocol violation and drops the connection.
		if data, ok := op.tryReadCached(rr.Offset(), length); ok {
			if len(data) < int(rr.MinimumCount()) {
				resp := smb2.NewErrorResponse(rr, smb2.STATUS_END_OF_FILE, 0, nil)
				return resp, ss, nil
			}
			resp := &smb2.ReadResponse{}
			resp.FromRequest(rr)
			resp.Generate(data, rr.Padding())
			return resp, ss, nil
		}

		// An SMB2_READ request can take long enough, especially on the Sia network, for the client
		// to drop the connection. We send an interim response and process the request asynchronously
		// to prevent that.
		aid := make([]byte, 8)
		frand.Read(aid)
		asyncID := binary.LittleEndian.Uint64(aid)
		c.mu.Lock()
		c.asyncCommandList[asyncID] = req
		c.mu.Unlock()

		resp := smb2.NewErrorResponse(rr, smb2.STATUS_PENDING, 0, nil)
		resp.Header().SetAsyncID(asyncID)
		resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
		resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
		resp.Header()[len(resp.Header())-1] = 0x21

		go func() {
			var resp smb2.GenericResponse
			data, err := op.read(rr.Offset(), length)
			if err != nil {
				// A failed download is an I/O error, not an end of file: the client
				// may retry the read, while an EOF would truncate the file for it.
				resp = smb2.NewErrorResponse(rr, smb2.STATUS_DATA_ERROR, 0, nil)
			} else if len(data) < int(rr.MinimumCount()) {
				resp = smb2.NewErrorResponse(rr, smb2.STATUS_END_OF_FILE, 0, nil)
			} else {
				resp = &smb2.ReadResponse{}
				resp.FromRequest(rr)
				resp.(*smb2.ReadResponse).Generate(data, rr.Padding())
			}
			// The final response of an async operation must carry the async flag and
			// ID in all cases, including errors.
			resp.Header().SetAsyncID(asyncID)
			resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)

			c.mu.Lock()
			delete(c.requestList, resp.Header().MessageID())
			delete(c.asyncCommandList, asyncID)
			c.mu.Unlock()

			// Check if the context is still valid before sending the response.
			select {
			case <-op.ctx.Done():
				return
			default:
			}

			c.releaseOpen(req)
			c.server.writeResponse(c, ss, resp)
		}()

		return resp, ss, nil

	case smb2.SMB2_WRITE:
		wr := smb2.WriteRequest{Request: *req}
		if err := wr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(wr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_WRITE request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[wr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(wr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		length := uint64(len(wr.Buffer()))
		if length > c.maxWriteSize {
			resp := smb2.NewErrorResponse(wr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		id := wr.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(wr, status, 0, nil)
			return resp, ss, nil
		}

		op.mu.Lock()
		co := op.createOptions
		ga := op.grantedAccess
		name := op.fileName
		size := op.size
		op.mu.Unlock()
		if c.dialect == "2.1" || c.dialect == "3.0" {
			if wr.Flags()&smb2.WRITEFLAG_WRITE_THROUGH > 0 && co&smb2.FILE_NO_INTERMEDIATE_BUFFERING == 0 {
				resp := smb2.NewErrorResponse(wr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}
		}

		if c.dialect == "3.0.2" || c.dialect == "3.1.1" {
			if wr.Flags()&smb2.WRITEFLAG_WRITE_THROUGH > 0 && wr.Flags()&smb2.WRITEFLAG_WRITE_UNBUFFERED == 0 && co&smb2.FILE_NO_INTERMEDIATE_BUFFERING == 0 {
				resp := smb2.NewErrorResponse(wr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}
		}

		if name != "" && name[0] == '.' { // Ignore SMB2_WRITE requests to any hidden file (whose name starts with a dot)
			resp := &smb2.WriteResponse{}
			resp.FromRequest(wr)
			resp.Generate(uint32(len(wr.Buffer())))
			return resp, ss, nil
		}

		if (length <= size && ga&(smb2.FILE_WRITE_DATA|smb2.GENERIC_WRITE) == 0) || ga&(smb2.FILE_APPEND_DATA|smb2.GENERIC_WRITE) == 0 {
			resp := smb2.NewErrorResponse(wr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		// A special case: some clients use the SRVSVC named pipe for writing requests to it
		// and reading responses from it. Usually, an SMB2_IOCTL request serves this purpose.
		if strings.ToLower(name) == "srvsvc" {
			if smb2.Is3X(c.negotiateDialect) && wr.Flags()&(smb2.WRITEFLAG_WRITE_THROUGH|smb2.WRITEFLAG_WRITE_UNBUFFERED) != 0 {
				resp := smb2.NewErrorResponse(wr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}
			buf := make([]byte, len(wr.Buffer()))
			copy(buf, wr.Buffer())
			op.mu.Lock()
			op.srvsvcData = buf
			op.mu.Unlock()
			resp := &smb2.WriteResponse{}
			resp.FromRequest(wr)
			resp.Generate(uint32(len(buf)))
			return resp, ss, nil
		}

		// An SMB2_WRITE request can take long enough, especially on the Sia network, for the client
		// to drop the connection. We send an interim response and process the request asynchronously
		// to prevent that.
		aid := make([]byte, 8)
		frand.Read(aid)
		asyncID := binary.LittleEndian.Uint64(aid)
		c.mu.Lock()
		c.asyncCommandList[asyncID] = req
		c.mu.Unlock()

		resp := smb2.NewErrorResponse(wr, smb2.STATUS_PENDING, 0, nil)
		resp.Header().SetAsyncID(asyncID)
		resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
		resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
		resp.Header()[len(resp.Header())-1] = 0x21

		op.mu.Lock()
		op.inflight++
		op.mu.Unlock()
		go func() {
			defer func() {
				op.mu.Lock()
				op.inflight--
				op.cond.Broadcast()
				op.mu.Unlock()
			}()
			var resp smb2.GenericResponse

			if err := op.write(wr.Offset(), wr.Buffer()); err != nil {
				op.cancelUpload()
				log.Println("Error writing data:", err)
				resp = smb2.NewErrorResponse(wr, smb2.STATUS_DATA_ERROR, 0, nil)
			} else {
				resp = &smb2.WriteResponse{}
				resp.FromRequest(wr)
				resp.(*smb2.WriteResponse).Generate(uint32(len(wr.Buffer())))
				resp.Header().SetAsyncID(asyncID)
				resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
			}

			c.mu.Lock()
			delete(c.requestList, resp.Header().MessageID())
			delete(c.asyncCommandList, asyncID)
			c.mu.Unlock()

			// Check if the context is still valid before sending the response.
			select {
			case <-op.ctx.Done():
				return
			default:
			}

			c.releaseOpen(req)
			c.server.writeResponse(c, ss, resp)
		}()

		return resp, ss, nil

	case smb2.SMB2_LOCK: // We don't do anything on an SMB2_LOCK request, only send a response
		lr := smb2.LockRequest{Request: *req}
		if err := lr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(lr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_LOCK request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[lr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(lr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		resp := &smb2.LockResponse{}
		resp.FromRequest(lr)

		return resp, ss, nil

	case smb2.SMB2_IOCTL:
		ir := smb2.IoctlRequest{Request: *req}
		if err := ir.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_IOCTL request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[ir.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(ir, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		tc, found := ss.treeConnectTable[ir.Header().TreeID()]
		ss.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		if ir.MaxInputResponse() > uint32(c.maxTransactSize) || ir.MaxOutputResponse() > uint32(c.maxTransactSize) || len(ir.InputBuffer()) > int(c.maxTransactSize) {
			resp := smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		if ir.Flags()&smb2.IOCTL_IS_FSCTL == 0 {
			resp := smb2.NewErrorResponse(ir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
			return resp, ss, nil
		}

		id := ir.FileID()
		switch ir.CtlCode() {
		case smb2.FSCTL_DFS_GET_REFERRALS,
			smb2.FSCTL_DFS_GET_REFERRALS_EX,
			smb2.FSCTL_QUERY_NETWORK_INTERFACE_INFO,
			smb2.FSCTL_VALIDATE_NEGOTIATE_INFO,
			smb2.FSCTL_PIPE_WAIT:
			var resp smb2.GenericResponse
			if !bytes.Equal(id, smb2.DummyFileID) {
				resp = smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			} else {
				if ir.CtlCode() == smb2.FSCTL_VALIDATE_NEGOTIATE_INFO {
					if smb2.Is3X(c.negotiateDialect) {
						if c.negotiateDialect == smb2.SMB_DIALECT_311 {
							return nil, nil, smb2.ErrInvalidParameter
						}
						if ir.MaxOutputResponse() < 24 {
							return nil, nil, smb2.ErrWrongLength
						}
						caps, guid, sm, dialects, err := ir.ValidateNegotiateInfo()
						if err != nil {
							return nil, nil, smb2.ErrInvalidParameter
						} else if smb2.MaxSupportedDialect == smb2.SMB_DIALECT_311 && (!utils.Equal(dialects, c.clientDialects) || utils.MaxCommon(dialects, c.clientDialects) != c.negotiateDialect) {
							return nil, nil, smb2.ErrInvalidParameter
						} else if !bytes.Equal(guid, c.clientGuid) {
							return nil, nil, smb2.ErrInvalidParameter
						} else if sm != c.clientSecurityMode {
							return nil, nil, smb2.ErrInvalidParameter
						} else if caps != c.clientCapabilities {
							return nil, nil, smb2.ErrInvalidParameter
						}
						r := &smb2.IoctlResponse{}
						r.FromRequest(ir)
						r.Generate(ir.CtlCode(), smb2.DummyFileID, 0, smb2.ValidateNegotiateInfo(c.serverCapabilities, c.server.serverGuid[:], c.serverSecurityMode, c.negotiateDialect))
						return r, ss, nil
					} else {
						resp = smb2.NewErrorResponse(ir, smb2.STATUS_FILE_CLOSED, 0, nil)
					}
				} else if ir.CtlCode() == smb2.FSCTL_QUERY_NETWORK_INTERFACE_INFO {
					// The interfaces are only worth listing to a client that is able to
					// open a channel to them. The condition is the one the multi-channel
					// capability is advertised under, so that the two cannot disagree.
					info := smb2.NetworkInterfaceInfo(networkInterfaces())
					if !smb2.Is3X(c.negotiateDialect) || !c.server.isMultiChannelCapable || len(info) == 0 {
						resp = smb2.NewErrorResponse(ir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
					} else if uint32(len(info)) > ir.MaxOutputResponse() {
						resp = smb2.NewErrorResponse(ir, smb2.STATUS_BUFFER_TOO_SMALL, 0, nil)
					} else {
						r := &smb2.IoctlResponse{}
						r.FromRequest(ir)
						r.Generate(ir.CtlCode(), smb2.DummyFileID, 0, info)
						resp = r
					}
				} else if ir.CtlCode() == smb2.FSCTL_DFS_GET_REFERRALS || ir.CtlCode() == smb2.FSCTL_DFS_GET_REFERRALS_EX {
					resp = smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_DEVICE_REQUEST, 0, nil)
				} else {
					resp = smb2.NewErrorResponse(ir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
				}
			}

			return resp, ss, nil

		case smb2.FSCTL_PIPE_TRANSCEIVE:
			if tc.share.name != "ipc$" { // FSCTL_PIPE_TRANSCEIVE is only allowed on the IPC$ share
				resp := smb2.NewErrorResponse(ir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
				return resp, ss, nil
			}

			op, status := c.findOpen(ss, id, req)
			if status != smb2.STATUS_OK {
				resp := smb2.NewErrorResponse(ir, status, 0, nil)
				return resp, ss, nil
			}

			ip := rpc.InboundPacket{}
			ip.Read(bytes.NewBuffer(ir.InputBuffer()))

			var packet *rpc.OutboundPacket
			op.mu.Lock()
			name := strings.ToLower(op.fileName)
			op.mu.Unlock()
			switch ip.Header.PacketType {
			case rpc.PACKET_TYPE_BIND:
				var addr string
				switch name {
				case "lsarpc":
					addr = "\\pipe\\lsass"
				case "srvsvc":
					addr = "\\pipe\\srvsvc"
				case "mdssvc":
					addr = "\\pipe\\mdssvc"
				}

				body := ip.Body.(*rpc.Bind)
				packet = rpc.NewBindAck(ip.Header.CallID, addr, body.ContextList)

			case rpc.PACKET_TYPE_REQUEST:
				body := ip.Body.(*rpc.Request)
				switch name {
				case "lsarpc":
					switch body.OpNum {
					case rpc.LSA_GET_USER_NAME:
						packet = rpc.NewGetUserNameResponse(
							ip.Header.CallID,
							c.ntlmServer.Session().User(),
							c.ntlmServer.Session().Domain(),
							smb2.STATUS_OK,
						)

					case rpc.LSA_OPEN_POLICY_2:
						// Create an LSA frame for future requests.
						ctx := ss.securityContext
						frame := op.newLSAFrame(ctx)
						packet = rpc.NewOpenPolicy2Response(
							ip.Header.CallID,
							frame,
							smb2.STATUS_OK,
						)

					case rpc.LSA_LOOKUP_NAMES:
						var request lsarpc.LookupNamesRequest
						if err := ndr.Unmarshal(ip.Payload, &request); err != nil {
							log.Println("Error decoding request:", err)
						} else {
							op.mu.Lock()
							frame, ok := op.lsaFrames[request.Policy.UUID.Data1]
							op.mu.Unlock()
							if !ok {
								resp := smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
								return resp, ss, nil
							}

							packet = rpc.NewLookupNamesResponse(
								ip.Header.CallID,
								frame.SecurityContext,
								smb2.STATUS_OK,
							)
						}

					case rpc.LSA_CLOSE:
						var request lsarpc.CloseRequest
						if err := ndr.Unmarshal(ip.Payload, &request); err != nil {
							log.Println("Error decoding request:", err)
						} else {
							op.mu.Lock()
							_, ok := op.lsaFrames[request.Object.UUID.Data1]
							delete(op.lsaFrames, request.Object.UUID.Data1)
							op.mu.Unlock()
							if !ok {
								resp := smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
								return resp, ss, nil
							}

							packet = rpc.NewCloseResponse(
								ip.Header.CallID,
								smb2.STATUS_OK,
							)
						}
					}

				case "srvsvc":
					switch body.OpNum {
					case rpc.NET_SHARE_GET_INFO:
						var request rpc.NetShareGetInfoRequest
						request.Unmarshal(ip.Payload)
						if request.Level == 1 {
							packet = rpc.NewNetShareGetInfo1Response(
								ip.Header.CallID,
								request.Share,
								tc.share.remark,
								smb2.STATUS_OK,
							)
						}

					case rpc.NET_SHARE_ENUM_ALL:
						var request rpc.NetShareEnumAllRequest
						request.Unmarshal(ip.Payload)
						if request.Level == 1 {
							packet = rpc.NewNetShareEnumAllResponse(
								ip.Header.CallID,
								c.server.enumShares(),
								smb2.STATUS_OK,
							)
						}
					}

				case "mdssvc":
					switch body.OpNum {
					case rpc.MDS_OPEN:
						var request rpc.MdsOpenRequest
						request.Unmarshal(ip.Payload)
						packet = rpc.NewMdsOpenResponse(
							ip.Header.CallID,
							request,
							"",
							smb2.STATUS_OK,
						)
					}
				}
			}

			var buf bytes.Buffer
			packet.Write(&buf)
			resp := &smb2.IoctlResponse{}
			resp.FromRequest(ir)
			resp.Generate(ir.CtlCode(), id, 0, buf.Bytes())
			return resp, ss, nil

		case smb2.FSCTL_SRV_REQUEST_RESUME_KEY:
			op, status := c.findOpen(ss, id, req)
			if status != smb2.STATUS_OK {
				resp := smb2.NewErrorResponse(ir, status, 0, nil)
				return resp, ss, nil
			}

			resp := &smb2.IoctlResponse{}
			resp.FromRequest(ir)
			resp.Generate(ir.CtlCode(), id, 0, op.getResumeKey())
			return resp, ss, nil

		case smb2.FSCTL_CREATE_OR_GET_OBJECT_ID:
			op, status := c.findOpen(ss, id, req)
			if status != smb2.STATUS_OK {
				resp := smb2.NewErrorResponse(ir, status, 0, nil)
				return resp, ss, nil
			}

			resp := &smb2.IoctlResponse{}
			resp.FromRequest(ir)
			resp.Generate(ir.CtlCode(), id, 0, op.getObjectID())
			return resp, ss, nil

		case smb2.FSCTL_SVHDX_SYNC_TUNNEL_REQUEST, smb2.FSCTL_QUERY_SHARED_VIRTUAL_DISK_SUPPORT:
			resp := smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_DEVICE_REQUEST, 0, nil)
			return resp, ss, nil

		default: // Other FSCTL codes are not supported yet
			resp := smb2.NewErrorResponse(ir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
			return resp, ss, nil
		}

	case smb2.SMB2_ECHO:
		er := smb2.EchoRequest{Request: *req}
		if err := er.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(er, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_ECHO request:", err)
			return nil, nil, err
		}

		var ss *session
		var found bool
		if er.Header().SessionID() != 0 || er.Header().IsFlagSet(smb2.FLAGS_SIGNED) {
			c.mu.Lock()
			ss, found = c.sessionTable[er.Header().SessionID()]
			c.mu.Unlock()

			if !found {
				resp := smb2.NewErrorResponse(er, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
				return resp, nil, nil
			}

			ss.mu.Lock()
			ss.idleTime = time.Now()
			ss.mu.Unlock()
		}

		resp := &smb2.EchoResponse{}
		resp.FromRequest(er)

		return resp, ss, nil

	case smb2.SMB2_QUERY_DIRECTORY:
		qdr := smb2.QueryDirectoryRequest{Request: *req}
		if err := qdr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(qdr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_QUERY_DIRECTORY request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[qdr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(qdr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		switch qdr.FileInformationClass() {
		case smb2.FILE_BOTH_DIRECTORY_INFORMATION,
			smb2.FILE_DIRECTORY_INFORMATION,
			smb2.FILE_ID_64_EXTD_BOTH_DIRECTORY_INFORMATION,
			smb2.FILE_ID_64_EXTD_DIRECTORY_INFORMATION,
			smb2.FILE_ID_ALL_EXTD_BOTH_DIRECTORY_INFORMATION,
			smb2.FILE_ID_ALL_EXTD_DIRECTORY_INFORMATION,
			smb2.FILE_ID_BOTH_DIRECTORY_INFORMATION,
			smb2.FILE_ID_EXTD_DIRECTORY_INFORMATION,
			smb2.FILE_ID_FULL_DIRECTORY_INFORMATION:
		default: // Other classes are not supported yet
			resp := smb2.NewErrorResponse(qdr, smb2.STATUS_NOT_SUPPORTED, 0, nil)
			return resp, ss, nil
		}

		ss.mu.Lock()
		tc, found := ss.treeConnectTable[qdr.Header().TreeID()]
		ss.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(qdr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(qdr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		if qdr.OutputBufferLength() > uint32(c.maxTransactSize) {
			resp := smb2.NewErrorResponse(qdr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		id := qdr.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(qdr, status, 0, nil)
			return resp, ss, nil
		}

		op.mu.Lock()
		attr := op.fileAttributes
		ga := op.grantedAccess
		op.mu.Unlock()
		if attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 { // The Open must be a directory
			resp := smb2.NewErrorResponse(qdr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		if ga&smb2.FILE_LIST_DIRECTORY == 0 {
			resp := smb2.NewErrorResponse(qdr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		searchPath := qdr.FileName()
		single := qdr.Flags()&smb2.RETURN_SINGLE_ENTRY > 0
		var buf []byte
		op.mu.Lock()
		ls := op.lastSearch
		res := op.searchResults
		op.mu.Unlock()
		if ls != "" && ls == searchPath && qdr.Flags()&smb2.RESTART_SCANS == 0 {
			// If the search has already run with the same parameters, and all results have been sent
			// to the client, respond with the status STATUS_NO_MORE_FILES.
			if len(res) == 0 {
				op.mu.Lock()
				op.lastSearch = ""
				op.mu.Unlock()
				resp := smb2.NewErrorResponse(qdr, smb2.STATUS_NO_MORE_FILES, 0, nil)
				return resp, ss, nil
			}

			// Send as many search results as the buffer length allows.
			var num int
			buf, num = smb2.QueryDirectoryBuffer(qdr.FileInformationClass(), res, qdr.OutputBufferLength(), single, false, client.FileInfo{}, client.FileInfo{})
			op.mu.Lock()
			op.searchResults = op.searchResults[num:]
			op.mu.Unlock()
		} else {
			// Run a new search.
			if err := op.queryDirectory(acc, searchPath); err != nil && searchPath != "*" {
				if errors.Is(err, errNoFiles) { // No such file exists
					resp := smb2.NewErrorResponse(qdr, smb2.STATUS_NO_SUCH_FILE, 0, nil)
					return resp, ss, nil
				}

				log.Printf("Error running query directory on path %s: %v", searchPath, err)
				resp := smb2.NewErrorResponse(qdr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}

			dir, parentDir, err := tc.client.Parents(op.ctx, acc, searchPath)
			if err != nil {
				log.Printf("Error getting parent directories of path %s: %v", searchPath, err)
				resp := smb2.NewErrorResponse(qdr, smb2.STATUS_BAD_NETWORK_NAME, 0, nil)
				return resp, ss, nil
			}

			// Send as many search results as the buffer length allows.
			var num int
			buf, num = smb2.QueryDirectoryBuffer(qdr.FileInformationClass(), res, qdr.OutputBufferLength(), single, qdr.FileName() == "*", dir, parentDir)
			op.mu.Lock()
			op.searchResults = op.searchResults[num:]
			op.mu.Unlock()
		}

		resp := &smb2.QueryDirectoryResponse{}
		resp.FromRequest(qdr)
		resp.Generate(buf)

		return resp, ss, nil

	case smb2.SMB2_CHANGE_NOTIFY:
		cnr := smb2.ChangeNotifyRequest{Request: *req}
		if err := cnr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(cnr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_CHANGE_NOTIFY request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[cnr.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(cnr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(cnr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		if cnr.OutputBufferLength() > uint32(c.maxTransactSize) {
			resp := smb2.NewErrorResponse(cnr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		id := cnr.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(cnr, status, 0, nil)
			return resp, ss, nil
		}

		op.mu.Lock()
		attr := op.fileAttributes
		ga := op.grantedAccess
		op.mu.Unlock()
		if attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 { // The Open must be a directory
			resp := smb2.NewErrorResponse(cnr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		if ga&smb2.FILE_LIST_DIRECTORY == 0 {
			resp := smb2.NewErrorResponse(cnr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		// Put the request in the async command list.
		aid := make([]byte, 8)
		frand.Read(aid)
		asyncID := binary.LittleEndian.Uint64(aid)
		req.Header().SetAsyncID(asyncID)
		req.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		c.mu.Lock()
		c.asyncCommandList[asyncID] = req
		ch := make(chan struct{})
		c.stopChans[req.CancelRequestID()] = ch
		c.mu.Unlock()

		// Start a thread to monitor the directory for changes.
		go op.checkForChanges(cnr, c, acc, ch)

		// Send an interim response.
		resp := smb2.NewErrorResponse(cnr, smb2.STATUS_PENDING, 0, nil)
		resp.Header().SetAsyncID(asyncID)
		resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
		resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
		resp.Header()[len(resp.Header())-1] = 0x21

		return resp, ss, nil

	case smb2.SMB2_QUERY_INFO:
		qir := smb2.QueryInfoRequest{Request: *req}
		if err := qir.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(qir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_QUERY_INFO request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[qir.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(qir, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		tc, found := ss.treeConnectTable[qir.Header().TreeID()]
		ss.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(qir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		if qir.OutputBufferLength() > uint32(c.maxTransactSize) {
			resp := smb2.NewErrorResponse(qir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		id := qir.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(qir, status, 0, nil)
			return resp, ss, nil
		}

		op.mu.Lock()
		ga := op.grantedAccess
		op.mu.Unlock()

		var info []byte
		switch qir.InfoType() {
		case smb2.INFO_FILE:
			switch qir.FileInfoClass() {
			case smb2.FileAllInformation:
				info = op.fileAllInformation()
			case smb2.FileStandardInformation:
				info = op.fileStandardInformation()
			case smb2.FileNetworkOpenInformation:
				info = op.fileNetworkOpenInformation()
			case smb2.FileNormalizedNameInformation:
				info = op.fileNormalizedNameInformation()
			case smb2.FileEaInformation:
				info = op.fileEaInformation()
			case smb2.FileStreamInformation:
				info = op.fileStreamInformation()
			default: // Other classes are not supported yet
				resp := smb2.NewErrorResponse(qir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
				return resp, ss, nil
			}
			if qir.OutputBufferLength() < uint32(len(info)) {
				var ecd []byte
				if c.negotiateDialect == smb2.SMB_DIALECT_311 {
					ecd = smb2.ErrorContextData(0, nil)
				}
				resp := smb2.NewErrorResponse(qir, smb2.STATUS_INFO_LENGTH_MISMATCH, 0, ecd)
				return resp, ss, nil
			}

		case smb2.INFO_FILESYSTEM:
			switch qir.FileInfoClass() {
			case smb2.FileFsVolumeInformation:
				info = smb2.FileFsVolumeInfo(tc.createdAt, uint32(tc.volumeID), tc.share.name)
			case smb2.FileFsAttributeInformation:
				info = smb2.FileFsAttributeInfo(tc.share.backend)
			case smb2.FileFsSizeInformation:
				si, err := tc.client.Storage(op.ctx)
				if err != nil {
					log.Println("Error getting storage info:", err)
				} else {
					info = smb2.FileFsSizeInfo(si)
				}
			case smb2.FileFsFullSizeInformation:
				// Same as above.
				si, err := tc.client.Storage(op.ctx)
				if err != nil {
					log.Println("Error getting storage info:", err)
				} else {
					info = smb2.FileFsFullSizeInfo(si)
				}
			case smb2.FileFsDeviceInformation:
				info = smb2.FileFsDeviceInfo()
			case smb2.FileFsObjectIdInformation:
				info = smb2.FileFsObjectIDInfo(tc.volumeID)
			default: // Other classes are not supported yet
				resp := smb2.NewErrorResponse(qir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
				return resp, ss, nil
			}
			if qir.OutputBufferLength() < uint32(len(info)) {
				var ecd []byte
				if c.negotiateDialect == smb2.SMB_DIALECT_311 {
					ecd = smb2.ErrorContextData(0, nil)
				}
				resp := smb2.NewErrorResponse(qir, smb2.STATUS_INFO_LENGTH_MISMATCH, 0, ecd)
				return resp, ss, nil
			}

		case smb2.INFO_SECURITY:
			info = smb2.NewSecInfo(ss.securityContext, qir.AdditionalInformation(), ga)
			if qir.OutputBufferLength() < uint32(len(info)) {
				if c.negotiateDialect == smb2.SMB_DIALECT_311 {
					ecd := smb2.ErrorContextData(0, binary.LittleEndian.AppendUint32(nil, uint32(len(info))))
					resp := smb2.NewErrorResponse(qir, smb2.STATUS_BUFFER_OVERFLOW, 1, ecd)
					return resp, ss, nil
				} else {
					ecd := binary.LittleEndian.AppendUint32(nil, uint32(len(info)))
					resp := smb2.NewErrorResponse(qir, smb2.STATUS_BUFFER_OVERFLOW, 0, ecd)
					return resp, ss, nil
				}
			}

		default: // Other info types are not supported yet
			resp := smb2.NewErrorResponse(qir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
			return resp, ss, nil
		}

		resp := &smb2.QueryInfoResponse{}
		resp.FromRequest(qir)
		resp.Generate(info)
		return resp, ss, nil

	case smb2.SMB2_SET_INFO:
		sir := smb2.SetInfoRequest{Request: *req}
		if err := sir.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_SET_INFO request:", err)
			return nil, nil, err
		}

		c.mu.Lock()
		ss, found := c.sessionTable[sir.Header().SessionID()]
		c.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(sir, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		tc, found := ss.treeConnectTable[sir.Header().TreeID()]
		ss.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(sir, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, nil, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		ss.mu.Unlock()

		if len(sir.Buffer()) == 0 || uint64(len(sir.Buffer())) > c.maxTransactSize {
			resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		id := sir.FileID()
		op, status := c.findOpen(ss, id, req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(sir, status, 0, nil)
			return resp, ss, nil
		}

		op.mu.Lock()
		attr := op.fileAttributes
		ga := op.grantedAccess
		path := op.pathName
		op.mu.Unlock()

		switch sir.InfoType() {
		case smb2.INFO_FILE:
			switch sir.FileInfoClass() {
			case smb2.FileEndOfFileInformation:
				if ga&smb2.FILE_WRITE_DATA == 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}

				size := binary.LittleEndian.Uint64(sir.Buffer())
				op.mu.Lock()
				op.allocated = size
				op.mu.Unlock()

			case smb2.FileBasicInformation:
				if ga&smb2.FILE_WRITE_ATTRIBUTES == 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}

				var fbi smb2.FileBasicInfo
				if err := fbi.Decode(sir.Buffer()); err != nil {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
					return resp, ss, nil
				}

				var modTime time.Time
				op.mu.Lock()
				if !fbi.CreationTime.IsZero() {
					modTime = fbi.CreationTime
				}

				if !fbi.LastWriteTime.IsZero() && fbi.LastWriteTime.After(modTime) {
					modTime = fbi.LastWriteTime
				}

				if !fbi.ChangeTime.IsZero() && fbi.ChangeTime.After(modTime) {
					modTime = fbi.ChangeTime
				}

				if modTime.After(op.lastModified) {
					op.lastModified = modTime
				}

				if fbi.FileAttributes != 0 {
					op.fileAttributes = fbi.FileAttributes
				}
				op.mu.Unlock()

			case smb2.FileDispositionInformation:
				if ga&smb2.DELETE == 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}

				if sir.Buffer()[0] == 1 { // Set the delete flag
					if attr&smb2.FILE_ATTRIBUTE_DIRECTORY != 0 {
						empty, err := tc.client.IsEmpty(op.ctx, acc, path+"/")
						if err != nil {
							log.Printf("Error listing directory contents on %s: %v", path, err)
							resp := smb2.NewErrorResponse(sir, smb2.STATUS_NETWORK_NAME_DELETED, 0, nil)
							return resp, ss, nil
						} else {
							if empty {
								if op != nil {
									op.mu.Lock()
									op.createOptions |= smb2.FILE_DELETE_ON_CLOSE
									op.mu.Unlock()
								}
							} else {
								resp := smb2.NewErrorResponse(sir, smb2.STATUS_DIRECTORY_NOT_EMPTY, 0, nil)
								return resp, ss, nil
							}
						}
					} else {
						op.mu.Lock()
						op.createOptions |= smb2.FILE_DELETE_ON_CLOSE
						op.mu.Unlock()
					}
				}

			case smb2.FileRenameInformation:
				if ga&smb2.DELETE == 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}

				var fri smb2.FileRenameInfo
				if err := fri.Decode(sir.Buffer()); err != nil {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_INFO_LENGTH_MISMATCH, 0, nil)
					return resp, ss, nil
				}

				if fri.RootDirectory != 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
					return resp, ss, nil
				}

				// Rename the file or the directory.
				newName := strings.ReplaceAll(fri.FileName, "\\", "/")
				op.mu.Lock()
				size := op.size
				op.mu.Unlock()
				if size == 0 && attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 {
					tc.mu.Lock()
					_, found := tc.persistedOpens[newName]
					if found {
						tc.mu.Unlock()
						resp := smb2.NewErrorResponse(sir, smb2.STATUS_OBJECT_NAME_COLLISION, 0, nil)
						return resp, ss, nil
					}
					tc.persistedOpens[newName] = op
					delete(tc.persistedOpens, path)
					op.mu.Lock()
					op.pathName = newName
					op.fileName = utils.TrimPath(op.pathName)
					op.lastModified = time.Now()
					op.mu.Unlock()
					tc.mu.Unlock()
				} else {
					if err := tc.client.Rename(
						op.ctx,
						acc,
						path,
						newName,
						attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0,
						fri.ReplaceIfExists,
					); err != nil {
						log.Printf("Error renaming path %s to %s: %v", path, newName, err)
						status := uint32(smb2.STATUS_OBJECT_NAME_COLLISION)
						if errors.Is(err, stores.ErrAccessDenied) {
							status = smb2.STATUS_ACCESS_DENIED
						}
						resp := smb2.NewErrorResponse(sir, status, 0, nil)
						return resp, ss, nil
					}
				}

			case smb2.FileAllocationInformation:
				if ga&smb2.FILE_WRITE_DATA == 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}

				buf := sir.Buffer()
				if len(buf) != 8 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
					return resp, ss, nil
				}

				op.mu.Lock()
				op.allocated = binary.LittleEndian.Uint64(buf)
				op.mu.Unlock()

			default:
				resp := smb2.NewErrorResponse(sir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
				return resp, ss, nil
			}

		default:
			resp := smb2.NewErrorResponse(sir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
			return resp, ss, nil
		}

		resp := &smb2.SetInfoResponse{}
		resp.FromRequest(sir)
		return resp, ss, nil

	default: // Other commands are not supported yet
		log.Println("Unrecognized command:", req.Header().Command())
		return nil, nil, errors.New("unrecognized command")
	}
}

// processRequests pulls requests from the queue one by one and submits them for processing.
func (c *connection) processRequests() {
	for {
		var req *smb2.Request
		c.mu.Lock()
		if len(c.requestList) > 0 {
			_, req = utils.FindMinKey(c.requestList)
		}
		c.mu.Unlock()

		if req != nil {
			resp, ss, err := c.processRequest(req)
			if err != nil {
				if c.server.debug {
					log.Printf("Error processing request (Message ID: %d, Command: %d): %v", req.Header().MessageID(), req.Header().Command(), err)
				}
				c.server.closeConnection(c)
				return
			}

			c.mu.Lock()
			delete(c.requestList, resp.Header().MessageID())
			var pendingResp smb2.GenericResponse
			if resp.GroupID() > 0 { // This response is a part of a chain, pull the chain
				pendingResp = c.pendingResponses[resp.GroupID()]
			}
			c.mu.Unlock()

			if resp.Header().Command() == smb2.SMB2_CHANGE_NOTIFY { // Send the chain if it's complete, then the response
				if pendingResp != nil && req.Header().NextCommand() == 0 {
					pendingResp.Header().SetCreditResponse(1)
					c.server.writeResponse(c, ss, pendingResp)
					c.mu.Lock()
					delete(c.pendingResponses, resp.GroupID())
					c.mu.Unlock()
				}
				c.server.writeResponse(c, ss, resp)
			} else if pendingResp != nil { // Add the response to the chain, then send the chain if it's complete
				pendingResp.Append(resp)
				if req.Header().NextCommand() == 0 {
					c.server.writeResponse(c, ss, pendingResp)
					c.mu.Lock()
					delete(c.pendingResponses, resp.GroupID())
					c.mu.Unlock()
				}
			} else if resp.GroupID() == 0 || req.Header().NextCommand() == 0 { // A standalone response, send it
				c.server.writeResponse(c, ss, resp)
			} else { // Start the response chain
				c.mu.Lock()
				resp.SetSessionID(resp.Header().SessionID())
				resp.SetTreeID(resp.Header().TreeID())
				c.pendingResponses[resp.GroupID()] = resp
				c.mu.Unlock()
			}

			// An interim response doesn't complete the request: the counters are
			// decremented when the asynchronous command sends its final response.
			if resp.Header().Status() != smb2.STATUS_PENDING {
				c.releaseOpen(req)
			}
		}

		select {
		case <-c.closeChan:
			return
		default:
		}
	}
}

// sendResponses takes an SMB message from the sending queue and writes it to the underlying TCP connection.
func (c *connection) sendResponses() {
	for {
		select {
		case <-c.closeChan:
			return
		case msg := <-c.writeChan:
			err := writeMessage(c.conn, msg)
			if err != nil {
				log.Println("Error sending message:", err)
				c.server.closeConnection(c)
			}
		}
	}
}

// findOpen is a helper function that tries to find an open by its ID. It returns the status
// to fail the request with, or STATUS_OK if the request may be processed.
func (c *connection) findOpen(ss *session, id []byte, req *smb2.Request) (*open, uint32) {
	fid := binary.LittleEndian.Uint64(id[:8])
	dfid := binary.LittleEndian.Uint64(id[8:16])

	ss.mu.Lock()
	op, found := ss.openTable[fid]
	ss.mu.Unlock()

	if !found || op.durableFileID != dfid {
		if req.GroupID() > 0 {
			op = c.findOpenByGroupID(req.GroupID())
		}

		if op != nil {
			req.SetOpenID(op.id())
		}
	}

	if op == nil {
		return nil, smb2.STATUS_FILE_CLOSED
	}

	return op, c.verifyChannelSequence(op, req)
}

// verifyChannelSequence verifies the channel sequence number of a request that includes a
// FileId, and maintains the outstanding request counters of the Open, which tell the requests
// sent before the client reconnected the channel apart from those sent after it. A request
// that is counted is also remembered, so that the counter it was added to is decremented once
// the response to it goes out. Requests that cannot be counted are failed with
// STATUS_FILE_NOT_AVAILABLE, but only if they modify the file: the client is expected to
// resend those on a healthy channel, while the rest are harmless to process uncounted.
// Dialects older than 3.x have no channel sequence, so nothing is verified or counted.
func (c *connection) verifyChannelSequence(op *open, req *smb2.Request) uint32 {
	if !smb2.Is3X(c.negotiateDialect) {
		return smb2.STATUS_OK
	}

	cs := req.Header().ChannelSequence()
	replay := req.Header().IsFlagSet(smb2.FLAGS_REPLAY_OPERATION)

	op.mu.Lock()

	// The difference is calculated using 16-bit arithmetic, so that it stays correct when
	// the channel sequence of the client wraps around.
	diff := cs - op.channelSequence

	var counted bool
	switch {
	case diff == 0:
		// The request belongs to the same channel as the previous ones. A replayed request
		// may only be counted if none of the requests that preceded the reconnect are
		// still outstanding, because it could otherwise overtake one of them.
		if !replay || op.outstandingPreviousRequestCount == 0 {
			op.outstandingRequestCount++
			counted = true
		}

	case diff <= 0x7FFF:
		// The client has reconnected the channel, so everything counted so far belongs to
		// the previous channel now.
		op.outstandingPreviousRequestCount += op.outstandingRequestCount
		op.channelSequence = cs
		if !replay || op.outstandingPreviousRequestCount == 0 {
			op.outstandingRequestCount = 1
			counted = true
		} else {
			op.outstandingRequestCount = 0
		}

		// Otherwise the channel sequence of the request is older than the one of the Open,
		// which means the request comes from a channel that the client has already given
		// up on. It isn't counted.
	}

	op.mu.Unlock()

	if counted {
		c.mu.Lock()
		c.requestOpens[req.Header().MessageID()] = op
		c.mu.Unlock()
		return smb2.STATUS_OK
	}

	switch req.Header().Command() {
	case smb2.SMB2_WRITE, smb2.SMB2_SET_INFO, smb2.SMB2_IOCTL:
		return smb2.STATUS_FILE_NOT_AVAILABLE
	}

	return smb2.STATUS_OK
}

// releaseOpen decrements the outstanding request counters of the Open that the request
// refers to. It is called when the response to the request is about to be sent: if the
// negotiated dialect belongs to the SMB 3.x family and the ChannelSequence of the request
// equals Open.ChannelSequence, Open.OutstandingRequestCount is decremented, otherwise
// Open.OutstandingPreRequestCount is. Requests that don't carry a FileId, and those whose
// Open could not be resolved, are not counted, so calling this on them does nothing.
// Repeated calls for the same request are a no-op as well, which keeps the interim and the
// final response of an asynchronous command from being counted twice.
func (c *connection) releaseOpen(req *smb2.Request) {
	mid := req.Header().MessageID()
	c.mu.Lock()
	op, found := c.requestOpens[mid]
	delete(c.requestOpens, mid)
	c.mu.Unlock()

	if !found {
		return
	}

	op.mu.Lock()
	defer op.mu.Unlock()

	if smb2.Is3X(c.negotiateDialect) && req.Header().ChannelSequence() == op.channelSequence {
		if op.outstandingRequestCount > 0 {
			op.outstandingRequestCount--
		}
	} else if op.outstandingPreviousRequestCount > 0 {
		op.outstandingPreviousRequestCount--
	}
}

// findOpenByGroupID finds an Open by the group ID of the response.
func (c *connection) findOpenByGroupID(groupID uint64) *open {
	c.mu.Lock()
	resp, found := c.pendingResponses[groupID]
	c.mu.Unlock()
	if !found {
		return nil
	}

	id := resp.OpenID()
	if id == nil {
		return nil
	}

	dfid := binary.LittleEndian.Uint64(id[8:16])
	c.server.mu.Lock()
	op := c.server.globalOpenTable[dfid]
	c.server.mu.Unlock()

	return op
}

// cancelRequest cancels a pending asynchronous request.
func (c *connection) cancelRequest(req *smb2.Request) error {
	cr := smb2.CancelRequest{Request: *req}
	if err := cr.Validate(c.supportsMultiCredit); err != nil {
		return err
	}

	c.mu.Lock()
	ss, found := c.sessionTable[cr.Header().SessionID()]
	c.mu.Unlock()
	if cr.Header().IsFlagSet(smb2.FLAGS_SIGNED) {
		if found {
			// An SMB2_CANCEL request is never answered, so a request that cannot be
			// verified is dropped rather than failed with a status code.
			if err := ss.validateRequest(req, c); err != nil {
				return err
			}
		} else {
			return errSessionNotFound
		}
	}

	// The provided request is an SMB2_CANCEL request; we need to find the target request
	// and the connection that carries it, which need not be this one.
	target, owner := c.findCancelTarget(cr, ss)
	if target == nil {
		return nil
	}

	// If we are cancelling an SMB2_WRITE request, we should abort the upload. An unsigned
	// cancel is not required to name a session that still exists, so the open can only be
	// looked up when one was found.
	if target.Header().Command() == smb2.SMB2_WRITE && ss != nil {
		wr := smb2.WriteRequest{Request: *target}
		var op *open
		id := wr.FileID()
		fid := binary.LittleEndian.Uint64(id[:8])
		dfid := binary.LittleEndian.Uint64(id[8:16])
		ss.mu.Lock()
		op, found := ss.openTable[fid]
		ss.mu.Unlock()
		if found && op.durableFileID == dfid {
			op.cancelUpload()
		}
	}

	if cr.IsEncrypted() {
		target.SetEncrypted(true)
	}

	resp := smb2.NewErrorResponse(target, smb2.STATUS_CANCELLED, 0, nil)
	resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
	if target.Header().IsFlagSet(smb2.FLAGS_ASYNC_COMMAND) {
		resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		resp.Header().SetCreditResponse(0)
		resp.Header().SetAsyncID(target.Header().AsyncID())
	}

	// The request is answered and cleaned up on the connection that carries it, which is
	// where its outstanding request count, its entry in the async command list and its stop
	// channel all live.
	owner.releaseOpen(target)
	owner.server.writeResponse(owner, ss, resp)

	owner.mu.Lock()
	delete(owner.asyncCommandList, target.Header().AsyncID())

	ch, ok := owner.stopChans[target.CancelRequestID()]
	if ok {
		close(ch)
		delete(owner.stopChans, target.CancelRequestID())
	}
	owner.mu.Unlock()

	return nil
}

// findCancelTarget locates the request that the cancel refers to, together with the
// connection that carries it. An asynchronous request is found by its AsyncId, which is
// unique across the server, so every channel of the session is searched: a client is free
// to cancel over a channel other than the one the request was sent on. A synchronous one is
// found by its MessageId, which only means anything inside the command sequence window of a
// single connection, so that search never leaves the connection the cancel arrived on.
func (c *connection) findCancelTarget(cr smb2.CancelRequest, ss *session) (*smb2.Request, *connection) {
	if !cr.Header().IsFlagSet(smb2.FLAGS_ASYNC_COMMAND) {
		mid := cr.Header().MessageID()
		c.mu.Lock()
		defer c.mu.Unlock()

		for _, r := range c.asyncCommandList {
			if r != nil && r.Header().MessageID() == mid {
				return r, c
			}
		}

		return nil, nil
	}

	// The connection the cancel arrived on is searched first: it is the one that carries the
	// request in all but the unusual case.
	connections := []*connection{c}
	if ss != nil && smb2.Is3X(c.negotiateDialect) {
		ss.mu.Lock()
		for _, ch := range ss.channelList {
			if ch.connection != c {
				connections = append(connections, ch.connection)
			}
		}
		ss.mu.Unlock()
	}

	aid := cr.Header().AsyncID()
	for _, conn := range connections {
		conn.mu.Lock()
		r, found := conn.asyncCommandList[aid]
		conn.mu.Unlock()
		if found && r != nil {
			return r, conn
		}
	}

	return nil, nil
}

// isStale returns true if the connection hasn't been used for a certain amount of time.
// This is done to drop unused connections.
func (c *connection) isStale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If there are no sessions on the connection, check the connection's creation time.
	if len(c.sessionTable) == 0 && time.Since(c.creationTime) > staleThreshold {
		return true
	}

	// Check each individual session: if at least one session is being used, the connection is alive.
	for _, ss := range c.sessionTable {
		ss.mu.Lock()
		idle := ss.idleTime
		ss.mu.Unlock()

		if time.Since(idle) <= staleThreshold {
			return false
		}
	}

	return true
}

// createFile carries out an SMB2_CREATE request that has passed its preliminary checks.
// It is kept apart from the dispatcher because a create that has to revoke somebody else's
// oplock first can only finish once the break is over, which is too long to keep the
// connection waiting, so it runs on a goroutine of its own.
func (c *connection) createFile(req *smb2.Request, cr smb2.CreateRequest, ss *session, tc *treeConnect, acc stores.Account, contexts map[uint32][]byte, path string) smb2.GenericResponse {
	ctx, cancel := context.WithCancel(context.Background())
	var info client.ObjectInfo
	var result uint32
	var restored bool
	var op *open
	var err error
	if tc.share.name == "ipc$" { // A named pipe is being created
		switch strings.ToLower(path) {
		case "srvsvc", "lsarpc", "mdssvc":
			info = client.ObjectInfo{
				Key: path,
			}
			result = smb2.FILE_OPENED
		default: // Other named pipes are not supported
			cancel()
			c.server.mu.Lock()
			c.server.stats.PermErrors++
			c.server.mu.Unlock()
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp
		}
	} else {
		info, err = tc.client.Object(ctx, acc, path)
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			cancel()
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_IO_TIMEOUT, 0, nil)
			return resp
		}

		switch cr.CreateDisposition() {
		case smb2.FILE_SUPERSEDE:
			if err != nil {
				tc.mu.Lock()
				op, restored = tc.persistedOpens[path]
				tc.mu.Unlock()
				if !restored {
					info = client.ObjectInfo{
						Key:        "/" + path,
						CreatedAt:  time.Now(),
						ModifiedAt: time.Now(),
					}
					result = smb2.FILE_CREATED
				} else {
					result = smb2.FILE_SUPERSEDED
				}
			} else {
				result = smb2.FILE_SUPERSEDED
			}
		case smb2.FILE_OPEN:
			if err != nil {
				tc.mu.Lock()
				op, restored = tc.persistedOpens[path]
				tc.mu.Unlock()
				if !restored {
					cancel()
					resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
					return resp
				} else {
					result = smb2.FILE_OPENED
				}
			} else {
				result = smb2.FILE_OPENED
			}
		case smb2.FILE_CREATE:
			if err != nil {
				info = client.ObjectInfo{
					Key:        "/" + path,
					CreatedAt:  time.Now(),
					ModifiedAt: time.Now(),
				}
				result = smb2.FILE_CREATED
				if cr.CreateOptions()&smb2.FILE_DIRECTORY_FILE > 0 { // Make a new directory
					info.Key += "/"
					if err := tc.client.MakeDirectory(ctx, acc, path); err != nil {
						cancel()
						if errors.Is(err, stores.ErrDirectoryExists) {
							resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_COLLISION, 0, nil)
							return resp
						} else {
							log.Printf("Couldn't create directory %s: %v\n", path, err)
							resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
							return resp
						}
					}
				}
			} else {
				cancel()
				resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_COLLISION, 0, nil)
				return resp
			}
		case smb2.FILE_OPEN_IF:
			if err != nil {
				tc.mu.Lock()
				op, restored = tc.persistedOpens[path]
				tc.mu.Unlock()
				if !restored {
					info = client.ObjectInfo{
						Key:        "/" + path,
						CreatedAt:  time.Now(),
						ModifiedAt: time.Now(),
					}
					result = smb2.FILE_CREATED
					if cr.CreateOptions()&smb2.FILE_DIRECTORY_FILE > 0 { // Make a new directory
						info.Key += "/"
						if err := tc.client.MakeDirectory(ctx, acc, path); err != nil {
							cancel()
							if errors.Is(err, stores.ErrDirectoryExists) {
								resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_COLLISION, 0, nil)
								return resp
							} else {
								log.Printf("Couldn't create directory %s: %v\n", path, err)
								resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
								return resp
							}
						}
					}
				} else {
					result = smb2.FILE_OPENED
				}
			} else {
				result = smb2.FILE_OPENED
			}
		case smb2.FILE_OVERWRITE:
			if err != nil {
				tc.mu.Lock()
				op, restored = tc.persistedOpens[path]
				tc.mu.Unlock()
				if !restored {
					cancel()
					resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
					return resp
				} else {
					result = smb2.FILE_OVERWRITTEN
				}
			} else {
				result = smb2.FILE_OVERWRITTEN
			}
		case smb2.FILE_OVERWRITE_IF:
			if err != nil {
				tc.mu.Lock()
				op, restored = tc.persistedOpens[path]
				tc.mu.Unlock()
				if !restored {
					info = client.ObjectInfo{
						Key:        "/" + path,
						CreatedAt:  time.Now(),
						ModifiedAt: time.Now(),
					}
					result = smb2.FILE_CREATED
					if cr.CreateOptions()&smb2.FILE_DIRECTORY_FILE > 0 { // Make a new directory
						info.Key += "/"
						if err := tc.client.MakeDirectory(ctx, acc, path); err != nil {
							cancel()
							if errors.Is(err, stores.ErrDirectoryExists) {
								resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_COLLISION, 0, nil)
								return resp
							} else {
								log.Printf("Couldn't create directory %s: %v\n", path, err)
								resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
								return resp
							}
						}
					}
				} else {
					result = smb2.FILE_OVERWRITTEN
				}
			} else {
				result = smb2.FILE_OVERWRITTEN
			}
		}
	}

	if restored { // This file has already been "created", "restore" it
		cancel()
		c.server.restoreOpen(op, c)
	} else {
		op = ss.registerOpen(cr, c, tc, info, ctx, cancel)
		if op == nil {
			cancel()
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp
		}
	}

	op.mu.Lock()
	attr := op.fileAttributes
	op.mu.Unlock()
	if result == smb2.FILE_CREATED && attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 { // Persist the file for any future requests
		tc.mu.Lock()
		tc.persistedOpens[path] = op
		tc.mu.Unlock()
	}

	if result == smb2.FILE_SUPERSEDED || result == smb2.FILE_OVERWRITTEN {
		op.mu.Lock()
		op.size = 0
		op.allocated = 0
		op.lastModified = time.Now()
		op.mu.Unlock()
	}

	// The oplock is decided last, once the open exists and counts among those on the file.
	// Whatever the client asked for, the level it is told about is the level it gets.
	oplockLevel := uint8(smb2.OPLOCK_LEVEL_NONE)
	if oplockEligible(cr.RequestedOplockLevel(), tc, attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0) {
		oplockLevel = c.server.grantOplock(op, cr.RequestedOplockLevel(), tc, path)
	}

	respContexts := make(map[uint32][]byte)
	for id, ctx := range contexts {
		switch id {
		case smb2.CREATE_EA_BUFFER: // renterd doesn't support extended file attributes, so why should we?
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_EAS_NOT_SUPPORTED, 0, nil)
			return resp
		case smb2.CREATE_QUERY_MAXIMAL_ACCESS_REQUEST:
			respContexts[id] = smb2.HandleCreateQueryMaximalAccessRequest(ctx, op.lastModified, op.grantedAccess)
		case smb2.CREATE_QUERY_ON_DISK_ID:
			respContexts[id] = smb2.HandleCreateQueryOnDiskID(op.handle, tc.volumeID)
		case smb2.CREATE_ALLOCATION_SIZE: // The file is about to be uploaded, we just got its size
			op.mu.Lock()
			op.allocated = binary.LittleEndian.Uint64(ctx)
			op.mu.Unlock()
		case smb2.SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2:
			// The handle is to survive the loss of the connection. A directory has no
			// work in progress worth preserving, and a named pipe has nothing to go
			// back to, so only files are granted durability.
			dh, ok := smb2.ParseDurableHandleRequestV2(ctx)
			if !ok || tc.share.name == "ipc$" {
				break
			}
			op.mu.Lock()
			isDir := op.fileAttributes&smb2.FILE_ATTRIBUTE_DIRECTORY > 0
			op.mu.Unlock()
			if isDir {
				break
			}
			respContexts[id] = smb2.HandleCreateDurableHandleRequestV2(op.grantDurability(dh))
		}
	}

	resp := &smb2.CreateResponse{}
	resp.FromRequest(cr)
	op.mu.Lock()
	resp.Generate(
		oplockLevel,
		result,
		op.size,
		op.allocated,
		op.lastModified,
		op.fileAttributes&smb2.FILE_ATTRIBUTE_DIRECTORY > 0,
		op.fileID,
		op.durableFileID,
		respContexts,
	)
	op.mu.Unlock()

	gid := req.GroupID()
	if gid > 0 {
		resp.SetOpenID(op.id())
	}

	return resp
}
