package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/ntlm"
	"github.com/mike76-dev/sombrero/rpc"
	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"github.com/oiweiwei/go-msrpc/msrpc/dtyp"
	"github.com/oiweiwei/go-msrpc/msrpc/lsat/lsarpc/v0"
	"go.sia.tech/renterd/v2/api"
	"golang.org/x/crypto/blake2b"
	"lukechampine.com/frand"
)

var (
	errNoDirectory = errors.New("not a directory")
	errNoFiles     = errors.New("no files found")

	// errNotUploaded is the read of a part of a file that is still being written and is no longer
	// in memory. The store cannot answer it either: what has gone up so far are the parts of a
	// multipart upload, which is not an object until it is completed.
	errNotUploaded = errors.New("file is still being uploaded")
)

// uploadChunk represents a single part of a multipart upload.
type uploadChunk struct {
	offset uint64
	data   []byte
}

// upload holds the information about an active multipart upload.
type upload struct {
	uploadID   string
	partCount  int
	parts      []api.MultipartCompletedPart
	totalSize  uint64
	nextOffset uint64
	pending    map[uint64]*uploadChunk
	buf        []byte
	bufOffset  uint64
	maxLength  uint64
	mu         sync.Mutex
}

// readBuffered serves a range out of the data the upload is still holding, and says whether it
// could. Only what sits in the contiguous buffer can be answered for: the parts already sent are
// no longer in memory, and the backend has nothing to offer either, since a multipart upload is
// not an object until it is completed.
//
// The buffer is where a client reading a file it is in the middle of writing looks: an unaligned
// write that the client has to round out reads the block it is about to write over, and that block
// is the one just written. Served from here, it is the data the client itself sent.
func (u *upload) readBuffered(offset, length uint64) ([]byte, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if offset < u.bufOffset || offset+length > u.bufOffset+uint64(len(u.buf)) {
		return nil, false
	}

	start := offset - u.bufOffset
	data := make([]byte, length)
	copy(data, u.buf[start:start+length])

	return data, true
}

// open represents an Open object.
type open struct {
	// The handle is what uniquely identifies the file within a share.
	// It is deterministically derived from the path of the object.
	handle                          uint64
	fileID                          uint64
	durableFileID                   uint64
	session                         *session
	treeConnect                     *treeConnect
	connection                      *connection
	grantedAccess                   uint32
	pathName                        string
	resumeKey                       []byte
	fileName                        string
	createOptions                   uint32
	createGuid                      [16]byte
	applicationInstanceVersion      [16]byte
	channelSequence                 uint16
	outstandingRequestCount         uint32
	outstandingPreviousRequestCount uint32

	// A durable open survives the loss of the connection it was created on: it is kept
	// aside for durableTimeout after disconnectTime, so that the client may reclaim it by
	// presenting the same createGuid instead of starting the work over. disconnectTime is
	// zero for as long as the open is attached to a session.
	isDurable      bool
	durableTimeout time.Duration
	disconnectTime time.Time

	// A create that was answered may never have reached the client, so the client is allowed
	// to send it again marked as a replay. Until the handle is used for anything, the open the
	// first attempt made can still answer the second one, which is what keeps a retry from
	// quietly opening the file twice. clientGuid is kept because the open outlives the
	// connection it was made on.
	clientGuid       [16]byte
	isReplayEligible bool

	// An open may hold an opportunistic lock, which lets the client cache the file locally
	// on the promise that nobody else gets at it without the client being told first.
	// oplockBreak is open while the client is being told, and is closed once it has answered,
	// once the wait has run out, or once the open has died; whoever wants the file waits on it.
	oplockLevel   uint8
	oplockState   int
	oplockBreakTo uint8
	oplockBreak   chan struct{}

	// An open may instead share a lease, which promises the same thing to the client that
	// holds it rather than to this open alone: every open that client has on the file under
	// the same key shares the lease, and none of them breaks the others. An open holds either
	// an oplock or a lease, never both.
	lease *lease

	// file is what this open shares with every other open on the same file: the size, the
	// timestamps, the attributes and the upload carrying it to the backend. Never nil.
	file *fileState

	ctx    context.Context
	cancel context.CancelFunc

	// The parameters and the result of the most recent search done on a directory (if the current open
	// is a directory). It is needed, because it's common for the clients to send two consecutive
	// SMB2_QUERY_DIRECTORY requests; the second one should be responded with the NO_MORE_FILES status.
	lastSearch    string
	searchResults []client.ObjectInfo

	// To speed up the downloads, read buffering is implemented. The buffer consists of several
	// caches, because the SMB2_READ requests may come out of order. Chunks are downloaded in the
	// background, so an entry may still be in flight; readers wait on its done channel. When the
	// reads look sequential, the next few chunks are prefetched so the network transfer overlaps
	// with serving cached data.
	// cacheGeneration is which writing of the file the cache below was filled in. The cache
	// belongs to this handle, and a write through another handle on the same file has to reach
	// it: the generation of the file moves on when it is written anew, and a cache left behind
	// is dropped rather than served.
	cacheGeneration uint64

	buffer       map[uint64]*readChunk
	cacheOrder   []uint64
	chunkSize    uint64
	maxCacheSize int
	lastReadEnd  uint64

	// A collection of LSARPS frames (if the open is associated with the IPC$ share).
	lsaFrames map[uint32]*rpc.Frame

	// SRVSVC data written to or read from the SRVSVC named pipe
	// (if the open is associated with the IPC$ share).
	srvsvcData []byte

	mu sync.Mutex
}

// readChunk is a cached chunk of a file. The download runs in the background;
// done is closed once data (or err) is set.
type readChunk struct {
	data []byte
	err  error
	done chan struct{}
}

