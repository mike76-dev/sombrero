package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/api/app"
	"go.sia.tech/indexd/slabs"
	"go.sia.tech/renterd/v2/api"
	sdk "go.sia.tech/siastorage"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/sync/errgroup"
)

// uploadWorkers is the number of concurrent slab uploads per share.
// Slab uploads are latency-bound, so a few parallel uploads multiply
// throughput at the cost of holding that many slabs in memory.
const uploadWorkers = 3

// slabDownloadThreads is the maximum number of concurrent slab downloads
// within a single Read call. A read range only spans multiple slabs when
// the slab size is smaller than the read chunk size (low dataShards).
const slabDownloadThreads = 4

// packInterval is how often the packer looks for buffered pieces to combine on
// its own. It only serves as a fallback: an upload that leaves a piece behind
// signals the packer directly.
const packInterval = time.Minute

// retryDelay is how long a worker waits after a failed job, and retryMax how far
// that wait is allowed to grow while the failures keep coming. A storage backend
// with no hosts to write to fails every job it is given, and at the shortest
// wait each worker asks it again once a second, for as long as the outage lasts.
const (
	retryDelay = time.Second
	retryMax   = time.Minute
)

// nextRetry doubles the wait after another failure, up to retryMax.
func nextRetry(d time.Duration) time.Duration {
	if d < retryDelay {
		return retryDelay
	}

	return min(d*2, retryMax)
}

// shutdownDrainTimeout is how long Close lets the background workers finish
// what they already have in flight before cutting them off. A backend call that
// is cut short after it has done its work leaves a slab behind — uploaded but
// never pinned — so shutdown gives it a window to come back on its own. The
// cancellation that follows is what guarantees that Close returns at all.
const shutdownDrainTimeout = 30 * time.Second

// completionAttempts and completionRetryDelay bound the retries of recording
// an uploaded slab in the database. By that point the slab has been paid for
// and the claimed pieces are no longer in the queue, so completion is worth a
// few attempts before the pieces are put back.
const (
	completionAttempts   = 3
	completionRetryDelay = time.Second
)

// completeWithRetry retries the given completion on failures, so that a batch
// whose slab has already been uploaded is not abandoned over a database
// hiccup. ErrNotFound is not retried: it is a definite answer, not a failure.
func completeWithRetry(complete func() error) (err error) {
	for i := range completionAttempts {
		if i > 0 {
			time.Sleep(completionRetryDelay)
		}
		if err = complete(); err == nil || errors.Is(err, stores.ErrNotFound) {
			return err
		}
	}
	return err
}

// storageBackend is the minimal interface for `indexd` SDK.
type storageBackend interface {
	Account(ctx context.Context) (app.AccountResponse, error)
	Upload(ctx context.Context, r io.Reader, dataShards, parityShards uint8) (types.Hash256, error)
	Download(ctx context.Context, key types.Hash256, offset, length uint64, w io.Writer) error
	DeleteObject(ctx context.Context, key types.Hash256) error
	PruneSlabs(ctx context.Context) error
	ListObjects(ctx context.Context, cursor slabs.Cursor, limit int) ([]PinnedObject, error)
	Close() error
}

// PinnedObject is one entry of the object event log of the connection's indexd
// app account: a slab this connection pinned, or the record of one it dropped.
type PinnedObject struct {
	Key       types.Hash256
	Size      uint64
	UpdatedAt time.Time
	Deleted   bool
}

// objectPageSize is how many object events are fetched per request when the
// whole log is walked.
const objectPageSize = 100

// objCacheSize is the maximum number of slab objects cached by sdkBackend.
const objCacheSize = 128

// sdkBackend is a wrapper around the SDK.
type sdkBackend struct {
	sdk *sdk.SDK

	// Slab objects are content-addressed and immutable, so the lookups are
	// cached to save a round trip on repeat downloads of the same slab.
	mu       sync.Mutex
	objCache map[types.Hash256]sdk.Object
	objOrder []types.Hash256
}

// object returns the slab object with the given key, looking it up remotely
// on a cache miss.
func (b *sdkBackend) object(ctx context.Context, key types.Hash256) (sdk.Object, error) {
	b.mu.Lock()
	obj, ok := b.objCache[key]
	b.mu.Unlock()
	if ok {
		return obj, nil
	}

	obj, err := b.sdk.Object(ctx, key)
	if err != nil {
		return sdk.Object{}, err
	}

	b.mu.Lock()
	if _, ok := b.objCache[key]; !ok {
		b.objCache[key] = obj
		b.objOrder = append(b.objOrder, key)
		if len(b.objOrder) > objCacheSize {
			delete(b.objCache, b.objOrder[0])
			b.objOrder = b.objOrder[1:]
		}
	}
	b.mu.Unlock()

	return obj, nil
}

// forgetObject removes the slab object with the given key from the cache.
func (b *sdkBackend) forgetObject(key types.Hash256) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.objCache[key]; !ok {
		return
	}

	delete(b.objCache, key)
	for i, k := range b.objOrder {
		if k == key {
			b.objOrder = append(b.objOrder[:i], b.objOrder[i+1:]...)
			break
		}
	}
}

// Account calls sdk.Account.
func (b *sdkBackend) Account(ctx context.Context) (app.AccountResponse, error) {
	return b.sdk.Account(ctx)
}

// Upload uploads the object and directly pins it.
func (b *sdkBackend) Upload(ctx context.Context, r io.Reader, dataShards, parityShards uint8) (types.Hash256, error) {
	obj := sdk.NewEmptyObject()
	if err := b.sdk.Upload(ctx, &obj, r, sdk.WithRedundancy(dataShards, parityShards)); err != nil {
		return types.Hash256{}, err
	}

	key := obj.ID()
	if err := b.sdk.PinObject(ctx, obj); err != nil {
		if derr := b.sdk.DeleteObject(ctx, key); derr != nil {
			return types.Hash256{}, fmt.Errorf("failed to pin slab %s: %w; failed to delete orphaned slab: %w", key, err, derr)
		}

		if perr := b.sdk.PruneSlabs(ctx); perr != nil {
			return types.Hash256{}, fmt.Errorf("failed to pin slab %s: %w; failed to prune: %w", key, err, perr)
		}

		return types.Hash256{}, fmt.Errorf("failed to pin slab %s: %w", key, err)
	}

	return key, nil
}

