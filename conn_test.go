package main

import (
	"encoding/binary"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/utils"
)

// newTestRequest builds a bare SMB2 request with the given command, channel sequence, and
// replay flag, which is all that the verification of the channel sequence number looks at.
func newTestRequest(t *testing.T, command uint16, cs uint16, replay bool) *smb2.Request {
	t.Helper()

	buf := make([]byte, smb2.SMB2HeaderSize)
	h := smb2.Header(buf)
	h.SetProtocolID(smb2.PROTOCOL_SMB2)
	binary.LittleEndian.PutUint16(buf[4:6], smb2.SMB2HeaderStructureSize)
	h.SetCommand(command)
	h.SetMessageID(1)
	binary.LittleEndian.PutUint16(buf[8:10], cs)
	if replay {
		h.SetFlag(smb2.FLAGS_REPLAY_OPERATION)
	}

	reqs, err := smb2.GetRequests(buf, 0, false)
	if err != nil {
		t.Fatalf("couldn't build request: %v", err)
	}

	return reqs[0]
}

func TestVerifyChannelSequence(t *testing.T) {
	tests := []struct {
		name string

		// The state of the Open before the request.
		channelSequence uint16
		outstanding     uint32
		previous        uint32

		// The request.
		command uint16
		cs      uint16
		replay  bool

		// The expected outcome.
		status          uint32
		wantSequence    uint16
		wantOutstanding uint32
		wantPrevious    uint32
		wantCounted     bool
	}{
		{
			name:            "same sequence",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_WRITE, cs: 7,
			status: smb2.STATUS_OK, wantSequence: 7, wantOutstanding: 3, wantPrevious: 1, wantCounted: true,
		},
		{
			name:            "newer sequence carries the outstanding requests over",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_WRITE, cs: 9,
			status: smb2.STATUS_OK, wantSequence: 9, wantOutstanding: 1, wantPrevious: 3, wantCounted: true,
		},
		{
			name:            "newer sequence wrapping around",
			channelSequence: 0xffff, outstanding: 2, previous: 0,
			command: smb2.SMB2_WRITE, cs: 1,
			status: smb2.STATUS_OK, wantSequence: 1, wantOutstanding: 1, wantPrevious: 2, wantCounted: true,
		},
		{
			name:            "older sequence fails a write",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_WRITE, cs: 6,
			status: smb2.STATUS_FILE_NOT_AVAILABLE, wantSequence: 7, wantOutstanding: 2, wantPrevious: 1, wantCounted: false,
		},
		{
			name:            "older sequence lets a read through uncounted",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_READ, cs: 6,
			status: smb2.STATUS_OK, wantSequence: 7, wantOutstanding: 2, wantPrevious: 1, wantCounted: false,
		},
		{
			name:            "replay of the same sequence with nothing pending",
			channelSequence: 7, outstanding: 2, previous: 0,
			command: smb2.SMB2_WRITE, cs: 7, replay: true,
			status: smb2.STATUS_OK, wantSequence: 7, wantOutstanding: 3, wantPrevious: 0, wantCounted: true,
		},
		{
			name:            "replay of the same sequence with requests pending",
			channelSequence: 7, outstanding: 2, previous: 1,
			command: smb2.SMB2_SET_INFO, cs: 7, replay: true,
			status: smb2.STATUS_FILE_NOT_AVAILABLE, wantSequence: 7, wantOutstanding: 2, wantPrevious: 1, wantCounted: false,
		},
		{
			name:            "replay of a newer sequence with nothing pending",
			channelSequence: 7, outstanding: 0, previous: 0,
			command: smb2.SMB2_IOCTL, cs: 8, replay: true,
			status: smb2.STATUS_OK, wantSequence: 8, wantOutstanding: 1, wantPrevious: 0, wantCounted: true,
		},
		{
			name:            "replay of a newer sequence with requests pending",
			channelSequence: 7, outstanding: 2, previous: 0,
			command: smb2.SMB2_IOCTL, cs: 8, replay: true,
			status: smb2.STATUS_FILE_NOT_AVAILABLE, wantSequence: 8, wantOutstanding: 0, wantPrevious: 2, wantCounted: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &connection{
				negotiateDialect: smb2.SMB_DIALECT_311,
				requestOpens:     make(map[uint64]*open),
			}
			op := &open{
				file:                            &fileState{},
				channelSequence:                 test.channelSequence,
				outstandingRequestCount:         test.outstanding,
				outstandingPreviousRequestCount: test.previous,
			}
			req := newTestRequest(t, test.command, test.cs, test.replay)

			status := c.verifyChannelSequence(op, req)
			if status != test.status {
				t.Errorf("status = %#x, want %#x", status, test.status)
			}
			if op.channelSequence != test.wantSequence {
				t.Errorf("Open.ChannelSequence = %d, want %d", op.channelSequence, test.wantSequence)
			}
			if op.outstandingRequestCount != test.wantOutstanding {
				t.Errorf("Open.OutstandingRequestCount = %d, want %d", op.outstandingRequestCount, test.wantOutstanding)
			}
			if op.outstandingPreviousRequestCount != test.wantPrevious {
				t.Errorf("Open.OutstandingPreRequestCount = %d, want %d", op.outstandingPreviousRequestCount, test.wantPrevious)
			}

			// A counted request must be remembered, so that the counter it was added to is
			// decremented when the response goes out; an uncounted one must not be.
			_, remembered := c.requestOpens[req.Header().MessageID()]
			if remembered != test.wantCounted {
				t.Errorf("request remembered = %v, want %v", remembered, test.wantCounted)
			}
		})
	}
}