const (
	// Bounds for the read chunk size. Within these bounds, chunks are kept aligned to the
	// slab size of the backend, so that a chunk download translates into as few range
	// requests as possible.
	minReadChunkSize = 16 * 1024 * 1024
	maxReadChunkSize = 32 * 1024 * 1024

	// How many chunks to prefetch ahead of a sequential read.
	readPrefetchDepth = 3

	// Rough per-open memory budget of the read cache, used to derive maxCacheSize.
	readCacheBudget = 128 * 1024 * 1024
)

// readChunkSize picks a read chunk size for the given slab size: a whole multiple of
// small slabs, or an exact fraction of a large slab.
func readChunkSize(slabSize uint64) uint64 {
	if slabSize == 0 {
		return minReadChunkSize
	}

	chunk := slabSize
	for chunk < minReadChunkSize {
		chunk += slabSize
	}
	for chunk > maxReadChunkSize {
		chunk /= 2
	}

	return chunk
}

// readCacheSize derives the chunk count limit of the read cache from the chunk size.
func readCacheSize(chunkSize uint64) int {
	size := int(readCacheBudget / chunkSize)
	if size < readPrefetchDepth+1 {
		size = readPrefetchDepth + 1
	}

	return size
}

// grantAccess returns true if the user's access rights are sufficient for performing the requested operation(s) on the file.
func grantAccess(cr smb2.CreateRequest, tc *treeConnect, ss *session) bool {
	if tc.share.connectSecurity == nil || tc.share.fileSecurity == nil {
		return true
	}

	_, ok := tc.share.connectSecurity[ss.workgroup+"/"+ss.userName]
	if !ok {
		return false
	}

	fs := tc.share.fileSecurity[ss.workgroup+"/"+ss.userName]
	write := fs&(smb2.FILE_WRITE_DATA|smb2.FILE_APPEND_DATA|smb2.FILE_WRITE_EA|smb2.FILE_WRITE_ATTRIBUTES) > 0
	del := fs&(smb2.DELETE|smb2.FILE_DELETE_CHILD) > 0

	cd := cr.CreateDisposition()
	co := cr.CreateOptions()
	da := cr.DesiredAccess()

	if fs&da == 0 {
		return false
	}

	if !write && ((cd&(smb2.FILE_SUPERSEDE|smb2.FILE_CREATE|smb2.FILE_OPEN_IF|smb2.FILE_OVERWRITE|smb2.FILE_OVERWRITE_IF) > 0) || (co&smb2.FILE_WRITE_THROUGH > 0)) {
		return false
	}

	if !del && (co&smb2.FILE_DELETE_ON_CLOSE > 0) {
		return false
	}

	return true
}

// fileState is what every open on the same file shares: the size and the timestamps that a write
// through one handle has to be visible through another, the attributes, and the upload that is
// carrying the file to the backend. An open points at it rather than holding the fields itself, so
// that a client asking after a file while another of its handles is still uploading it is answered
// with what the file is now, and not with what the handle was opened on.
//
// It is also the record by which a file that has been created but not yet uploaded is known. The
// state outlives the opens on it, which is what lets a create that comes later find such a file
// although the backend has no object for it. Nothing else about an open is shared: the file ID, the
// caching promise and the read cache all belong to the handle alone, and every create makes its own.
//
// mu guards all of it, and is a leaf: no other lock of the server is taken while it is held.
type fileState struct {
	mu sync.Mutex

	created  time.Time
	modified time.Time

	// size is how much space the file occupies. allocated usually means the same, except while a
	// file is being uploaded, when it may hold the size the file is going to be - which of the
	// two a client sets is up to the client.
	size      uint64
	allocated uint64

	attributes uint32

	// upload, when it is not nil, is the multipart upload the file is being written through.
	// Every open on the file writes through the same one.
	upload *upload

	// generation counts the times the file has been written anew. A read cache belongs to the
	// generation it was filled in, and an open drops its own once the file has moved on: the
	// caches belong to the handles, so a write through one of them has to reach the rest.
	generation uint64

	// inflight is how many writes are on their way into the upload, through any handle on the
	// file, and writes is what waits for them to land. They are counted per file and not per
	// handle because the upload is the file's: a handle that finalizes it while another handle's
	// write is still buffering finalizes an upload with a hole in it, which the flush then refuses
	// as non-contiguous and the file is lost.
	inflight int
	writes   *sync.Cond

	// startingMu serializes the start of an upload, which takes a call to the backend and so
	// cannot be done under mu.
	startingMu sync.Mutex
}

// writesCond returns what waits for the writes in flight, making it if this is the first waiter.
// fs.mu must be held.
func (fs *fileState) writesCond() *sync.Cond {
	if fs.writes == nil {
		fs.writes = sync.NewCond(&fs.mu)
	}

	return fs.writes
}

// beginWrite counts a write on its way into the file.
func (fs *fileState) beginWrite() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.inflight++
}

// endWrite counts one that has landed, and wakes whoever is waiting to finalize the upload.
func (fs *fileState) endWrite() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.inflight--
	fs.writesCond().Broadcast()
}

// waitForWrites waits until nothing is on its way into the file through any handle, and hands back
// the upload that is carrying it, if there is one.
//
// The waiting does not depend on there being an upload yet. A write starts one, so a write still in
// flight may not have started it: waiting only while there is one would let a close finish before the
// write it is racing had begun, and the file would be left with an upload nobody finishes and a client
// that was told its close succeeded.
func (fs *fileState) waitForWrites() *upload {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	for fs.inflight > 0 {
		fs.writesCond().Wait()
	}

	return fs.upload
}

// generationNow is which writing of the file the state is on.
func (fs *fileState) generationNow() uint64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.generation
}

// stat reads out everything a client is told about a file at once, so that the answer is of one
// moment rather than of several.
func (fs *fileState) stat() (size, allocated uint64, created, modified time.Time, attributes uint32) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.size, fs.allocated, fs.created, fs.modified, fs.attributes
}