// Download downloads the object by its key.
func (b *sdkBackend) Download(ctx context.Context, key types.Hash256, offset, length uint64, w io.Writer) error {
	obj, err := b.object(ctx, key)
	if err != nil {
		return err
	}

	rc, err := b.sdk.Download(obj, sdk.WithDownloadRange(offset, length))
	if err != nil {
		return err
	}
	defer rc.Close()

	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := rc.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// DeleteObject calls sdk.DeleteObject.
func (b *sdkBackend) DeleteObject(ctx context.Context, key types.Hash256) error {
	b.forgetObject(key)
	return b.sdk.DeleteObject(ctx, key)
}

// PruneSlabs calls sdk.PruneSlabs.
func (b *sdkBackend) PruneSlabs(ctx context.Context) error {
	return b.sdk.PruneSlabs(ctx)
}

// ListObjects returns one page of the account's object event log, oldest event
// first. A deleted object is reported as an event without an object, which is
// how the log keeps a record of what is gone.
func (b *sdkBackend) ListObjects(ctx context.Context, cursor slabs.Cursor, limit int) ([]PinnedObject, error) {
	events, err := b.sdk.ObjectEvents(ctx, cursor, limit)
	if err != nil {
		return nil, err
	}

	objs := make([]PinnedObject, 0, len(events))
	for _, ev := range events {
		obj := PinnedObject{
			Key:       ev.Key,
			UpdatedAt: ev.UpdatedAt,
			Deleted:   ev.Deleted || ev.Object == nil,
		}
		if ev.Object != nil {
			obj.Size = ev.Object.Size()
		}
		objs = append(objs, obj)
	}

	return objs, nil
}

// Close calls sdk.Close.
func (b *sdkBackend) Close() error {
	return b.sdk.Close()
}

// IndexdClient implements a Client for interacting with indexd.
//
// A client serves one (workgroup, share) connection and is authenticated as
// that workgroup's indexd app. Its background workers therefore only claim
// the jobs of its own workgroup and share: data claimed by any other client
// would be pinned under an account its owners do not control.
type IndexdClient struct {
	share        string
	workgroup    int
	db           *stores.Database
	backend      storageBackend
	dataShards   uint8
	parityShards uint8
	slabSize     uint64
	minPackSize  uint64
	maxBufferAge time.Duration
	fragLevel    float64
	fragInterval time.Duration
	debug        bool

	// Shutdown happens in two stages. Close closes drainChan, on which the
	// background loops stop taking on new work, and cancels ctx once they
	// have had shutdownDrainTimeout to finish what is still in flight. A
	// backend call aborted by that cancellation fails like any other, so the
	// piece it was uploading is requeued rather than lost.
	ctx          context.Context
	cancel       context.CancelFunc
	drainChan    chan struct{}
	drainTimeout time.Duration
	closeOnce    sync.Once
	jobsChan     chan struct{}
	packChan     chan struct{}
	wg           sync.WaitGroup

	// claimed holds the metadata IDs of the pieces this client has claimed
	// and not yet completed or requeued, so that the janitor for stranded
	// pieces does not mistake work in progress for a leftover of a crash.
	claimedMu sync.Mutex
	claimed   map[uint64]struct{}
}

// markClaimed records the given pieces as being worked on.
func (ic *IndexdClient) markClaimed(ids ...uint64) {
	ic.claimedMu.Lock()
	defer ic.claimedMu.Unlock()
	for _, id := range ids {
		ic.claimed[id] = struct{}{}
	}
}

// unmarkClaimed removes the given pieces from the work in progress.
func (ic *IndexdClient) unmarkClaimed(ids ...uint64) {
	ic.claimedMu.Lock()
	defer ic.claimedMu.Unlock()
	for _, id := range ids {
		delete(ic.claimed, id)
	}
}

// isClaimed reports whether the piece is being worked on right now.
func (ic *IndexdClient) isClaimed(id uint64) bool {
	ic.claimedMu.Lock()
	defer ic.claimedMu.Unlock()
	_, ok := ic.claimed[id]
	return ok
}

// PackingOptions describes when the data that does not fill a slab is uploaded
// anyway, instead of waiting to be packed together with the data of other
// files. The zero value keeps it waiting for as long as that takes, which is
// what makes every uploaded slab a full one.
type PackingOptions struct {
	// MinSize is the least amount of leftover data, in bytes, that an
	// incomplete slab is uploaded with. Zero puts no lower bound on it.
	MinSize uint64

	// MaxAge is how long the leftover data may wait. Zero means forever, in
	// which case MinSize has no effect either.
	MaxAge time.Duration
}

// FragmentationOptions describes how the slabs are watched for the dead space
// that editing and deleting files leaves behind in them.
type FragmentationOptions struct {
	// Threshold is the fraction of a slab that may be dead space before the
	// slab is reported. Anything outside of (0, 1] falls back to the default.
	Threshold float64

	// Interval is how often to look. Zero turns the check off.
	Interval time.Duration
}

// NewIndexdClient returns an initialized IndexdClient serving the given
// workgroup's connection to the share.
func NewIndexdClient(db *stores.Database, sdkClient *sdk.SDK, share string, workgroup int, dataShards, parityShards uint8, packing PackingOptions, fragmentation FragmentationOptions, debug bool) Client {
	backend := &sdkBackend{
		sdk:      sdkClient,
		objCache: make(map[types.Hash256]sdk.Object),
	}
	return newIndexdClient(db, backend, share, workgroup, dataShards, parityShards, packing, fragmentation, debug)
}

// newIndexdClient allows using a mock SDK for testing.
func newIndexdClient(db *stores.Database, backend storageBackend, share string, workgroup int, dataShards, parityShards uint8, packing PackingOptions, fragmentation FragmentationOptions, debug bool) Client {
	ctx, cancel := context.WithCancel(context.Background())
	ic := &IndexdClient{
		share:        share,
		workgroup:    workgroup,
		db:           db,
		backend:      backend,
		dataShards:   dataShards,
		parityShards: parityShards,
		slabSize:     uint64(dataShards) * proto.SectorSize,
		minPackSize:  packing.MinSize,
		maxBufferAge: packing.MaxAge,
		fragLevel:    fragmentation.Threshold,
		fragInterval: fragmentation.Interval,
		debug:        debug,
		ctx:          ctx,
		cancel:       cancel,
		drainChan:    make(chan struct{}),
		drainTimeout: shutdownDrainTimeout,
		jobsChan:     make(chan struct{}, uploadWorkers),
		packChan:     make(chan struct{}, 1),
		claimed:      make(map[uint64]struct{}),
	}

	// Leftover data that reaches the slab size is uploaded as a full slab
	// before it can ever age, so a minimum at or above that size leaves the
	// age with nothing left to trigger on.
	if ic.maxBufferAge > 0 && ic.minPackSize >= ic.slabSize {
		log.Printf("share %s: minPackedSlabSize of %d is not below the slab size of %d, so maxBufferAge of %s will never upload anything", share, ic.minPackSize, ic.slabSize, ic.maxBufferAge)
	}

	// A threshold of zero would report every slab that holds any dead space
	// at all, which is not what leaving the setting out asks for.
	if ic.fragLevel <= 0 || ic.fragLevel > 1 {
		ic.fragLevel = stores.DefaultFragmentationThreshold
	}

	// A slab can never hold more dead space than it was filled with, so one
	// uploaded at the minimum size never reaches a threshold above what that
	// minimum is of a slab. Only an age uploads a slab short of full, and an
	// unset minimum puts no floor on how short.
	if ic.maxBufferAge > 0 && ic.minPackSize > 0 && ic.fragLevel*float64(ic.slabSize) > float64(ic.minPackSize) {
		log.Printf("share %s: fragmentationThreshold of %.0f%% of a slab of %d is above the minPackedSlabSize of %d, so the holes in slabs uploaded at that minimum will never be reported", share, ic.fragLevel*100, ic.slabSize, ic.minPackSize)
	}

	// Start background upload threads.
	for range uploadWorkers {
		ic.wg.Add(1)
		go func() {
			defer ic.wg.Done()
			ic.processUploads(ic.ctx)
		}()
	}

	// Start the background packer. The claims are scoped to this client's
	// workgroup and share, so this is the only packer for them: a second one
	// would compete for the same buffered pieces, each ending up with too few
	// of them to fill a slab.
	ic.wg.Add(1)
	go func() {
		defer ic.wg.Done()
		ic.packSlabs(ic.ctx)
	}()

	// Start background cleanup of stale upload jobs.
	ic.wg.Add(1)
	go func() {
		defer ic.wg.Done()
		ic.cleanupUploadJobs(ic.ctx)
	}()

	// Start the fragmentation monitor, unless it is turned off.
	if ic.fragInterval > 0 {
		ic.wg.Add(1)
		go func() {
			defer ic.wg.Done()
			ic.monitorFragmentation(ic.ctx)
		}()
	}

	return ic
}

// Info queries the general information about the share.
func (ic *IndexdClient) Info(ctx context.Context) (GeneralInfo, error) {
	sh, err := ic.db.GetShare(ic.share)
	if err != nil {
		return GeneralInfo{}, err
	}

	return GeneralInfo{
		Bucket:    "",
		CreatedAt: sh.CreatedAt,
	}, nil
}

// Storage queries the information about the underlying storage.
func (ic *IndexdClient) Storage(ctx context.Context) (StorageInfo, error) {
	acc, err := ic.backend.Account(ctx)
	if err != nil {
		return StorageInfo{}, err
	}

	return StorageInfo{
		Type:             "indexd",
		RemainingStorage: acc.MaxPinnedData - acc.PinnedData,
		UsedStorage:      acc.PinnedData,
		MinShards:        int(ic.dataShards),
		TotalShards:      int(ic.parityShards + ic.dataShards),
	}, nil
}

// IsEmpty returns true if the directory contains no objects.
func (ic *IndexdClient) IsEmpty(ctx context.Context, acc stores.Account, path string) (bool, error) {
	return ic.db.DirectoryEmpty(acc, ic.share, path)
}

// List lists the contents of a directory.
func (ic *IndexdClient) List(ctx context.Context, acc stores.Account, path string) (ois []ObjectInfo, err error) {
	oms, err := ic.db.ListObjects(acc, ic.share, path)
	if err != nil {
		return nil, err
	}

	for _, om := range oms {
		oi := ObjectInfo{
			Key:        om.Path,
			CreatedAt:  om.CreatedAt,
			ModifiedAt: om.ModifiedAt,
			Size:       om.Size,
		}
		if om.IsDir && om.Path != "/" {
			oi.Key += "/"
		}
		ois = append(ois, oi)
	}

	return ois, nil
}

// Object retrieves the information about a file or a directory.
func (ic *IndexdClient) Object(ctx context.Context, acc stores.Account, path string) (ObjectInfo, error) {
	if path == "" {
		info, err := ic.Info(ctx)
		if err != nil {
			return ObjectInfo{}, err
		}

		return ObjectInfo{
			Key:        "/",
			CreatedAt:  info.CreatedAt,
			ModifiedAt: info.CreatedAt,
			Size:       0,
		}, nil
	}

	om, err := ic.db.Object(acc, ic.share, path)
	if err != nil {
		return ObjectInfo{}, err
	}

	oi := ObjectInfo{
		Key:        om.Path,
		CreatedAt:  om.CreatedAt,
		ModifiedAt: om.ModifiedAt,
		Size:       om.Size,
	}

	if om.IsDir && om.Path != "/" {
		oi.Key += "/"
	}

	return oi, nil
}

// hashPath is a helper function that calculates the hash of a path.
func hashPath(acc stores.Account, path string) [32]byte {
	return blake2b.Sum256(append([]byte(acc.Username), []byte(acc.Workgroup+path)...))
}

// Parents retrieves the information about the directory at path and the one above it.
func (ic *IndexdClient) Parents(ctx context.Context, acc stores.Account, path string) (currentDir, parentDir FileInfo, err error) {
	current, parent, err := ic.db.CurrentAndParent(acc, ic.share, path)
	if err != nil {
		return
	}

	var info GeneralInfo
	var rootHash [32]byte
	if (current.Path == "") || (parent.Path == "") {
		info, err = ic.Info(ctx)
		if err != nil {
			return
		}
		rootHash = hashPath(acc, "/")
	}

	currentDir.ID = make([]byte, 16)
	if current.Path == "" {
		currentDir.ID64 = binary.LittleEndian.Uint64(rootHash[:8])
		currentDir.CreatedAt = info.CreatedAt
		currentDir.ModifiedAt = info.CreatedAt
		copy(currentDir.ID, rootHash[:16])
	} else {
		hash := hashPath(acc, current.Path)
		currentDir.ID64 = binary.LittleEndian.Uint64(hash[:8])
		currentDir.CreatedAt = current.CreatedAt
		currentDir.ModifiedAt = current.ModifiedAt
		copy(currentDir.ID, hash[:16])
	}

	parentDir.ID = make([]byte, 16)
	if parent.Path == "" {
		parentDir.ID64 = binary.LittleEndian.Uint64(rootHash[:8])
		parentDir.CreatedAt = info.CreatedAt
		parentDir.ModifiedAt = info.CreatedAt
		copy(parentDir.ID, rootHash[:16])
	} else {
		hash := hashPath(acc, parent.Path)
		parentDir.ID64 = binary.LittleEndian.Uint64(hash[:8])
		parentDir.CreatedAt = parent.CreatedAt
		parentDir.ModifiedAt = parent.ModifiedAt
		copy(parentDir.ID, hash[:16])
	}

	return
}

// Read downloads a file from the Sia network. The slabs covering the requested
// range are downloaded concurrently and assembled in order.
func (ic *IndexdClient) Read(ctx context.Context, acc stores.Account, path string, offset, length uint64, buf io.Writer) (err error) {
	slabs, err := ic.db.GetMetadata(acc, ic.share, path, offset, length)
	if err != nil {
		return err
	}

	end := offset + length

	parts := make([][]byte, len(slabs))
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(slabDownloadThreads)

	for i, slab := range slabs {
		// The slices returned by GetMetadata are already clipped to the
		// requested range; clamp once more to be safe.
		slabStart := slab.At
		slabEnd := slab.At + slab.Length
		if slabEnd <= offset || slabStart >= end {
			continue
		}

		readStart := max(offset, slabStart)
		readEnd := min(end, slabEnd)
		rangeOffset := slab.Offset + (readStart - slabStart)
		rangeLength := readEnd - readStart

		if (slab.Key != types.Hash256{}) {
			eg.Go(func() error {
				var part bytes.Buffer
				part.Grow(int(rangeLength))
				if err := ic.backend.Download(egCtx, slab.Key, rangeOffset, rangeLength, &part); err != nil {
					return err
				}

				parts[i] = part.Bytes()
				return nil
			})
		} else if slab.Data != nil {
			parts[i] = slab.Data[readStart-slabStart : readEnd-slabStart]
		}
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	for _, part := range parts {
		if part == nil {
			continue
		}
		if _, err := buf.Write(part); err != nil {
			return err
		}
	}

	return nil
}

// StartUpload initiates a multipart upload.
func (ic *IndexdClient) StartUpload(ctx context.Context, acc stores.Account, path string) (uploadID string, err error) {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return
	}

	return ic.db.CreateUpload(acc, ic.share, path)
}

// AbortUpload aborts an initiated multipart upload.
func (ic *IndexdClient) AbortUpload(ctx context.Context, path string, uploadID string) (err error) {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return nil
	}

	slabs, err := ic.db.RemoveUpload(uploadID)
	if err != nil {
		return fmt.Errorf("couldn't abort upload: %v", err)
	}
	if len(slabs) == 0 {
		return nil
	}

	// The slabs are staged in the database, so a failure here only delays
	// their unpinning until the periodic retry.
	if ic.unpinSlabs(ctx, slabs) {
		if err := ic.backend.PruneSlabs(ctx); err != nil {
			log.Printf("failed to prune slabs after aborting upload of %s: %v", path, err)
		}
	}

	return nil
}

