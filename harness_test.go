package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"go.sia.tech/renterd/v2/api"
)

var errNoObject = errors.New("no such object")

// fakeClient stands in for the object store behind a share. Only what the create path reaches
// for is answered; the rest of the interface is here to satisfy it.
type fakeClient struct {
	mu      sync.Mutex
	objects map[string]client.ObjectInfo
}

func newFakeClient() *fakeClient {
	return &fakeClient{objects: make(map[string]client.ObjectInfo)}
}

// put makes a file appear in the store, as if it had been uploaded out of band.
func (fc *fakeClient) put(path string, size uint64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	now := time.Now()
	fc.objects[path] = client.ObjectInfo{
		Key:        "/" + path,
		Size:       size,
		CreatedAt:  now,
		ModifiedAt: now,
	}
}

func (fc *fakeClient) Object(_ context.Context, _ stores.Account, path string) (client.ObjectInfo, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	oi, found := fc.objects[path]
	if !found {
		return client.ObjectInfo{}, errNoObject
	}

	return oi, nil
}

func (fc *fakeClient) List(_ context.Context, _ stores.Account, path string) ([]client.ObjectInfo, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	var ois []client.ObjectInfo
	for _, oi := range fc.objects {
		ois = append(ois, oi)
	}

	return ois, nil
}

func (fc *fakeClient) MakeDirectory(_ context.Context, _ stores.Account, path string) error {
	fc.put(path+"/", 0)
	return nil
}

func (fc *fakeClient) Info(context.Context) (client.GeneralInfo, error) {
	return client.GeneralInfo{}, nil
}

func (fc *fakeClient) Storage(context.Context) (client.StorageInfo, error) {
	return client.StorageInfo{}, nil
}

func (fc *fakeClient) IsEmpty(context.Context, stores.Account, string) (bool, error) {
	return true, nil
}

func (fc *fakeClient) Parents(context.Context, stores.Account, string) (client.FileInfo, client.FileInfo, error) {
	return client.FileInfo{}, client.FileInfo{}, nil
}

func (fc *fakeClient) Read(context.Context, stores.Account, string, uint64, uint64, io.Writer) error {
	return nil
}

func (fc *fakeClient) StartUpload(context.Context, stores.Account, string) (string, error) {
	return "upload", nil
}

func (fc *fakeClient) AbortUpload(context.Context, string, string) error { return nil }

func (fc *fakeClient) FinishUpload(context.Context, string, string, []api.MultipartCompletedPart) error {
	return nil
}

func (fc *fakeClient) Write(context.Context, io.Reader, string, string, int, uint64, uint64) (string, error) {
	return "etag", nil
}

func (fc *fakeClient) Delete(context.Context, stores.Account, string, bool) error { return nil }

func (fc *fakeClient) Rename(context.Context, stores.Account, string, string, bool, bool) error {
	return nil
}

func (fc *fakeClient) DeleteAll(context.Context) error { return nil }

func (fc *fakeClient) Close() error { return nil }

// smbTest is a server with a single share behind a fake object store, driven by hand-built
// SMB2 messages.
//
// It stops short of the transport. There is no negotiate and no session setup, so signing,
// encryption and authentication are out of the picture; requests are handed to the dispatcher
// the way the reading loop of a connection would hand them over, and responses are read back
// either as the dispatcher returns them or off the sending queue of the connection, which is
// where an asynchronous answer and a break notification both land.
type smbTest struct {
	t         *testing.T
	srv       *server
	share     *share
	files     *fakeClient
	workgroup string
}

func newSMBTest(t *testing.T) *smbTest {
	t.Helper()

	store, err := stores.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatalf("could not create the store: %v", err)
	}
	t.Cleanup(store.Close)

	wg := uuid.New()
	if err := store.AddWorkgroup(stores.Workgroup{UUID: wg, Name: "testwg"}); err != nil {
		t.Fatalf("could not add the workgroup: %v", err)
	}
	for _, user := range []string{"alice", "bob"} {
		if err := store.AddAccount(stores.Account{Username: user, Workgroup: wg.String()}); err != nil {
			t.Fatalf("could not add the account of %s: %v", user, err)
		}
	}

	h := &smbTest{
		t:         t,
		share:     &share{name: "files", maxUses: maxShareUses},
		files:     newFakeClient(),
		workgroup: wg.String(),
	}

	h.srv = &server{
		shareList:          map[string]*share{h.share.name: h.share},
		globalOpenTable:    make(map[uint64]*open),
		globalSessionTable: make(map[uint64]*session),
		connectionList:     make(map[string]*connection),
		connectionCount:    make(map[string]int),
		globalClientTable:  make(map[[16]byte]*smbClient),
		store:              store,
	}
	h.srv.globalLeaseTableList = make(map[[16]byte]*leaseTable)

	return h
}