// sizeNow is how large the file is at this moment.
func (fs *fileState) sizeNow() uint64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.size
}

// attributesNow is what the file is at this moment.
func (fs *fileState) attributesNow() uint32 {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.attributes
}

// isDirectory reports whether the file is a directory.
func (fs *fileState) isDirectory() bool {
	return fs.attributesNow()&smb2.FILE_ATTRIBUTE_DIRECTORY > 0
}

// markDirectory turns the file into a directory, which it is either from the moment it is made or
// never.
func (fs *fileState) markDirectory() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.attributes |= smb2.FILE_ATTRIBUTE_DIRECTORY
	fs.attributes &^= smb2.FILE_ATTRIBUTE_NORMAL
}

// setAttributes replaces what the file is.
func (fs *fileState) setAttributes(attributes uint32) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.attributes = attributes
}

// setAllocated sets the space the file is to occupy, which a client may ask for before it has
// written anything.
func (fs *fileState) setAllocated(allocated uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.allocated = allocated
}

// setModified moves the modification time forward, and never back: the time a client sets is
// refused if the file has been written since.
func (fs *fileState) setModified(modified time.Time) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if modified.After(fs.modified) {
		fs.modified = modified
	}
}

// touch marks the file as modified now.
func (fs *fileState) touch() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.modified = time.Now()
}

// empty is the file after a create that superseded or overwrote it. The contents are gone, so the
// file moves on a generation and every read cache on it is left behind: what a handle had cached is
// the contents of a file that no longer exists, and the handle that emptied it need not be the one
// that cached it.
func (fs *fileState) empty() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.size = 0
	fs.allocated = 0
	fs.modified = time.Now()
	fs.generation++
}

// uploadNow is the upload the file is being written through, if there is one.
func (fs *fileState) uploadNow() *upload {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.upload
}

// startUpload takes the upload on, and reports whether it did: a file already being written
// through one upload is not written through a second. The file is being written anew, so it moves
// on a generation and every read cache on it is left behind.
func (fs *fileState) startUpload(u *upload) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.upload != nil {
		return false
	}

	fs.upload = u
	fs.generation++

	return true
}

// advanceUpload takes the size the upload has reached, so that a client asking after the file
// while it is being written is told how much of it there is.
func (fs *fileState) advanceUpload(u *upload, size uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.upload != u {
		return
	}

	fs.size = size
	fs.allocated = size
	fs.modified = time.Now()
}

// finishUpload settles the file at the size it was written to and lets the upload go.
func (fs *fileState) finishUpload(u *upload, size uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.upload != u {
		return
	}

	fs.size = size
	fs.allocated = size
	fs.upload = nil
	fs.modified = time.Now()
}

// dropUpload lets an upload go without settling anything, for one that came to nothing.
func (fs *fileState) dropUpload(u *upload) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.upload == u {
		fs.upload = nil
	}
}

// registerOpen creates a new Open object and registers it with the server.
// fs is the state of the file the open is on, which is nil for a file being opened for the first
// time and the state every other open on it shares for one that is already known.
func (ss *session) registerOpen(cr smb2.CreateRequest, c *connection, tc *treeConnect, info client.ObjectInfo, ctx context.Context, cancel context.CancelFunc, fs *fileState) *open {
	h, _ := blake2b.New256(nil)
	h.Write([]byte(info.Key))
	id := h.Sum(nil)

	var filepath, filename string
	var isDir bool
	access := tc.maximalAccess
	name := strings.ToLower(info.Key)
	switch name {
	case "lsarpc", "srvsvc", "mdssvc": // Standard named pipes on MacOS, Linux, and Windows
		filename = name
		filepath = name
		access = cr.DesiredAccess()
	default:
		filepath, filename, isDir = utils.ExtractFilename(info.Key)
	}

	// A file that has been created but not yet uploaded is already known by its state, which
	// every open on it shares. Anything else is met here for the first time.
	if fs == nil {
		attributes := uint32(smb2.FILE_ATTRIBUTE_NORMAL)
		if isDir {
			attributes = smb2.FILE_ATTRIBUTE_DIRECTORY
		}
		fs = &fileState{
			created:    info.CreatedAt,
			modified:   info.ModifiedAt,
			size:       info.Size,
			allocated:  info.Size,
			attributes: attributes,
		}
	} else if isDir {
		fs.markDirectory()
	}

	fid := make([]byte, 16)
	frand.Read(fid)
	op := &open{
		handle:        binary.LittleEndian.Uint64(id[:8]),
		fileID:        binary.LittleEndian.Uint64(fid[:8]),
		durableFileID: binary.LittleEndian.Uint64(fid[8:]),
		session:       ss,
		connection:    c,
		treeConnect:   tc,
		grantedAccess: access,
		fileName:      filename,
		pathName:      filepath,
		resumeKey:     id[:24],
		createOptions: cr.CreateOptions(),
		file:          fs,
		ctx:           ctx,
		cancel:        cancel,
		lsaFrames:     make(map[uint32]*rpc.Frame),
		buffer:        make(map[uint64]*readChunk),
		// For indexd shares, maxUploadSize equals the slab size, which is also the
		// alignment that makes downloads the cheapest. For renterd shares, it is
		// merely the multipart part size; renterd serves arbitrary ranges, so the
		// chunk size derived from it is just a reasonable default.
		chunkSize: readChunkSize(tc.maxUploadSize),
	}
	op.maxCacheSize = readCacheSize(op.chunkSize)

	// The open remembers which client made it, because it may outlive the connection it was
	// made on and a replay may arrive over another one.
	if len(c.clientGuid) == 16 {
		op.clientGuid = [16]byte(c.clientGuid)
	}

	// The open starts off at the channel sequence the client created it with, so that the
	// requests that follow can be told apart from the ones that were issued before a
	// reconnect. The dialects without channels have no such field.
	if smb2.Is3X(c.negotiateDialect) {
		op.channelSequence = cr.Header().ChannelSequence()
	}

	ss.mu.Lock()
	ss.openTable[op.fileID] = op
	ss.mu.Unlock()

	c.server.mu.Lock()
	c.server.globalOpenTable[op.durableFileID] = op
	c.server.stats.FOpens++
	c.server.mu.Unlock()
	c.server.indexOpen(op)

	tc.mu.Lock()
	tc.openCount++
	tc.mu.Unlock()

	return op
}