// FinishUpload completes a multipart upload.
func (ic *IndexdClient) FinishUpload(ctx context.Context, path string, uploadID string, _ []api.MultipartCompletedPart) error {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return nil
	}

	if err := ic.db.FinalizeUpload(uploadID); err != nil {
		return fmt.Errorf("couldn't finalize upload: %v", err)
	}

	// Finalizing is what makes a piece that is left buffered eligible for
	// packing, so this is the point at which the packer has new work.
	select {
	case ic.packChan <- struct{}{}:
	default:
	}

	return nil
}

// Write uploads the provided chunk of data to the Sia network.
func (ic *IndexdClient) Write(ctx context.Context, r io.Reader, path string, uploadID string, partNumber int, offset, length uint64) (_ string, err error) {
	if strings.HasSuffix(path, ":Zone.Identifier") { // Don't upload Windows' zone identifier files
		return
	}

	buf, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("couldn't read data: %v", err)
	}
	if uint64(len(buf)) != length {
		return "", fmt.Errorf("short read: expected %d bytes, got %d", length, len(buf))
	}

	if err := ic.db.AddBufferedSlab(uploadID, offset, buf); err != nil {
		return "", fmt.Errorf("couldn't add buffered slab to the database: %v", err)
	}

	// Wake up an idle upload worker.
	select {
	case ic.jobsChan <- struct{}{}:
	default:
	}

	return
}