// restrictTo limits the share to the named users, so that everybody else is turned away by the
// access check. A share with no security set at all lets everybody in.
func (h *smbTest) restrictTo(users ...string) {
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)
	for _, user := range users {
		key := h.workgroup + "/" + user
		h.share.connectSecurity[key] = struct{}{}
		h.share.fileSecurity[key] = smb2.FILE_READ_DATA | smb2.FILE_WRITE_DATA |
			smb2.FILE_APPEND_DATA | smb2.FILE_WRITE_EA | smb2.FILE_WRITE_ATTRIBUTES | smb2.DELETE
	}
}

const (
	// writeAccess is what a client asks for when it means to change the file, and readAccess
	// when it only means to look at it. Which of the two a create asks for is what decides
	// whether the read caches of the other clients survive it.
	writeAccess = smb2.FILE_READ_DATA | smb2.FILE_WRITE_DATA
	readAccess  = smb2.FILE_READ_DATA | smb2.FILE_READ_ATTRIBUTES
)

// smbTestClients counts the clients built across a test binary, so that each gets a name and a
// session of its own: the channel list of a session is keyed by the name of the connection.
var smbTestClients int

// testClient is one client of the harness: a connection, a session and a tree connect on the
// share, as they would stand once a negotiate, a session setup and a tree connect had gone
// through.
type testClient struct {
	h    *smbTest
	conn *connection
	ss   *session
	tc   *treeConnect
	sent chan []byte
	mid  uint64
}

// dial brings up a client of its own, with a GUID nobody else shares.
func (h *smbTest) dial(user string) *testClient {
	h.t.Helper()

	var guid [16]byte
	guid[0] = byte(smbTestClients + 1)

	return h.dialAs(user, guid)
}

// dialAs brings up a connection belonging to the client with the given GUID. Two connections
// dialled with the same GUID are the same client as far as leases are concerned, however many
// sessions they carry between them.
func (h *smbTest) dialAs(user string, guid [16]byte) *testClient {
	h.t.Helper()

	smbTestClients++
	sent := make(chan []byte, 16)

	c := &connection{
		server:           h.srv,
		clientGuid:       guid[:],
		clientName:       fmt.Sprintf("%s-%d", user, smbTestClients),
		negotiateDialect: smb2.SMB_DIALECT_311,
		// A real connection settles these at negotiate time; without them every read and write
		// is longer than the server is willing to handle.
		maxTransactSize:  smb2.MaxTransactSize,
		maxReadSize:      smb2.MaxReadSize,
		maxWriteSize:     smb2.MaxWriteSize,
		writeChan:        sent,
		closeChan:        make(chan struct{}),
		sessionTable:     make(map[uint64]*session),
		requestList:      make(map[uint64]*smb2.Request),
		asyncCommandList: make(map[uint64]*smb2.Request),
		pendingResponses: make(map[uint64]smb2.GenericResponse),
		requestOpens:     make(map[uint64]*open),
		stopChans:        make(map[uint64]chan struct{}),
	}

	ss := &session{
		sessionID:        uint64(smbTestClients),
		state:            sessionValid,
		userName:         user,
		workgroup:        h.workgroup,
		openTable:        make(map[uint64]*open),
		treeConnectTable: make(map[uint32]*treeConnect),
		channelList:      map[string]*channel{c.clientName: {connection: c}},
		connection:       c,
	}

	tc := &treeConnect{
		treeID:        1,
		session:       ss,
		share:         h.share,
		client:        h.files,
		maxUploadSize: 64 << 20,
		// The share security of a real tree connect holds concrete access bits, not the
		// generic rights, and the replay rules are written in terms of the concrete ones.
		maximalAccess: smb2.FILE_READ_DATA | smb2.FILE_WRITE_DATA | smb2.FILE_APPEND_DATA |
			smb2.FILE_WRITE_EA | smb2.FILE_WRITE_ATTRIBUTES | smb2.DELETE,
		persistedOpens: make(map[string]*open),
	}

	ss.treeConnectTable[tc.treeID] = tc
	c.sessionTable[ss.sessionID] = ss

	h.srv.mu.Lock()
	h.srv.globalSessionTable[ss.sessionID] = ss
	h.srv.connectionList[c.clientName] = c
	h.srv.mu.Unlock()

	return &testClient{h: h, conn: c, ss: ss, tc: tc, sent: sent}
}