func TestVerifyChannelSequenceSkippedForOldDialects(t *testing.T) {
	c := &connection{
		negotiateDialect: smb2.SMB_DIALECT_21,
		requestOpens:     make(map[uint64]*open),
	}
	op := &open{file: &fileState{}, channelSequence: 7, outstandingRequestCount: 2}
	req := newTestRequest(t, smb2.SMB2_WRITE, 9, false)

	if status := c.verifyChannelSequence(op, req); status != smb2.STATUS_OK {
		t.Fatalf("status = %#x, want STATUS_OK", status)
	}
	if op.channelSequence != 7 || op.outstandingRequestCount != 2 {
		t.Errorf("the counters of the Open were touched: %d, %d", op.channelSequence, op.outstandingRequestCount)
	}
	if len(c.requestOpens) != 0 {
		t.Error("the request was remembered, but it wasn't counted")
	}
}

// TestDialectName pins the spelling of each dialect. The name is not decoration: several rules
// are written as a comparison against it, so a wrong string here silently turns a per-dialect
// check off.
func TestDialectName(t *testing.T) {
	names := map[uint16]string{
		smb2.SMB_DIALECT_202:     "2.0.2",
		smb2.SMB_DIALECT_21:      "2.1",
		smb2.SMB_DIALECT_30:      "3.0",
		smb2.SMB_DIALECT_302:     "3.0.2",
		smb2.SMB_DIALECT_311:     "3.1.1",
		smb2.SMB_DIALECT_UNKNOWN: "Unknown",
	}

	for dialect, want := range names {
		if got := dialectName(dialect); got != want {
			t.Errorf("dialectName(%#x) = %q, want %q", dialect, got, want)
		}
	}
}