// Delete deletes a file or a directory. Only the slabs that are left
// unreferenced by the deletion are unpinned; a slab shared with a surviving
// file stays in place.
func (ic *IndexdClient) Delete(ctx context.Context, acc stores.Account, path string, batch bool) (err error) {
	var slabs []types.Hash256
	if batch {
		slabs, err = ic.db.DeleteDirectory(acc, ic.share, path)
	} else {
		slabs, err = ic.db.DeleteFile(acc, ic.share, path)
	}
	if err != nil {
		return err
	}

	if len(slabs) == 0 {
		return nil
	}

	// The slabs are staged in the database, so a failure here only delays
	// their unpinning until the periodic retry.
	if ic.unpinSlabs(ctx, slabs) {
		if err := ic.backend.PruneSlabs(ctx); err != nil {
			log.Printf("failed to prune slabs after deleting %s: %v", path, err)
		}
	}

	return nil
}

// MakeDirectory creates a new directory in the specified path.
// If the directory name matches one of the workgroup's public folders, it is created as
// non-private so that all workgroup members can see files placed inside it, and it inherits
// the read-only flag of that public folder.
func (ic *IndexdClient) MakeDirectory(ctx context.Context, acc stores.Account, path string) error {
	private, readOnly := true, false
	if u, err := uuid.Parse(acc.Workgroup); err == nil {
		if wg, err := ic.db.FindWorkgroup(u); err == nil {
			name := path[strings.LastIndex(path, "/")+1:]
			if dir, ok := wg.FindPublicDir(name); ok {
				private, readOnly = false, dir.ReadOnly
			}
		}
	}
	return ic.db.CreateDirectory(acc, ic.share, path, private, readOnly)
}