// send hands a message to the dispatcher the way the reading loop would. It reports an error
// rather than failing the test, so that it may be called from a goroutine of its own.
func (cl *testClient) send(msg []byte) (smb2.GenericResponse, error) {
	reqs, err := smb2.GetRequests(msg, 0, 0, false)
	if err != nil {
		return nil, fmt.Errorf("the message did not parse as a request: %w", err)
	}

	resp, _, err := cl.conn.processRequest(reqs[0])
	if err != nil {
		return nil, fmt.Errorf("the server gave up on the request: %w", err)
	}

	return resp, nil
}

// create opens a file and returns the response as it goes on the wire, together with whether
// the server had to answer it asynchronously.
func (cl *testClient) create(name string, oplock uint8, disposition uint32) (buf []byte, async bool) {
	cl.h.t.Helper()

	resp, err := cl.createErr(name, oplock, disposition)
	if err != nil {
		cl.h.t.Fatalf("create of %s: %v", name, err)
	}

	if smb2.Header(resp).Status() != smb2.STATUS_PENDING {
		return resp, false
	}

	// The interim response promises a real one later, over the same connection.
	return cl.recv(20 * time.Second), true
}

// createErr is create without the waiting and without failing the test, for use off the main
// goroutine. The response it returns may still be the interim one.
func (cl *testClient) createErr(name string, oplock uint8, disposition uint32) ([]byte, error) {
	return cl.createWith(name, oplock, disposition, nil)
}

// createWith is createErr carrying create contexts.
func (cl *testClient) createWith(name string, oplock uint8, disposition uint32, contexts []byte) ([]byte, error) {
	return cl.createAccessing(name, oplock, disposition, writeAccess, contexts)
}

// createAccessing is createWith asking for a particular access. A create that asks for nothing
// it could change the file with lets the other clients keep their read caches.
func (cl *testClient) createAccessing(name string, oplock uint8, disposition, access uint32, contexts []byte) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(createRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, name, oplock, disposition, access, contexts))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
}

// createLeased opens a file asking for a lease in the state given, and returns the response as
// it goes on the wire together with whether the server had to answer asynchronously.
func (cl *testClient) createLeased(name string, key [16]byte, state uint32, version int, disposition uint32) (buf []byte, async bool) {
	cl.h.t.Helper()

	resp, err := cl.createWith(name, smb2.OPLOCK_LEVEL_LEASE, disposition, leaseContext(key, state, version))
	if err != nil {
		cl.h.t.Fatalf("leased create of %s: %v", name, err)
	}

	if smb2.Header(resp).Status() != smb2.STATUS_PENDING {
		return resp, false
	}

	return cl.recv(20 * time.Second), true
}

// ackLeaseBreak answers a lease break notification, keeping the state named.
func (cl *testClient) ackLeaseBreak(key [16]byte, state uint32) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(leaseBreakAck(cl.mid, cl.ss.sessionID, cl.tc.treeID, key, state))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
}

// createReading opens a file for reading only, asking for the given oplock.
func (cl *testClient) createReading(name string, oplock uint8) (buf []byte, async bool) {
	cl.h.t.Helper()

	resp, err := cl.createAccessing(name, oplock, smb2.FILE_OPEN, readAccess, nil)
	if err != nil {
		cl.h.t.Fatalf("read-only create of %s: %v", name, err)
	}

	if smb2.Header(resp).Status() != smb2.STATUS_PENDING {
		return resp, false
	}

	return cl.recv(20 * time.Second), true
}

