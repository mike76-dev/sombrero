package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
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
	ListObjectKeys(ctx context.Context, cursor slabs.Cursor, limit int) ([]types.Hash256, error)
	Close() error
}

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

// ListObjectKeys returns a slice of object keys instead of objects.
func (b *sdkBackend) ListObjectKeys(ctx context.Context, cursor slabs.Cursor, limit int) ([]types.Hash256, error) {
	objs, err := b.sdk.ObjectEvents(ctx, cursor, limit)
	if err != nil {
		return nil, err
	}

	keys := make([]types.Hash256, 0, len(objs))
	for _, obj := range objs {
		keys = append(keys, obj.Object.ID())
	}

	return keys, nil
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
	closeChan    chan struct{}
	jobsChan     chan struct{}
	packChan     chan struct{}
	wg           sync.WaitGroup
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

// NewIndexdClient returns an initialized IndexdClient serving the given
// workgroup's connection to the share.
func NewIndexdClient(db *stores.Database, sdkClient *sdk.SDK, share string, workgroup int, dataShards, parityShards uint8, packing PackingOptions) Client {
	backend := &sdkBackend{
		sdk:      sdkClient,
		objCache: make(map[types.Hash256]sdk.Object),
	}
	return newIndexdClient(db, backend, share, workgroup, dataShards, parityShards, packing)
}

// newIndexdClient allows using a mock SDK for testing.
func newIndexdClient(db *stores.Database, backend storageBackend, share string, workgroup int, dataShards, parityShards uint8, packing PackingOptions) Client {
	cc := make(chan struct{})
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
		closeChan:    cc,
		jobsChan:     make(chan struct{}, uploadWorkers),
		packChan:     make(chan struct{}, 1),
	}

	// Leftover data that reaches the slab size is uploaded as a full slab
	// before it can ever age, so a minimum at or above that size leaves the
	// age with nothing left to trigger on.
	if ic.maxBufferAge > 0 && ic.minPackSize >= ic.slabSize {
		log.Printf("share %s: minPackedSlabSize of %d is not below the slab size of %d, so maxBufferAge of %s will never upload anything", share, ic.minPackSize, ic.slabSize, ic.maxBufferAge)
	}

	// Start background upload threads.
	for range uploadWorkers {
		ic.wg.Add(1)
		go func() {
			defer ic.wg.Done()
			ic.processUploads(ic.closeChan)
		}()
	}

	// Start the background packer. The claims are scoped to this client's
	// workgroup and share, so this is the only packer for them: a second one
	// would compete for the same buffered pieces, each ending up with too few
	// of them to fill a slab.
	ic.wg.Add(1)
	go func() {
		defer ic.wg.Done()
		ic.packSlabs(ic.closeChan)
	}()

	// Start background cleanup of stale upload jobs.
	ic.wg.Add(1)
	go func() {
		defer ic.wg.Done()
		ic.cleanupUploadJobs(ic.closeChan)
	}()

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

// IsEmpty returns true if the directory contains at least one object.
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

// Parents retrieves the information about the current and the parent directories where the file is located.
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

// DeleteAll deletes all objects on the share. This is used when a share is removed to ensure
// that all data is deleted from the Sia network.
func (ic *IndexdClient) DeleteAll(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		keys, err := ic.backend.ListObjectKeys(ctx, slabs.Cursor{}, 10)
		if err != nil {
			return fmt.Errorf("couldn't list object keys: %v", err)
		}
		if len(keys) == 0 {
			break
		}

		for _, key := range keys {
			if err := ic.backend.DeleteObject(ctx, key); err != nil {
				log.Printf("couldn't delete object %s: %v", key, err)
			}
		}
	}

	if err := ic.backend.PruneSlabs(ctx); err != nil {
		return fmt.Errorf("couldn't prune slabs: %v", err)
	}

	return nil
}

// Close closes the client and releases all resources.
func (ic *IndexdClient) Close() error {
	close(ic.closeChan)
	ic.wg.Wait()
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

// processPackedSlab checks if the buffered pieces of several files add up to a
// slab and, if so, uploads them together as a single packed slab.
func (ic *IndexdClient) processPackedSlab(ctx context.Context) error {
	jobs, err := ic.db.ClaimPackedSlab(ic.share, ic.workgroup, ic.slabSize, ic.minPackSize, ic.maxBufferAge)
	if err != nil {
		return err
	}

	key, err := ic.backend.Upload(ctx, bytes.NewReader(packSlab(jobs, ic.slabSize)), ic.dataShards, ic.parityShards)
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

	return nil
}

// packSlabs combines the pieces that are left buffered into packed slabs in the
// background.
func (ic *IndexdClient) packSlabs(closeChan chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticker := time.NewTicker(packInterval)
	defer ticker.Stop()

	for {
		select {
		case <-closeChan:
			return
		default:
		}

		// A packed slab may leave a remainder behind that, together with
		// what is still buffered, fills another one, so keep going until
		// there is nothing left to pack.
		err := ic.processPackedSlab(ctx)
		if err == nil {
			continue
		}

		if !errors.Is(err, stores.ErrNoUploadJobs) {
			log.Printf("failed to pack a slab: %v", err)

			// The pieces are back in the queue and the failure is
			// likely transient, so retry shortly.
			select {
			case <-closeChan:
				return
			case <-time.After(time.Second):
			}
			continue
		}

		// Nothing to pack: wait until another upload has left a piece
		// behind, with a periodic fallback in case the signal was missed.
		select {
		case <-closeChan:
			return
		case <-ic.packChan:
		case <-ticker.C:
		}
	}
}

// cleanupUploadJobs periodically removes upload jobs that can no longer be
// processed and retries unconfirmed unpins. Running once right away picks up
// the unpins a previous run left unconfirmed.
func (ic *IndexdClient) cleanupUploadJobs(closeChan chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		if err := ic.db.CleanupUploadJobs(); err != nil {
			log.Printf("failed to clean up upload jobs: %v", err)
		}
		ic.retryPendingUnpins(ctx)

		select {
		case <-closeChan:
			return
		case <-ticker.C:
		}
	}
}

// processUploads runs the upload jobs in the background.
func (ic *IndexdClient) processUploads(closeChan chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		select {
		case <-closeChan:
			return
		default:
		}

		err := ic.processUpload(ctx)
		if err == nil {
			continue
		}

		if errors.Is(err, stores.ErrNoUploadJobs) {
			// Wait until a new job is signaled, with a periodic fallback
			// poll in case the signal was missed.
			select {
			case <-closeChan:
				return
			case <-ic.jobsChan:
			case <-time.After(time.Second):
			}
			continue
		}

		log.Printf("failed to run upload job: %v", err)

		select {
		case <-closeChan:
			return
		case <-time.After(time.Second):
		}
	}
}