// closeOpen cancels all operations on the Open and destroys it.
func (s *server) closeOpen(op *open) {
	op.cancel()

	// An open that goes away takes its oplock with it, and releases whoever was waiting for
	// the break it was in the middle of. The create that made it can no longer be replayed
	// either: a replay must never hand back a handle that has been closed.
	op.releaseCaching()
	s.clearReplayEligible(op)

	op.treeConnect.mu.Lock()
	op.treeConnect.openCount--
	op.treeConnect.mu.Unlock()

	op.session.mu.Lock()
	delete(op.session.openTable, op.fileID)
	op.session.mu.Unlock()

	s.mu.Lock()
	delete(s.globalOpenTable, op.durableFileID)
	s.mu.Unlock()
	s.unindexOpen(op)
}

// queryDirectory performs a search within the directory using the provided pattern.
// Wildcards are supported.
func (op *open) queryDirectory(acc stores.Account, pattern string) error {
	attr := op.file.attributesNow()
	op.mu.Lock()
	dir := op.pathName
	ctx := op.ctx
	op.mu.Unlock()

	if attr&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errNoDirectory
	}

	ois, err := op.treeConnect.client.List(ctx, acc, dir+"/")
	if err != nil {
		return err
	}

	var results []client.ObjectInfo
	found := make(map[string]struct{})
	for _, oi := range ois {
		path, name, _ := utils.ExtractFilename(oi.Key)
		if utils.MatchPattern(pattern, name) {
			results = append(results, oi)
			found[path] = struct{}{}
		}
	}

	// Search persisted Opens, too.
	results = append(results, op.treeConnect.persistedObjects(func(path string) bool {
		if _, ok := found[path]; ok {
			return false
		}
		if utils.TrimName(path) != dir {
			return false
		}
		return utils.MatchPattern(pattern, utils.TrimPath(path))
	})...)

	op.mu.Lock()
	op.lastSearch = pattern
	op.searchResults = results
	op.mu.Unlock()

	if len(results) == 0 {
		return errNoFiles
	}

	return nil
}

// id is a helper method that marshals the volatile and persistent ID parts into a byte sequence.
func (op *open) id() []byte {
	i := make([]byte, 16)
	binary.LittleEndian.PutUint64(i[:8], op.fileID)
	binary.LittleEndian.PutUint64(i[8:], op.durableFileID)
	return i
}

// fileAllInformation generates a FileAllInfo structure.
func (op *open) fileAllInformation() []byte {
	var size, alloc uint64
	var lc uint32
	var pd bool
	fileSize, allocated, _, modified, attributes := op.file.stat()
	isDir := attributes&smb2.FILE_ATTRIBUTE_DIRECTORY > 0
	op.mu.Lock()
	if strings.ToLower(op.fileName) == "srvsvc" {
		alloc = 4096
		lc = 1
		pd = true
	} else if !isDir {
		size = fileSize
		alloc = allocated
	}

	// A handle whose file is on its way out says so. Whether the deletion was asked for by the
	// create that made the handle or by a disposition set on it later, it is kept in the same
	// place, and the client decides what to do with the file on the strength of this.
	if op.createOptions&smb2.FILE_DELETE_ON_CLOSE > 0 {
		pd = true
	}

	fai := smb2.FileAllInfo{
		BasicInfo: smb2.FileBasicInfo{
			CreationTime:   modified,
			LastAccessTime: modified,
			LastWriteTime:  modified,
			ChangeTime:     modified,
			FileAttributes: attributes,
		},
		StandardInfo: smb2.FileStandardInfo{
			AllocationSize: alloc,
			EndOfFile:      size,
			NumberOfLinks:  lc,
			DeletePending:  pd,
			Directory:      isDir,
		},
		InternalInfo: smb2.FileInternalInfo{
			IndexNumber: op.handle,
		},
		AccessInfo: smb2.FileAccessInfo{
			AccessFlags: op.grantedAccess,
		},
		PositionInfo: smb2.FilePositionInfo{
			CurrentByteOffset: size,
		},
		ModeInfo: smb2.FileModeInfo{
			Mode: op.createOptions,
		},
		NameInfo: smb2.FileNameInfo{
			FileName: op.fileName,
		},
	}
	op.mu.Unlock()
	return fai.Encode()
}

// fileStandardInformation generates a FileStandardInfo structure.
func (op *open) fileStandardInformation() []byte {
	var size, alloc uint64
	var lc uint32
	var pd bool
	fileSize, allocated, _, _, attributes := op.file.stat()
	isDir := attributes&smb2.FILE_ATTRIBUTE_DIRECTORY > 0
	op.mu.Lock()
	if strings.ToLower(op.fileName) == "srvsvc" {
		alloc = 4096
		lc = 1
		pd = true
	} else if !isDir {
		size = fileSize
		alloc = allocated
	}

	// As above: a file that is going says so.
	if op.createOptions&smb2.FILE_DELETE_ON_CLOSE > 0 {
		pd = true
	}

	fsi := smb2.FileStandardInfo{
		AllocationSize: alloc,
		EndOfFile:      size,
		NumberOfLinks:  lc,
		DeletePending:  pd,
		Directory:      isDir,
	}
	op.mu.Unlock()
	return fsi.Encode()
}