// createLeasedReading opens a file for reading only, asking for a lease.
func (cl *testClient) createLeasedReading(name string, key [16]byte, state uint32) (buf []byte, async bool) {
	cl.h.t.Helper()

	resp, err := cl.createAccessing(name, smb2.OPLOCK_LEVEL_LEASE, smb2.FILE_OPEN, readAccess, leaseContext(key, state, 2))
	if err != nil {
		cl.h.t.Fatalf("leased read-only create of %s: %v", name, err)
	}

	if smb2.Header(resp).Status() != smb2.STATUS_PENDING {
		return resp, false
	}

	return cl.recv(20 * time.Second), true
}

// ackBreak answers a break notification, giving up the oplock down to the given level.
func (cl *testClient) ackBreak(fid []byte, level uint8) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(oplockBreakAck(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, level))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
}

// closeHandle closes an open. It reports an error rather than failing the test, so that it may
// be called from a goroutine of its own.
func (cl *testClient) closeHandle(fid []byte) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(closeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
}

// recv takes the next message the server sent to this client on its own initiative.
func (cl *testClient) recv(timeout time.Duration) []byte {
	cl.h.t.Helper()

	select {
	case buf := <-cl.sent:
		return buf
	case <-time.After(timeout):
		cl.h.t.Fatal("the server sent nothing")
		return nil
	}
}

// quiet fails the test if the server sends anything at all within the grace period.
func (cl *testClient) quiet(grace time.Duration, what string) {
	cl.h.t.Helper()

	select {
	case <-cl.sent:
		cl.h.t.Error(what)
	case <-time.After(grace):
	}
}

// createRequest builds the bytes of an SMB2_CREATE request, carrying the given create contexts
// if there are any.
func createRequest(mid, sid uint64, tid uint32, name string, oplock uint8, disposition, access uint32, contexts []byte) []byte {
	encoded := utils.EncodeStringToBytes(name)

	// The name follows the fixed part of the request, which is what puts it at the eight-byte
	// boundary the offset is required to sit on.
	nameOff := smb2.SMB2HeaderSize + smb2.SMB2CreateRequestMinSize - 1

	msg := make([]byte, nameOff)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_CREATE)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2CreateRequestStructureSize)
	body[3] = oplock
	binary.LittleEndian.PutUint32(body[24:28], access)
	binary.LittleEndian.PutUint32(body[36:40], disposition)
	binary.LittleEndian.PutUint16(body[44:46], uint16(nameOff))
	binary.LittleEndian.PutUint16(body[46:48], uint16(len(encoded)))

	msg = append(msg, encoded...)
	if len(contexts) == 0 {
		return msg
	}

	// The contexts sit at an eight-byte boundary of their own after the name. From here on the
	// fields are written through the message: appending to it may have moved the body.
	for len(msg)%8 != 0 {
		msg = append(msg, 0)
	}
	binary.LittleEndian.PutUint32(msg[smb2.SMB2HeaderSize+48:smb2.SMB2HeaderSize+52], uint32(len(msg)))
	binary.LittleEndian.PutUint32(msg[smb2.SMB2HeaderSize+52:smb2.SMB2HeaderSize+56], uint32(len(contexts)))

	return append(msg, contexts...)
}

// createContext frames the data of a create context under the given name.
func createContext(name uint32, data []byte) []byte {
	//        SMB2_CREATE_CONTEXT
	//   0-4: Next
	//   4-6: NameOffset
	//   6-8: NameLength
	// 10-12: DataOffset
	// 12-16: DataLength
	// 16-20: Name
	//   24-: Data
	ctx := make([]byte, 24+len(data))
	binary.LittleEndian.PutUint16(ctx[4:6], 16)
	binary.LittleEndian.PutUint16(ctx[6:8], 4)
	binary.LittleEndian.PutUint16(ctx[10:12], 24)
	binary.LittleEndian.PutUint32(ctx[12:16], uint32(len(data)))
	binary.BigEndian.PutUint32(ctx[16:20], name)
	copy(ctx[24:], data)

	return ctx
}