// negotiateRequest builds the bytes of an SMB2_NEGOTIATE request offering the given dialects, which
// is the one thing about it the choice of dialect turns on.
//
// A request that offers 3.1.1 carries the negotiate contexts that dialect settles its terms in: the
// preauthentication integrity hash, without which the server turns the request away before it
// settles a capability at all, and a cipher to encrypt with. The list of dialects is padded to a
// multiple of eight, which is where the contexts have to start, and each context to the same.
func negotiateRequest(capabilities uint32, dialects ...uint16) []byte {
	var contexts []byte
	var count uint16
	for _, dialect := range dialects {
		if dialect == smb2.SMB_DIALECT_311 {
			contexts = smb2.PreauthIntegrityCapabilities(make([]byte, 32))
			contexts = append(contexts, make([]byte, utils.Roundup(len(contexts), 8)-len(contexts))...)
			contexts = append(contexts, smb2.EncryptionCapabilities(smb2.AES_128_GCM)...)
			count = 2
			break
		}
	}

	listed := utils.Roundup(smb2.SMB2NegotiateRequestMinSize+2*len(dialects), 8)
	msg := make([]byte, smb2.SMB2HeaderSize+listed+len(contexts))
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_NEGOTIATE)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2NegotiateRequestStructureSize)
	binary.LittleEndian.PutUint16(body[2:4], uint16(len(dialects)))
	binary.LittleEndian.PutUint32(body[8:12], capabilities)
	for i, dialect := range dialects {
		off := smb2.SMB2NegotiateRequestMinSize + i*2
		binary.LittleEndian.PutUint16(body[off:off+2], dialect)
	}

	if len(contexts) > 0 {
		// The offset of the contexts is from the start of the message, not of the body.
		binary.LittleEndian.PutUint32(body[28:32], uint32(smb2.SMB2HeaderSize+listed))
		binary.LittleEndian.PutUint16(body[32:34], count)
		copy(body[listed:], contexts)
	}

	return msg
}

// negotiatedCapabilities runs a negotiate over a bare connection, with the client asking for the
// given capabilities, and returns the capabilities and the dialect the server answered with - which
// is what this client is told the server can do.
func (h *smbTest) negotiatedCapabilities(t *testing.T, asks uint32, dialects ...uint16) (uint32, uint16) {
	t.Helper()

	// The server is put in the state a running one is in, by the same call that puts it there,
	// so that what is settled once for the whole server is under test here rather than restated.
	h.srv.applyCapabilities()

	c := h.newTestConnection("negotiating")
	c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

	resp, _, err := c.processRequest(request(t, negotiateRequest(asks, dialects...)))
	if err != nil {
		t.Fatalf("the server gave up on the negotiate: %v", err)
	}
	if status := resp.Header().Status(); status != smb2.STATUS_OK {
		t.Fatalf("the negotiate was answered with %#x", status)
	}

	buf := resp.Encode()

	return binary.LittleEndian.Uint32(buf[smb2.SMB2HeaderSize+24 : smb2.SMB2HeaderSize+28]),
		binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize+4 : smb2.SMB2HeaderSize+6])
}

// TestIntegrationNegotiateAdvertisesWhatTheDialectHas is what the server answers a negotiate with:
// everything it can do, narrowed to what the settled dialect allows ([MS-SMB2] 3.3.5.4). The
// narrowing is what is under test - a capability the server takes up must reach the dialects that
// have it and no others, without anything having to remember to hold it back.
//
// The whole set is checked rather than a bit at a time, so that a capability leaking onto a dialect
// that has no such thing fails here whichever capability it is.
func TestIntegrationNegotiateAdvertisesWhatTheDialectHas(t *testing.T) {
	const leasing = smb2.GLOBAL_CAP_LEASING | smb2.GLOBAL_CAP_LARGE_MTU

	for _, tt := range []struct {
		name     string
		dialects []uint16
		want     uint32
	}{
		// 2.0.2 predates every capability this server has to offer.
		{"2.0.2 alone", []uint16{smb2.SMB_DIALECT_202}, 0},

		// 2.1 brought leases and the large MTU, and has neither channels nor encryption.
		{"2.1", []uint16{smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_21}, leasing},

		// 3.0.2 is where encryption is carried as a capability. Channels are left out of what is
		// expected because this client does not ask for them; the case below is the one that does.
		{"3.0.2", []uint16{smb2.SMB_DIALECT_302}, leasing | smb2.GLOBAL_CAP_ENCRYPTION},

		// 3.1.1 settles a cipher in a negotiate context instead, so the capability is not carried.
		{"3.1.1", []uint16{smb2.SMB_DIALECT_202, smb2.SMB_DIALECT_311}, leasing},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			caps, dialect := h.negotiatedCapabilities(t, 0, tt.dialects...)

			if caps != tt.want {
				t.Errorf("the server answered %#x on dialect %#x, want %#x", caps, dialect, tt.want)
			}
		})
	}
}

