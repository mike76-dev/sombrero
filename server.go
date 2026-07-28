package main

import (
	"context"
	"encoding/binary"
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
	connectionList              map[string]*connection
	serverGuid                  [16]byte
	serverCapabilities          uint32
	globalClientTable           map[[16]byte]*smbClient
	encryptData                 bool
	compressionSupported        bool
	chainedCompressionSupported bool
	isMultiChannelCapable       bool

	// Auxiliary fields.
	listener        net.Listener
	mu              sync.Mutex
	connectionCount map[string]int
	store           stores.Store
	debug           bool
	cfg             stores.IndexdConfig
	ctx             context.Context
}

// newServer returns an initialized SMB server.
func newServer(ctx context.Context, l net.Listener, db stores.Store, debug bool, cfg stores.IndexdConfig) *server {
	s := &server{
		enabled:            true,
		serverGuid:         uuid.New(),
		shareList:          make(map[string]*share),
		connectionList:     make(map[string]*connection),
		globalOpenTable:    make(map[uint64]*open),
		globalSessionTable: make(map[uint64]*session),
		globalClientTable:  make(map[[16]byte]*smbClient),
		listener:           l,
		connectionCount:    make(map[string]int),
		store:              db,
		debug:              debug,
		cfg:                cfg,
		ctx:                ctx,
	}
	s.stats.Start = time.Now()
	return s
}

// Stats returns a snapshot of the current server statistics.
func (s *server) Stats() api.ServerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// newConnection creates a new Connection object.
func (s *server) newConnection(conn net.Conn) *connection {
	c := &connection{
		commandSequenceWindow: make(map[uint64]struct{}),
		requestList:           make(map[uint64]*smb2.Request),
		asyncCommandList:      make(map[uint64]*smb2.Request),
		pendingResponses:      make(map[uint64]smb2.GenericResponse),
		requestOpens:          make(map[uint64]*open),
		sessionTable:          make(map[uint64]*session),
		preauthSessionTable:   make(map[uint64]*preauthSession),
		conn:                  conn,
		negotiateDialect:      smb2.SMB_DIALECT_UNKNOWN,
		dialect:               "Unknown",
		clientName:            conn.RemoteAddr().String(),
		creationTime:          time.Now(),
		maxTransactSize:       smb2.MaxTransactSize,
		maxReadSize:           smb2.MaxReadSize,
		maxWriteSize:          smb2.MaxWriteSize,
		serverCapabilities:    s.serverCapabilities,
		serverSecurityMode:    smb2.NEGOTIATE_SIGNING_ENABLED,
		server:                s,
		writeChan:             make(chan []byte),
		closeChan:             make(chan struct{}),
		stopChans:             make(map[uint64]chan struct{}),
	}

	c.mu.Lock()
	c.commandSequenceWindow[0] = struct{}{}
	c.mu.Unlock()

	s.mu.Lock()
	s.connectionList[c.clientName] = c
	s.mu.Unlock()

	go c.sendResponses()
	go c.processRequests()

	return c
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
	for _, ch := range c.stopChans {
		close(ch)
	}
	c.mu.Unlock()

	for _, ss := range sessions {
		ss.unbindChannel(c)
	}

	// A session that still has a channel left carries on over it, and so do its opens. Only
	// the opens of a session that has just lost its last channel are abandoned. In the
	// dialects without channels the list is always empty, so every session the connection
	// carried loses its opens, which is the only outcome those dialects allow.
	for _, ss := range sessions {
		ss.mu.Lock()
		if len(ss.channelList) > 0 {
			ss.mu.Unlock()
			continue
		}
		opens := make([]*open, 0, len(ss.openTable))
		for _, op := range ss.openTable {
			opens = append(opens, op)
		}
		ss.mu.Unlock()

		for _, op := range opens {
			op.mu.Lock()
			if op.cancel != nil {
				op.cancel()
			}
			op.mu.Unlock()
		}
	}

	c.conn.Close()
	c.once.Do(func() { close(c.closeChan) })
}

// writeResponse encodes the response and adds it to the sending queue.
func (s *server) writeResponse(c *connection, ss *session, resp smb2.GenericResponse) {
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

	if ss != nil && ss.state == sessionValid { // A session exists, sign if required
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
