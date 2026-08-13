package main

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/api"
	"github.com/mike76-dev/sombrero/rpc"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
)

// ServerHashLevel values.
const (
	HashEnableAll = iota
	HashDisableAll
	HashEnableShare
)

var (
	// Supported algorithms.
	supportedHashAlgos        = []uint16{smb2.SHA_512}
	supportedEncryptionAlgos  = []uint16{smb2.AES_128_CCM, smb2.AES_128_GCM}
	supportedCompressionAlgos = []uint16{smb2.COMPRESSION_LZ4, smb2.COMPRESSION_LZ77, smb2.COMPRESSION_LZ77_HUFFMAN, smb2.COMPRESSION_LZNT1, smb2.COMPRESSION_PATTERN_V1}
	supportedSigningAlgos     = []uint16{smb2.HMAC_SHA256, smb2.AES_CMAC, smb2.AES_GMAC}
)

// server is the implementation of an SMB server.
type server struct {
	enabled                     bool
	stats                       api.ServerStats
	shareList                   map[string]*share
	globalOpenTable             map[uint64]*open
	globalSessionTable          map[uint64]*session
	globalLeaseTableList        map[[16]byte]*leaseTable
	connectionList              map[string]*connection
	serverGuid                  [16]byte
	serverCapabilities          uint32
	globalClientTable           map[[16]byte]*smbClient
	encryptData                 bool
	compressionSupported        bool
	chainedCompressionSupported bool
	isMultiChannelCapable       bool

	mu sync.Mutex

	// cachingMu guards the granting of oplocks and leases. Either is only promised to an open
	// that has its file to itself, and only this lock keeps two creates racing for the same
	// file from both finding it free. It is never held while a break is waited for.
	cachingMu sync.Mutex

	// openIndexMu guards the two indexes into the global open table: the opens by the file
	// they are on, and the replayable opens by the create GUID that made them.
	openIndexMu     sync.Mutex
	opensByFile     map[fileKey]map[uint64]*open
	replayableOpens map[replayKey]*open

	// oplockBreakTimeout and leaseBreakTimeout are how long a client is given to acknowledge a
	// break before it loses what it held. They are fields carrying the constants rather than
	// the constants themselves so that a test can wind them down.
	oplockBreakTimeout time.Duration
	leaseBreakTimeout  time.Duration

	// watchInterval is how often a directory being watched is looked at again, and is a field for
	// the same reason: a test of what becomes of a watch cannot wait a quarter of a minute for
	// every look.
	watchInterval time.Duration

	connectionCount map[string]int
	store           stores.Store
	debug           bool
	cfg             stores.IndexdConfig
	ctx             context.Context
}

// newServerState returns a server with its tables in place and nothing running behind it: no
// listener, and no sweep of the durable opens.
//
// It is the half of newServer that the tests share, and the reason to put a new table here
// rather than in the caller. A table created only in newServer is nil in every test, and the
// test that first reaches it fails somewhere far from the omission.
func newServerState(ctx context.Context, db stores.Store, debug bool, cfg stores.IndexdConfig) *server {
	s := &server{
		enabled:              true,
		serverGuid:           uuid.New(),
		shareList:            make(map[string]*share),
		connectionList:       make(map[string]*connection),
		globalOpenTable:      make(map[uint64]*open),
		globalSessionTable:   make(map[uint64]*session),
		globalClientTable:    make(map[[16]byte]*smbClient),
		globalLeaseTableList: make(map[[16]byte]*leaseTable),
		opensByFile:          make(map[fileKey]map[uint64]*open),
		replayableOpens:      make(map[replayKey]*open),
		oplockBreakTimeout:   oplockBreakTimeout,
		leaseBreakTimeout:    leaseBreakTimeout,
		watchInterval:        watchInterval,
		connectionCount:      make(map[string]int),
		store:                db,
		debug:                debug,
		cfg:                  cfg,
		ctx:                  ctx,
	}
	s.stats.Start = time.Now()

	return s
}

// applyCapabilities settles what this build of the server can do, which follows from the highest
// dialect it speaks. It is separate from the tables so that a test can put a server in the state a
// running one is in without starting one.
//
// serverCapabilities is everything the server is able to offer anybody, not what any one client is
// told. What a client is told is this set narrowed to what its dialect allows, which is settled per
// connection at the negotiate ([MS-SMB2] 3.3.5.4) - so a capability taken up here reaches only the
// clients whose dialect has it, and nothing has to remember to hold it back from the rest.
func (s *server) applyCapabilities() {
	if smb2.MaxSupportedDialect != smb2.SMB_DIALECT_202 {
		s.serverCapabilities |= smb2.GLOBAL_CAP_LEASING | smb2.GLOBAL_CAP_LARGE_MTU
	}
	if smb2.Is3X(smb2.MaxSupportedDialect) {
		s.serverCapabilities |= smb2.GLOBAL_CAP_ENCRYPTION | smb2.GLOBAL_CAP_MULTI_CHANNEL
		s.encryptData = true
		s.isMultiChannelCapable = true
	}
	if smb2.MaxSupportedDialect == smb2.SMB_DIALECT_311 {
		s.compressionSupported = true
		s.chainedCompressionSupported = true
	}
}

