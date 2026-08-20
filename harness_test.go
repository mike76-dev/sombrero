package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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

	// contents holds the bytes of the files a test wants to be able to read back. A file put in
	// the store without any is a file of the given size that reads as nothing.
	contents map[string][]byte

	// emptyErr is what the emptiness check fails with, when a test asks it to. It is the one
	// lookup the deletion of a directory cannot be decided without, so what the server does when
	// it cannot be answered is worth being able to reach.
	emptyErr error

	// uploads are the parts of each upload that has been begun, by the ID the backend gave it. An
	// upload is not an object until it is completed, and it is completed with the parts the completion
	// names: the ones left out of it are no part of the file, which is what lets a file be cut back to
	// a point already sent. One that is called off leaves nothing behind at all.
	uploads      map[string]map[int]stagedPart
	nextUploadID int

	// started counts the uploads begun on a path, which is what tells one upload carrying the
	// writes of every handle on a file from two uploads racing to be the last one finished.
	started map[string]int

	// dirErr is what making a directory fails with, and finishErr what finishing an upload fails
	// with, when a test asks them to. They stand for a store that is there but will not do the
	// work — the Sia node out of sync is the one that prompted them.
	dirErr    error
	finishErr error

	// listErr is what listing a directory fails with, when a test asks it to. It stands for a
	// store that cannot be reached at all, which a search must not read as an empty directory.
	listErr error

	// readGate holds up every read until a test lets it go, so that a test can arrange for
	// something to happen to the handle while a read on it is still being worked on.
	readGate chan struct{}

	// readPanic and writePanic make the backend panic rather than answer. A backend is reached
	// from goroutines nobody is left waiting on, so what the server does when one of them comes
	// apart is worth being able to reach.
	readPanic  bool
	writePanic bool

	// partGate holds up the part uploads a test names — every one of them if partHeld is zero — so
	// that a test can see what the server does while a part is still on its way to the backend.
	partGate chan struct{}
	partHeld int

	// writeErr is what a part upload fails with, when a test asks it to. A part is sent long after
	// the write it came from was answered, so where its failure surfaces is worth being able to
	// reach.
	writeErr error

	// partsWritten is the parts that were sent, in the order they were sent, and partsFinished the
	// list the backend was handed to put them together. The backend needs that list in the order the
	// parts make the file up in, which is not the order they land in.
	partsWritten  []int
	partsFinished []int

	// deleteErr is what a deletion fails with, when a test asks it to. A deletion that the
	// backend refuses for a reason other than the file not being there is the one the server
	// still has to complain about.
	deleteErr error
}

// stagedPart is a part the backend has taken and not yet put into an object.
type stagedPart struct {
	data []byte
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		objects:  make(map[string]client.ObjectInfo),
		contents: make(map[string][]byte),
		uploads:  make(map[string]map[int]stagedPart),
	}
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

// putData makes a file with contents appear in the store, so that reading it back gives those
// bytes rather than a hole of the right size.
func (fc *fakeClient) putData(path string, data []byte) {
	fc.put(path, uint64(len(data)))

	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.contents[path] = data
}

// putDir makes a directory appear in the store. A directory is held under its own path, exactly
// as a file is, and is told apart by the trailing slash on the key: that is the shape both of the
// real stores hand back, and the only thing the create path reads the directory attribute out of.
func (fc *fakeClient) putDir(path string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	now := time.Now()
	fc.objects[path] = client.ObjectInfo{
		Key:        "/" + path + "/",
		CreatedAt:  now,
		ModifiedAt: now,
	}
}

// putSizedDir makes a directory appear in the store whose key carries a size, which is what a query
// for the share root comes back with on renterd.
func (fc *fakeClient) putSizedDir(path string, size uint64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	now := time.Now()
	fc.objects[path] = client.ObjectInfo{
		Key:        "/" + path + "/",
		Size:       size,
		CreatedAt:  now,
		ModifiedAt: now,
	}
}

// holdParts makes the upload of the given part wait, or of every part when the number is zero, and
// returns what lets them go.
func (fc *fakeClient) holdParts(part int) func() {
	gate := make(chan struct{})

	fc.mu.Lock()
	fc.partGate = gate
	fc.partHeld = part
	fc.mu.Unlock()

	return func() { close(gate) }
}

// failParts makes every part upload fail from here on.
func (fc *fakeClient) failParts(err error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.writeErr = err
}