// fileNetworkOpenInformation genereates a FileNetworkOpenInfo structure.
func (op *open) fileNetworkOpenInformation() []byte {
	var size, alloc uint64
	fileSize, allocated, _, modified, attributes := op.file.stat()
	if attributes&smb2.FILE_ATTRIBUTE_DIRECTORY == 0 {
		size = fileSize
		alloc = allocated
	}
	fnoi := smb2.FileNetworkOpenInfo{
		CreationTime:   modified,
		LastAccessTime: modified,
		LastWriteTime:  modified,
		ChangeTime:     modified,
		AllocationSize: alloc,
		EndOfFile:      size,
		FileAttributes: attributes,
	}
	return fnoi.Encode()
}

// fileNormalizedNameInformation genereates a FileNormalizedNameInfo structure.
func (op *open) fileNormalizedNameInformation() []byte {
	op.mu.Lock()
	fnni := smb2.FileNormalizedNameInfo{
		Filename: op.pathName,
	}
	op.mu.Unlock()
	return fnni.Encode()
}

// fileEaInformation genereates a FileEaInfo structure.
func (op *open) fileEaInformation() []byte {
	feai := smb2.FileEaInfo{
		EaSize: 0,
	}
	return feai.Encode()
}

// fileStreamInformation generates a FileStreamInfo structure.
func (op *open) fileStreamInformation() []byte {
	size, allocated, _, _, _ := op.file.stat()
	fsi := smb2.FileStreamInfo{
		StreamName:           "::$DATA",
		StreamSize:           size,
		StreamAllocationSize: allocated,
	}
	return fsi.Encode()
}

// newLSAFrame generates an LSA frame from the NTLM security context.
func (op *open) newLSAFrame(ctx ntlm.SecurityContext) *rpc.Frame {
	op.mu.Lock()
	defer op.mu.Unlock()

	id := make([]byte, 16)
	frand.Read(id)
	guid, _ := dtyp.GUIDFromBytes(id)
	frame := &rpc.Frame{
		Handle: lsarpc.Handle{
			Attributes: 1,
			UUID:       guid,
		},
		SecurityContext: ctx,
	}

	op.lsaFrames[frame.Handle.UUID.Data1] = frame
	return frame
}

// directorySnapshot reduces the directory of the open to a fingerprint that changes whenever
// anything a client would care about changes: names, sizes, modify times. The files created
// during the session and not yet uploaded are folded in from the persisted opens, since the
// backend does not know of them yet.
func (op *open) directorySnapshot(acc stores.Account) ([]byte, error) {
	op.mu.Lock()
	dir := op.pathName
	ctx := op.ctx
	op.mu.Unlock()

	ois, err := op.treeConnect.client.List(ctx, acc, dir)
	if err != nil {
		return nil, err
	}

	found := make(map[string]struct{})
	for _, oi := range ois {
		if oi.Key == "" {
			continue
		}
		if strings.HasPrefix(oi.Key[1:], dir) {
			found[oi.Key[1:]] = struct{}{}
		}
	}

	ois = append(ois, op.treeConnect.persistedObjects(func(path string) bool {
		_, ok := found[path]
		return !ok
	})...)

	return makeSnapshot(ois), nil
}

// checkForChanges monitors if any significant changes have occurred in the specified directory.
// Significant changes include: file names, sizes, modify times, or contents. The snapshot is the
// state of the directory as it stood when the watch was granted: it is taken by the caller, so
// that a directory that cannot be looked at at all is answered synchronously rather than armed
// and left to fail behind an interim response.
func (op *open) checkForChanges(req smb2.ChangeNotifyRequest, c *connection, acc stores.Account, snapshot []byte, stopChan chan struct{}) {
	for {
		select {
		case <-stopChan: // Execution terminated
			// Whoever closed the channel — a cancel answering the request, or the connection
			// going away underneath it — the request is over, and its share of the
			// outstanding request counters of the open goes back. The open may well outlive
			// the connection, so the counters must not wait for the connection's tables to
			// go. The cancel path has already released it, and a second release is a no-op.
			c.releaseOpen(&req.Request)
			return
		case <-time.After(15 * time.Second): // Check every 15 seconds
		}

		newSnapshot, err := op.directorySnapshot(acc)
		if err != nil {
			continue
		}

		if !bytes.Equal(newSnapshot, snapshot) {
			// Normally, the server should monitor the changes according to the filter specified in each
			// SMB2_CHANGE_NOTIFY request. If the WATCH_TREE flag is set, the server should also monitor
			// the entire directory tree underneath. This is a lot of effort. Fortunately, there is a
			// way to catch just any change and respond with the status STATUS_NOTIFY_ENUM_DIR, which
			// will simply trigger a rescan of the directory, exactly what we need.
			resp := &smb2.ChangeNotifyResponse{}
			resp.FromRequest(req)
			resp.Header().SetStatus(smb2.STATUS_NOTIFY_ENUM_DIR)
			// The bookkeeping of the request belongs to the connection it arrived on, but
			// the response itself may travel over any channel of the session. An answered
			// request leaves the async command list as well, or the close of the open would
			// find it there and answer it a second time with STATUS_NOTIFY_CLEANUP.
			c.releaseOpen(&req.Request)
			conn := op.selectConnection(c)
			conn.server.writeResponse(conn, op.session, resp)
			c.mu.Lock()
			delete(c.asyncCommandList, req.Header().AsyncID())
			delete(c.stopChans, req.CancelRequestID())
			c.mu.Unlock()
			return
		}
	}
}

// makeSnapshot takes a snapshot of the directory.
func makeSnapshot(ois []client.ObjectInfo) []byte {
	h, err := blake2b.New256(nil)
	if err != nil {
		return nil
	}

	for _, oi := range ois {
		h.Write([]byte(oi.Key))
		h.Write([]byte(oi.CreatedAt.String()))
		h.Write([]byte(oi.ModifiedAt.String()))
		h.Write(binary.LittleEndian.AppendUint64(nil, oi.Size))
	}

	return h.Sum(nil)
}