// newServer returns an initialized SMB server, listening and reaping.
func newServer(ctx context.Context, l net.Listener, db stores.Store, debug bool, cfg stores.IndexdConfig) *server {
	s := newServerState(ctx, db, debug, cfg)

	go s.reapDurableOpens()
	go s.reapConnections()

	return s
}

// Stats returns a snapshot of the current server statistics.
func (s *server) Stats() api.ServerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// newConnectionState returns a Connection object as it stands before a negotiate, with its
// tables in place. Nothing is reading or writing it yet, and the server does not know about it.
//
// It is the half of newConnection that the tests share; the same reasoning applies as for
// newServerState.
//
// The capabilities are left empty rather than taken from the server: the field is what this client
// was told, and until it has negotiated a dialect it has been told nothing.
func (s *server) newConnectionState(clientName string) *connection {
	c := &connection{
		commandSequenceWindow: make(map[uint64]struct{}),
		creditsGranted:        make(map[uint64]uint16),
		interimSent:           make(map[uint64]chan struct{}),
		requestList:           make(map[uint64]*smb2.Request),
		asyncCommandList:      make(map[uint64]*smb2.Request),
		pendingResponses:      make(map[uint64]smb2.GenericResponse),
		requestOpens:          make(map[uint64]*open),
		sessionTable:          make(map[uint64]*session),
		preauthSessionTable:   make(map[uint64]*preauthSession),
		negotiateDialect:      smb2.SMB_DIALECT_UNKNOWN,
		dialect:               "Unknown",
		clientName:            clientName,
		creationTime:          time.Now(),
		maxTransactSize:       smb2.MaxTransactSize,
		maxReadSize:           smb2.MaxReadSize,
		maxWriteSize:          smb2.MaxWriteSize,
		serverSecurityMode:    smb2.NEGOTIATE_SIGNING_ENABLED,
		server:                s,
		writeChan:             make(chan []byte),
		closeChan:             make(chan struct{}),
		stopChans:             make(map[uint64]chan struct{}),
	}

	// The first request a client may send is the one that opens the window.
	c.commandSequenceWindow[0] = struct{}{}

	return c
}

// newConnection creates a new Connection object over a transport and starts serving it.
func (s *server) newConnection(conn net.Conn) *connection {
	c := s.newConnectionState(conn.RemoteAddr().String())
	c.conn = conn

	s.mu.Lock()
	s.connectionList[c.clientName] = c
	s.mu.Unlock()

	go c.sendResponses()
	go c.processRequests()

	return c
}

// grantOnResponse settles the credits the response goes out with: no more than the window was opened
// by for the request it answers, and no more than the response already meant to grant. The grant is
// taken by the first response to carry it, so an interim response hands the credits back and the final
// response of the same request grants nothing - which is what [MS-SMB2] 3.3.4.2 has the two of them do.
//
// Nothing is done for a message the server sends of its own accord: a break notification answers no
// request, and has no credits of anybody's to hand back.
func (c *connection) grantOnResponse(resp smb2.GenericResponse) {
	if resp.Header().Command() == smb2.SMB2_OPLOCK_BREAK && resp.Header().Status() == smb2.STATUS_OK {
		return
	}

	mid := resp.Header().MessageID()

	c.mu.Lock()
	granted, found := c.creditsGranted[mid]
	if found {
		delete(c.creditsGranted, mid)
	}
	c.mu.Unlock()

	if !found {
		return
	}

	// The credit field of a response sits where a request carries the credits it asks for.
	if carried := resp.Header().CreditRequest(); carried < granted {
		granted = carried
	}

	resp.Header().SetCreditResponse(granted)
}

// closeConnection destroys the Connection object.
func (s *server) closeConnection(c *connection) {
	s.mu.Lock()
	delete(s.connectionList, c.clientName)
	s.mu.Unlock()

	// The connection is no longer a channel of any of the sessions it carried.
	c.mu.Lock()
	sessions := make([]*session, 0, len(c.sessionTable))
	for _, ss := range c.sessionTable {
		sessions = append(sessions, ss)
	}
	// Every request still being worked on is told to give up, and its channel goes from the
	// table as it is closed. A connection is torn down by whoever notices it first, and more than
	// one thing may: the reading loop finds the socket gone, the reaper finds the connection idle,
	// a ban takes down everything from the host. All of them come through here, so a table left
	// holding channels that have already been closed is a table the next of them closes twice,
	// which is a panic rather than an error. The cancel path clears its entry the same way, and
	// only the two of them ever close one of these channels.
	for id, ch := range c.stopChans {
		close(ch)
		delete(c.stopChans, id)
	}
	c.mu.Unlock()

	for _, ss := range sessions {
		ss.unbindChannel(c)
	}

	// A session that still has a channel left carries on over it. One that has just lost its
	// last channel is destroyed: nothing can reach it any more, so its opens, its tree
	// connects and the session itself all go. In the dialects without channels the list is
	// always empty, so every session the connection carried is destroyed with it, which is
	// the only outcome those dialects allow.
	for _, ss := range sessions {
		ss.mu.Lock()
		empty := len(ss.channelList) == 0
		ss.mu.Unlock()

		if empty {
			// A durable open is what the client asked to keep across exactly this kind of
			// loss, so it is set aside before the session is torn down around it.
			if n := ss.orphanDurableOpens(); n > 0 && s.debug {
				log.Printf("Keeping %d durable handle(s) of session %d for reclaiming", n, ss.sessionID)
			}
			s.deregisterSession(c, ss.sessionID)
		}
	}

	c.conn.Close()
	c.once.Do(func() { close(c.closeChan) })
}

