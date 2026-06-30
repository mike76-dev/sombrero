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
)

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

// sdkBackend is a wrapper around the SDK.
type sdkBackend struct {
	sdk *sdk.SDK
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
	obj, err := b.sdk.Object(ctx, key)
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
type IndexdClient struct {
	share        string
	db           *stores.Database
	backend      storageBackend
	dataShards   uint8
	parityShards uint8
	closeChan    chan struct{}
	wg           sync.WaitGroup
}

// NewIndexdClient returns an initialized IndexdClient.
func NewIndexdClient(db *stores.Database, sdkClient *sdk.SDK, share string, dataShards, parityShards uint8) Client {
	return newIndexdClient(db, &sdkBackend{sdk: sdkClient}, share, dataShards, parityShards)
}

// newIndexdClient allows using a mock SDK for testing.
func newIndexdClient(db *stores.Database, backend storageBackend, share string, dataShards, parityShards uint8) Client {
	cc := make(chan struct{})
	ic := &IndexdClient{
		share:        share,
		db:           db,
		backend:      backend,
		dataShards:   dataShards,
		parityShards: parityShards,
		closeChan:    cc,
	}

	// Start background upload thread.
	ic.wg.Add(1)
	go func() {
		defer ic.wg.Done()
		ic.processUploads(ic.closeChan)
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

// Read downloads a file from the Sia network.
func (ic *IndexdClient) Read(ctx context.Context, acc stores.Account, path string, offset, length uint64, buf io.Writer) (err error) {
	slabs, err := ic.db.GetMetadata(acc, ic.share, path)
	if err != nil {
		return err
	}

	end := offset + length

	for _, slab := range slabs {
		slabStart := slab.At
		slabEnd := slab.At + slab.Length
		if slabEnd <= offset {
			continue
		}
		if slabStart >= end {
			break
		}

		readStart := max(offset, slabStart)
		readEnd := min(end, slabEnd)
		rangeOffset := slab.Offset + (readStart - slabStart)
		rangeLength := readEnd - readStart

		if (slab.Key != types.Hash256{}) {
			if err = ic.backend.Download(ctx, slab.Key, rangeOffset, rangeLength, buf); err != nil {
				return err
			}
		} else if slab.Data != nil {
			if _, err = io.Copy(buf, bytes.NewReader(slab.Data[readStart-slabStart:readEnd-slabStart])); err != nil {
				return err
			}
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

	for _, key := range slabs {
		if err := ic.backend.DeleteObject(ctx, key); err != nil {
			return fmt.Errorf("couldn't delete slab: %v", err)
		}
	}

	if err := ic.backend.PruneSlabs(ctx); err != nil {
		return fmt.Errorf("couldn't prune slabs: %v", err)
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

	return
}

// Delete deletes a file or a directory.
func (ic *IndexdClient) Delete(ctx context.Context, acc stores.Account, path string, batch bool) (err error) {
	slabs, err := ic.db.ListSlabs(acc, ic.share, path)
	if err != nil {
		return err
	}

	for _, key := range slabs {
		if err := ic.backend.DeleteObject(ctx, key); err != nil {
			log.Printf("failed to delete slab %s from %s", key, path)
		}
	}

	if err := ic.backend.PruneSlabs(ctx); err != nil {
		return fmt.Errorf("couldn't prune slabs: %v", err)
	}

	if batch {
		return ic.db.DeleteDirectory(acc, ic.share, path)
	}

	return ic.db.DeleteFile(acc, ic.share, path)
}

// MakeDirectory creates a new directory in the specified path.
// If the directory name matches one of the workgroup's public dirs, it is created as non-private
// so that all workgroup members can see files placed inside it.
func (ic *IndexdClient) MakeDirectory(ctx context.Context, acc stores.Account, path string) error {
	private := true
	if u, err := uuid.Parse(acc.Workgroup); err == nil {
		if wg, err := ic.db.FindWorkgroup(u); err == nil && len(wg.PublicDirs) > 0 {
			name := path[strings.LastIndex(path, "/")+1:]
			for _, dir := range wg.PublicDirs {
				if wg.CaseSensitive {
					if dir == name {
						private = false
						break
					}
				} else if strings.EqualFold(dir, name) {
					private = false
					break
				}
			}
		}
	}
	return ic.db.CreateDirectory(acc, ic.share, path, private)
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

// processUpload checks if there is a complete slab and uploads it.
func (ic *IndexdClient) processUpload(ctx context.Context) error {
	job, err := ic.db.ClaimUploadJob(uint64(ic.dataShards) * proto.SectorSize)
	if err != nil {
		return err
	}

	key, err := ic.backend.Upload(ctx, bytes.NewReader(job.Data), ic.dataShards, ic.parityShards)
	if err != nil {
		_ = ic.db.RequeueUploadJob(job.UploadID, job.MetadataID)
		return fmt.Errorf("couldn't upload slab: %v", err)
	}

	if err := ic.db.CompleteUploadJob(job.MetadataID, job.BufferID, key); err != nil {
		if errors.Is(err, stores.ErrNotFound) {
			// The file has likely been deleted.
			if derr := ic.backend.DeleteObject(ctx, key); derr != nil {
				log.Printf("failed to delete orphaned uploaded slab %s after late completion: %v", key, derr)
			} else if perr := ic.backend.PruneSlabs(ctx); perr != nil {
				log.Printf("failed to prune slabs after late completion for %s: %v", key, perr)
			}
			return nil
		}
		return fmt.Errorf("couldn't complete upload job: %v", err)
	}

	return nil
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

		if err := ic.processUpload(ctx); err != nil {
			select {
			case <-closeChan:
				return
			default:
			}

			time.Sleep(time.Second)

			select {
			case <-closeChan:
				return
			default:
			}

			if errors.Is(err, stores.ErrNoUploadJobs) {
				continue
			}
			log.Printf("failed to run upload job: %v", err)
		}
	}
}