// getResumeKey is a helper method that generates a response to the FSCTL_SRV_REQUEST_RESUME_KEY query.
func (op *open) getResumeKey() []byte {
	key := make([]byte, 32)
	copy(key[:24], op.resumeKey)
	return key
}

// getObjectID is a helper method that generates a response to the FSCTL_CREATE_OR_GET_OBJECT_ID query.
func (op *open) getObjectID() []byte {
	id := make([]byte, 64)
	copy(id[:16], op.resumeKey[:16])
	binary.LittleEndian.PutUint64(id[16:24], op.treeConnect.volumeID)
	copy(id[32:48], op.resumeKey[:16])
	return id
}

// tryReadCached serves a read synchronously if every chunk of the requested range
// has already been downloaded. It returns false if any part of the range is missing
// or still in flight, in which case the caller should fall back to op.read.
func (op *open) tryReadCached(offset, length uint64) ([]byte, bool) {
	// A file with an upload running is being written, and the only account of what it holds is
	// the one the upload keeps. The cache is emptied when the upload starts and nothing fills it
	// again while the upload runs, so this is the same answer either way - but it is said here
	// rather than left to be worked out from the two of them, because this path answers the
	// client on its own and must not be able to serve the file as it stood before the write.
	size, _, _, _, _ := op.file.stat()
	if op.file.uploadNow() != nil {
		return nil, false
	}
	generation := op.file.generationNow()

	op.mu.Lock()
	op.dropCacheOfOlderGeneration(generation)
	chunkSize := op.chunkSize

	if offset >= size {
		op.mu.Unlock()
		return nil, false
	}
	if offset+length >= size {
		length = size - offset
	}

	firstChunk := (offset / chunkSize) * chunkSize
	lastChunk := ((offset + length - 1) / chunkSize) * chunkSize

	var chunks []*readChunk
	for chunkOffset := firstChunk; chunkOffset <= lastChunk; chunkOffset += chunkSize {
		chunk, ok := op.buffer[chunkOffset]
		if !ok {
			op.mu.Unlock()
			return nil, false
		}
		select {
		case <-chunk.done:
		default:
			op.mu.Unlock()
			return nil, false
		}
		if chunk.err != nil {
			op.mu.Unlock()
			return nil, false
		}
		op.touchChunk(chunkOffset)
		chunks = append(chunks, chunk)
	}

	// If the reads look sequential, prefetch the next chunks so that the transfer
	// keeps going while the already cached data is being served. A read counts as
	// sequential if it lands near where the previous read ended, or if it continues
	// a chunk that is already cached — the latter keeps the prefetch alive when the
	// reads of two file regions are interleaved (e.g. a video player periodically
	// consulting the index at the end of the file during playback). This is the only
	// path that prefetches: a prefetch started by a read that is itself waiting for
	// a download would compete with it for bandwidth.
	prevCached := false
	if firstChunk >= chunkSize {
		_, prevCached = op.buffer[firstChunk-chunkSize]
	}
	sequential := offset == 0 || prevCached ||
		(offset <= op.lastReadEnd+2*chunkSize && op.lastReadEnd <= offset+length+2*chunkSize)
	op.lastReadEnd = offset + length

	var prefetch []uint64
	if sequential {
		for i, chunkOffset := 0, lastChunk+chunkSize; i < readPrefetchDepth && chunkOffset < size; i, chunkOffset = i+1, chunkOffset+chunkSize {
			if _, ok := op.buffer[chunkOffset]; !ok {
				prefetch = append(prefetch, chunkOffset)
			}
		}
	}

	result := make([]byte, 0, length)
	for i, chunk := range chunks {
		chunkOffset := firstChunk + uint64(i)*chunkSize
		start := uint64(0)
		if offset > chunkOffset {
			start = offset - chunkOffset
		}
		end := uint64(len(chunk.data))
		if chunkOffset+end > offset+length {
			end = offset + length - chunkOffset
		}
		result = append(result, chunk.data[start:end]...)
	}
	op.mu.Unlock()

	if len(prefetch) > 0 {
		acc, err := op.session.connection.server.store.FindAccount(op.session.userName, op.session.workgroup)
		if err == nil {
			op.mu.Lock()
			for _, chunkOffset := range prefetch {
				op.ensureChunk(acc, chunkOffset, size)
			}
			op.mu.Unlock()
		}
	}

	return result, true
}

// read retrieves the requested chunk of data, downloading the missing parts from
// the Sia network and caching them. It blocks until the downloads complete; reads
// that can be served from the cache alone should use tryReadCached instead.
func (op *open) read(offset, length uint64) ([]byte, error) {
	// Fetch variables for convenience.
	size := op.file.sizeNow()
	generation := op.file.generationNow()
	op.mu.Lock()
	op.dropCacheOfOlderGeneration(generation)
	path := op.pathName
	chunkSize := op.chunkSize
	op.mu.Unlock()

	if offset >= size {
		return nil, nil
	}

	if offset+length >= size {
		length = size - offset
	}

	// A file that is being uploaded is not an object in the store yet, so there is nothing there
	// to fetch: the read is answered out of what the upload is still holding, or not at all.
	if u := op.file.uploadNow(); u != nil {
		if data, ok := u.readBuffered(offset, length); ok {
			return data, nil
		}

		// The range is behind the part of the file still in memory. Nothing can answer it until
		// the upload is completed, which is the file being closed. It is not a failure of the
		// store and is not reported as one.
		return nil, errNotUploaded
	}

	acc, err := op.session.connection.server.store.FindAccount(op.session.userName, op.session.workgroup)
	if err != nil {
		log.Printf("Access denied (%s): %v", path, err)
		return nil, err
	}

	firstChunk := (offset / chunkSize) * chunkSize
	lastChunk := ((offset + length - 1) / chunkSize) * chunkSize

	// Start (or find in flight) the downloads of all chunks of the requested range,
	// then release the lock before waiting on them, so that concurrent reads on the
	// same open can proceed in parallel. No prefetch is started here — it would
	// compete with the blocking downloads for bandwidth; cache-hit reads keep the
	// prefetch pipeline going instead (see tryReadCached).
	op.mu.Lock()
	var chunks []*readChunk
	for chunkOffset := firstChunk; chunkOffset <= lastChunk; chunkOffset += chunkSize {
		chunks = append(chunks, op.ensureChunk(acc, chunkOffset, size))
	}
	op.lastReadEnd = offset + length
	op.mu.Unlock()

	result := make([]byte, 0, length)
	for i, chunk := range chunks {
		<-chunk.done
		if chunk.err != nil {
			log.Printf("Error reading object: %s: %v", path, chunk.err)
			return nil, chunk.err
		}

		chunkOffset := firstChunk + uint64(i)*chunkSize
		start := uint64(0)
		if offset > chunkOffset {
			start = offset - chunkOffset
		}
		end := uint64(len(chunk.data))
		if chunkOffset+end > offset+length {
			end = offset + length - chunkOffset
		}

		result = append(result, chunk.data[start:end]...)
	}

	return result, nil
}