// storedParts is the order the backend was given the parts of the file in.
func (fc *fakeClient) storedParts() []int {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	return append([]int(nil), fc.partsFinished...)
}

// holdReads makes every read wait, and returns what lets them all go.
func (fc *fakeClient) holdReads() func() {
	gate := make(chan struct{})

	fc.mu.Lock()
	fc.readGate = gate
	fc.mu.Unlock()

	return func() { close(gate) }
}

// panicOnReads and panicOnWrites make the backend come apart rather than answer, from here on.
func (fc *fakeClient) panicOnReads() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.readPanic = true
}

func (fc *fakeClient) panicOnWrites() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.writePanic = true
}

// failDeletion makes every deletion fail from here on.
func (fc *fakeClient) failDeletion(err error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.deleteErr = err
}

// failDirectories makes every directory creation fail from here on.
func (fc *fakeClient) failDirectories(err error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.dirErr = err
}

// failFinishingUploads makes the finishing of every upload fail from here on, which is what a store
// that took the parts and will not put them together does.
func (fc *fakeClient) failFinishingUploads(err error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.finishErr = err
}

// failListing makes every directory listing fail from here on.
func (fc *fakeClient) failListing(err error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.listErr = err
}

// failEmptiness makes the emptiness check fail from here on.
func (fc *fakeClient) failEmptiness(err error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.emptyErr = err
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

	if fc.listErr != nil {
		return nil, fc.listErr
	}

	var ois []client.ObjectInfo
	for _, oi := range fc.objects {
		ois = append(ois, oi)
	}

	return ois, nil
}

func (fc *fakeClient) MakeDirectory(_ context.Context, _ stores.Account, path string) error {
	fc.mu.Lock()
	err := fc.dirErr
	fc.mu.Unlock()
	if err != nil {
		return err
	}

	fc.putDir(path)

	return nil
}

func (fc *fakeClient) Info(context.Context) (client.GeneralInfo, error) {
	return client.GeneralInfo{}, nil
}

func (fc *fakeClient) Storage(context.Context) (client.StorageInfo, error) {
	return client.StorageInfo{}, nil
}

// IsEmpty reports whether the directory holds nothing at all. The path arrives with a trailing
// slash, which makes it the prefix of everything inside the directory and of nothing else: the
// directory's own entry is held under the path without one, and a sibling whose name merely starts
// the same way is cut off by the slash.
func (fc *fakeClient) IsEmpty(_ context.Context, _ stores.Account, path string) (bool, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.emptyErr != nil {
		return false, fc.emptyErr
	}

	for key := range fc.objects {
		if strings.HasPrefix(key, path) {
			return false, nil
		}
	}

	return true, nil
}

func (fc *fakeClient) Parents(context.Context, stores.Account, string) (client.FileInfo, client.FileInfo, error) {
	return client.FileInfo{}, client.FileInfo{}, nil
}

// Read serves the range out of the contents the test gave the file, which is what lets a test tell
// the bytes of one version of a file from those of another. A file that was only ever given a size
// reads as nothing, as it did before any test cared what came back.
func (fc *fakeClient) Read(_ context.Context, _ stores.Account, path string, offset, length uint64, w io.Writer) error {
	fc.mu.Lock()
	gate, boom := fc.readGate, fc.readPanic
	fc.mu.Unlock()

	if gate != nil {
		<-gate
	}

	if boom {
		panic("the backend came apart on a read")
	}

	fc.mu.Lock()
	data, found := fc.contents[path]
	fc.mu.Unlock()

	if !found {
		return nil
	}

	if offset >= uint64(len(data)) {
		return nil
	}
	if offset+length > uint64(len(data)) {
		length = uint64(len(data)) - offset
	}

	_, err := w.Write(data[offset : offset+length])

	return err
}

func (fc *fakeClient) StartUpload(_ context.Context, _ stores.Account, path string) (string, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.started == nil {
		fc.started = make(map[string]int)
	}
	fc.started[path]++

	// An ID of its own apiece, as a real backend gives: two uploads of one file are two uploads, and
	// what one of them holds is no part of the other.
	fc.nextUploadID++
	id := fmt.Sprintf("upload-%d", fc.nextUploadID)
	fc.uploads[id] = make(map[int]stagedPart)

	return id, nil
}

// uploadsOf is how many uploads have been started on the file.
func (fc *fakeClient) uploadsOf(path string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	return fc.started[path]
}