// chainContexts links create contexts so that they travel in one request. Every context but the
// last points at the one after it.
func chainContexts(contexts ...[]byte) []byte {
	var buf []byte
	for i, ctx := range contexts {
		if i < len(contexts)-1 {
			binary.LittleEndian.PutUint32(ctx[:4], uint32(len(ctx)))
		}
		buf = append(buf, ctx...)
	}

	return buf
}

// leaseContext formats an SMB2_CREATE_REQUEST_LEASE create context of the given version, ready
// to be handed to createRequest.
func leaseContext(key [16]byte, state uint32, version int) []byte {
	size := 32
	if version == 2 {
		size = 52
	}

	data := make([]byte, size)
	copy(data[:16], key[:])
	binary.LittleEndian.PutUint32(data[16:20], state)

	return createContext(smb2.CREATE_REQUEST_LEASE, data)
}

// durableContext formats an SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2 create context, ready to be
// handed to createRequest.
func durableContext(createGuid [16]byte, timeout uint32) []byte {
	data := make([]byte, 32)
	binary.LittleEndian.PutUint32(data[0:4], timeout)
	copy(data[16:32], createGuid[:])

	return createContext(smb2.SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2, data)
}

// reconnectContext formats an SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2 create context.
func reconnectContext(fileID, durableID uint64, createGuid [16]byte) []byte {
	data := make([]byte, 36)
	binary.LittleEndian.PutUint64(data[0:8], fileID)
	binary.LittleEndian.PutUint64(data[8:16], durableID)
	copy(data[16:32], createGuid[:])

	return createContext(smb2.SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2, data)
}

// createDurable opens a file asking for a durable handle. Marking the request as a replay is
// what a client does when it never got an answer to the first attempt.
func (cl *testClient) createDurable(name string, createGuid [16]byte, replay bool) (buf []byte, async bool) {
	cl.h.t.Helper()

	return cl.createDurableFor(name, createGuid, replay, 30_000)
}

// createDurableFor is createDurable asking for a particular timeout, in milliseconds.
func (cl *testClient) createDurableFor(name string, createGuid [16]byte, replay bool, timeout uint32) (buf []byte, async bool) {
	cl.h.t.Helper()

	cl.mid++
	msg := createRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, name,
		smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN, writeAccess, durableContext(createGuid, timeout))
	if replay {
		smb2.Header(msg).SetFlag(smb2.FLAGS_REPLAY_OPERATION)
	}

	resp, err := cl.send(msg)
	if err != nil {
		cl.h.t.Fatalf("durable create of %s: %v", name, err)
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode(), false
	}

	return cl.recv(20 * time.Second), true
}

// createdDurableTimeout returns the timeout granted by a create response, and whether the
// response carried a durable handle context at all.
func createdDurableTimeout(buf []byte) (uint32, bool) {
	data, found := createdContext(buf, smb2.SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2)
	if !found || len(data) < 4 {
		return 0, false
	}

	return binary.LittleEndian.Uint32(data[:4]), true
}

// leaseBreakAck builds the bytes of a lease break acknowledgment.
func leaseBreakAck(mid, sid uint64, tid uint32, key [16]byte, state uint32) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2LeaseBreakRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_OPLOCK_BREAK)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2LeaseBreakRequestStructureSize)
	copy(body[8:24], key[:])
	binary.LittleEndian.PutUint32(body[24:28], state)

	return msg
}

// oplockBreakAck builds the bytes of an SMB2_OPLOCK_BREAK acknowledgment.
func oplockBreakAck(mid, sid uint64, tid uint32, fid []byte, level uint8) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2OplockBreakRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_OPLOCK_BREAK)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2OplockBreakRequestStructureSize)
	body[2] = level
	copy(body[8:24], fid)

	return msg
}

// writeRequest builds the bytes of an SMB2_WRITE request.
func writeRequest(mid, sid uint64, tid uint32, fid []byte, offset uint64, data []byte) []byte {
	// The data follows the fixed part of the request, at an eight-byte boundary.
	dataOff := smb2.SMB2HeaderSize + smb2.SMB2WriteRequestMinSize

	msg := make([]byte, dataOff)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_WRITE)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2WriteRequestStructureSize)
	binary.LittleEndian.PutUint16(body[2:4], uint16(dataOff))
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(data)))
	binary.LittleEndian.PutUint64(body[8:16], offset)
	copy(body[16:32], fid)

	return append(msg, data...)
}

