package main

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"runtime/debug"
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

	// connectionScavengeTimeout is how long a connection is given to get as far as an
	// authenticated session before it is dropped. Windows uses the same value.
	connectionScavengeTimeout = 45 * time.Second

	// connectionScavengeInterval is how often the connections that never got that far are
	// looked for. Windows runs this timer every 45 seconds as well.
	connectionScavengeInterval = 45 * time.Second
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
	commandSequenceWindow map[uint64]struct{}

	// interimSent is what an asynchronous command waits on before it answers, by the message ID of
	// the request. Both responses travel over the one queue, so the order of them is settled the
	// moment the interim is put on it - but the work may be finished before the interim has been
	// handed over at all, and then the client is given the answer to a request it has never been
	// told is pending. What it makes of the interim that follows is nothing good: a macOS client
	// gives up on the file with "RPC struct is bad", which is its way of saying it was sent a
	// message it cannot place.
	interimSent map[uint64]chan struct{}

	// creditsGranted is how many credits each request in hand was granted, so that the response
	// tells the client exactly what the window was opened by. Told more than that, the client sends
	// beyond the window and the message it sends is one the server has to turn away; told less, it
	// holds back for credits it has already been given. The first response to carry the grant takes
	// it, which for a command answered with an interim response is that interim: the final response
	// of an asynchronous command grants nothing.
	creditsGranted   map[uint64]uint16
	requestList      map[uint64]*smb2.Request
	pendingResponses map[uint64]smb2.GenericResponse

	// chainRemaining is how many requests of a compound chain are still to be dealt with, by the
	// group ID of the chain. The chain goes out when the count reaches zero rather than when its
	// last request is answered: a request turned away before it ever reaches the dispatcher is
	// dealt with all the same, and a chain waiting for one of those waits for as long as the
	// connection lives, with everything already assembled for it unsent.
	chainRemaining             map[uint64]int
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

	// wakeChan tells the dispatcher that a request has been put in the queue. It holds one signal:
	// the dispatcher empties the queue before it waits again.
	wakeChan chan struct{}

	// stopChans holds the channel that tells the work behind a request to give up, keyed by the
	// message ID of that request.
	stopChans map[uint64]chan struct{}

	// requestOpens maps the message ID of an in-flight request that carries a FileId
	// to the Open it refers to.
	requestOpens map[uint64]*open
}

// maxSequenceWindow is the most message IDs a client may hold at once, which is the most credits it
// may be granted: it may have exactly as many requests outstanding as it has credits, and every credit
// is an ID this server has to hold open for it.
const maxSequenceWindow = 65536

// grantCredits increases the number of credits available to the client, up to what it asked for and
// no further than the window allows, and returns how many it granted.
func (c *connection) grantCredits(mid uint64, numCredits, charge uint16) (uint16, error) {
	if charge == 0 {
		charge = 1
	}

	if room := maxSequenceWindow - len(c.commandSequenceWindow); room < int(numCredits) {
		numCredits = uint16(max(room, 0))
	}
	if numCredits < charge {
		numCredits = charge
	}

	// Find the maximal message ID that a request may come in with.
	max, _ := utils.FindMaxKey(c.commandSequenceWindow)
	if max == 0 { // Window empty or only containing zero
		max = mid
	}

	if uint64(numCredits) > math.MaxUint64-max {
		return 0, errCommandSecuenceWindowExceeded
	}

	var i uint64
	for i = 0; i < uint64(numCredits); i++ {
		c.commandSequenceWindow[max+i+1] = struct{}{}
	}

	return numCredits, nil
}

// acceptRequest processes an SMB message into one or more requests and puts them in the queue.
func (c *connection) acceptRequest(msg []byte) error {
	if uint64(len(msg)) > c.maxTransactSize+256 {
		return errLongRequest
	}

	if len(msg) < smb2.SMB2HeaderSize {
		return smb2.ErrWrongLength
	}

	// Check for encryption.
	var tsid uint64
	var size uint32
	if smb2.Header(msg).ProtocolID() == smb2.PROTOCOL_SMB2_ENCRYPTED {
		if c.cipherID == 0 {
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
		if msg == nil {
			return errDecryptionError
		}
		if uint32(len(msg)) != size {
			return smb2.ErrWrongLength
		}
		if len(msg) < smb2.SMB2HeaderSize {
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

	reqs, err := smb2.GetRequests(msg, tsid, compressed)
	if err != nil {
		return err
	}

	// The whole of a chain is counted before any of it is dealt with, so that the count cannot
	// reach zero on a chain whose later requests have yet to be read out of the message.
	c.mu.Lock()
	for _, req := range reqs {
		if gid := req.GroupID(); gid > 0 {
			c.chainRemaining[gid]++
		}
	}
	c.mu.Unlock()

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
			c.grantCredits(mid, 1, 1) // Grant just one credit
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

			if req.Header().Command() == smb2.SMB2_CANCEL { // SMB2_CANCEL requests are handled separately
				// A cancel neither spends a sequence number nor earns a credit ([MS-SMB2] 3.3.1.1,
				// 3.3.4.1.2, 3.3.5.16): it reuses the message ID of the request it cancels, which
				// has been retired already, or carries zero alongside an async ID. So the window is
				// left exactly as it stands - granting against a cancel opened an ID per cancel that
				// nothing would ever close, and retiring its ID would take one the client still has.
				if err := c.cancelRequest(req); err != nil {
					log.Printf("Couldn't cancel request %d:, %v\n", req.Header().Command(), err)
				}

				if chain := c.chainMemberDone(req.GroupID()); chain != nil {
					// A cancel that opens a chain is the one request of it that never looks the
					// session up below, so the chain would go out for nobody.
					if ss == nil {
						c.mu.Lock()
						ss = c.sessionTable[req.Header().SessionID()]
						c.mu.Unlock()
					}

					c.server.writeResponse(c, ss, chain)
				}

				continue
			}

			credits := max(req.Header().CreditCharge(), req.Header().CreditRequest()) // Grant whatever the CreditRequest is. If CreditCharge is greater, grant that much.
			if credits == 0 {                                                         // The number of credits cannot be zero
				credits = 1
			}

			c.mu.Lock()
			granted, _ := c.grantCredits(mid, credits, req.Header().CreditCharge())
			c.creditsGranted[mid] = granted
			c.mu.Unlock()
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
			switch err := ss.validateRequest(req, c); {
			case err == nil:

			case errors.Is(err, errNoSigningKey):
				// The key required to verify the signature is not available. The request
				// must be failed and not processed any further.
				rejectStatus = smb2.STATUS_NOT_SUPPORTED

			case errors.Is(err, errUnsignedRequest):
				// The session requires signing and the request carries none. It is refused
				// rather than the connection being torn down: nothing here says the client
				// is an impostor, only that what it sent cannot be acted on.
				rejectStatus = smb2.STATUS_ACCESS_DENIED

			default:
				// A signature that does not match is the one case that ends the connection,
				// since what arrived cannot be trusted to have come from the client at all.
				return err
			}
		}

		// Request processed; this message ID is not allowed anymore.
		c.retireMessageID(mid, req.Header().CreditCharge())

		if rejectStatus != 0 {
			// The response carries a copy of the request header, so the signature of the
			// client has to be cleared: the response cannot be signed, since the key to
			// sign it with is the one that couldn't be found.
			resp := smb2.NewErrorResponse(*req, rejectStatus, 0, nil)
			resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
			resp.Header().WipeSignature()
			c.server.writeResponse(c, ss, resp)

			// The request is dealt with, however it was dealt with, so the chain it belonged to
			// is no longer waiting on it.
			if chain := c.chainMemberDone(req.GroupID()); chain != nil {
				c.server.writeResponse(c, ss, chain)
			}

			continue
		}

		// Put request in the queue.
		c.mu.Lock()
		c.requestList[mid] = req
		c.mu.Unlock()

		c.wake()
	}

	return nil
}

// validPath reports whether a path a request names is one the server may act on. The components
// have to resolve inside the share ([MS-SMB2] 3.3.5.9): a name that walks out of it, or that names
// the same file two ways, is refused rather than handed to the backend, where the key is a string
// and ".." is a segment like any other. The empty path is the root of the share, which a client
// opens to ask about the volume.
func validPath(path string) bool {
	if path == "" {
		return true
	}

	if strings.HasPrefix(path, "/") {
		return false
	}

	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".", "..":
			return false
		}
	}

	return true
}

// windsDownSession reports whether the command is one that a session serves even when nobody has
// authenticated over it yet, or nobody does any longer ([MS-SMB2] 3.3.5.2.9).
func windsDownSession(command uint16) bool {
	switch command {
	case smb2.SMB2_LOGOFF, smb2.SMB2_CLOSE, smb2.SMB2_LOCK:
		return true
	}

	return false
}

// sessionFor resolves the session a request names, and returns the status to fail the request with
// ([MS-SMB2] 3.3.5.2.9). The session table holds a session from the moment the setup starts, and the
// client is told its ID in the very first response, so being in the table is not the same as having
// authenticated: until it has, the session serves only the commands that wind it down.
func (c *connection) sessionFor(req *smb2.Request) (*session, uint32) {
	c.mu.Lock()
	ss, found := c.sessionTable[req.Header().SessionID()]
	c.mu.Unlock()

	if !found {
		return nil, smb2.STATUS_USER_SESSION_DELETED
	}

	if windsDownSession(req.Header().Command()) {
		return ss, smb2.STATUS_OK
	}

	switch ss.stateNow() {
	case sessionInProgress:
		// The spec leaves the code to the implementation here.
		return ss, smb2.STATUS_INVALID_PARAMETER
	case sessionExpired:
		return ss, smb2.STATUS_NETWORK_SESSION_EXPIRED
	}

	return ss, smb2.STATUS_OK
}