// dataOf is what the store holds for the file, which is what the parts of the last upload to be
// finished carried.
func (fc *fakeClient) dataOf(path string) []byte {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	return fc.contents[path]
}

// AbortUpload throws away what the upload was holding, which is what makes an upload that was called
// off leave nothing behind.
func (fc *fakeClient) AbortUpload(_ context.Context, _, uploadID string) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	delete(fc.uploads, uploadID)

	return nil
}

// FinishUpload turns the parts the completion names into the object, in the order it names them,
// which is what a real multipart completion does. A part that is not named is no part of the file.
func (fc *fakeClient) FinishUpload(_ context.Context, path, uploadID string, given []api.MultipartCompletedPart) error {
	fc.mu.Lock()
	staged := fc.uploads[uploadID]
	err := fc.finishErr
	fc.partsFinished = nil
	for _, p := range given {
		fc.partsFinished = append(fc.partsFinished, p.PartNumber)
	}
	fc.mu.Unlock()

	if err != nil {
		return err
	}

	var data []byte
	for _, p := range given {
		part, found := staged[p.PartNumber]
		if !found {
			return fmt.Errorf("the completion names part %d, which was never uploaded", p.PartNumber)
		}
		data = append(data, part.data...)
	}

	fc.mu.Lock()
	delete(fc.uploads, uploadID)
	fc.mu.Unlock()

	fc.put(path, uint64(len(data)))
	if len(data) > 0 {
		fc.mu.Lock()
		fc.contents[path] = data
		fc.mu.Unlock()
	}

	return nil
}

func (fc *fakeClient) Write(_ context.Context, r io.Reader, path, uploadID string, number int, offset, length uint64) (string, error) {
	fc.mu.Lock()
	gate, held, err := fc.partGate, fc.partHeld, fc.writeErr
	boom := fc.writePanic
	fc.partsWritten = append(fc.partsWritten, number)
	fc.mu.Unlock()

	if gate != nil && (held == 0 || held == number) {
		<-gate
	}

	if boom {
		panic("the backend came apart on a part")
	}

	if err != nil {
		return "", err
	}

	part := make([]byte, length)
	n, rerr := io.ReadFull(r, part)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return "", rerr
	}
	part = part[:n]

	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.uploads[uploadID] == nil {
		fc.uploads[uploadID] = make(map[int]stagedPart)
	}
	fc.uploads[uploadID][number] = stagedPart{data: part}

	return "etag", nil
}

// Delete takes the file out of the store, and reports the one that was never in it the way both
// of the real backends do: a file that was created and never written to has no object behind it,
// so this is the answer the delete of it always gets.
func (fc *fakeClient) Delete(_ context.Context, _ stores.Account, path string, _ bool) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if fc.deleteErr != nil {
		return fc.deleteErr
	}

	if _, found := fc.objects[path]; !found {
		return stores.ErrNotFound
	}

	delete(fc.objects, path)

	return nil
}

// has reports whether the file is still in the store.
func (fc *fakeClient) has(path string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	_, found := fc.objects[path]

	return found
}

func (fc *fakeClient) Rename(_ context.Context, _ stores.Account, from, to string, _, _ bool) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	oi, found := fc.objects[from]
	if !found {
		return errNoObject
	}

	oi.Key = "/" + to
	fc.objects[to] = oi
	delete(fc.objects, from)

	// The bytes go with the name, as they do on a backend where a rename is metadata: left behind,
	// the renamed file reads as nothing and a test cannot tell that from a file that never moved.
	if data, found := fc.contents[from]; found {
		fc.contents[to] = data
		delete(fc.contents, from)
	}

	return nil
}

func (fc *fakeClient) DeleteAll(context.Context) error { return nil }

// The fake store pins nothing of its own, so it has no orphans to report.
func (fc *fakeClient) OrphanedSlabs(context.Context, time.Duration) ([]client.OrphanedSlab, error) {
	return nil, nil
}

func (fc *fakeClient) UnpinOrphanedSlabs(context.Context, time.Duration) (client.UnpinResult, error) {
	return client.UnpinResult{}, nil
}