// Rename renames a file or a directory.
func (ic *IndexdClient) Rename(ctx context.Context, acc stores.Account, oldName, newName string, isDir, force bool) error {
	if isDir {
		return ic.db.RenameDirectory(acc, ic.share, oldName, newName, force)
	}
	return ic.db.RenameFile(acc, ic.share, oldName, newName, force)
}

// pinnedObjects walks the whole object event log of this connection's app
// account and returns what it currently holds, keyed by slab. The log records
// every pin and unpin the account ever made, so the events are folded into the
// latest state of each key and the dropped ones are left out.
func (ic *IndexdClient) pinnedObjects(ctx context.Context) (map[types.Hash256]PinnedObject, error) {
	pinned := make(map[types.Hash256]PinnedObject)

	var cursor slabs.Cursor
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		page, err := ic.backend.ListObjects(ctx, cursor, objectPageSize)
		if err != nil {
			return nil, fmt.Errorf("couldn't list objects: %v", err)
		}
		if len(page) == 0 {
			return pinned, nil
		}

		for _, obj := range page {
			if obj.Deleted {
				delete(pinned, obj.Key)
				continue
			}
			pinned[obj.Key] = obj
		}

		// The log is paginated on (UpdatedAt, Key) ascending and read with a
		// strict "greater than", so the last event of a page is the next
		// cursor. A backend that returns a page ending where the previous one
		// did would have this walk go around forever, so it stops instead.
		last := page[len(page)-1]
		if !last.UpdatedAt.After(cursor.After) && last.Key == cursor.Key {
			return pinned, nil
		}
		cursor = slabs.Cursor{After: last.UpdatedAt, Key: last.Key}
	}
}

// DeleteAll deletes all objects on the share. This is used when a share is removed to ensure
// that all data is deleted from the Sia network.
func (ic *IndexdClient) DeleteAll(ctx context.Context) error {
	pinned, err := ic.pinnedObjects(ctx)
	if err != nil {
		return err
	}

	for key := range pinned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ic.backend.DeleteObject(ctx, key); err != nil {
			log.Printf("couldn't delete object %s: %v", key, err)
		}
	}

	if err := ic.backend.PruneSlabs(ctx); err != nil {
		return fmt.Errorf("couldn't prune slabs: %v", err)
	}

	return nil
}

// OrphanedSlabs reports the slabs this connection has pinned that no file in
// the database references and that have been pinned for at least minAge. The
// age is what separates an orphan from an upload in flight: a slab is pinned
// before it is recorded, so a slab that has just appeared is expected to be
// unreferenced and must be left alone.
func (ic *IndexdClient) OrphanedSlabs(ctx context.Context, minAge time.Duration) ([]OrphanedSlab, error) {
	if minAge <= 0 {
		minAge = DefaultOrphanMinAge
	}

	pinned, err := ic.pinnedObjects(ctx)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-minAge)
	keys := make([]types.Hash256, 0, len(pinned))
	for key, obj := range pinned {
		if obj.UpdatedAt.After(cutoff) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, nil
	}

	unreferenced, err := ic.db.UnreferencedSlabs(keys)
	if err != nil {
		return nil, fmt.Errorf("couldn't check slab references: %v", err)
	}

	orphans := make([]OrphanedSlab, 0, len(unreferenced))
	for _, key := range unreferenced {
		orphans = append(orphans, OrphanedSlab{
			Key:      key,
			Size:     pinned[key].Size,
			PinnedAt: pinned[key].UpdatedAt,
		})
	}

	// Oldest first: the longest-standing leak is the one worth looking at.
	sort.Slice(orphans, func(i, j int) bool {
		if !orphans[i].PinnedAt.Equal(orphans[j].PinnedAt) {
			return orphans[i].PinnedAt.Before(orphans[j].PinnedAt)
		}
		return bytes.Compare(orphans[i].Key[:], orphans[j].Key[:]) < 0
	})

	return orphans, nil
}