// retireMessageID takes the IDs a request spent back out of the command sequence window.
func (c *connection) retireMessageID(mid uint64, charge uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.negotiateDialect == smb2.SMB_DIALECT_202 || !c.supportsMultiCredit {
		delete(c.commandSequenceWindow, mid)
		return
	}

	// As many IDs as the request charged for, and a request that charges nothing still
	// costs the one it was sent under: granting reads a charge of zero as one, which is
	// what a client sends for anything that fits in a single credit, and a window handing
	// an ID out without taking one back grows for as long as the connection lives.
	charge = max(charge, 1)
	var count uint16
	m, _ := utils.FindMaxKey(c.commandSequenceWindow)
	for i := mid; i <= m && count < charge; i++ {
		if _, found := c.commandSequenceWindow[i]; found {
			delete(c.commandSequenceWindow, i)
			count++
		}
	}
}

// wake tells the dispatcher there is something to pick up, without waiting for it to listen.
func (c *connection) wake() {
	select {
	case c.wakeChan <- struct{}{}:
	default:
	}
}

// dialectName is how a negotiated dialect is written down: the spelling the spec uses when a
// rule names a dialect, and the one the connection reports.
func dialectName(dialect uint16) string {
	switch dialect {
	case smb2.SMB_DIALECT_202:
		return "2.0.2"
	case smb2.SMB_DIALECT_21:
		return "2.1"
	case smb2.SMB_DIALECT_30:
		return "3.0"
	case smb2.SMB_DIALECT_302:
		return "3.0.2"
	case smb2.SMB_DIALECT_311:
		return "3.1.1"
	default:
		return "Unknown"
	}
}