// TestIntegrationNegotiateOffersChannelsToAClientThatAsks is the one capability that turns on what
// the client said as well as on the dialect: there is no use offering to bind a second connection
// to a client that never said it could.
func TestIntegrationNegotiateOffersChannelsToAClientThatAsks(t *testing.T) {
	for _, tt := range []struct {
		name string
		asks uint32
		want bool
	}{
		{"the client asks", smb2.GLOBAL_CAP_MULTI_CHANNEL, true},
		{"the client says nothing", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			caps, dialect := h.negotiatedCapabilities(t, tt.asks, smb2.SMB_DIALECT_311)

			if got := caps&smb2.GLOBAL_CAP_MULTI_CHANNEL != 0; got != tt.want {
				t.Errorf("the server answered %#x on dialect %#x: channels = %v, want %v", caps, dialect, got, tt.want)
			}
		})
	}
}

// TestIntegrationTheCapabilitiesOfAConnectionAreWhatWentOut is the invariant behind
// FSCTL_VALIDATE_NEGOTIATE_INFO: the capabilities the server answers that request with are read
// off the connection, and the client holds them against the NEGOTIATE response it saw. A
// connection that took a capability up afterwards would answer with a set that never went on the
// wire, and the client drops the connection - which is the whole purpose of the request.
//
// 3.1.1 is where the two used to come apart. Its cipher is agreed in a negotiate context after the
// response is built, and taking the encryption capability up along with the cipher left the
// connection holding one more capability than it had sent. Windows does not send this request on
// 3.1.1, so nothing ever tripped over it.
func TestIntegrationTheCapabilitiesOfAConnectionAreWhatWentOut(t *testing.T) {
	for _, tt := range []struct {
		name    string
		dialect uint16
	}{
		{"3.1.1", smb2.SMB_DIALECT_311},
		{"3.0.2", smb2.SMB_DIALECT_302},
		{"2.1", smb2.SMB_DIALECT_21},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			h.srv.applyCapabilities()

			c := h.newTestConnection("negotiating")
			c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

			// The client offers a cipher, which is what a 3.1.1 negotiate settles one from.
			msg := negotiateRequest(0, tt.dialect)
			resp, _, err := c.processRequest(request(t, msg))
			if err != nil {
				t.Fatalf("the server gave up on the negotiate: %v", err)
			}

			sent := binary.LittleEndian.Uint32(resp.Encode()[smb2.SMB2HeaderSize+24 : smb2.SMB2HeaderSize+28])
			if c.serverCapabilities != sent {
				t.Errorf("the connection holds %#x, but %#x went out in the negotiate response",
					c.serverCapabilities, sent)
			}
		})
	}
}