// UnpinOrphanedSlabs drops the slabs that OrphanedSlabs finds. The scan is run
// again here rather than taking a list from the caller, so that what is dropped
// is what is unreferenced and old enough at this moment, not what was when
// somebody last looked.
//
// Each slab is staged in the database before the backend is asked to drop it,
// so a slab the backend refuses is retried by the periodic unpin retry instead
// of being forgotten.
func (ic *IndexdClient) UnpinOrphanedSlabs(ctx context.Context, minAge time.Duration) (UnpinResult, error) {
	orphans, err := ic.OrphanedSlabs(ctx, minAge)
	if err != nil {
		return UnpinResult{}, err
	}
	if len(orphans) == 0 {
		return UnpinResult{}, nil
	}

	var res UnpinResult
	for _, orphan := range orphans {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		if err := ic.db.StageUnpin(ic.share, ic.workgroup, orphan.Key); err != nil {
			log.Printf("failed to stage unpin of orphaned slab %s: %v", orphan.Key, err)
			res.Failed++
			continue
		}
		if err := ic.backend.DeleteObject(ctx, orphan.Key); err != nil {
			log.Printf("failed to unpin orphaned slab %s, leaving it staged for retry: %v", orphan.Key, err)
			res.Failed++
			continue
		}
		if err := ic.db.UnstageUnpin(ic.share, ic.workgroup, orphan.Key); err != nil {
			log.Printf("failed to confirm unpin of orphaned slab %s: %v", orphan.Key, err)
		}

		res.Unpinned++
		res.Freed += orphan.Size
	}

	if res.Unpinned > 0 {
		if err := ic.backend.PruneSlabs(ctx); err != nil {
			log.Printf("failed to prune slabs after unpinning orphans of share %s: %v", ic.share, err)
		}
		log.Printf("share %s, workgroup %d: unpinned %d orphaned slab(s), freeing %d bytes", ic.share, ic.workgroup, res.Unpinned, res.Freed)
	}

	return res, nil
}

// Close closes the client and releases all resources. The background workers
// are first asked to stop taking on new work and given shutdownDrainTimeout to
// finish what they still have in flight; whatever is left running by then is
// cancelled, so that Close always returns.
func (ic *IndexdClient) Close() error {
	ic.closeOnce.Do(func() { close(ic.drainChan) })

	drained := make(chan struct{})
	go func() {
		ic.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(ic.drainTimeout):
		ic.cancel()
		<-drained
	}

	// Also releases the context of a shutdown that drained in time.
	ic.cancel()

	return ic.backend.Close()
}

// unpinSlabs deletes the given slabs, which have been staged as pending
// unpins, from the storage backend and confirms each successful one, so that
// only the failed ones stay staged for the periodic retry. It reports whether
// anything was unpinned, in which case the caller wants to prune.
func (ic *IndexdClient) unpinSlabs(ctx context.Context, keys []types.Hash256) bool {
	var dropped bool
	for _, key := range keys {
		if err := ic.backend.DeleteObject(ctx, key); err != nil {
			log.Printf("failed to delete slab %s, leaving it staged for retry: %v", key, err)
			continue
		}
		dropped = true
		if err := ic.db.UnstageUnpin(ic.share, ic.workgroup, key); err != nil {
			log.Printf("failed to confirm unpin of slab %s: %v", key, err)
		}
	}

	return dropped
}

// retryPendingUnpins retries the unpins that could not be confirmed earlier,
// e.g. because the storage backend was unreachable or the process stopped in
// between. A slab whose key has become referenced again in the meantime — an
// upload of the same content is assigned the same key — is unstaged instead
// of unpinned.
func (ic *IndexdClient) retryPendingUnpins(ctx context.Context) {
	keys, err := ic.db.PendingUnpins(ic.share, ic.workgroup)
	if err != nil {
		log.Printf("failed to list pending unpins: %v", err)
		return
	}

	pending := keys[:0]
	for _, key := range keys {
		referenced, err := ic.db.SlabReferenced(key)
		if err != nil {
			log.Printf("failed to check references of slab %s: %v", key, err)
			continue
		}
		if referenced {
			if err := ic.db.UnstageUnpin(ic.share, ic.workgroup, key); err != nil {
				log.Printf("failed to unstage referenced slab %s: %v", key, err)
			}
			continue
		}
		pending = append(pending, key)
	}

	if len(pending) == 0 {
		return
	}
	if ic.unpinSlabs(ctx, pending) {
		if err := ic.backend.PruneSlabs(ctx); err != nil {
			log.Printf("failed to prune slabs after retrying unpins: %v", err)
		}
	}
}

// requeueStrandedPieces requeues the pieces whose claim never completed,
// which is what a process stop between claiming and completing leaves behind.
// suspects carries the pieces that were already stranded on the previous
// sweep, and only those are requeued: a claim that is merely slow gets a full
// sweep interval to finish, and the pieces this client is working on right
// now are skipped outright. It returns the suspects for the next sweep.
func (ic *IndexdClient) requeueStrandedPieces(suspects map[uint64]struct{}) map[uint64]struct{} {
	ids, err := ic.db.StrandedPieces(ic.share, ic.workgroup)
	if err != nil {
		log.Printf("failed to list stranded pieces: %v", err)
		return suspects
	}

	next := make(map[uint64]struct{}, len(ids))
	var confirmed []uint64
	for _, id := range ids {
		if ic.isClaimed(id) {
			continue
		}
		next[id] = struct{}{}
		if _, ok := suspects[id]; ok {
			confirmed = append(confirmed, id)
		}
	}

	if len(confirmed) == 0 {
		return next
	}

	if err := ic.db.RequeueStrandedPieces(confirmed); err != nil {
		log.Printf("failed to requeue stranded pieces: %v", err)
		return next
	}
	log.Printf("share %s: requeued %d stranded pieces", ic.share, len(confirmed))

	// The requeued pieces are new work for the workers and the packer.
	select {
	case ic.jobsChan <- struct{}{}:
	default:
	}
	select {
	case ic.packChan <- struct{}{}:
	default:
	}

	return next
}

// dropSlabIfUnreferenced unpins the given slab unless a file still references
// it. A slab whose completion found none of its files left is usually
// orphaned, but slabs are content-addressed: another file of the same content
// may share the key, and unpinning it would destroy that file's data. When in
// doubt, the slab is left pinned, which at worst leaks storage.
func (ic *IndexdClient) dropSlabIfUnreferenced(ctx context.Context, key types.Hash256) {
	referenced, err := ic.db.SlabReferenced(key)
	if err != nil {
		log.Printf("failed to check references of slab %s, leaving it pinned: %v", key, err)
		return
	}
	if referenced {
		return
	}

	// Staging first makes the unpin survive a failure to reach the backend:
	// the slab stays listed until the periodic retry confirms it.
	if err := ic.db.StageUnpin(ic.share, ic.workgroup, key); err != nil {
		log.Printf("failed to stage unpin of slab %s: %v", key, err)
	}
	if ic.unpinSlabs(ctx, []types.Hash256{key}) {
		if err := ic.backend.PruneSlabs(ctx); err != nil {
			log.Printf("failed to prune slabs after late completion for %s: %v", key, err)
		}
	}
}