// refusesChannel reports whether the Channel field of an SMB2_READ or SMB2_WRITE request names
// something this server cannot do, so that the request has to be failed with
// STATUS_INVALID_PARAMETER ([MS-SMB2] 3.3.5.12, 3.3.5.13).
func (c *connection) refusesChannel(channel uint32) bool {
	return smb2.Is3X(c.negotiateDialect) && channel != smb2.SMB2_CHANNEL_NONE
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
		c.grantCredits(nr.Header().MessageID(), 1, 1) // Grant just one credit

		// A legacy negotiate settles no more than which of the two answers to give, so there is no
		// client capability to weigh: what goes back is the server's own set, narrowed to the
		// dialect it names.
		c.serverCapabilities = c.server.serverCapabilities & smb2.DialectCapabilities(dialect)

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
		c.dialect = dialectName(c.negotiateDialect)
		if c.negotiateDialect == smb2.SMB_DIALECT_311 {
			c.clientDialects = nr.Dialects()
		}

		// What this client is told: everything the server can do, narrowed to what its dialect
		// allows. Encryption drops out here for 3.1.1, which settles a cipher in a negotiate
		// context further down instead of carrying the capability.
		c.serverCapabilities = c.server.serverCapabilities & smb2.DialectCapabilities(c.negotiateDialect)

		// Channels are the one capability that also turns on what the client asked for: there is
		// no use offering them to a client that never said it could bind one.
		if c.clientCapabilities&smb2.GLOBAL_CAP_MULTI_CHANNEL == 0 {
			c.serverCapabilities &^= smb2.GLOBAL_CAP_MULTI_CHANNEL
		}

		// The cipher of the connection is what says whether it encrypts at all, and zero says it
		// does not. Before 3.1.1 there is nothing to agree on - a dialect that carries the
		// encryption capability encrypts with AES-128-CCM and no other - so settling it here means
		// the same field answers the question whichever dialect asked it.
		if c.serverCapabilities&smb2.GLOBAL_CAP_ENCRYPTION != 0 {
			c.cipherID = smb2.AES_128_CCM
		}

		if nr.SecurityMode()&smb2.NEGOTIATE_SIGNING_REQUIRED > 0 {
			c.shouldSign = true
		}

		// Multi-credit is the other half of large MTU: a request worth more than a single credit
		// is how a read or a write grows past 64 KiB, so the dialect that has one has the other.
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
			// The cipher this dialect settles on, which is none if the client offered nothing the
			// server has. The capability is deliberately left alone: it was not advertised on
			// 3.1.1 and must not be taken up afterwards, because the capabilities of the
			// connection are answered back to the client by FSCTL_VALIDATE_NEGOTIATE_INFO and a
			// pair that disagrees is what that request exists to catch.
			if ciphers != nil {
				c.cipherID = utils.FirstMatch(ciphers, supportedEncryptionAlgos)
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
			if smb2.Is3X(c.negotiateDialect) && c.server.encryptData && c.cipherID != 0 {
				ss.signingRequired = false
				ss.encryptData = true
			} else {
				ss.signingRequired = true
				ss.encryptData = false
			}
			ss.activate()

			ss.mu.Lock()
			ss.idleTime = time.Now()
			ss.mu.Unlock()

			token = spnego.FinalNegTokenResp
			if smb2.Is3X(c.negotiateDialect) {
				c.server.mu.Lock()
				cl, ok := c.server.globalClientTable[[16]byte(c.clientGuid)]
				c.server.mu.Unlock()
				if !ok {
					cl = &smbClient{dialect: c.negotiateDialect}
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
		if ss.stateNow() == sessionValid {
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(tcr, status, 0, nil)
			return resp, ss, nil
		}

		// Validate signature or encryption. An anonymous or guest session is exempt: it holds no
		// key to sign with, so demanding a signature of it is demanding the impossible ([MS-SMB2]
		// 3.3.5.7). The session has to be in hand before that can be asked, which is why this
		// follows the lookup rather than leading it.
		if c.negotiateDialect == smb2.SMB_DIALECT_311 && !ss.isAnonymous && !ss.isGuest {
			if !tcr.Header().IsFlagSet(smb2.FLAGS_SIGNED) && !tcr.IsEncrypted() {
				if c.server.debug {
					log.Println("Unsigned or unencrypted SMB2_TREE_CONNECT request")
				}
				return nil, nil, smb2.ErrWrongSecurity
			}
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(tdr, status, 0, nil)
			return resp, ss, nil
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(cr, status, 0, nil)
			return resp, ss, nil
		}

		ss.mu.Lock()
		ss.idleTime = time.Now()
		tc, found := ss.treeConnectTable[cr.Header().TreeID()]
		ss.mu.Unlock()

		if !found {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		// A pipe is only opened over an anonymous session if that pipe allows anonymous callers,
		// and this server offers none that do ([MS-SMB2] 3.3.5.9). It is refused as the permission
		// matter it is, ahead of the account lookup below, which would otherwise answer that the
		// session is gone when what is missing is the right to open the pipe.
		if tc.share.name == "ipc$" && ss.isAnonymous {
			c.server.mu.Lock()
			c.server.stats.PermErrors++
			c.server.mu.Unlock()
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, ss, nil
		}

		contexts, err := cr.CreateContexts()
		if err != nil {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		// Extended attributes are not supported, and a create asking for them is refused here:
		// nothing has been made yet, so the refusal leaves nothing behind.
		if _, found := contexts[smb2.CREATE_EA_BUFFER]; found {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_EAS_NOT_SUPPORTED, 0, nil)
			return resp, ss, nil
		}

		// A reconnect claims a handle that already exists, so it is answered from the open
		// that was kept aside rather than by resolving the path and going to the backend.
		if ctx, found := contexts[smb2.SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2]; found {
			// Claiming a handle and asking for a new one at the same time is a contradiction.
			if _, both := contexts[smb2.SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2]; both {
				resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}

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

			// The reconnect is a create like any other, and the contexts it carries are
			// answered the same way, out of the open being handed back. The lease is the
			// exception: it was released when the connection was lost, precisely so that
			// others could get at the file in the meantime, so a client that asks is told
			// it holds nothing rather than left to guess — a cache from before a gap the
			// server cannot vouch for must not be blessed.
			_, _, _, modified, _ := op.file.stat()
			op.mu.Lock()
			access := op.grantedAccess
			handle := op.handle
			op.mu.Unlock()

			respContexts := make(map[uint32][]byte)
			for id, ctx := range contexts {
				switch id {
				case smb2.CREATE_QUERY_MAXIMAL_ACCESS_REQUEST:
					respContexts[id] = smb2.HandleCreateQueryMaximalAccessRequest(ctx, modified, access)
				case smb2.CREATE_QUERY_ON_DISK_ID:
					respContexts[id] = smb2.HandleCreateQueryOnDiskID(handle, tc.volumeID)
				}
			}

			if lr := c.leaseRequest(cr, contexts); lr != nil {
				respContexts[smb2.CREATE_REQUEST_LEASE] = smb2.HandleCreateRequestLease(*lr, smb2.SMB2_LEASE_NONE, 0)
			}

			size, allocated, _, modifiedNow, attributes := op.file.stat()
			resp := &smb2.CreateResponse{}
			resp.FromRequest(cr)
			op.mu.Lock()
			resp.Generate(
				smb2.OPLOCK_LEVEL_NONE,
				smb2.FILE_OPENED,
				size,
				allocated,
				modifiedNow,
				attributes&smb2.FILE_ATTRIBUTE_DIRECTORY > 0,
				op.fileID,
				op.durableFileID,
				respContexts,
			)
			op.mu.Unlock()
			req.SetOpenID(op.id())

			return resp, ss, nil
		}

		path := strings.ReplaceAll(cr.Filename(), "\\", "/")
		if !validPath(path) {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
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

		// A lease the client already holds on this file is not something the create has to
		// break: every open that client has under the same key shares it.
		lr := c.leaseRequest(cr, contexts)

		// A create bearing the GUID of one that has already been answered is either a replay of
		// it, which is answered from the open the first attempt made, or a client reusing a GUID
		// it should not. Either way the file is never opened a second time.
		if resp, handled := c.replayCreate(cr, ss, tc, contexts, lr); handled {
			return resp, ss, nil
		}

		var own *lease
		if lr != nil {
			own = c.server.findLease([16]byte(c.clientGuid), lr.LeaseKey)
		}

		// Whoever else holds an oplock or a lease on this file has to give it up before the
		// create may look at the file at all, because the holder may be sitting on writes it
		// has not sent yet. A create that only means to read the file lets the holders keep
		// their read caches; one that means to change it does not.
		sharedOK := !createChangesFile(cr)

		// Only a promise that has to be acknowledged is worth waiting for, and the waiting
		// cannot happen here: a connection serves its requests one at a time, and the
		// acknowledgment being waited for may be on its way in over this very connection. A
		// read cache has no level below it to argue about, so its holder is simply told.
		// Who is asking is what says whose promises stand: a promise the client already holds
		// covers the file it is opening again, and it has nothing to give up.
		asking := c.askedBy(own, lr)

		if !c.server.needsBreakWait(tc.share, path, nil, asking, sharedOK) {
			c.server.tellHoldersOn(tc.share, path, nil, asking, sharedOK)
			resp, _ := c.createFile(req, cr, ss, tc, acc, contexts, path, lr)
			return resp, ss, nil
		}

		// The create is answered with an interim response and finished on a goroutine of its own.
		aid := make([]byte, 8)
		frand.Read(aid)
		asyncID := binary.LittleEndian.Uint64(aid)

		// The request carries the ID it is now known by, and says that it is being worked on
		// asynchronously. A cancel names the request by that ID and cleans up by it, so a request
		// left unmarked is one a cancel answers without ever finding the work behind it.
		req.Header().SetAsyncID(asyncID)
		req.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		c.mu.Lock()
		c.asyncCommandList[asyncID] = req
		c.mu.Unlock()

		interim := c.expectInterim(req.Header().MessageID())
		go func() {
			defer c.recoverConnection("creating a file")

			c.server.breakHoldersOn(tc.share, path, nil, asking, sharedOK)

			resp, made := c.createFile(req, cr, ss, tc, acc, contexts, path, lr)

			c.awaitInterim(interim)

			finalAsync(resp, asyncID)

			if !c.claimAnswer(asyncID, resp.Header().MessageID()) {
				// The client gave up on the create and has been answered. Whatever the file was
				// opened as, it can never be told which handle it got, so the handle is closed
				// here rather than left holding the file until the session goes.
				if made != nil {
					c.server.closeOpen(made)
				}
				c.releaseOpen(req)

				return
			}

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
		// The oplock and the lease acknowledgment share this command and are told apart by
		// their structure size alone.
		lbr := smb2.LeaseBreakRequest{Request: *req}
		if lbr.Validate(c.supportsMultiCredit) == nil {
			ss, status := c.sessionFor(req)
			if status != smb2.STATUS_OK {
				resp := smb2.NewErrorResponse(lbr, status, 0, nil)
				return resp, ss, nil
			}

			ss.mu.Lock()
			ss.idleTime = time.Now()
			ss.mu.Unlock()

			if len(c.clientGuid) != 16 {
				resp := smb2.NewErrorResponse(lbr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
				return resp, ss, nil
			}

			status, left := c.server.acknowledgeLeaseBreak([16]byte(c.clientGuid), lbr.LeaseKey(), lbr.LeaseState())
			if status != smb2.STATUS_OK {
				resp := smb2.NewErrorResponse(lbr, status, 0, nil)
				return resp, ss, nil
			}

			resp := &smb2.LeaseBreakResponse{}
			resp.FromRequest(lbr)
			resp.Generate(lbr.LeaseKey(), left)

			return resp, ss, nil
		}

		obr := smb2.OplockBreakRequest{Request: *req}
		if err := obr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrWrongFormat) {
				// Neither acknowledgment: the structure size belongs to nothing the server
				// ever sends a break for.
				resp := smb2.NewErrorResponse(obr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_OPLOCK_BREAK request:", err)
			return nil, nil, err
		}

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(obr, status, 0, nil)
			return resp, ss, nil
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

		status, left := op.acknowledgeOplockBreak(obr.OplockLevel())
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(obr, status, 0, nil)
			return resp, ss, nil
		}

		// The response tells the client what it is left holding.
		resp := &smb2.OplockBreakResponse{}
		resp.FromRequest(obr)
		resp.Generate(left, id)

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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(cr, status, 0, nil)
			return resp, ss, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, ss, nil
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

		// A close is what finishes an upload, so a close whose upload fails is a file that was not
		// stored. The open is torn down all the same - the handle is gone either way - but the
		// answer says so, because a client told its close succeeded believes the file it just
		// wrote is on the share.
		attr := op.file.attributesNow()
		op.mu.Lock()
		co := op.createOptions
		path := op.pathName
		op.mu.Unlock()
		deleteOnClose := co&smb2.FILE_DELETE_ON_CLOSE > 0

		var unsaved bool
		if pu := op.file.uploadNow(); pu != nil { // This SMB2_CLOSE request is a sign for us to flush any active multipart upload
			// Whichever of them comes last is what the file is: a save after a deletion puts the
			// file back, and a deletion after a save takes it away. What that leaves the close to
			// decide is whether this upload is still anybody's to store.
			switch {
			case deleteOnClose && op.file.lastHandle():
				// Nothing else is on the file, so the upload holds this client's own writing and
				// the client has asked for the file to go. Storing it first is an upload of a file
				// that is deleted the moment it lands, and one more thing that can fail on the way
				// out: told its close failed, a client keeps the handle, and a handle it will not
				// let go of is what stands between it and disconnecting the share.
				op.cancelUpload()

			case deleteOnClose:
				// Somebody else is still on the file, and the upload is the file's: what is in it
				// may be their writing, and their close is what stores it. Storing it here would
				// leave the deletion below to take their file away again, and calling it off would
				// throw their save out. The deletion is what happens now; if their save comes after
				// it, the file comes back as they wrote it.

			default:
				if err := op.flush(); err != nil {
					op.cancelUpload()
					log.Println("Error completing write:", err)
					unsaved = true
				}
			}
		}

		if deleteOnClose { // Delete the file or directory
			isDir := attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0

			// This is the moment the directory has to be empty, whatever it was when the deletion
			// was asked for. The answer given then may have gone stale - anything may have been
			// put in the directory since - and the create options are a second way to ask that
			// never went past the emptiness check at all. Deleting a directory that has filled up
			// takes the entry away and leaves what is inside it behind, reachable by nobody.
			//
			// The close succeeds either way: it is the deletion that does not happen. That is what
			// a local file system does with a delete-on-close it cannot honour, and there is
			// nowhere left to report it by the time the handle is going.
			deleting := true
			if isDir {
				empty, err := tc.client.IsEmpty(op.ctx, acc, path+"/")
				if err != nil {
					log.Printf("Error listing directory contents on %s: %v", path, err)
					deleting = false
				} else if !empty {
					log.Printf("Not deleting directory %s: it is no longer empty", path)
					deleting = false
				}
			}

			if deleting {
				tc.forgetPersistedFile(path)

				// A file that was created and never written to is not in the store at all:
				// nothing empty can be uploaded, so the entry taken off the table above was the
				// whole of it. The backend having nothing to delete is the expected end of that,
				// not a failure, and reporting it as one says the server could not do what was
				// asked when it has just done it.
				err := tc.client.Delete(op.ctx, acc, path, isDir)
				if err != nil && !errors.Is(err, stores.ErrNotFound) {
					log.Printf("Error deleting object %s: %v", path, err)
				}
			}
		}

		c.server.closeOpen(op)
		c.completeWatches(ss, id)

		if unsaved {
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_UNEXPECTED_NETWORK_ERROR, 0, nil)
			return resp, ss, nil
		}

		closeSize, closeAllocated, _, closeModified, closeAttributes := op.file.stat()
		resp := &smb2.CloseResponse{}
		resp.FromRequest(cr)
		resp.Generate(closeModified, closeSize, closeAllocated, closeAttributes)

		return resp, ss, nil

	// A flush waits for what the client has sent to be taken in, and does not store it.
	case smb2.SMB2_FLUSH:
		fr := smb2.FlushRequest{Request: *req}
		if err := fr.Validate(c.supportsMultiCredit); err != nil {
			if errors.Is(err, smb2.ErrInvalidParameter) {
				resp := smb2.NewErrorResponse(fr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, nil, nil
			}
			log.Println("Invalid SMB2_FLUSH request:", err)
			return nil, nil, err
		}

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(fr, status, 0, nil)
			return resp, ss, nil
		}

		if op, status := c.findOpen(ss, fr.FileID(), req); status == smb2.STATUS_OK {
			op.file.waitForWrites()
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(rr, status, 0, nil)
			return resp, ss, nil
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

		// A read on a named pipe cannot ask for the data to go unbuffered, whichever pipe it is
		// ([MS-SMB2] 3.3.5.12). The share is what says the handle is on one.
		if (c.negotiateDialect == smb2.SMB_DIALECT_302 || c.negotiateDialect == smb2.SMB_DIALECT_311) &&
			op.treeConnect.share.shareType == smb2.SHARE_TYPE_PIPE &&
			rr.Flags()&smb2.READFLAG_READ_UNBUFFERED != 0 {
			resp := smb2.NewErrorResponse(rr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		if c.refusesChannel(rr.Channel()) {
			resp := smb2.NewErrorResponse(rr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		// A special case: some clients use the SRVSVC named pipe for writing requests to it
		// and reading responses from it. Usually, an SMB2_IOCTL request serves this purpose.
		if strings.ToLower(name) == "srvsvc" {
			op.mu.Lock()
			data := bytes.Clone(op.srvsvcData)
			op.mu.Unlock()
			if data != nil {
				ip := rpc.InboundPacket{}
				if err := ip.Read(bytes.NewBuffer(data)); err != nil {
					// The packet was written by whoever is on the far end, so one that cannot
					// be read is answered rather than reached into: the body and the payload
					// are only there to reach for when the whole of it arrived. What was sent
					// is dropped as well, so the same bad bytes are not read again.
					op.mu.Lock()
					op.srvsvcData = nil
					op.mu.Unlock()
					resp := smb2.NewErrorResponse(rr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
					return resp, ss, nil
				}

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
						err := request.Unmarshal(ip.Payload)
						if err == nil && request.Level == 1 {
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
				if err := packet.Write(&buf); err != nil {
					log.Println("Couldn't write the RPC response:", err)
					op.mu.Lock()
					op.srvsvcData = nil
					op.mu.Unlock()
					resp := smb2.NewErrorResponse(rr, smb2.STATUS_INSUFFICIENT_RESOURCES, 0, nil)
					return resp, ss, nil
				}

				resp := &smb2.ReadResponse{}
				resp.FromRequest(rr)
				resp.Generate(buf.Bytes(), rr.Padding())
				op.mu.Lock()
				op.srvsvcData = nil
				op.mu.Unlock()
				return resp, ss, nil
			}
		}

		size := op.file.sizeNow()
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

		// The request carries the ID it is now known by, and says that it is being worked on
		// asynchronously. A cancel names the request by that ID and cleans up by it, so a request
		// left unmarked is one a cancel answers without ever finding the work behind it.
		req.Header().SetAsyncID(asyncID)
		req.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		c.mu.Lock()
		c.asyncCommandList[asyncID] = req
		c.mu.Unlock()

		resp := smb2.NewErrorResponse(rr, smb2.STATUS_PENDING, 0, nil)
		resp.Header().SetAsyncID(asyncID)
		resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
		resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
		resp.Header()[len(resp.Header())-1] = 0x21

		interim := c.expectInterim(rr.Header().MessageID())
		go func() {
			defer c.recoverConnection("reading a file")

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
			finalAsync(resp, asyncID)

			if !c.claimAnswer(asyncID, resp.Header().MessageID()) {
				// The client gave up on it and has been answered already.
				c.releaseOpen(req)

				return
			}

			// The handle may have gone while the work was being done, and what was worked out
			// is of no use if it has. The client is still waiting on the request either way, so
			// it is answered rather than left: an unanswered request is one the client counts
			// as outstanding on the connection for as long as it holds it, and the answer to a
			// handle that is gone is that it is gone.
			select {
			case <-op.ctx.Done():
				resp = smb2.NewErrorResponse(req, smb2.STATUS_FILE_CLOSED, 0, nil)
				finalAsync(resp, asyncID)
			default:
			}

			c.awaitInterim(interim)

			c.releaseOpen(req)
			c.server.trySendResponse(c, ss, resp)
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(wr, status, 0, nil)
			return resp, ss, nil
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
		op.mu.Unlock()
		size := op.file.sizeNow()
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

		// These have to come before the hidden file is shrugged off below, or a write the server
		// cannot carry out is answered as though it had been.
		if c.refusesChannel(wr.Channel()) {
			resp := smb2.NewErrorResponse(wr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		// Past this point the data travels in the request itself, so it has to start somewhere
		// the request can reasonably have put it ([MS-SMB2] 3.3.5.13).
		if wr.DataOffset() > smb2.MaxWriteDataOffset {
			resp := smb2.NewErrorResponse(wr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
			return resp, ss, nil
		}

		// Which right the write needs is decided by the range it covers, not by both at once
		// ([MS-SMB2] 3.3.5.13): one that stays inside the file needs FILE_WRITE_DATA, and one that
		// carries the file past its end needs FILE_APPEND_DATA. The comparison is written so that
		// an offset beyond the end of the file cannot wrap it round.
		extends := wr.Offset() > size || length > size-wr.Offset()
		if (!extends && ga&(smb2.FILE_WRITE_DATA|smb2.GENERIC_WRITE) == 0) ||
			(extends && ga&(smb2.FILE_APPEND_DATA|smb2.GENERIC_WRITE) == 0) {
			resp := smb2.NewErrorResponse(wr, smb2.STATUS_ACCESS_DENIED, 0, nil)
			return resp, ss, nil
		}

		// Whatever anybody else has cached of this file is about to be out of date. This comes
		// after the access check: a write that is going to be refused changes nothing, and has
		// no business emptying anybody's cache.
		c.server.breakForChange(op)

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

		// The request carries the ID it is now known by, and says that it is being worked on
		// asynchronously. A cancel names the request by that ID and cleans up by it, so a request
		// left unmarked is one a cancel answers without ever finding the work behind it.
		req.Header().SetAsyncID(asyncID)
		req.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		c.mu.Lock()
		c.asyncCommandList[asyncID] = req
		c.mu.Unlock()

		resp := smb2.NewErrorResponse(wr, smb2.STATUS_PENDING, 0, nil)
		resp.Header().SetAsyncID(asyncID)
		resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
		resp.Header().ClearFlag(smb2.FLAGS_RELATED_OPERATIONS)
		resp.Header().ClearFlag(smb2.FLAGS_SIGNED)
		resp.Header()[len(resp.Header())-1] = 0x21

		// This is where the credits of the write go back, so it is where the client is paced: the
		// further the backend is behind on this file, the less it is given to send with.
		waiting := op.file.waitingOnTheBackend()
		wanted := creditsToGrant(wr.Header().CreditCharge(), wr.Header().CreditRequest(),
			waiting, pacingCapacity(op.treeConnect.maxUploadSize))
		resp.Header().SetCreditResponse(wanted)

		// Every write is named as it arrives, however many there are: the timestamps of the
		// arrivals are what a transfer that stalls is read out of, and the credits are what a
		// client stalls for want of.
		// The write is counted against the file rather than against this handle: whoever finalizes
		// the upload has to wait for every write going into it, whichever handle sent it.
		interim := c.expectInterim(wr.Header().MessageID())
		op.file.beginWrite()
		go func() {
			// Deferred before the write is ended, so that a panic still ends it: a write left
			// counted is a write the next flush waits for and never sees.
			defer c.recoverConnection("writing to a file")
			defer op.file.endWrite()
			var resp smb2.GenericResponse

			if err := op.write(wr.Offset(), wr.Buffer()); err != nil {
				op.cancelUpload()
				log.Println("Error writing data:", err)
				resp = smb2.NewErrorResponse(wr, smb2.STATUS_DATA_ERROR, 0, nil)
			} else {
				resp = &smb2.WriteResponse{}
				resp.FromRequest(wr)
				resp.(*smb2.WriteResponse).Generate(uint32(len(wr.Buffer())))
				finalAsync(resp, asyncID)
			}

			if !c.claimAnswer(asyncID, resp.Header().MessageID()) {
				// The client gave up on it and has been answered already.
				c.releaseOpen(req)

				return
			}

			// The handle may have gone while the work was being done, and what was worked out
			// is of no use if it has. The client is still waiting on the request either way, so
			// it is answered rather than left: an unanswered request is one the client counts
			// as outstanding on the connection for as long as it holds it, and the answer to a
			// handle that is gone is that it is gone.
			select {
			case <-op.ctx.Done():
				resp = smb2.NewErrorResponse(req, smb2.STATUS_FILE_CLOSED, 0, nil)
				finalAsync(resp, asyncID)
			default:
			}

			c.awaitInterim(interim)

			// The uneventful writes are reported now and then rather than every time, and the
			// ones that went wrong or took long enough for a client to give up on the server
			// are reported whenever they happen.
			c.releaseOpen(req)
			c.server.trySendResponse(c, ss, resp)
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(lr, status, 0, nil)
			return resp, ss, nil
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(ir, status, 0, nil)
			return resp, ss, nil
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
			if err := ip.Read(bytes.NewBuffer(ir.InputBuffer())); err != nil {
				// As above: nothing of the packet is looked at until the whole of it is known
				// to have arrived.
				resp := smb2.NewErrorResponse(ir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
				return resp, ss, nil
			}

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
						err := request.Unmarshal(ip.Payload)
						if err == nil && request.Level == 1 {
							packet = rpc.NewNetShareGetInfo1Response(
								ip.Header.CallID,
								request.Share,
								tc.share.remark,
								smb2.STATUS_OK,
							)
						}

					case rpc.NET_SHARE_ENUM_ALL:
						var request rpc.NetShareEnumAllRequest
						err := request.Unmarshal(ip.Payload)
						if err == nil && request.Level == 1 {
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
						if err := request.Unmarshal(ip.Payload); err == nil {
							packet = rpc.NewMdsOpenResponse(
								ip.Header.CallID,
								request,
								"",
								smb2.STATUS_OK,
							)
						}
					}
				}
			}

			var buf bytes.Buffer
			if err := packet.Write(&buf); err != nil {
				log.Println("Couldn't write the RPC response:", err)
				resp := smb2.NewErrorResponse(ir, smb2.STATUS_INSUFFICIENT_RESOURCES, 0, nil)
				return resp, ss, nil
			}

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
		if er.Header().SessionID() != 0 || er.Header().IsFlagSet(smb2.FLAGS_SIGNED) {
			var status uint32
			ss, status = c.sessionFor(req)
			if status != smb2.STATUS_OK {
				resp := smb2.NewErrorResponse(er, status, 0, nil)
				return resp, ss, nil
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(qdr, status, 0, nil)
			return resp, ss, nil
		}

		switch qdr.FileInformationClass() {
		case smb2.FILE_BOTH_DIRECTORY_INFORMATION,
			smb2.FILE_DIRECTORY_INFORMATION,
			smb2.FILE_FULL_DIRECTORY_INFORMATION,
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
			return resp, ss, nil
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

		attr := op.file.attributesNow()
		op.mu.Lock()
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

		// Whether this carries on the search the handle already ran, and whether that search has
		// anything left to send, are decided together: read apart, a search restarting on another
		// channel lands between them and this one carries on into results it never ran for.
		op.mu.Lock()
		carryOn := op.lastSearch != "" && op.lastSearch == searchPath && qdr.Flags()&smb2.RESTART_SCANS == 0
		exhausted := carryOn && len(op.searchResults) == 0
		if exhausted {
			op.lastSearch = ""
		}
		op.mu.Unlock()

		if carryOn {
			// If the search has already run with the same parameters, and all results have been sent
			// to the client, respond with the status STATUS_NO_MORE_FILES.
			if exhausted {
				resp := smb2.NewErrorResponse(qdr, smb2.STATUS_NO_MORE_FILES, 0, nil)
				return resp, ss, nil
			}

			// Send as many search results as the buffer length allows.
			buf = op.takeSearchResults(qdr.FileInformationClass(), qdr.OutputBufferLength(), single, false, client.FileInfo{}, client.FileInfo{})
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

			// Send as many search results as the buffer length allows. The results are the ones
			// the search just found, not the ones read before it ran: a search whose first
			// answer is built out of what the handle held beforehand carries nothing at all,
			// and a client that reads an empty buffer as the end of the enumeration never sees
			// the file it asked about.
			buf = op.takeSearchResults(qdr.FileInformationClass(), qdr.OutputBufferLength(), single, qdr.FileName() == "*", dir, parentDir)
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(cnr, status, 0, nil)
			return resp, ss, nil
		}

		acc, err := c.server.store.FindAccount(ss.userName, ss.workgroup)
		if err != nil {
			resp := smb2.NewErrorResponse(cnr, smb2.STATUS_USER_SESSION_DELETED, 0, nil)
			return resp, ss, nil
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

		attr := op.file.attributesNow()
		op.mu.Lock()
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

		// The first look at the directory decides whether the watch can start at all, and is
		// taken before anything is registered. A watch that cannot start is answered here and
		// now: armed first and failed after, it could only fail behind an interim response the
		// client has already been given, and the answer would race the interim on its way out.
		snapshot, err := op.directorySnapshot(acc)
		if err != nil {
			log.Printf("Error listing directory contents on %s: %v", op.pathName, err)
			resp := smb2.NewErrorResponse(cnr, smb2.STATUS_UNSUCCESSFUL, 0, nil)
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
		go op.checkForChanges(cnr, c, acc, snapshot, ch)

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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(qir, status, 0, nil)
			return resp, ss, nil
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
			// The classes that answer with the attributes of the file are only served to a handle
			// that was opened to read them ([MS-SMB2] 3.3.5.20.1).
			switch qir.FileInfoClass() {
			case smb2.FileBasicInformation, smb2.FileAllInformation,
				smb2.FileNetworkOpenInformation, smb2.FileAttributeTagInformation:
				if ga&(smb2.FILE_READ_ATTRIBUTES|smb2.GENERIC_READ) == 0 {
					resp := smb2.NewErrorResponse(qir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}
			}

			switch qir.FileInfoClass() {
			case smb2.FileAllInformation:
				info = op.fileAllInformation()
			case smb2.FileStandardInformation:
				info = op.fileStandardInformation()
			case smb2.FileNetworkOpenInformation:
				info = op.fileNetworkOpenInformation()
			case smb2.FileNormalizedNameInformation:
				// The class belongs to 3.1.1 alone. [MS-SMB2] 3.3.5.20.1 names 2.0.2, 2.1 and
				// 3.0.2 as the dialects that must refuse it and leaves 3.0 out of that list,
				// which reads as an oversight rather than a dialect that carries it.
				if c.negotiateDialect != smb2.SMB_DIALECT_311 {
					resp := smb2.NewErrorResponse(qir, smb2.STATUS_NOT_SUPPORTED, 0, nil)
					return resp, ss, nil
				}

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
			case smb2.FileFsSizeInformation, smb2.FileFsFullSizeInformation:
				// How much room the share has is what a client checks before it copies anything
				// and again while it copies: a destination that looks too small is a copy given
				// up on. An answer that could not be worked out is said to be one, because the
				// alternative - an answer of no bytes at all, where the client is waiting for a
				// structure - is one the client cannot read as anything.
				si, err := tc.client.Storage(op.ctx)
				if err != nil {
					log.Println("Error getting storage info:", err)
					resp := smb2.NewErrorResponse(qir, smb2.STATUS_INSUFFICIENT_RESOURCES, 0, nil)
					return resp, ss, nil
				}

				if qir.FileInfoClass() == smb2.FileFsSizeInformation {
					info = smb2.FileFsSizeInfo(si)
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

			// A security descriptor is never sent in part: the client is told how much room it
			// takes and asks again. [MS-SMB2] 3.3.5.20.3 names STATUS_BUFFER_OVERFLOW as the one
			// answer this must not carry, because that is what says a truncated answer follows.
			if qir.OutputBufferLength() < uint32(len(info)) {
				if c.negotiateDialect == smb2.SMB_DIALECT_311 {
					ecd := smb2.ErrorContextData(0, binary.LittleEndian.AppendUint32(nil, uint32(len(info))))
					resp := smb2.NewErrorResponse(qir, smb2.STATUS_BUFFER_TOO_SMALL, 1, ecd)
					return resp, ss, nil
				} else {
					ecd := binary.LittleEndian.AppendUint32(nil, uint32(len(info)))
					resp := smb2.NewErrorResponse(qir, smb2.STATUS_BUFFER_TOO_SMALL, 0, ecd)
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

		ss, status := c.sessionFor(req)
		if status != smb2.STATUS_OK {
			resp := smb2.NewErrorResponse(sir, status, 0, nil)
			return resp, ss, nil
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
			return resp, ss, nil
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

		attr := op.file.attributesNow()
		op.mu.Lock()
		ga := op.grantedAccess
		path := op.pathName
		op.mu.Unlock()

		// Renaming, deleting or resizing the file leaves whatever anybody else has cached of it
		// out of date, and the handle they may be holding open pointing at the wrong thing. An
		// open with no way of changing the file cannot be about to do any of that.
		if ga&(smb2.FILE_WRITE_DATA|smb2.FILE_WRITE_ATTRIBUTES|smb2.DELETE) > 0 {
			c.server.breakForChange(op)
		}

		switch sir.InfoType() {
		case smb2.INFO_FILE:
			switch sir.FileInfoClass() {
			case smb2.FileEndOfFileInformation:
				if ga&smb2.FILE_WRITE_DATA == 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}

				buf := sir.Buffer()
				if len(buf) != 8 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
					return resp, ss, nil
				}

				// The end of the file is where the file ends, and a client that moves it back is
				// throwing away what is beyond it. Recording it as the space the file takes up and
				// nothing else left the truncation to whatever happened to be written afterwards.
				if err := op.setEndOfFile(acc, binary.LittleEndian.Uint64(buf)); err != nil {
					log.Printf("Error setting the end of %s: %v", path, err)
					status := uint32(smb2.STATUS_UNEXPECTED_NETWORK_ERROR)
					if errors.Is(err, errTruncateSent) {
						status = smb2.STATUS_DATA_ERROR
					}
					resp := smb2.NewErrorResponse(sir, status, 0, nil)
					return resp, ss, nil
				}

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
				if !fbi.CreationTime.IsZero() {
					modTime = fbi.CreationTime
				}

				if !fbi.LastWriteTime.IsZero() && fbi.LastWriteTime.After(modTime) {
					modTime = fbi.LastWriteTime
				}

				if !fbi.ChangeTime.IsZero() && fbi.ChangeTime.After(modTime) {
					modTime = fbi.ChangeTime
				}

				// setModified keeps whichever is later, so a time from before the last write to
				// the file is not taken.
				op.file.setModified(modTime)

				if fbi.FileAttributes != 0 {
					op.file.setAttributes(fbi.FileAttributes)
				}

			case smb2.FileDispositionInformation:
				if ga&smb2.DELETE == 0 {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_ACCESS_DENIED, 0, nil)
					return resp, ss, nil
				}

				// DeleteFile is a boolean, so anything other than zero asks for the deletion and
				// zero calls off one that was pending. A request carrying no buffer at all was
				// turned away before reaching here.
				pending := sir.Buffer()[0] > 0
				if pending {
					if attr&smb2.FILE_ATTRIBUTE_DIRECTORY != 0 {
						empty, err := tc.client.IsEmpty(op.ctx, acc, path+"/")
						if err != nil {
							log.Printf("Error listing directory contents on %s: %v", path, err)
							resp := smb2.NewErrorResponse(sir, smb2.STATUS_NETWORK_NAME_DELETED, 0, nil)
							return resp, ss, nil
						}
						if !empty {
							resp := smb2.NewErrorResponse(sir, smb2.STATUS_DIRECTORY_NOT_EMPTY, 0, nil)
							return resp, ss, nil
						}
					}

					op.mu.Lock()
					op.createOptions |= smb2.FILE_DELETE_ON_CLOSE
					op.mu.Unlock()
				} else {
					// Nothing is deleted until the handle is closed, so up to that point the
					// client may change its mind and keep the file.
					op.mu.Lock()
					op.createOptions &^= smb2.FILE_DELETE_ON_CLOSE
					op.mu.Unlock()
				}

				// Whether the file is on its way out is also what decides whether the lease key
				// it was taken out under is free for another file ([MS-SMB2] 3.3.5.21.1).
				op.setLeaseDeleteOnClose(pending)

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

				// Rename the file or the directory. The name it is moving to has to resolve
				// inside the share, exactly as the one it was created under did.
				newName := strings.ReplaceAll(fri.FileName, "\\", "/")
				if newName == "" || !validPath(newName) {
					resp := smb2.NewErrorResponse(sir, smb2.STATUS_INVALID_PARAMETER, 0, nil)
					return resp, ss, nil
				}

				// A rename moves what the store holds, and a file still being written is not held
				// by it yet: the bytes are in the upload buffer, under a name the rename is about
				// to take away. So the upload is finished under the name it was started with, and
				// the rename moves what it stored. Renaming is metadata on both backends, so this
				// costs the upload it was always going to cost and nothing besides.
				if op.file.uploadNow() != nil {
					if err := op.flush(); err != nil {
						op.cancelUpload()
						log.Printf("Error completing write of %s before renaming it: %v", path, err)
						resp := smb2.NewErrorResponse(sir, smb2.STATUS_UNEXPECTED_NETWORK_ERROR, 0, nil)
						return resp, ss, nil
					}
				}

				// A file the store has nothing for is renamed on the share and nowhere else. What
				// makes it that file is not its size but the store having no object for it: an
				// emptied file the store still holds an object for is renamed at the backend like
				// any other, or the object would be left behind under the old name.
				if !op.file.isStored() && attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 {
					if fs, found := tc.persistedFile(newName); found && !fs.isStored() {
						resp := smb2.NewErrorResponse(sir, smb2.STATUS_OBJECT_NAME_COLLISION, 0, nil)
						return resp, ss, nil
					}

					tc.movePersistedFile(path, newName, op.file)

					// The path of the open changes through the index, so that a create
					// racing this rename finds the open under exactly one of its names,
					// never neither. Every handle on the file goes, not only this one.
					for _, other := range c.server.moveOpensOnFile(tc.share, path, newName) {
						if other != op {
							other.renameLease(newName)
						}
					}
					c.server.moveOpen(op, newName)
					op.file.touch()
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

					// The open follows the file to its new name, as in the branch above. Left
					// pointing at the old one, every later read and write on this handle would
					// reach for an object the backend no longer has, and the old name would go
					// on blocking creates while the new one stood unguarded. A persisted entry
					// moves with it, so that the old name stops resolving to this file.
					if fs, found := tc.persistedFile(path); found {
						tc.movePersistedFile(path, newName, fs)
					}

					// A renamed directory takes everything inside it along, so the entries under
					// it are re-keyed with it.
					if attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0 {
						tc.movePersistedTree(path, newName)
					}

					for _, other := range c.server.moveOpensOnFile(tc.share, path, newName) {
						if other != op {
							other.renameLease(newName)
						}
					}
					c.server.moveOpen(op, newName)

					// The opens on the files inside the directory follow their files, and the
					// lease of each follows its open: a renamed file is not one on its way
					// out, however it came by its new name ([MS-SMB2] 3.3.5.21.1).
					if attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0 {
						for _, child := range c.server.moveOpensUnder(tc.share, path, newName) {
							child.mu.Lock()
							childPath := child.pathName
							child.mu.Unlock()
							child.renameLease(childPath)
						}
					}
				}

				// The lease follows the file under its new name, and a file that has just been
				// renamed is not one on its way out ([MS-SMB2] 3.3.5.21.1).
				op.renameLease(newName)

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

				op.file.setAllocated(binary.LittleEndian.Uint64(buf))

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

// recoverGoroutine turns a panic raised on a goroutine that answers to no one connection into the
// loss of whatever that goroutine was doing rather than of the whole server.
func recoverGoroutine(where string) {
	r := recover()
	if r == nil {
		return
	}

	log.Printf("panic while %s: %v\n%s", where, r, debug.Stack())
}

// recoverConnection turns a panic raised while serving one connection into the loss of that
// connection rather than of the whole server.
//
// The socket is closed first: that takes no lock and is what puts the peer out of reach, while the
// teardown after it takes the connection lock a panic may have been raised while holding.
func (c *connection) recoverConnection(where string) {
	r := recover()
	if r == nil {
		return
	}

	c.conn.Close()
	log.Printf("panic while %s for %s: %v\n%s", where, c.clientName, r, debug.Stack())
	c.server.closeConnection(c)
}

// readLoop serves one connection, taking the messages off the socket and handing them to the
// request path until the peer goes or the connection is torn down under it.
func (c *connection) readLoop(host string) {
	defer c.recoverConnection("reading from the connection")

	for {
		msg, err := readMessage(c.conn)
		if err != nil {
			// A peer that has gone and a socket this server has closed itself are the connection
			// ending rather than anything to report. Whichever it is, there is nothing further to
			// read: the end of a stream is the end of it, and a read that finds one finds it again
			// every time it is asked.
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, net.ErrClosed) {
				log.Println("Error reading message:", err)
			}
			c.server.closeConnection(c)

			return
		}

		c.server.mu.Lock()
		c.server.stats.BytesRcvd += uint64(len(msg))
		c.server.mu.Unlock()

		if err := c.acceptRequest(msg); err != nil {
			log.Println("couldn't accept request:", err)
			c.server.closeConnection(c)
			if errors.Is(err, smb2.ErrWrongProtocol) {
				// Ban the remote host if it keeps sending SMB requests after receiving
				// an SMB2_NEGOTIATE response.
				c.server.blockHost(host, "old protocol")
				log.Printf("Blocked host %s for using old protocol\n", host)
			}

			return
		}
	}
}

// chainMemberDone counts off a request of a compound chain and returns the chain to send if that
// was the last of them, or nil if the chain is still being assembled or there is no chain at all.
func (c *connection) chainMemberDone(gid uint64) smb2.GenericResponse {
	if gid == 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.chainRemaining[gid]--
	if c.chainRemaining[gid] > 0 {
		return nil
	}
	delete(c.chainRemaining, gid)

	chain := c.pendingResponses[gid]
	delete(c.pendingResponses, gid)

	return chain
}

// processRequests pulls requests from the queue one by one and submits them for processing.
func (c *connection) processRequests() {
	defer c.recoverConnection("processing requests")

	for {
		var req *smb2.Request
		c.mu.Lock()
		if len(c.requestList) > 0 {
			// Taken off the queue as it is picked up, so that what is left in the queue is what
			// nothing has started on. A cancel answers out of the queue and relies on that.
			var mid uint64
			mid, req = utils.FindMinKey(c.requestList)
			delete(c.requestList, mid)
		}
		c.mu.Unlock()

		// Nothing to do: wait to be told rather than looking again.
		if req == nil {
			select {
			case <-c.closeChan:
				return
			case <-c.wakeChan:
			}

			continue
		}

		resp, ss, err := c.processRequest(req)
		if err != nil {
			if c.server.debug {
				log.Printf("Error processing request (Message ID: %d, Command: %d): %v", req.Header().MessageID(), req.Header().Command(), err)
			}
			c.server.closeConnection(c)
			return
		}

		// The credits of the request it answers go back on it - here rather than where the
		// message goes out, because a chain goes out as one message and carries a header of
		// its own for every request in it.
		c.grantOnResponse(resp)

		c.mu.Lock()
		var pendingResp smb2.GenericResponse
		if resp.GroupID() > 0 { // This response is a part of a chain, pull the chain
			pendingResp = c.pendingResponses[resp.GroupID()]
		}
		c.mu.Unlock()

		if resp.Header().Command() == smb2.SMB2_CHANGE_NOTIFY { // Send the chain if it's complete, then the response
			if chain := c.chainMemberDone(resp.GroupID()); chain != nil {
				chain.Header().SetCreditResponse(1)
				c.server.writeResponse(c, ss, chain)
			}
			c.server.writeResponse(c, ss, resp)
		} else if pendingResp != nil { // Add the response to the chain, then send the chain if it's complete
			pendingResp.Append(resp)
			if chain := c.chainMemberDone(resp.GroupID()); chain != nil {
				c.server.writeResponse(c, ss, chain)
			}
		} else if resp.GroupID() == 0 { // A standalone response, send it
			c.server.writeResponse(c, ss, resp)
		} else { // Start the response chain
			c.mu.Lock()
			resp.SetSessionID(resp.Header().SessionID())
			resp.SetTreeID(resp.Header().TreeID())
			c.pendingResponses[resp.GroupID()] = resp
			c.mu.Unlock()

			// A chain of one is complete as soon as it is started.
			if chain := c.chainMemberDone(resp.GroupID()); chain != nil {
				c.server.writeResponse(c, ss, chain)
			}
		}

		// Whatever is going out for this request is on the queue now, so the work behind an
		// asynchronous one may answer.
		c.interimQueued(resp.Header().MessageID())

		// An interim response doesn't complete the request: the counters are
		// decremented when the asynchronous command sends its final response.
		if resp.Header().Status() != smb2.STATUS_PENDING {
			c.releaseOpen(req)
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
	defer c.recoverConnection("sending responses")

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

// The credits a client is answered with are how fast it is allowed to write. A client may only have
// as many requests outstanding as it has credits, so granting fewer is how a server tells it to send
// less at a time - and it is the only way of doing so that costs the client nothing: every request is
// answered at once, and the client paces itself.
func creditsToGrant(charge, request uint16, waiting, capacity uint64) uint16 {
	spent := max(charge, 1)

	switch {
	case capacity == 0:
		return max(spent, request)
	case waiting >= capacity:
		return 1
	case waiting >= capacity/2:
		return spent
	default:
		return max(spent, request)
	}
}

// expectInterim records that the request will be answered with an interim response, and returns what
// the work behind it waits on before it answers for real.
func (c *connection) expectInterim(mid uint64) chan struct{} {
	sent := make(chan struct{})

	c.mu.Lock()
	c.interimSent[mid] = sent
	c.mu.Unlock()

	return sent
}

// awaitInterim waits until the interim response has been queued, or until the connection goes.
func (c *connection) awaitInterim(sent chan struct{}) {
	select {
	case <-sent:
	case <-c.closeChan:
	}
}

// interimQueued says that whatever was going out for the request has been queued, which lets the work
// behind it answer.
func (c *connection) interimQueued(mid uint64) {
	c.mu.Lock()
	sent, found := c.interimSent[mid]
	delete(c.interimSent, mid)
	c.mu.Unlock()

	if found {
		close(sent)
	}
}

// claimAnswer reports whether the work behind an asynchronous request still owes its client an
// answer, taking the request off the async command list if so. A cancel answers the request and
// takes it off that same list, so whichever of the two gets there first is the one that answers:
// two responses to the one message ID is not a protocol a client can follow, and it drops the
// connection rather than try.
func (c *connection) claimAnswer(asyncID, mid uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.requestList, mid)
	if _, owed := c.asyncCommandList[asyncID]; !owed {
		return false
	}
	delete(c.asyncCommandList, asyncID)

	return true
}

// finalAsync marks the response as the final one of an asynchronous command. It carries the async ID
// and flag in every case, errors included, and grants no credits: those were handed back with the
// interim response, when the work was taken on. Granted twice for the one request, they would leave
// the client believing it may send further than the sequence window this server has opened, and the
// message it sent on the strength of that is one the server has to turn away.
func finalAsync(resp smb2.GenericResponse, asyncID uint64) {
	resp.Header().SetAsyncID(asyncID)
	resp.Header().SetFlag(smb2.FLAGS_ASYNC_COMMAND)
	resp.Header().SetCreditResponse(0)
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
		// Whatever the volatile half found, the persistent half does not agree, so it is not the
		// handle the request names. A chain member is allowed to name none and take the one the
		// operation before it used.
		op = nil
		if req.GroupID() > 0 {
			op = c.findOpenByGroupID(req.GroupID())
		}

		if op != nil {
			req.SetOpenID(op.id())
		}
	}

	if op == nil {
		// A chain member has no handle of its own, so what became of the operation before it is
		// what answers for this one ([MS-SMB2] 3.3.5.2.7.2).
		if req.GroupID() > 0 {
			if status := c.chainFailure(req.GroupID()); status != smb2.STATUS_OK {
				return nil, status
			}

			return nil, smb2.STATUS_INVALID_HANDLE
		}

		return nil, smb2.STATUS_FILE_CLOSED
	}

	// Using the handle is what ends the window in which the create that made it may be
	// replayed: this is every command that takes a FileId, so it is the one place to say so.
	c.server.clearReplayEligible(op)

	// The request is on this open from here on, and says so. It was only ever recorded for a
	// request in a chain, which named no handle of its own and had to be traced back to one, so
	// whatever went looking for the requests outstanding on an open found none of the ones sent
	// on their own - the directory watches among them.
	if req != nil {
		req.SetOpenID(op.id())
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

// completeWatches answers every SMB2_CHANGE_NOTIFY the open is still holding, which is what the end
// of that open - a close, or the tree connect or session going out from under it - makes of a watch:
// the client is waiting on the request, and nothing is ever going to come of it now.
//
// It is also what stops the watch. Left in the list, the request is one the client counts as an
// unfinished directory search for as long as it holds the connection, and the goroutine behind it
// goes on listing a directory nobody is waiting to hear about.
func (c *connection) completeWatches(ss *session, id []byte) {
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
		c.server.trySendResponse(c, ss, resp)
	}
}

// chainFailure returns the status the last response assembled for a chain carried, or STATUS_OK if
// nothing has been assembled for it yet and if what was assembled went well. An interim response is
// not a failure: the work behind it is still running.
func (c *connection) chainFailure(gid uint64) uint32 {
	c.mu.Lock()
	resp, found := c.pendingResponses[gid]
	c.mu.Unlock()

	if !found {
		return smb2.STATUS_OK
	}

	buf := resp.Encode()
	var off uint32
	for {
		if uint64(off)+smb2.SMB2HeaderSize > uint64(len(buf)) {
			return smb2.STATUS_OK
		}

		h := smb2.Header(buf[off:])
		next := h.NextCommand()
		if next == 0 {
			if status := h.Status(); status != smb2.STATUS_OK && status != smb2.STATUS_PENDING {
				return status
			}

			return smb2.STATUS_OK
		}

		off += next
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
	if cr.Header().IsFlagSet(smb2.FLAGS_SIGNED) && !found {
		return errSessionNotFound
	}

	if found {
		// An SMB2_CANCEL request is never answered, so a request that cannot be verified is
		// dropped rather than failed with a status code. That covers the cancel which carries no
		// signature at all on a session that requires one: a cancel is obeyed without anything
		// coming back, so an unsigned one would let anybody who can reach the wire stop the work
		// of a session whose every other request has to be signed.
		if err := ss.validateRequest(req, c); err != nil {
			return err
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
	} else {
		// A target taken out of the queue was never answered with an interim, so this is the
		// response its credits go back on.
		owner.grantOnResponse(resp)
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

		// Taken on asynchronously, but cancelled before the client saw the interim response.
		for _, r := range c.asyncCommandList {
			if r != nil && r.Header().MessageID() == mid {
				return r, c
			}
		}

		// Still waiting its turn ([MS-SMB2] 3.3.5.16). Removing it here is what keeps the
		// dispatcher from answering it too. A member of a compound chain is left to be processed:
		// the chain is answered as one, and a cancel may come to nothing.
		if r, found := c.requestList[mid]; found && r.GroupID() == 0 {
			delete(c.requestList, mid)
			return r, c
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

	// A connection nobody has authenticated over yet is judged by its age alone, and the answer
	// has to be given here. Letting it fall through to the end reaches an answer arrived at with
	// nothing to go on, which gives up on the connection: a client still working through its
	// negotiate and session setup has no session in the table yet, and dropping it would end a
	// connection in the middle of being set up.
	if len(c.sessionTable) == 0 {
		return time.Since(c.creationTime) > staleThreshold
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

// isUnauthenticated reports whether nobody has got as far as an authenticated session over this
// connection.
//
// The spec names three cases ([MS-SMB2] 3.3.6.3): no dialect negotiated, a dialect but no
// sessions, and sessions none of which is Valid or Expired. They come to the same thing, since
// a session cannot be set up before a dialect is agreed and cannot reach either of those two
// states without authenticating, so one walk of the table answers all three.
func (c *connection) isUnauthenticated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, ss := range c.sessionTable {
		if state := ss.stateNow(); state == sessionValid || state == sessionExpired {
			return false
		}
	}

	return true
}

// scavengeConnections drops the connections that have been open a while without anybody
// authenticating over them: a client that negotiated and stopped, one that never even did that,
// and one whose session setup was left half finished. None of them can be reached for anything,
// and each holds a slot against the limit on how many connections one address may have.
func (s *server) scavengeConnections() {
	s.mu.Lock()
	conns := make([]*connection, 0, len(s.connectionList))
	for _, c := range s.connectionList {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		if time.Since(c.creationTime) <= connectionScavengeTimeout {
			continue
		}
		if !c.isUnauthenticated() {
			continue
		}

		if s.debug {
			log.Printf("Dropping connection from %s: nothing was authenticated over it", c.clientName)
		}
		s.closeConnection(c)
	}
}

// reapConnections drops the connections nobody authenticated over, until the server shuts down.
func (s *server) reapConnections() {
	ticker := time.NewTicker(connectionScavengeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer recoverGoroutine("scavenging the connections")
				s.scavengeConnections()
			}()
		}
	}
}

// createFile carries out an SMB2_CREATE request that has passed its preliminary checks.
// It is kept apart from the dispatcher because a create that has to revoke somebody else's
// oplock first can only finish once the break is over, which is too long to keep the
// connection waiting, so it runs on a goroutine of its own.
func (c *connection) createFile(req *smb2.Request, cr smb2.CreateRequest, ss *session, tc *treeConnect, acc stores.Account, contexts map[uint32][]byte, path string, lr *smb2.LeaseRequest) (smb2.GenericResponse, *open) {
	// A lease key is bound to the file it was first used for. Naming the same key on another
	// file is the one thing a client may not do with one, and is settled before the open is
	// made, so that a refusal leaves nothing behind.
	var own *lease
	if lr != nil {
		l, matches := c.server.leaseFor([16]byte(c.clientGuid), *lr, path)
		if !matches {
			return smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil), nil
		}
		own = l
	}

	ctx, cancel := context.WithCancel(context.Background())
	var info client.ObjectInfo
	var result uint32
	var restored, stored bool
	var known *fileState
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
			return resp, nil
		}
	} else {
		info, err = tc.client.Object(ctx, acc, path)
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			cancel()
			resp := smb2.NewErrorResponse(cr, smb2.STATUS_IO_TIMEOUT, 0, nil)
			return resp, nil
		}
		stored = err == nil

		// Every handle on a file shares one state, so the share is asked for it here rather than
		// only where the store has nothing to offer. A create that makes its own state for a file
		// somebody else already has open gives that file a second size and a second upload to the
		// same path, and the store keeps whichever upload was finished last: the writes that went
		// into the other one are lost, and until then each handle reads the object the store holds
		// cut off at a length of its own.
		known, _ = tc.persistedFile(path)

		switch cr.CreateDisposition() {
		case smb2.FILE_SUPERSEDE:
			if err != nil {
				restored = known != nil
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
				restored = known != nil
				if !restored {
					cancel()
					resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
					return resp, nil
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
							return resp, nil
						} else {
							// The store failed the call, which is not the same as the name not
							// being there: answering that it is not there has the client tell
							// whoever asked that the folder they are making no longer exists.
							log.Printf("Couldn't create directory %s: %v\n", path, err)
							resp := smb2.NewErrorResponse(cr, smb2.STATUS_UNEXPECTED_NETWORK_ERROR, 0, nil)
							return resp, nil
						}
					}
				}
			} else {
				cancel()
				resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_COLLISION, 0, nil)
				return resp, nil
			}
		case smb2.FILE_OPEN_IF:
			if err != nil {
				restored = known != nil
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
								return resp, nil
							} else {
								// As above: the store failing is not the name being absent.
								log.Printf("Couldn't create directory %s: %v\n", path, err)
								resp := smb2.NewErrorResponse(cr, smb2.STATUS_UNEXPECTED_NETWORK_ERROR, 0, nil)
								return resp, nil
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
				restored = known != nil
				if !restored {
					cancel()
					resp := smb2.NewErrorResponse(cr, smb2.STATUS_OBJECT_NAME_NOT_FOUND, 0, nil)
					return resp, nil
				} else {
					result = smb2.FILE_OVERWRITTEN
				}
			} else {
				result = smb2.FILE_OVERWRITTEN
			}
		case smb2.FILE_OVERWRITE_IF:
			if err != nil {
				restored = known != nil
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
								return resp, nil
							} else {
								// As above: the store failing is not the name being absent.
								log.Printf("Couldn't create directory %s: %v\n", path, err)
								resp := smb2.NewErrorResponse(cr, smb2.STATUS_UNEXPECTED_NETWORK_ERROR, 0, nil)
								return resp, nil
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

	// A file that is known only by its state has no object behind it for the lookup above to have
	// found, so what the open is built from is put together here: the name the file was created
	// under, and what its state says of it. The key is the one the file was created with, so the
	// handle a client is told the file is at does not move about between opens of it.
	if restored {
		size, _, created, modified, _ := known.stat()
		info = client.ObjectInfo{
			Key:        "/" + path,
			CreatedAt:  created,
			ModifiedAt: modified,
			Size:       size,
		}
	}

	// A file that has been created but not yet uploaded is known by its state, which every open
	// on it shares. The open itself is this handle's own, whether the file was met before or not:
	// two handles on one file are two opens, with a file ID and a caching promise apiece, and
	// handing the same open to both is what left the server breaking an oplock on the very handle
	// a create was about to answer with.
	op = ss.registerOpen(cr, c, tc, info, ctx, cancel, known)
	if op == nil {
		cancel()
		resp := smb2.NewErrorResponse(cr, smb2.STATUS_INVALID_PARAMETER, 0, nil)
		return resp, nil
	}

	// The state answers for the file for as long as it is open, and the share is where every handle
	// on it finds the one state. A file the store has an object for is answered for by the store
	// again once the last handle goes; one it has nothing for is known by this and by nothing else.
	attr := op.file.attributesNow()
	if tc.share.name != "ipc$" && attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 {
		if stored {
			op.file.markStored()
		}
		tc.persistFile(path, op.file)
	}

	if result == smb2.FILE_SUPERSEDED || result == smb2.FILE_OVERWRITTEN {
		op.file.empty()
	}

	_, _, _, createdModified, _ := op.file.stat()

	respContexts := make(map[uint32][]byte)
	var replayable bool
	for id, ctx := range contexts {
		switch id {
		case smb2.CREATE_QUERY_MAXIMAL_ACCESS_REQUEST:
			respContexts[id] = smb2.HandleCreateQueryMaximalAccessRequest(ctx, createdModified, op.grantedAccess)
		case smb2.CREATE_QUERY_ON_DISK_ID:
			respContexts[id] = smb2.HandleCreateQueryOnDiskID(op.handle, tc.volumeID)
		case smb2.CREATE_ALLOCATION_SIZE: // The file is about to be uploaded, we just got its size
			op.file.setAllocated(binary.LittleEndian.Uint64(ctx))
		case smb2.SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2:
			// The handle is to survive the loss of the connection. A directory has no
			// work in progress worth preserving, and a named pipe has nothing to go
			// back to, so only files are granted durability.
			dh, ok := smb2.ParseDurableHandleRequestV2(ctx)
			if !ok || tc.share.name == "ipc$" {
				break
			}
			if op.file.isDirectory() {
				break
			}
			respContexts[id] = smb2.HandleCreateDurableHandleRequestV2(op.grantDurability(dh))

			// A create that was granted durability is one the client may replay if the answer
			// never reaches it. The open is not offered up for that yet: it is still being
			// built, and what it is still to gain is exactly what a replay would answer with.
			replayable = true
		}
	}

	// What the client may cache is decided last, once the open exists and counts among those on
	// the file. Whatever was asked for, the level the client is told about is the level it gets.
	oplockLevel := uint8(smb2.OPLOCK_LEVEL_NONE)
	isDir := attr&smb2.FILE_ATTRIBUTE_DIRECTORY > 0
	switch {
	case own != nil:
		// A directory can only be cached through a lease that the change notifications keep
		// honest, and a named pipe has nothing behind it worth caching at all.
		granted := uint32(smb2.SMB2_LEASE_NONE)
		if leaseGrantable(lr.LeaseState) && tc.share.name != "ipc$" && !isDir {
			granted = c.server.grantLease(op, own, lr.LeaseState, tc, path)
		}
		if granted != smb2.SMB2_LEASE_NONE {
			oplockLevel = smb2.OPLOCK_LEVEL_LEASE

			// A create that takes the file out to delete it says so on the lease, so that the
			// key is free for another file once this one is gone.
			if cr.CreateOptions()&smb2.FILE_DELETE_ON_CLOSE > 0 {
				op.setLeaseDeleteOnClose(true)
			}
		}

		// A client that asked for a lease is answered either way, so that it learns it was
		// given nothing rather than being left to guess.
		respContexts[smb2.CREATE_REQUEST_LEASE] = smb2.HandleCreateRequestLease(*lr, granted, own.currentEpoch())

	case oplockEligible(cr.RequestedOplockLevel(), tc, isDir):
		oplockLevel = c.server.grantOplock(op, cr.RequestedOplockLevel(), tc, path)
	}

	// The open is finished, caching promise and all, so it can now answer for the create that
	// made it. Offered up any earlier - while the lease was still being granted, as it was - a
	// replay arriving over another channel would answer out of an open that was still being
	// put together, and tell the client it holds nothing on a file it is about to be given a
	// lease on.
	if replayable {
		c.server.markReplayEligible(op, tc)
	}

	resp := &smb2.CreateResponse{}
	resp.FromRequest(cr)
	size, allocated, _, modified, attributes := op.file.stat()
	op.mu.Lock()
	resp.Generate(
		oplockLevel,
		result,
		size,
		allocated,
		modified,
		attributes&smb2.FILE_ATTRIBUTE_DIRECTORY > 0,
		op.fileID,
		op.durableFileID,
		respContexts,
	)
	op.mu.Unlock()

	gid := req.GroupID()
	if gid > 0 {
		resp.SetOpenID(op.id())
	}

	return resp, op
}