// Nor does it pack anything, so no slab of its can be fragmented.
func (fc *fakeClient) Fragmentation(context.Context, float64) (client.FragmentationReport, error) {
	return client.FragmentationReport{}, nil
}

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
		share:     &share{name: "files"},
		files:     newFakeClient(),
		workgroup: wg.String(),
	}

	// The server is built the way the real one is, so that a table added to it reaches the tests
	// without anybody remembering to add it here. Only the listener and the reaping of durable
	// opens are left out, neither of which the tests go through.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h.srv = newServerState(ctx, store, false, stores.IndexdConfig{})
	h.srv.shareList[h.share.name] = h.share

	// A share holds security for whoever may use it, and grants nothing to anybody else, so the
	// accounts the tests dial with are on it from the start.
	h.restrictTo("alice", "bob")

	return h
}

// restrictTo limits the share to the named users, so that everybody else is turned away by the
// access check.
func (h *smbTest) restrictTo(users ...string) {
	h.share.connectSecurity = make(map[string]struct{})
	h.share.fileSecurity = make(map[string]uint32)
	for _, user := range users {
		key := h.workgroup + "/" + user
		h.share.connectSecurity[key] = struct{}{}
		h.share.fileSecurity[key] = shareAccess
	}
}

// signing turns the client's session into one that requires signing, as every session that is
// not encrypting does once it has authenticated.
func (cl *testClient) signing() *testClient {
	cl.h.t.Helper()

	cl.ss.sessionKey = bytes.Repeat([]byte{0x5a}, 16)
	cl.ss.deriveKeys()
	cl.ss.bindChannel(cl.conn, cl.ss.signingKey)
	cl.ss.signingRequired = true
	cl.ss.encryptData = false

	return cl
}

// encrypting turns the client's session into an encrypting one, the way a 3.x session setup
// does when the server has encryption switched on. The keys come out of the same derivation the
// server uses, so what the test decrypts with is what the server encrypted with.
func (cl *testClient) encrypting() *testClient {
	cl.h.t.Helper()

	cl.conn.cipherID = smb2.AES_128_GCM
	cl.ss.sessionKey = bytes.Repeat([]byte{0xa5}, 16)
	cl.ss.deriveKeys()
	cl.ss.bindChannel(cl.conn, cl.ss.signingKey)
	cl.ss.encryptData = true
	cl.ss.signingRequired = false

	return cl
}

// decrypted undoes the protection the server put on a message meant for this client, failing the
// test if it was not encrypted or does not come apart. The server seals with its outbound key,
// so that is what opens it.
func (cl *testClient) decrypted(buf []byte) []byte {
	cl.h.t.Helper()

	if id := smb2.Header(buf).ProtocolID(); id != smb2.PROTOCOL_SMB2_ENCRYPTED {
		cl.h.t.Fatalf("the message carries protocol ID %#x, want an encrypted one", id)
	}

	opener, err := cl.ss.aead(cl.ss.encryptionKey, cl.conn)
	if err != nil {
		cl.h.t.Fatalf("could not build the cipher: %v", err)
	}

	h := smb2.Header(buf)
	sealed := append(buf[smb2.SMB2TransformHeaderSize:], h.EncryptionSignature()...)
	msg, err := opener.Open(sealed[:0], h.Nonce()[:opener.NonceSize()], sealed, h.AssociatedData())
	if err != nil {
		cl.h.t.Fatalf("the message did not come apart: %v", err)
	}

	return msg
}

// impatient shortens the acknowledgment timers, so that a test of what happens when a client
// never answers a break spends milliseconds on it rather than the 35 seconds a real client is
// given. It returns the timeout it set, to save the caller repeating it.
func (h *smbTest) impatient(d time.Duration) time.Duration {
	h.srv.oplockBreakTimeout = d
	h.srv.leaseBreakTimeout = d
	return d
}