// encodeResponse encodes the response and applies whatever protection the session calls for.
func (s *server) encodeResponse(c *connection, ss *session, resp smb2.GenericResponse) []byte {
	wipeSignatures := func(msg []byte) {
		var off uint32
		var zero [16]byte
		for {
			next := binary.LittleEndian.Uint32(msg[off+20 : off+24])
			copy(msg[off+48:off+64], zero[:])
			smb2.Header(msg[off:]).ClearFlag(smb2.FLAGS_SIGNED)
			off += next
			if next == 0 {
				return
			}
		}
	}

	buf := resp.Encode()

	if ss != nil && ss.stateNow() == sessionValid { // A session exists, sign if required
		if resp.ShouldEncrypt() {
			wipeSignatures(buf)
			if resp.MayCompress() {
				buf = c.compress(buf)
			}
			buf = ss.encrypt(buf, c)
		} else if resp.Header().Command() != smb2.SMB2_SESSION_SETUP && ss.encryptData {
			wipeSignatures(buf)
			if resp.MayCompress() {
				buf = c.compress(buf)
			}
			buf = ss.encrypt(buf, c)
		} else if resp.Header().Command() == smb2.SMB2_SESSION_SETUP || resp.Header().IsFlagSet(smb2.FLAGS_SIGNED) {
			ss.sign(buf, c)
		} else { // Otherwise, wipe the signature(s)
			wipeSignatures(buf)
		}
	}

	return buf
}

// writeResponse encodes the response and adds it to the sending queue.
func (s *server) writeResponse(c *connection, ss *session, resp smb2.GenericResponse) {
	buf := s.encodeResponse(c, ss, resp)

	// The preauthentication integrity hash of a binding exchange covers the interim response
	// exactly as it goes on the wire, signature included, so it can only be updated once the
	// message is complete. There is no such hash outside of a binding exchange, which makes
	// this a no-op for an ordinary session setup.
	if resp.Header().Command() == smb2.SMB2_SESSION_SETUP && resp.Header().Status() == smb2.STATUS_MORE_PROCESSING_REQUIRED {
		c.updatePreauthHash(resp.Header().SessionID(), buf)
	}

	c.writeChan <- buf

	s.mu.Lock()
	s.stats.BytesSent += uint64(len(buf))
	s.mu.Unlock()
}

// trySendResponse is writeResponse for a message the server sends on its own initiative rather
// than in reply to a request. It reports whether the message was handed over for sending, and
// gives up instead of waiting forever when the connection has already gone: nothing drains the
// sending queue of a connection whose sender has stopped.
func (s *server) trySendResponse(c *connection, ss *session, resp smb2.GenericResponse) bool {
	// A connection that is already gone is given up on before anything is queued for it: the
	// two cases of the select below are picked between at random whenever both are ready, so
	// a message would otherwise stand a chance of being queued for a connection that will
	// never send it.
	select {
	case <-c.closeChan:
		return false
	default:
	}

	// Granted here as well as where a chain is answered: the final response of an asynchronous read
	// or write goes out this way, and its credits would otherwise stay with the request it answers.
	c.grantOnResponse(resp)

	buf := s.encodeResponse(c, ss, resp)

	select {
	case c.writeChan <- buf:
	case <-c.closeChan:
		return false
	}

	s.mu.Lock()
	s.stats.BytesSent += uint64(len(buf))
	s.mu.Unlock()

	return true
}

// enumShares generates a NetShareInfo Type 1 structure for each available share.
func (s *server) enumShares() []rpc.NetShareInfo1 {
	var shares []rpc.NetShareInfo1
	for _, sh := range s.shareList {
		share := rpc.NetShareInfo1{
			Share:   sh.name,
			Type:    rpc.STYPE_DISKTREE,
			Comment: sh.remark,
		}

		shares = append(shares, share)
	}

	// Add the "imaginary" IPC (Inter-Protocol Communication) share.
	shares = append(shares, rpc.NetShareInfo1{
		Share:   "IPC$",
		Type:    rpc.STYPE_IPC_HIDDEN,
		Comment: "IPC service",
	})

	return shares
}