// processUpload checks if there is a complete slab and uploads it.
func (ic *IndexdClient) processUpload(ctx context.Context) error {
	job, err := ic.db.ClaimUploadJob(ic.share, ic.workgroup, ic.slabSize)
	if err != nil {
		return err
	}
	ic.markClaimed(job.MetadataID)
	defer ic.unmarkClaimed(job.MetadataID)

	key, err := ic.backend.Upload(ctx, bytes.NewReader(job.Data), ic.dataShards, ic.parityShards)
	if err != nil {
		_ = ic.db.RequeueUploadJob(job.UploadID, job.MetadataID)
		return fmt.Errorf("couldn't upload slab: %v", err)
	}

	err = completeWithRetry(func() error {
		return ic.db.CompleteUploadJob(job.MetadataID, job.BufferID, key)
	})
	if err != nil {
		if errors.Is(err, stores.ErrNotFound) {
			// The file has likely been deleted.
			ic.dropSlabIfUnreferenced(ctx, key)
			return nil
		}

		// The claim is already out of the queue, so leaving the completion
		// unrecorded would strand the buffer; requeue it instead. The slab is
		// left pinned, because the failure may have been an ambiguous commit
		// that did record it, and deleting it then would lose file data. If
		// it truly went unrecorded, the requeued buffer uploads the same
		// content again, which yields the same key.
		if rerr := ic.db.RequeueUploadJob(job.UploadID, job.MetadataID); rerr != nil {
			log.Printf("failed to requeue piece %d of unrecorded slab %s: %v", job.MetadataID, key, rerr)
		}
		return fmt.Errorf("couldn't complete upload job for slab %s: %v", key, err)
	}

	return nil
}

// packSlab concatenates the claimed pieces into the slab to upload, cutting the
// last one short at the slab boundary. The pieces are trimmed in place, so that
// what is left of each of them is the part that made it into the slab.
func packSlab(jobs []stores.UploadJob, size uint64) []byte {
	slab := make([]byte, 0, size)
	for i := range jobs {
		if uint64(len(slab)+len(jobs[i].Data)) > size {
			jobs[i].Data = jobs[i].Data[:size-uint64(len(slab))]
		}
		slab = append(slab, jobs[i].Data...)
	}

	return slab
}

// logPackedSlab reports which pieces went into the slab about to be uploaded,
// in the order in which they are laid out in it. The pieces are already trimmed
// to what fitted, so this is what the slab is made of, not what was claimed.
func (ic *IndexdClient) logPackedSlab(jobs []stores.UploadJob, size int) {
	var b strings.Builder
	fmt.Fprintf(&b, "share %s, workgroup %d: packing %d piece(s) into a slab of %d out of %d bytes", ic.share, ic.workgroup, len(jobs), size, ic.slabSize)

	var offset uint64
	for _, job := range jobs {
		taken := uint64(len(job.Data))
		fmt.Fprintf(&b, "\n\tobject %d, metadata %d, buffer %d: %d bytes at slab offset %d", job.ObjectID, job.MetadataID, job.BufferID, taken, offset)
		if taken < job.DataLength {
			fmt.Fprintf(&b, " (split, %d bytes left for the next slab)", job.DataLength-taken)
		}
		offset += taken
	}

	log.Println(b.String())
}

// processPackedSlab checks if the buffered pieces of several files add up to a
// slab and, if so, uploads them together as a single packed slab.
func (ic *IndexdClient) processPackedSlab(ctx context.Context) error {
	jobs, err := ic.db.ClaimPackedSlab(ic.share, ic.workgroup, ic.slabSize, ic.minPackSize, ic.maxBufferAge)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		ic.markClaimed(job.MetadataID)
	}
	defer func() {
		for _, job := range jobs {
			ic.unmarkClaimed(job.MetadataID)
		}
	}()

	slab := packSlab(jobs, ic.slabSize)
	if ic.debug {
		ic.logPackedSlab(jobs, len(slab))
	}

	key, err := ic.backend.Upload(ctx, bytes.NewReader(slab), ic.dataShards, ic.parityShards)
	if err != nil {
		for _, job := range jobs {
			if rerr := ic.db.RequeueUploadJob(job.UploadID, job.MetadataID); rerr != nil {
				log.Printf("failed to requeue packed piece %d: %v", job.MetadataID, rerr)
			}
		}
		return fmt.Errorf("couldn't upload packed slab: %v", err)
	}

	err = completeWithRetry(func() error {
		return ic.db.CompletePackedSlab(jobs, key)
	})
	if err != nil {
		if errors.Is(err, stores.ErrNotFound) {
			// Every file that went into the slab has been deleted since.
			ic.dropSlabIfUnreferenced(ctx, key)
			return nil
		}

		// The claim is already out of the queue, so leaving the completion
		// unrecorded would strand the pieces; requeue them instead. The slab
		// is left pinned, because the failure may have been an ambiguous
		// commit that did record it, and deleting it then would lose file
		// data. If it truly went unrecorded, the repacked pieces usually form
		// the identical slab again; at worst this one stays behind
		// unreferenced, under the key named below.
		for _, job := range jobs {
			if rerr := ic.db.RequeueUploadJob(job.UploadID, job.MetadataID); rerr != nil {
				log.Printf("failed to requeue piece %d of unrecorded packed slab %s: %v", job.MetadataID, key, rerr)
			}
		}
		return fmt.Errorf("couldn't complete packed slab %s: %v", key, err)
	}

	if ic.debug {
		log.Printf("share %s, workgroup %d: uploaded packed slab %s of %d bytes from %d piece(s)", ic.share, ic.workgroup, key, len(slab), len(jobs))
	}

	return nil
}