const (
	// writeAccess is what a client asks for when it means to change the file, and readAccess
	// when it only means to look at it. Which of the two a create asks for is what decides
	// whether the read caches of the other clients survive it.
	//
	// A handle is granted what its create asked for, so what is asked for here has to cover
	// everything the tests then do through the handle - writing it, setting information on it,
	// renaming it and marking it for deletion - the way the ask of a client that means to
	// change a file does.
	writeAccess = smb2.FILE_READ_DATA | smb2.FILE_READ_ATTRIBUTES | smb2.FILE_WRITE_DATA |
		smb2.FILE_APPEND_DATA | smb2.FILE_WRITE_EA | smb2.FILE_WRITE_ATTRIBUTES | smb2.DELETE
	readAccess = smb2.FILE_READ_DATA | smb2.FILE_READ_ATTRIBUTES

	// shareAccess is what the share grants over a file: everything a test may want to do with
	// one. It is what a tree connect on the test share carries as its maximal access.
	//
	// FILE_READ_ATTRIBUTES and FILE_READ_EA come with reading on a real share - the stored
	// rights hand out 0x80120089 for read access, which carries both - so a fixture that grants
	// FILE_READ_DATA without them is a handle no share ever issues, and refuses the client that
	// asks for its reading in generic terms.
	shareAccess = smb2.FILE_READ_DATA | smb2.FILE_READ_ATTRIBUTES | smb2.FILE_READ_EA |
		smb2.FILE_WRITE_DATA | smb2.FILE_APPEND_DATA | smb2.FILE_WRITE_EA |
		smb2.FILE_WRITE_ATTRIBUTES | smb2.DELETE
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

// newTestConnection builds a connection the way the server does, with a transport under it that
// nothing reads from or writes to. It has to be there all the same: closing a connection closes
// its transport, and a real one always has one.
func (h *smbTest) newTestConnection(name string) *connection {
	c := h.srv.newConnectionState(name)
	c.conn, _ = net.Pipe()

	return c
}

// dial brings up a client of its own, with a GUID nobody else shares.
//
// The count goes into the second and third bytes and never into the first, which is what keeps it
// clear of the GUIDs the tests name for themselves: those carry the first byte, and a count that went
// there would wrap past every one of them as the binary dialled its way through 256 clients. A client
// that collides with another is not a test failing loudly - it is the same client as far as the lease
// paths are concerned, so a break that ought to be sent is correctly not sent, and the test that was
// waiting for it fails a long way from the cause.
func (h *smbTest) dial(user string) *testClient {
	h.t.Helper()

	n := smbTestClients + 1
	var guid [16]byte
	guid[1] = byte(n)
	guid[2] = byte(n >> 8)

	return h.dialAs(user, guid)
}

// dialAs brings up a connection belonging to the client with the given GUID. Two connections
// dialled with the same GUID are the same client as far as leases are concerned, however many
// sessions they carry between them.
func (h *smbTest) dialAs(user string, guid [16]byte) *testClient {
	h.t.Helper()

	smbTestClients++

	c := h.newTestConnection(fmt.Sprintf("%s-%d", user, smbTestClients))

	// What a negotiate would have settled.
	c.clientGuid = guid[:]
	c.negotiateDialect = smb2.SMB_DIALECT_311
	c.dialect = dialectName(c.negotiateDialect)

	// Including the capabilities the server answers a 3.1.1 negotiate with. Leasing is the one the
	// tests turn on: a lease is only granted over a connection the server offered leases on, so a
	// connection built here without it would refuse every lease the tests ask for.
	c.serverCapabilities |= smb2.GLOBAL_CAP_LEASING

	// Nothing is draining the connection here, so what the server sends is queued for the test to
	// read rather than handed to a sending goroutine.
	sent := make(chan []byte, 16)
	c.writeChan = sent

	// What a session setup would have settled.
	ss := newSessionState(uint64(smbTestClients), c)
	ss.state = sessionValid
	ss.userName = user
	ss.workgroup = h.workgroup
	ss.channelList[c.clientName] = &channel{connection: c}

	// The maximal access of a real tree connect comes from the share security, which holds
	// concrete access bits rather than the generic rights; the replay rules are written in terms
	// of the concrete ones.
	tc := newTreeConnectState(1, ss, h.share, shareAccess)
	tc.client = h.files
	tc.maxUploadSize = 64 << 20

	ss.treeConnectTable[tc.treeID] = tc
	c.sessionTable[ss.sessionID] = ss

	h.srv.mu.Lock()
	h.srv.globalSessionTable[ss.sessionID] = ss
	h.srv.connectionList[c.clientName] = c
	h.srv.mu.Unlock()

	return &testClient{h: h, conn: c, ss: ss, tc: tc, sent: sent}
}

// addChannel binds a second connection to the session of the client, the way a client that has
// bound another channel to a session it already holds would. The session and everything opened
// over it are shared between the two: what comes back is another way to reach the same client,
// not another client.
func (cl *testClient) addChannel() *testClient {
	cl.h.t.Helper()

	smbTestClients++

	c := cl.h.newTestConnection(fmt.Sprintf("%s-alt-%d", cl.conn.clientName, smbTestClients))
	c.clientGuid = cl.conn.clientGuid
	c.negotiateDialect = cl.conn.negotiateDialect
	c.dialect = cl.conn.dialect

	sent := make(chan []byte, 16)
	c.writeChan = sent

	c.mu.Lock()
	c.sessionTable[cl.ss.sessionID] = cl.ss
	c.mu.Unlock()

	cl.ss.mu.Lock()
	cl.ss.channelList[c.clientName] = &channel{connection: c}
	cl.ss.mu.Unlock()

	cl.h.srv.mu.Lock()
	cl.h.srv.connectionList[c.clientName] = c
	cl.h.srv.mu.Unlock()

	return &testClient{h: cl.h, conn: c, ss: cl.ss, tc: cl.tc, sent: sent}
}

// goesAway kills the transport under the connection without taking it out of the session. It is
// the state a connection is in between the client disappearing and the server noticing: still a
// channel of the session, and no use for sending anything.
func (cl *testClient) goesAway() {
	cl.conn.once.Do(func() { close(cl.conn.closeChan) })
}

// speaking puts the client on a dialect other than the 3.1.1 it is dialled with, for the rules
// that are worded per dialect.
func (cl *testClient) speaking(dialect uint16) *testClient {
	cl.conn.negotiateDialect = dialect
	cl.conn.dialect = dialectName(dialect)
	return cl
}

// send hands a message to the dispatcher the way the reading loop would. It reports an error
// rather than failing the test, so that it may be called from a goroutine of its own.
func (cl *testClient) send(msg []byte) (smb2.GenericResponse, error) {
	reqs, err := smb2.GetRequests(msg, 0, false)
	if err != nil {
		return nil, fmt.Errorf("the message did not parse as a request: %w", err)
	}

	resp, _, err := cl.conn.processRequest(reqs[0])
	if err != nil {
		return nil, fmt.Errorf("the server gave up on the request: %w", err)
	}

	// The dispatcher says when what is going out for a request has been queued, which is what the
	// work behind an asynchronous one waits for before it answers. This stands in for the
	// dispatcher, so it has to say so too, or that work waits for ever.
	cl.conn.interimQueued(resp.Header().MessageID())

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

// openDir opens a directory, making it if it is not there. FILE_DIRECTORY_FILE is what says the
// create is not about a file, and is what a client sends to make one.
func (cl *testClient) openDir(name string) []byte {
	cl.h.t.Helper()

	return cl.openDirWithOptions(name, 0)
}

// openDirWithOptions is openDir carrying further create options beside the one that says the
// create is about a directory.
func (cl *testClient) openDirWithOptions(name string, options uint32) []byte {
	cl.h.t.Helper()

	cl.mid++
	msg := createRequestWithOptions(cl.mid, cl.ss.sessionID, cl.tc.treeID, name,
		smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN_IF, writeAccess, smb2.FILE_DIRECTORY_FILE|options, nil)

	resp, err := cl.send(msg)
	if err != nil {
		cl.h.t.Fatalf("open of directory %s: %v", name, err)
	}

	if status := resp.Header().Status(); status != smb2.STATUS_OK {
		cl.h.t.Fatalf("open of directory %s returned %#x", name, status)
	}

	return resp.Encode()
}

// createLeasedWithOptions opens a file asking for a lease, with particular create options.
func (cl *testClient) createLeasedWithOptions(name string, key [16]byte, state, options uint32) (buf []byte, async bool) {
	cl.h.t.Helper()

	cl.mid++
	msg := createRequestWithOptions(cl.mid, cl.ss.sessionID, cl.tc.treeID, name,
		smb2.OPLOCK_LEVEL_LEASE, smb2.FILE_OPEN, writeAccess, options, leaseContext(key, state, 2))

	resp, err := cl.send(msg)
	if err != nil {
		cl.h.t.Fatalf("leased create of %s: %v", name, err)
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode(), false
	}

	return cl.recv(20 * time.Second), true
}

// setInfoRequest builds the bytes of an SMB2_SET_INFO request for a file information class.
func setInfoRequest(mid, sid uint64, tid uint32, fid []byte, class uint8, data []byte) []byte {
	bufOff := smb2.SMB2HeaderSize + smb2.SMB2SetInfoRequestMinSize

	msg := make([]byte, bufOff)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_SET_INFO)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2SetInfoRequestStructureSize)
	body[2] = smb2.INFO_FILE
	body[3] = class
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(data)))
	binary.LittleEndian.PutUint16(body[8:10], uint16(bufOff))
	copy(body[16:32], fid)

	return append(msg, data...)
}