// TestIntegrationNegotiateSettlesACipher is what a connection is left able to encrypt with. The
// cipher is the one field that answers it, whichever dialect settled it: 3.1.1 agrees one in a
// negotiate context, and the dialects before it have only ever had AES-128-CCM, which the negotiate
// writes down so that nothing has to work the cipher out from the dialect a second time.
//
// A dialect that stopped recording its cipher would not fail loudly - it would authenticate, and
// then quietly serve a session in the clear that was meant to be encrypted.
func TestIntegrationNegotiateSettlesACipher(t *testing.T) {
	for _, tt := range []struct {
		name    string
		dialect uint16
		want    uint16
	}{
		// 3.1.1 takes the cipher the client offered, which the request carries as a context.
		{"3.1.1", smb2.SMB_DIALECT_311, smb2.AES_128_GCM},

		// 3.0 and 3.0.2 have no context to carry one, and the one cipher they have is written down.
		{"3.0.2", smb2.SMB_DIALECT_302, smb2.AES_128_CCM},
		{"3.0", smb2.SMB_DIALECT_30, smb2.AES_128_CCM},

		// Encryption arrived with 3.x, so the dialects before it settle nothing.
		{"2.1", smb2.SMB_DIALECT_21, 0},
		{"2.0.2", smb2.SMB_DIALECT_202, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			h.srv.applyCapabilities()

			c := h.newTestConnection("negotiating")
			c.ntlmServer = ntlm.NewServer("SERVER", "", h.srv.store)

			if _, _, err := c.processRequest(request(t, negotiateRequest(0, tt.dialect))); err != nil {
				t.Fatalf("the server gave up on the negotiate: %v", err)
			}

			if c.cipherID != tt.want {
				t.Errorf("the connection settled on cipher %#x, want %#x", c.cipherID, tt.want)
			}
		})
	}
}

// dispatcherState returns the scheduler state of the goroutine running processRequests, as the
// runtime writes it in a stack dump: "select", "chan receive", "runnable" and so on.
func dispatcherState(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]

	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "processRequests") {
			continue
		}
		_, rest, ok := strings.Cut(g, "[")
		if !ok {
			continue
		}
		state, _, _ := strings.Cut(rest, "]")

		// The state carries how long it has been in it once that gets long enough to mention.
		state, _, _ = strings.Cut(state, ",")

		return state
	}

	return ""
}

// TestTheDispatcherWaitsInsteadOfPolling is what an idle connection costs. The dispatcher used to
// look at the queue in a loop with nothing to make it wait, so a connection with no traffic on it
// spun a core for as long as it lived. It has to be parked on a channel when there is nothing to do.
func TestTheDispatcherWaitsInsteadOfPolling(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	go cl.conn.processRequests()

	// Give it a moment to work through anything the dial left and settle.
	time.Sleep(100 * time.Millisecond)

	if state := dispatcherState(t); state != "select" {
		t.Errorf("the dispatcher of an idle connection is %q, want it parked in a select", state)
	}

	// And it still picks up what arrives once it is parked.
	mid := cl.conn.window()[0]
	if err := cl.conn.acceptRequest(echoRequest(mid, cl.ss.sessionID, cl.tc.treeID)); err != nil {
		t.Fatalf("the request was not accepted: %v", err)
	}
	if buf := cl.recv(5 * time.Second); smb2.Header(buf).Status() != smb2.STATUS_OK {
		t.Errorf("the parked dispatcher answered %#x, want it woken and the request served", smb2.Header(buf).Status())
	}
}

// TestAChainWhoseLastRequestIsTurnedAwayIsStillAnswered is the compound chain that loses its last
// request to a signature it cannot prove. The chain is assembled as its requests are answered and
// goes out when the last of them arrives, so a last request that never reaches the dispatcher
// leaves the whole chain sitting unsent: the requests before it are answered by nobody, and what
// was assembled for them is held for as long as the connection lives.
func TestAChainWhoseLastRequestIsTurnedAwayIsStillAnswered(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice").signing()

	// Two related requests. The first is signed and the second is not, on a session that has
	// every request signed, so the second is turned away where it is read.
	first := append(echoRequest(0, cl.ss.sessionID, cl.tc.treeID), make([]byte, 4)...)
	second := echoRequest(1, cl.ss.sessionID, cl.tc.treeID)
	smb2.Header(second).SetFlag(smb2.FLAGS_RELATED_OPERATIONS)
	smb2.Header(first).SetNextCommand(uint32(len(first)))
	signed(t, first, cl.ss.signingKey, smb2.SMB_DIALECT_311, 0)

	go cl.conn.processRequests()

	if err := cl.conn.acceptRequest(append(first, second...)); err != nil {
		t.Fatalf("the chain was not accepted: %v", err)
	}

	answered := make(map[uint64]uint32)
	for range 2 {
		buf := cl.recv(5 * time.Second)
		answered[smb2.Header(buf).MessageID()] = smb2.Header(buf).Status()
	}

	if status, ok := answered[1]; !ok || status != smb2.STATUS_ACCESS_DENIED {
		t.Errorf("the unsigned request was answered %#x (present: %v), want it turned away", status, ok)
	}
	if status, ok := answered[0]; !ok || status != smb2.STATUS_OK {
		t.Errorf("the request before it was answered %#x (present: %v), want it answered", status, ok)
	}

	cl.conn.mu.Lock()
	defer cl.conn.mu.Unlock()
	if n := len(cl.conn.pendingResponses); n != 0 {
		t.Errorf("%d chain(s) left assembled and unsent", n)
	}
	if n := len(cl.conn.chainRemaining); n != 0 {
		t.Errorf("%d chain(s) still counted as outstanding", n)
	}
}