// touchChunk moves the chunk to the back of the eviction queue. op.mu must be held.
func (op *open) touchChunk(chunkOffset uint64) {
	for i, co := range op.cacheOrder {
		if co == chunkOffset {
			op.cacheOrder = append(op.cacheOrder[:i], op.cacheOrder[i+1:]...)
			op.cacheOrder = append(op.cacheOrder, chunkOffset)
			return
		}
	}
}

// ensureChunk returns the cache entry for the chunk at chunkOffset, starting a
// background download if there is no entry yet. op.mu must be held.
func (op *open) ensureChunk(acc stores.Account, chunkOffset, size uint64) *readChunk {
	if chunk, ok := op.buffer[chunkOffset]; ok {
		op.touchChunk(chunkOffset)
		return chunk
	}

	chunk := &readChunk{done: make(chan struct{})}
	op.buffer[chunkOffset] = chunk
	op.cacheOrder = append(op.cacheOrder, chunkOffset)
	op.evictChunks()

	toRead := op.chunkSize
	if chunkOffset+toRead > size {
		toRead = size - chunkOffset
	}
	path := op.pathName

	go func() {
		var buf bytes.Buffer
		err := op.treeConnect.client.Read(op.ctx, acc, path, chunkOffset, toRead, &buf)

		op.mu.Lock()
		if err != nil {
			chunk.err = err
			// Drop the failed chunk from the cache so that a later read retries it.
			if op.buffer[chunkOffset] == chunk {
				delete(op.buffer, chunkOffset)
				for i, co := range op.cacheOrder {
					if co == chunkOffset {
						op.cacheOrder = append(op.cacheOrder[:i], op.cacheOrder[i+1:]...)
						break
					}
				}
			}
		} else {
			chunk.data = buf.Bytes()
		}
		op.mu.Unlock()

		close(chunk.done)
	}()

	return chunk
}

// invalidateReadCache drops everything the read cache is holding. It has to happen the moment
// the file stops being the one those chunks were downloaded from, which is the moment a write
// arrives: a write replaces the object wholesale rather than editing it in place, so every chunk
// of the previous contents is stale from then on. Left in place, the cache answers a read of a
// region the client has just overwritten with the bytes that were there before.
//
// A download still in flight is left to run. Its entry is gone from the table, so no later read
// finds it, and the read that started it is served the file as it stood when that read began,
// which is the most that can be said for a read racing a write over the same bytes anyway.
//
// op.mu must be held.
// dropCacheOfOlderGeneration empties the read cache if the file has been written anew since it was
// filled. op.mu must be held.
func (op *open) dropCacheOfOlderGeneration(generation uint64) {
	if op.cacheGeneration == generation {
		return
	}

	op.invalidateReadCache()
	op.cacheGeneration = generation
}

func (op *open) invalidateReadCache() {
	if len(op.buffer) == 0 {
		return
	}

	op.buffer = make(map[uint64]*readChunk)
	op.cacheOrder = nil
	op.lastReadEnd = 0
}

// evictChunks drops the least recently used completed chunks until the cache fits
// into its limit. Chunks that are still being downloaded are never dropped, and
// touchChunk refreshes a chunk's position, so periodically re-read chunks (like a
// video index) stay cached. op.mu must be held.
func (op *open) evictChunks() {
	for len(op.buffer) > op.maxCacheSize {
		evicted := false
		for i, chunkOffset := range op.cacheOrder {
			chunk := op.buffer[chunkOffset]
			select {
			case <-chunk.done:
			default:
				continue // still in flight
			}

			delete(op.buffer, chunkOffset)
			op.cacheOrder = append(op.cacheOrder[:i], op.cacheOrder[i+1:]...)
			evicted = true
			break
		}

		if !evicted {
			// Everything is still in flight; allow a temporary overshoot.
			break
		}
	}
}

// startUpload initiates a multipart upload.
// The upload belongs to the file and not to the handle, so every open on the file writes through
// the one upload. Starting it takes a call to the backend, which cannot be made under the lock that
// guards the state, so the start is serialized on a lock of its own instead.
func (op *open) startUpload() error {
	op.file.startingMu.Lock()
	defer op.file.startingMu.Unlock()

	if op.file.uploadNow() != nil {
		return nil
	}

	acc, err := op.session.connection.server.store.FindAccount(op.session.userName, op.session.workgroup)
	if err != nil {
		return err
	}

	op.mu.Lock()
	ctx, path := op.ctx, op.pathName
	op.mu.Unlock()

	id, err := op.treeConnect.client.StartUpload(ctx, acc, path)
	if err != nil {
		return err
	}

	// From here the file is being written anew, so whatever was cached of the old one is no
	// longer an answer to anything. The caches of the other opens on the file go with the
	// generation the upload moves the file on to.
	op.mu.Lock()
	op.invalidateReadCache()
	op.mu.Unlock()

	op.file.startUpload(&upload{
		uploadID:   id,
		pending:    make(map[uint64]*uploadChunk),
		nextOffset: 0,
		bufOffset:  0,
		maxLength:  op.treeConnect.maxUploadSize,
	})

	return nil
}