// setInfo sends an SMB2_SET_INFO request through a handle.
func (cl *testClient) setInfo(fid []byte, class uint8, data []byte) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(setInfoRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, class, data))
	if err != nil {
		return nil, err
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode(), nil
	}

	return cl.recv(20 * time.Second), nil
}

// markForDeletion sets FileDispositionInformation on a handle, so that the file goes when the
// last handle on it does.
func (cl *testClient) markForDeletion(fid []byte) ([]byte, error) {
	return cl.setInfo(fid, smb2.FileDispositionInformation, []byte{1})
}

// keepFile calls off a deletion that was pending, leaving the file where it is.
func (cl *testClient) keepFile(fid []byte) ([]byte, error) {
	return cl.setInfo(fid, smb2.FileDispositionInformation, []byte{0})
}

// rename sets FileRenameInformation on a handle.
func (cl *testClient) rename(fid []byte, to string) ([]byte, error) {
	//     0: ReplaceIfExists
	//  8-16: RootDirectory
	// 16-20: FileNameLength
	//   20-: FileName
	encoded := utils.EncodeStringToBytes(to)
	data := make([]byte, 20+len(encoded))
	binary.LittleEndian.PutUint32(data[16:20], uint32(len(encoded)))
	copy(data[20:], encoded)

	return cl.setInfo(fid, smb2.FileRenameInformation, data)
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
	return createRequestWithOptions(mid, sid, tid, name, oplock, disposition, access, 0, contexts)
}