// write sends data through a handle.
func (cl *testClient) write(fid []byte, offset uint64, data []byte) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(writeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, offset, data))
	if err != nil {
		return nil, err
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode(), nil
	}

	return cl.recv(20 * time.Second), nil
}

// closeRequest builds the bytes of an SMB2_CLOSE request.
func closeRequest(mid, sid uint64, tid uint32, fid []byte) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2CloseRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_CLOSE)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2CloseRequestStructureSize)
	copy(body[8:24], fid)

	return msg
}

// openIDOf returns the key under which the global open table holds the open a create response
// names: the durable half of the file ID.
func openIDOf(fid []byte) uint64 {
	return binary.LittleEndian.Uint64(fid[8:16])
}

// createdOplockLevel returns the oplock level of an SMB2_CREATE response.
func createdOplockLevel(buf []byte) uint8 {
	return buf[smb2.SMB2HeaderSize+2]
}

// createdFileID returns the FileId of an SMB2_CREATE response.
func createdFileID(buf []byte) []byte {
	return buf[smb2.SMB2HeaderSize+64 : smb2.SMB2HeaderSize+80]
}

// brokenFileID returns the FileId named by an SMB2_OPLOCK_BREAK notification.
func brokenFileID(buf []byte) []byte {
	return buf[smb2.SMB2HeaderSize+8 : smb2.SMB2HeaderSize+24]
}

// isLeaseBreak reports whether a break notification is about a lease rather than an oplock. The
// two go under the same command and are told apart by their structure size.
func isLeaseBreak(buf []byte) bool {
	size := binary.LittleEndian.Uint16(buf[smb2.SMB2HeaderSize : smb2.SMB2HeaderSize+2])
	return size == smb2.SMB2LeaseBreakNotificationStructureSize
}

// brokenLeaseKey returns the LeaseKey named by a lease break notification.
func brokenLeaseKey(buf []byte) [16]byte {
	var key [16]byte
	copy(key[:], buf[smb2.SMB2HeaderSize+8:smb2.SMB2HeaderSize+24])
	return key
}

// brokenLeaseStates returns the states a lease break notification is moving between, and
// whether the client has to acknowledge before the new one takes effect.
func brokenLeaseStates(buf []byte) (current, granted uint32, ackRequired bool) {
	body := buf[smb2.SMB2HeaderSize:]
	current = binary.LittleEndian.Uint32(body[24:28])
	granted = binary.LittleEndian.Uint32(body[28:32])
	ackRequired = binary.LittleEndian.Uint32(body[4:8])&smb2.SMB2_NOTIFY_BREAK_LEASE_FLAG_ACK_REQUIRED > 0
	return
}

// createdContext returns the data of the named create context in a create response.
func createdContext(buf []byte, want uint32) ([]byte, bool) {
	off := binary.LittleEndian.Uint32(buf[smb2.SMB2HeaderSize+80 : smb2.SMB2HeaderSize+84])
	length := binary.LittleEndian.Uint32(buf[smb2.SMB2HeaderSize+84 : smb2.SMB2HeaderSize+88])
	if off == 0 || length == 0 {
		return nil, false
	}

	for off+16 <= uint32(len(buf)) {
		next := binary.LittleEndian.Uint32(buf[off : off+4])
		nameOff := uint32(binary.LittleEndian.Uint16(buf[off+4 : off+6]))
		dataOff := uint32(binary.LittleEndian.Uint16(buf[off+10 : off+12]))
		dataLen := binary.LittleEndian.Uint32(buf[off+12 : off+16])

		if name := binary.BigEndian.Uint32(buf[off+nameOff : off+nameOff+4]); name == want {
			return buf[off+dataOff : off+dataOff+dataLen], true
		}

		if next == 0 {
			break
		}
		off += next
	}

	return nil, false
}

// createdLeaseState returns the lease state named by the create response, and whether the
// response carried a lease context at all.
func createdLeaseState(buf []byte) (uint32, bool) {
	data, found := createdContext(buf, smb2.CREATE_REQUEST_LEASE)
	if !found || len(data) < 20 {
		return 0, false
	}

	return binary.LittleEndian.Uint32(data[16:20]), true
}