// write buffers contiguous chunks of data and uploads them as needed.
func (op *open) write(offset uint64, data []byte) error {
	u := op.file.uploadNow()
	if u == nil {
		if err := op.startUpload(); err != nil {
			return err
		}
		u = op.file.uploadNow()
	}

	if u == nil {
		return errors.New("write: the upload went away before anything was written")
	}

	u.mu.Lock()

	// A client may write over what it has already written. An office application builds its file,
	// then writes the whole of it again from the start - longer the second time, as the last of the
	// parts is added - and saves. The upload takes the bytes in the order they are to be stored, so a
	// write landing behind where the buffering has reached is not another chunk to be queued: queued,
	// it would sit in pending for ever and take the whole file down with it at the flush, as data
	// that is not contiguous.
	//
	// It can be answered as long as the bytes it covers have not gone to the store yet, which is to
	// say the buffer still begins at or before it. What overlaps is written over, and anything past
	// the end carries the buffer on. Bytes already sent are beyond recall, and saying so is better
	// than storing a file that is not the one the client wrote.
	if offset < u.nextOffset {
		if offset < u.bufOffset {
			u.mu.Unlock()

			return errors.New("write: rewriting a part of the file that has already been stored")
		}

		written := copy(u.buf[offset-u.bufOffset:], data)
		if written < len(data) {
			u.buf = append(u.buf, data[written:]...)
			u.nextOffset = offset + uint64(len(data))
			u.totalSize = u.nextOffset
		}

		op.file.advanceUpload(u, u.totalSize)
	} else {
		buf := make([]byte, len(data))
		copy(buf, data)
		u.pending[offset] = &uploadChunk{offset: offset, data: buf}

		for {
			ch, ok := u.pending[u.nextOffset]
			if !ok {
				break
			}

			if len(u.buf) == 0 {
				u.bufOffset = u.nextOffset
			}

			u.buf = append(u.buf, ch.data...)
			u.totalSize += uint64(len(ch.data))
			u.nextOffset += uint64(len(ch.data))
			delete(u.pending, ch.offset)

			op.file.advanceUpload(u, u.totalSize)
		}
	}

	// Detach any complete slabs while holding the lock, then upload them
	// after releasing it, so that concurrent writes to the same file can
	// keep buffering in the meantime.
	var slabs []uploadChunk
	var partNumbers []int
	for uint64(len(u.buf)) >= u.maxLength {
		u.partCount++
		slabs = append(slabs, uploadChunk{offset: u.bufOffset, data: u.buf[:u.maxLength:u.maxLength]})
		partNumbers = append(partNumbers, u.partCount)
		u.buf = u.buf[u.maxLength:]
		u.bufOffset += u.maxLength
	}
	u.mu.Unlock()

	for i, slab := range slabs {
		eTag, err := op.treeConnect.client.Write(
			op.ctx,
			bytes.NewReader(slab.data),
			op.pathName,
			u.uploadID,
			partNumbers[i],
			slab.offset,
			uint64(len(slab.data)),
		)
		if err != nil {
			return err
		}

		u.mu.Lock()
		u.parts = append(u.parts, api.MultipartCompletedPart{
			PartNumber: partNumbers[i],
			ETag:       eTag,
		})
		u.mu.Unlock()
	}

	return nil
}

// flush uploads the remaining part and finalizes the upload.
func (op *open) flush() error {
	u := op.file.waitForWrites()
	if u == nil {
		return nil
	}

	u.mu.Lock()

	for {
		ch, ok := u.pending[u.nextOffset]
		if !ok {
			break
		}
		if len(u.buf) == 0 {
			u.bufOffset = u.nextOffset
		}
		u.buf = append(u.buf, ch.data...)
		u.totalSize += uint64(len(ch.data))
		u.nextOffset += uint64(len(ch.data))
		delete(u.pending, ch.offset)
	}

	if len(u.pending) != 0 {
		u.mu.Unlock()
		return errors.New("flush: non-contiguous pending write data")
	}

	if len(u.buf) > 0 {
		u.partCount++
		partOffset := u.bufOffset
		partSize := uint64(len(u.buf))
		eTag, err := op.treeConnect.client.Write(
			op.ctx,
			bytes.NewReader(u.buf),
			op.pathName,
			u.uploadID,
			u.partCount,
			partOffset,
			partSize,
		)
		if err != nil {
			u.mu.Unlock()
			return err
		}

		u.parts = append(u.parts, api.MultipartCompletedPart{
			PartNumber: u.partCount,
			ETag:       eTag,
		})
		u.buf = nil
	}

	uploadID := u.uploadID
	parts := append([]api.MultipartCompletedPart(nil), u.parts...)
	finalSize := u.totalSize
	u.mu.Unlock()

	if err := op.treeConnect.client.FinishUpload(op.ctx, op.pathName, uploadID, parts); err != nil {
		return err
	}

	op.file.finishUpload(u, finalSize)

	op.mu.Lock()
	path := op.pathName
	op.mu.Unlock()
	op.treeConnect.forgetPersistedFile(path)

	return nil
}

// cancelUpload aborts the running upload.
func (op *open) cancelUpload() {
	u := op.file.uploadNow()
	if u == nil {
		return
	}
	op.file.dropUpload(u)

	u.mu.Lock()
	uploadID := u.uploadID
	u.mu.Unlock()

	_ = op.treeConnect.client.AbortUpload(op.ctx, op.pathName, uploadID)
}