// packSlabs combines the pieces that are left buffered into packed slabs in the
// background.
func (ic *IndexdClient) packSlabs(ctx context.Context) {
	ticker := time.NewTicker(packInterval)
	defer ticker.Stop()

	delay := retryDelay
	for {
		select {
		case <-ic.drainChan:
			return
		default:
		}

		// A packed slab may leave a remainder behind that, together with
		// what is still buffered, fills another one, so keep going until
		// there is nothing left to pack.
		err := ic.processPackedSlab(ctx)
		if err == nil {
			delay = retryDelay
			continue
		}

		if !errors.Is(err, stores.ErrNoUploadJobs) {
			// A slab cut short by Close is not a failure worth
			// reporting; its pieces are back in the queue.
			if ctx.Err() != nil {
				return
			}

			log.Printf("failed to pack a slab, retrying in %s: %v", delay, err)

			// The pieces are back in the queue, so the wait is what
			// keeps a backend that fails every slab from being asked
			// again every second for as long as it is down.
			select {
			case <-ic.drainChan:
				return
			case <-time.After(delay):
			}
			delay = nextRetry(delay)
			continue
		}

		delay = retryDelay

		// Nothing to pack: wait until another upload has left a piece
		// behind, with a periodic fallback in case the signal was missed.
		select {
		case <-ic.drainChan:
			return
		case <-ic.packChan:
		case <-ticker.C:
		}
	}
}

// cleanupUploadJobs periodically removes upload jobs that can no longer be
// processed, retries unconfirmed unpins, and requeues stranded pieces.
// Running once right away picks up what a previous run left behind.
func (ic *IndexdClient) cleanupUploadJobs(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	var suspects map[uint64]struct{}
	for {
		if err := ic.db.CleanupUploadJobs(); err != nil {
			log.Printf("failed to clean up upload jobs: %v", err)
		}
		ic.retryPendingUnpins(ctx)
		suspects = ic.requeueStrandedPieces(suspects)

		select {
		case <-ic.drainChan:
			return
		case <-ticker.C:
		}
	}
}

// logFragmentation reports what the check found. A connection holding no slabs
// has nothing to report: that is what every check of a share nobody has
// uploaded to finds, from the first one the connection runs onwards.
func (ic *IndexdClient) logFragmentation(stats stores.FragmentationStats) {
	if stats.Slabs == 0 {
		return
	}

	if stats.Fragmented > 0 {
		log.Printf("share %s, workgroup %d: %d of %d slab(s) have %.0f%% or more dead space, which is %d bytes of the total %d wasted",
			ic.share, ic.workgroup, stats.Fragmented, stats.Slabs, ic.fragLevel*100, stats.FragmentedWasted, stats.Wasted)
		return
	}

	log.Printf("share %s, workgroup %d: no slab has %.0f%% or more dead space, with %d bytes wasted across %d slab(s)",
		ic.share, ic.workgroup, ic.fragLevel*100, stats.Wasted, stats.Slabs)
}

// checkFragmentation measures the dead space in the share's slabs. Nothing is
// repacked yet: the level is only reported.
func (ic *IndexdClient) checkFragmentation() (stores.FragmentationStats, error) {
	stats, err := ic.db.Fragmentation(ic.share, ic.workgroup, ic.slabSize, ic.fragLevel)
	if err != nil {
		return stores.FragmentationStats{}, err
	}

	if ic.debug {
		ic.logFragmentation(stats)
	}
	return stats, nil
}

// Fragmentation reports the dead space in this connection's slabs, listing
// those that reach threshold. A threshold outside of (0, 1] reports at the
// level the connection is configured with.
func (ic *IndexdClient) Fragmentation(ctx context.Context, threshold float64) (FragmentationReport, error) {
	if threshold <= 0 || threshold > 1 {
		threshold = ic.fragLevel
	}

	stats, err := ic.db.Fragmentation(ic.share, ic.workgroup, ic.slabSize, threshold)
	if err != nil {
		return FragmentationReport{}, fmt.Errorf("couldn't summarize the slabs: %v", err)
	}

	slabs, err := ic.db.PackedSlabs(ic.share, ic.workgroup, ic.slabSize, threshold)
	if err != nil {
		return FragmentationReport{}, fmt.Errorf("couldn't list the fragmented slabs: %v", err)
	}

	return FragmentationReport{
		Threshold: threshold,
		Stats:     stats,
		Slabs:     slabs,
	}, nil
}

// monitorFragmentation runs the fragmentation check in the background. Running
// once right away gives a reading without waiting out the first interval.
func (ic *IndexdClient) monitorFragmentation(ctx context.Context) {
	ticker := time.NewTicker(ic.fragInterval)
	defer ticker.Stop()

	for {
		// A check cut short by Close is not worth reporting.
		if _, err := ic.checkFragmentation(); err != nil && ctx.Err() == nil {
			log.Printf("failed to check the fragmentation of share %s, workgroup %d: %v", ic.share, ic.workgroup, err)
		}

		select {
		case <-ic.drainChan:
			return
		case <-ticker.C:
		}
	}
}

// processUploads runs the upload jobs in the background.
func (ic *IndexdClient) processUploads(ctx context.Context) {
	delay := retryDelay
	for {
		select {
		case <-ic.drainChan:
			return
		default:
		}

		err := ic.processUpload(ctx)
		if err == nil {
			delay = retryDelay
			continue
		}

		if errors.Is(err, stores.ErrNoUploadJobs) {
			delay = retryDelay
			// Wait until a new job is signaled, with a periodic fallback
			// poll in case the signal was missed.
			select {
			case <-ic.drainChan:
				return
			case <-ic.jobsChan:
			case <-time.After(time.Second):
			}
			continue
		}

		// A job cut short by Close is not a failure worth reporting.
		if ctx.Err() != nil {
			return
		}

		log.Printf("failed to run upload job, retrying in %s: %v", delay, err)

		select {
		case <-ic.drainChan:
			return
		case <-time.After(delay):
		}
		delay = nextRetry(delay)
	}
}