// createRequestWithOptions is createRequest asking for particular create options.
func createRequestWithOptions(mid, sid uint64, tid uint32, name string, oplock uint8, disposition, access, options uint32, contexts []byte) []byte {
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
	binary.LittleEndian.PutUint32(body[40:44], options)
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

// maximalAccessContext formats an SMB2_CREATE_QUERY_MAXIMAL_ACCESS_REQUEST create context, in the
// form that carries no timestamp and so is always answered.
func maximalAccessContext() []byte {
	return createContext(smb2.CREATE_QUERY_MAXIMAL_ACCESS_REQUEST, nil)
}

// createdMaximalAccess returns the access the create response reports over the file, and whether
// it carried the context at all.
func createdMaximalAccess(buf []byte) (uint32, bool) {
	data, found := createdContext(buf, smb2.CREATE_QUERY_MAXIMAL_ACCESS_REQUEST)
	if !found || len(data) < 8 {
		return 0, false
	}

	return binary.LittleEndian.Uint32(data[4:8]), true
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

// writeRequest builds the bytes of an SMB2_WRITE request, with the data straight after the fixed
// part of the request, at an eight-byte boundary.
func writeRequest(mid, sid uint64, tid uint32, fid []byte, offset uint64, data []byte) []byte {
	return writeRequestAt(mid, sid, tid, fid, offset, data, smb2.SMB2HeaderSize+smb2.SMB2WriteRequestMinSize)
}

// writeRequestAt is writeRequest with the data placed at the given offset from the start of the
// header, with whatever padding that takes.
func writeRequestAt(mid, sid uint64, tid uint32, fid []byte, offset uint64, data []byte, dataOff int) []byte {
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

// readRequest builds the bytes of an SMB2_READ request.
func readRequest(mid, sid uint64, tid uint32, fid []byte, offset uint64, length uint32) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2ReadRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_READ)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2ReadRequestStructureSize)
	binary.LittleEndian.PutUint32(body[4:8], length)
	binary.LittleEndian.PutUint64(body[8:16], offset)
	copy(body[16:32], fid)

	return msg
}

// readOver reads through a handle, naming the given channel. A channel other than none asks for
// the data to travel over RDMA rather than in the response.
//
// The field sits after MinimumCount in a read but in its place in a write, so the two are not
// interchangeable however alike the requests look.
func (cl *testClient) readOver(fid []byte, length uint32, channel uint32) ([]byte, error) {
	cl.mid++
	msg := readRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, 0, length)
	binary.LittleEndian.PutUint32(msg[smb2.SMB2HeaderSize+36:smb2.SMB2HeaderSize+40], channel)

	resp, err := cl.send(msg)
	if err != nil {
		return nil, err
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode(), nil
	}

	return cl.recv(20 * time.Second), nil
}