// TestAChainMemberInheritsTheFailureBeforeIt is the compound chain whose first operation fails. The
// operations after it name no handle of their own — they take the one the operation before them
// used — so a create that failed leaves them with nothing to work on. [MS-SMB2] 3.3.5.2.7.2 has
// them answered with the error the create gave, and with STATUS_INVALID_HANDLE when the operation
// before them produced no handle at all. Answering STATUS_FILE_CLOSED instead tells the client a
// handle it never got has been closed.
func TestAChainMemberInheritsTheFailureBeforeIt(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	// A create that fails, and a query on the handle it did not make.
	first := createRequest(0, cl.ss.sessionID, cl.tc.treeID, "missing.bin", smb2.OPLOCK_LEVEL_NONE,
		smb2.FILE_OPEN, writeAccess, nil)
	first = append(first, make([]byte, utils.Roundup(len(first), 8)-len(first))...)
	second := closeRequest(1, cl.ss.sessionID, cl.tc.treeID, make([]byte, 16))
	smb2.Header(second).SetFlag(smb2.FLAGS_RELATED_OPERATIONS)
	smb2.Header(first).SetNextCommand(uint32(len(first)))

	go cl.conn.processRequests()

	if err := cl.conn.acceptRequest(append(first, second...)); err != nil {
		t.Fatalf("the chain was not accepted: %v", err)
	}

	buf := cl.recv(5 * time.Second)

	// The chain goes out as one message: the create's answer, then the close's behind it.
	createStatus := smb2.Header(buf).Status()
	if createStatus == smb2.STATUS_OK {
		t.Fatalf("the create of a file that is not there succeeded")
	}

	next := smb2.Header(buf).NextCommand()
	if next == 0 || uint64(next)+smb2.SMB2HeaderSize > uint64(len(buf)) {
		t.Fatalf("the chain carries no second response (NextCommand %d of %d bytes)", next, len(buf))
	}

	if status := smb2.Header(buf[next:]).Status(); status != createStatus {
		t.Errorf("the operation after the failed create was answered %#x, want the %#x the create gave",
			status, createStatus)
	}
}

// TestAMismatchedPersistentIDIsRefused is the handle whose two halves disagree. A FileId names an
// open by its volatile half and proves it with the persistent one, and [MS-SMB2] 3.3.5.10 refuses
// the request when the two do not agree — the check that keeps a reused volatile ID from being
// taken for the handle that held it before.
func TestAMismatchedPersistentIDIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	// The volatile half of a handle that exists, with a persistent half that belongs to nothing.
	forged := make([]byte, 16)
	copy(forged, fid)
	binary.LittleEndian.PutUint64(forged[8:], binary.LittleEndian.Uint64(fid[8:])^0xdeadbeef)

	cl.mid++
	resp, err := cl.send(closeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, forged))
	if err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	if status := resp.Header().Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("a handle whose persistent half does not match was answered %#x, want STATUS_FILE_CLOSED", status)
	}
}
