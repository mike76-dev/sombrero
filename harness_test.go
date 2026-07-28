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

func (h *smbTest) dial(user string) *testClient {
	h.t.Helper()

	smbTestClients++
	sent := make(chan []byte, 16)

	c := &connection{
		server:           h.srv,
		clientName:       fmt.Sprintf("%s-%d", user, smbTestClients),
		negotiateDialect: smb2.SMB_DIALECT_311,
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
		treeID:         1,
		session:        ss,
		share:          h.share,
		client:         h.files,
		maxUploadSize:  64 << 20,
		maximalAccess:  smb2.GENERIC_ALL,
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
	cl.mid++
	resp, err := cl.send(createRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, name, oplock, disposition))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
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

// createRequest builds the bytes of an SMB2_CREATE request.
func createRequest(mid, sid uint64, tid uint32, name string, oplock uint8, disposition uint32) []byte {
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
	binary.LittleEndian.PutUint32(body[24:28], smb2.FILE_READ_DATA|smb2.FILE_WRITE_DATA)
	binary.LittleEndian.PutUint32(body[36:40], disposition)
	binary.LittleEndian.PutUint16(body[44:46], uint16(nameOff))
	binary.LittleEndian.PutUint16(body[46:48], uint16(len(encoded)))

	return append(msg, encoded...)
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