// writeOver sends data through a handle, naming the given channel.
func (cl *testClient) writeOver(fid []byte, data []byte, channel uint32) ([]byte, error) {
	cl.mid++
	msg := writeRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, 0, data)
	binary.LittleEndian.PutUint32(msg[smb2.SMB2HeaderSize+32:smb2.SMB2HeaderSize+36], channel)

	resp, err := cl.send(msg)
	if err != nil {
		return nil, err
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode(), nil
	}

	return cl.recv(20 * time.Second), nil
}

// writeFrom sends data through a handle, placing it at the given offset from the start of the
// header rather than straight after the fixed part of the request.
func (cl *testClient) writeFrom(fid []byte, data []byte, dataOff int) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(writeRequestAt(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, 0, data, dataOff))
	if err != nil {
		return nil, err
	}

	if resp.Header().Status() != smb2.STATUS_PENDING {
		return resp.Encode(), nil
	}

	return cl.recv(20 * time.Second), nil
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

// flushRequest builds an SMB2_FLUSH request, which is what a client sends when it wants what it has
// written to be made safe.
func flushRequest(mid, sid uint64, tid uint32, fid []byte) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2FlushRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_FLUSH)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2FlushRequestStructureSize)
	copy(body[8:24], fid)

	return msg
}

// flushHandle asks for what has been written through the handle to be made safe.
func (cl *testClient) flushHandle(fid []byte) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(flushRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
}

// lockRequest builds the bytes of an SMB2_LOCK request carrying a single lock element.
func lockRequest(mid, sid uint64, tid uint32, fid []byte, offset, length uint64, flags uint32) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2LockRequestMinSize+24)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_LOCK)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetTreeID(tid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2LockRequestStructureSize)
	binary.LittleEndian.PutUint16(body[2:4], 1)
	copy(body[8:24], fid)

	lock := body[smb2.SMB2LockRequestMinSize:]
	binary.LittleEndian.PutUint64(lock[0:8], offset)
	binary.LittleEndian.PutUint64(lock[8:16], length)
	binary.LittleEndian.PutUint32(lock[16:20], flags)

	return msg
}

// lockRange asks for a byte range of the file behind the handle to be locked.
func (cl *testClient) lockRange(fid []byte, offset, length uint64) ([]byte, error) {
	cl.mid++
	resp, err := cl.send(lockRequest(cl.mid, cl.ss.sessionID, cl.tc.treeID, fid, offset, length,
		smb2.LOCKFLAG_EXCLUSIVE_LOCK|smb2.LOCKFLAG_FAIL_IMMEDIATELY))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
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

// logoffRequest builds the bytes of an SMB2_LOGOFF request.
func logoffRequest(mid, sid uint64) []byte {
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2LogoffRequestMinSize)
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_LOGOFF)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetCreditCharge(1)
	binary.LittleEndian.PutUint16(msg[smb2.SMB2HeaderSize:smb2.SMB2HeaderSize+2], smb2.SMB2LogoffRequestStructureSize)

	return msg
}

// logoff ends the session of the client the way a client that is done with it does.
func (cl *testClient) logoff() ([]byte, error) {
	cl.mid++
	resp, err := cl.send(logoffRequest(cl.mid, cl.ss.sessionID))
	if err != nil {
		return nil, err
	}

	return resp.Encode(), nil
}

// treeConnectRequest builds the bytes of an SMB2_TREE_CONNECT request for the given share path.
func treeConnectRequest(mid, sid uint64, path string) []byte {
	enc := utils.EncodeStringToBytes(path)
	msg := make([]byte, smb2.SMB2HeaderSize+smb2.SMB2TreeConnectRequestMinSize+len(enc))
	h := smb2.NewHeader(msg)
	h.SetCommand(smb2.SMB2_TREE_CONNECT)
	h.SetMessageID(mid)
	h.SetSessionID(sid)
	h.SetCreditCharge(1)

	body := msg[smb2.SMB2HeaderSize:]
	binary.LittleEndian.PutUint16(body[0:2], smb2.SMB2TreeConnectRequestStructureSize)
	binary.LittleEndian.PutUint16(body[4:6], uint16(smb2.SMB2HeaderSize+smb2.SMB2TreeConnectRequestMinSize))
	binary.LittleEndian.PutUint16(body[6:8], uint16(len(enc)))
	copy(msg[smb2.SMB2HeaderSize+smb2.SMB2TreeConnectRequestMinSize:], enc)

	return msg
}
