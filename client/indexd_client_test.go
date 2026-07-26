package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/api/app"
	"go.sia.tech/indexd/slabs"
	"lukechampine.com/frand"
)

type fakeBackend struct {
	mu         sync.Mutex
	objects    map[types.Hash256][]byte
	nextID     uint64
	uploadGate chan struct{}
	uploadErr  error
	deleteErr  error
	uploads    int
	deletes    int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		objects: make(map[types.Hash256][]byte),
		nextID:  1,
	}
}

func newGatedFakeBackend() *fakeBackend {
	return &fakeBackend{
		objects:    make(map[types.Hash256][]byte),
		nextID:     1,
		uploadGate: make(chan struct{}, 1024),
	}
}

func (fb *fakeBackend) allowUploads(n int) {
	if fb.uploadGate == nil {
		return
	}
	for range n {
		fb.uploadGate <- struct{}{}
	}
}

// failUploads makes every following upload fail with the given error, until it
// is called again with nil.
func (fb *fakeBackend) failUploads(err error) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.uploadErr = err
}

// failDeletes makes every following object deletion fail with the given
// error, until it is called again with nil.
func (fb *fakeBackend) failDeletes(err error) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.deleteErr = err
}

// uploadAttempts returns how often an upload has been started.
func (fb *fakeBackend) uploadAttempts() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.uploads
}

// deleteCount returns how often an object has been deleted.
func (fb *fakeBackend) deleteCount() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.deletes
}

func (fb *fakeBackend) nextKey() types.Hash256 {
	var h types.Hash256
	h[0] = byte(fb.nextID)
	h[1] = byte(fb.nextID >> 8)
	h[2] = byte(fb.nextID >> 16)
	h[3] = byte(fb.nextID >> 24)
	h[4] = byte(fb.nextID >> 32)
	h[5] = byte(fb.nextID >> 40)
	h[6] = byte(fb.nextID >> 48)
	h[7] = byte(fb.nextID >> 56)
	fb.nextID++
	return h
}

func (fb *fakeBackend) Account(ctx context.Context) (app.AccountResponse, error) {
	return app.AccountResponse{
		MaxPinnedData: 1 << 40,
		PinnedData:    0,
	}, nil
}

func (fb *fakeBackend) Upload(ctx context.Context, r io.Reader, dataShards, parityShards uint8) (types.Hash256, error) {
	fb.mu.Lock()
	fb.uploads++
	fb.mu.Unlock()

	if fb.uploadGate != nil {
		select {
		case <-ctx.Done():
			return types.Hash256{}, ctx.Err()
		case <-fb.uploadGate:
		}
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()

	if fb.uploadErr != nil {
		return types.Hash256{}, fb.uploadErr
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return types.Hash256{}, err
	}

	key := fb.nextKey()
	fb.objects[key] = append([]byte(nil), data...)
	return key, nil
}

func (fb *fakeBackend) Download(ctx context.Context, key types.Hash256, offset, length uint64, w io.Writer) error {
	fb.mu.Lock()
	data, ok := fb.objects[key]
	fb.mu.Unlock()

	if !ok {
		return errors.New("object not found")
	}

	end := offset + length
	if offset > uint64(len(data)) || end > uint64(len(data)) {
		return errors.New("download range out of bounds")
	}

	_, err := w.Write(data[offset:end])
	return err
}

func (fb *fakeBackend) DeleteObject(ctx context.Context, key types.Hash256) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.deleteErr != nil {
		return fb.deleteErr
	}
	fb.deletes++
	delete(fb.objects, key)
	return nil
}

func (fb *fakeBackend) PruneSlabs(ctx context.Context) error {
	return nil
}

func (fb *fakeBackend) ListObjectKeys(ctx context.Context, cursor slabs.Cursor, limit int) ([]types.Hash256, error) {
	fb.mu.Lock()
	defer fb.mu.Unlock()

	keys := make([]types.Hash256, 0, len(fb.objects))
	for k := range fb.objects {
		keys = append(keys, k)
		if len(keys) >= limit {
			break
		}
	}

	return keys, nil
}

func (fb *fakeBackend) Close() error {
	return nil
}

func TestIndexdClient_FileLifecycle(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	content := []byte("hello world")
	rootFile := "root.txt"

	// 1. Upload a small file to the root folder.
	uploadID, err := c.StartUpload(ctx, acc, rootFile)
	if err != nil {
		t.Fatalf("StartUpload(root): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), rootFile, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(root): %v", err)
	}
	if err := c.FinishUpload(ctx, rootFile, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(root): %v", err)
	}
	waitForRead(t, ctx, c, acc, rootFile, content)

	// 2. Download that file.
	mustReadEquals(t, ctx, c, acc, rootFile, content)

	// 3. Rename that file.
	rootRenamed := "root-renamed.txt"
	if err := c.Rename(ctx, acc, rootFile, rootRenamed, false, false); err != nil {
		t.Fatalf("Rename(root): %v", err)
	}

	// 4. Download again.
	mustReadEquals(t, ctx, c, acc, rootRenamed, content)

	// 5. Create a directory in the root folder.
	dir := "docs"
	if err := c.MakeDirectory(ctx, acc, dir); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	// 6. Upload a small file to that directory.
	dirFile := "docs/file.txt"
	uploadID, err = c.StartUpload(ctx, acc, dirFile)
	if err != nil {
		t.Fatalf("StartUpload(dir): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), dirFile, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(dir): %v", err)
	}
	if err := c.FinishUpload(ctx, dirFile, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(dir): %v", err)
	}
	waitForRead(t, ctx, c, acc, dirFile, content)

	// 7. Rename the directory.
	dirRenamed := "docs-renamed"
	if err := c.Rename(ctx, acc, dir, dirRenamed, true, false); err != nil {
		t.Fatalf("Rename(dir): %v", err)
	}

	// 8. Download again.
	dirFileAfterDirRename := "docs-renamed/file.txt"
	mustReadEquals(t, ctx, c, acc, dirFileAfterDirRename, content)

	// 9. Rename the file.
	dirFileRenamed := "docs-renamed/file-renamed.txt"
	if err := c.Rename(ctx, acc, dirFileAfterDirRename, dirFileRenamed, false, false); err != nil {
		t.Fatalf("Rename(dir/file): %v", err)
	}

	// 10. Download again.
	mustReadEquals(t, ctx, c, acc, dirFileRenamed, content)

	// 11. Delete the file in the root folder.
	if err := c.Delete(ctx, acc, rootRenamed, false); err != nil {
		t.Fatalf("Delete(root): %v", err)
	}
	if _, err := c.Object(ctx, acc, rootRenamed); !errors.Is(err, stores.ErrNotFound) {
		t.Fatalf("expected deleted root file to be not found, got %v", err)
	}

	// 12. Delete the directory.
	if err := c.Delete(ctx, acc, dirRenamed, true); err != nil {
		t.Fatalf("Delete(dir): %v", err)
	}
	if _, err := c.Object(ctx, acc, dirRenamed); !errors.Is(err, stores.ErrNotFound) {
		t.Fatalf("expected deleted directory to be not found, got %v", err)
	}
}

func mustReadEquals(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, want []byte) {
	t.Helper()

	var buf bytes.Buffer
	if err := c.Read(ctx, acc, path, 0, uint64(len(want)), &buf); err != nil {
		t.Fatalf("Read(%s): %v", path, err)
	}

	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("Read(%s): got %q, want %q", path, buf.Bytes(), want)
	}
}

func waitForRead(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, want []byte) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		err := c.Read(ctx, acc, path, 0, uint64(len(want)), &buf)
		if err == nil && bytes.Equal(buf.Bytes(), want) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s to become readable", path)
}

func newTestAccount(t *testing.T, db *stores.Database, username, password string) stores.Account {
	t.Helper()

	u := uuid.New()
	if err := db.AddWorkgroup(stores.Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}

	acc := stores.Account{
		Username:  username,
		Password:  password,
		Workgroup: u.String(),
	}

	if err := db.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	got, err := db.FindAccount(username, u.String())
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}
	if got.ID == 0 {
		t.Fatalf("FindAccount returned empty account for %s/%s", username, u.String())
	}

	return got
}

func newTestShare(t *testing.T, db *stores.Database, name string) stores.Share {
	t.Helper()

	sh := stores.Share{
		Name:         name,
		Type:         "indexd",
		ServerName:   "test-server",
		Password:     "",
		Bucket:       "",
		Remark:       "test share",
		DataShards:   1,
		ParityShards: 0,
	}

	if err := db.RegisterShare(sh); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}

	got, err := db.GetShare(name)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if got.Name == "" {
		t.Fatalf("GetShare returned empty share for %s", name)
	}

	return got
}

func grantFullAccess(t *testing.T, db *stores.Database, sh stores.Share, acc stores.Account) {
	t.Helper()

	wgUUID, err := uuid.Parse(acc.Workgroup)
	if err != nil {
		t.Fatalf("parse workgroup UUID: %v", err)
	}
	wg, err := db.FindWorkgroup(wgUUID)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	// AddConnection is idempotent; safe to call once per (workgroup, share) pair.
	if err := db.AddConnection(wg, sh, make(types.PrivateKey, 64)); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}
	if err := db.SetAccessRights(stores.AccessRights{
		ShareName:     sh.Name,
		AccountID:     acc.ID,
		ReadAccess:    true,
		WriteAccess:   true,
		DeleteAccess:  true,
		ExecuteAccess: true,
	}); err != nil {
		t.Fatalf("SetAccessRights: %v", err)
	}
}

func makeLargeMixedContent() []byte {
	full := int(proto.SectorSize)
	buf := make([]byte, 2*full+12345)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	return buf
}

func uploadInThreeChunks(t *testing.T, ctx context.Context, c Client, path, uploadID string, content []byte) {
	t.Helper()

	full := int(proto.SectorSize)

	parts := []struct {
		partNumber int
		offset     uint64
		data       []byte
	}{
		{1, 0, content[:full]},
		{2, uint64(full), content[full : 2*full]},
		{3, uint64(2 * full), content[2*full:]},
	}

	for _, p := range parts {
		if _, err := c.Write(ctx, bytes.NewReader(p.data), path, uploadID, p.partNumber, p.offset, uint64(len(p.data))); err != nil {
			t.Fatalf("Write(%s, part %d): %v", path, p.partNumber, err)
		}
	}
}

func mustReadFull(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, want []byte) {
	t.Helper()

	var buf bytes.Buffer
	if err := c.Read(ctx, acc, path, 0, uint64(len(want)), &buf); err != nil {
		t.Fatalf("Read(%s): %v", path, err)
	}

	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("Read(%s) mismatch: got %d bytes, want %d bytes", path, len(buf.Bytes()), len(want))
	}
}

func waitForMixedState(t *testing.T, db *stores.Database, acc stores.Account, share, path string, wantRemote, wantLocal int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		slabs, err := db.GetMetadata(acc, share, path, 0, 1<<62)
		if err == nil {
			var remote, local int
			for _, s := range slabs {
				if s.Key != (types.Hash256{}) {
					remote++
				} else if s.Data != nil {
					local++
				}
			}
			if remote == wantRemote && local == wantLocal {
				return
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for mixed state on %s: want remote=%d, local=%d", path, wantRemote, wantLocal)
}

func waitForObjectNotFound(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, err := c.Object(ctx, acc, path)
		if errors.Is(err, stores.ErrNotFound) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s to disappear", path)
}

func TestIndexdClient_RenameDuringMixedUpload(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newGatedFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	content := makeLargeMixedContent()
	origPath := "big.bin"

	uploadID, err := c.StartUpload(ctx, acc, origPath)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}

	uploadInThreeChunks(t, ctx, c, origPath, uploadID, content)

	if err := c.FinishUpload(ctx, origPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}

	// Initially everything is local: 2 full slabs + 1 short local tail.
	waitForMixedState(t, db, acc, share.Name, origPath, 0, 3)
	mustReadFull(t, ctx, c, acc, origPath, content)

	// Let exactly one full slab upload.
	backend.allowUploads(1)
	waitForMixedState(t, db, acc, share.Name, origPath, 1, 2)
	mustReadFull(t, ctx, c, acc, origPath, content)

	// Rename while upload is still in progress.
	newPath := "big-renamed.bin"
	if err := c.Rename(ctx, acc, origPath, newPath, false, false); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// Old path should disappear, new path should be fully readable.
	waitForObjectNotFound(t, ctx, c, acc, origPath)
	mustReadFull(t, ctx, c, acc, newPath, content)

	// Allow remaining full slab to upload.
	backend.allowUploads(1)

	// Final state: 2 uploaded slabs, 1 short local tail.
	waitForMixedState(t, db, acc, share.Name, newPath, 2, 1)
	mustReadFull(t, ctx, c, acc, newPath, content)
}

func TestIndexdClient_RangedReads(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newGatedFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	content := makeLargeMixedContent()
	path := "ranges.bin"

	uploadID, err := c.StartUpload(ctx, acc, path)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}

	uploadInThreeChunks(t, ctx, c, path, uploadID, content)

	if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}

	// Let exactly one full slab upload, so that the ranges below cover both
	// remote and buffered slabs.
	backend.allowUploads(1)
	waitForMixedState(t, db, acc, share.Name, path, 1, 2)

	full := uint64(proto.SectorSize)
	size := uint64(len(content))
	ranges := []struct{ offset, length uint64 }{
		{0, size},            // whole file
		{100, 1000},          // within the remote slab
		{full - 500, 1000},   // across the remote/buffered boundary
		{full + 1234, 4096},  // within the buffered slab
		{2*full - 100, 200},  // across the buffered/tail boundary
		{size - 345, 345},    // end of the tail
		{0, 1},               // first byte
		{size - 1, 1},        // last byte
		{full - 1, full + 2}, // spanning all three slabs
	}

	for _, r := range ranges {
		mustReadRange(t, ctx, c, acc, path, content, r.offset, r.length)
	}

	// Let the remaining full slab upload and verify the same ranges again.
	backend.allowUploads(1)
	waitForMixedState(t, db, acc, share.Name, path, 2, 1)

	for _, r := range ranges {
		mustReadRange(t, ctx, c, acc, path, content, r.offset, r.length)
	}
}

func mustReadRange(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, content []byte, offset, length uint64) {
	t.Helper()

	var buf bytes.Buffer
	if err := c.Read(ctx, acc, path, offset, length, &buf); err != nil {
		t.Fatalf("Read(%s, %d, %d): %v", path, offset, length, err)
	}

	want := content[offset : offset+length]
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("Read(%s, %d, %d) mismatch: got %d bytes, want %d bytes", path, offset, length, len(buf.Bytes()), len(want))
	}
}

func TestIndexdClient_DeleteDuringMixedUpload(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newGatedFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	content := makeLargeMixedContent()
	path := "big-delete.bin"

	uploadID, err := c.StartUpload(ctx, acc, path)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}

	uploadInThreeChunks(t, ctx, c, path, uploadID, content)

	if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}

	waitForMixedState(t, db, acc, share.Name, path, 0, 3)
	mustReadFull(t, ctx, c, acc, path, content)

	// Allow just one slab to upload.
	backend.allowUploads(1)
	waitForMixedState(t, db, acc, share.Name, path, 1, 2)
	mustReadFull(t, ctx, c, acc, path, content)

	// Delete while some slabs are still local.
	if err := c.Delete(ctx, acc, path, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	waitForObjectNotFound(t, ctx, c, acc, path)

	// Reading after delete must fail.
	var buf bytes.Buffer
	err = c.Read(ctx, acc, path, 0, uint64(len(content)), &buf)
	if !errors.Is(err, stores.ErrNotFound) {
		t.Fatalf("expected Read after delete to return ErrNotFound, got %v", err)
	}

	// Even if uploads are later unlocked, the file must stay gone.
	backend.allowUploads(10)
	waitForObjectNotFound(t, ctx, c, acc, path)
}

func TestIndexdClient_RenameDirectoryDuringMixedUpload(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newGatedFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	dir := "docs"
	if err := c.MakeDirectory(ctx, acc, dir); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	content := makeLargeMixedContent()
	origPath := "docs/big.bin"

	uploadID, err := c.StartUpload(ctx, acc, origPath)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}

	uploadInThreeChunks(t, ctx, c, origPath, uploadID, content)

	if err := c.FinishUpload(ctx, origPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}

	// Initially all slabs are still local.
	waitForMixedState(t, db, acc, share.Name, origPath, 0, 3)
	mustReadFull(t, ctx, c, acc, origPath, content)

	// Let one full slab upload.
	backend.allowUploads(1)
	waitForMixedState(t, db, acc, share.Name, origPath, 1, 2)
	mustReadFull(t, ctx, c, acc, origPath, content)

	// Rename the directory while upload is still in progress.
	newDir := "docs-renamed"
	if err := c.Rename(ctx, acc, dir, newDir, true, false); err != nil {
		t.Fatalf("Rename(dir): %v", err)
	}

	// Old path should disappear.
	waitForObjectNotFound(t, ctx, c, acc, origPath)

	// New file path should remain fully readable.
	newPath := "docs-renamed/big.bin"
	mustReadFull(t, ctx, c, acc, newPath, content)

	// Metadata should have moved with the renamed directory.
	waitForMixedState(t, db, acc, share.Name, newPath, 1, 2)

	// Let the remaining full slab upload.
	backend.allowUploads(1)

	// Final state: 2 remote slabs, 1 short local tail.
	waitForMixedState(t, db, acc, share.Name, newPath, 2, 1)
	mustReadFull(t, ctx, c, acc, newPath, content)
}

func TestIndexdClient_OverwriteFileDuringMixedUpload(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newGatedFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	origContent := []byte("old content")
	targetPath := "overwrite.bin"

	uploadID, err := c.StartUpload(ctx, acc, targetPath)
	if err != nil {
		t.Fatalf("StartUpload(original): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(origContent), targetPath, uploadID, 1, 0, uint64(len(origContent))); err != nil {
		t.Fatalf("Write(original): %v", err)
	}
	if err := c.FinishUpload(ctx, targetPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(original): %v", err)
	}
	waitForRead(t, ctx, c, acc, targetPath, origContent)
	mustReadEquals(t, ctx, c, acc, targetPath, origContent)

	// Upload replacement content under a temp name.
	newContent := makeLargeMixedContent()
	tempPath := "temp.bin"

	uploadID, err = c.StartUpload(ctx, acc, tempPath)
	if err != nil {
		t.Fatalf("StartUpload(replacement): %v", err)
	}

	uploadInThreeChunks(t, ctx, c, tempPath, uploadID, newContent)

	if err := c.FinishUpload(ctx, tempPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(replacement): %v", err)
	}

	// Initially all three slabs are local.
	waitForMixedState(t, db, acc, share.Name, tempPath, 0, 3)
	mustReadFull(t, ctx, c, acc, tempPath, newContent)

	// Let one full slab upload.
	backend.allowUploads(1)
	waitForMixedState(t, db, acc, share.Name, tempPath, 1, 2)
	mustReadFull(t, ctx, c, acc, tempPath, newContent)

	// Overwrite the existing file with `force=true` mid-upload.
	if err := c.Rename(ctx, acc, tempPath, targetPath, false, true); err != nil {
		t.Fatalf("Rename(force overwrite): %v", err)
	}

	// The old content should become inaccessible.
	mustNotReadEquals(t, ctx, c, acc, targetPath, origContent)

	// The temp path should disappear.
	waitForObjectNotFound(t, ctx, c, acc, tempPath)

	// The target path should now return the new content immediately.
	mustReadFull(t, ctx, c, acc, targetPath, newContent)

	// And it should still be mid-upload.
	waitForMixedState(t, db, acc, share.Name, targetPath, 1, 2)

	// Let the remaining full slab upload.
	backend.allowUploads(1)

	// Final state: 2 remote slabs, 1 short local tail.
	waitForMixedState(t, db, acc, share.Name, targetPath, 1, 2)
	mustReadFull(t, ctx, c, acc, targetPath, newContent)
}

func mustNotReadEquals(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, notWant []byte) {
	t.Helper()

	var buf bytes.Buffer
	if err := c.Read(ctx, acc, path, 0, uint64(len(notWant)), &buf); err != nil {
		t.Fatalf("Read(%s): %v", path, err)
	}
	if bytes.Equal(buf.Bytes(), notWant) {
		t.Fatalf("Read(%s) unexpectedly matched old content", path)
	}
}

func newTestWorkgroup(t *testing.T, db *stores.Database) stores.Workgroup {
	t.Helper()

	u := uuid.New()
	if err := db.AddWorkgroup(stores.Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	got, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if got.ID == 0 {
		t.Fatalf("FindWorkgroup returned empty workgroup for %s", u)
	}
	return got
}

func newTestWorkgroupWithPublicDirs(t *testing.T, db *stores.Database, dirs ...stores.PublicDir) stores.Workgroup {
	t.Helper()

	u := uuid.New()
	if err := db.AddWorkgroup(stores.Workgroup{UUID: u, PublicDirs: dirs}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	got, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if got.ID == 0 {
		t.Fatalf("FindWorkgroup returned empty workgroup for %s", u)
	}
	return got
}

func newTestAccountInWorkgroup(t *testing.T, db *stores.Database, username, password string, wg stores.Workgroup) stores.Account {
	t.Helper()

	acc := stores.Account{
		Username:  username,
		Password:  password,
		Workgroup: wg.UUID.String(),
	}
	if err := db.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount(%s): %v", username, err)
	}
	got, err := db.FindAccount(username, wg.UUID.String())
	if err != nil {
		t.Fatalf("FindAccount(%s): %v", username, err)
	}
	if got.ID == 0 {
		t.Fatalf("FindAccount returned empty account for %s", username)
	}
	return got
}

// workgroupID returns the numeric ID of the account's workgroup, which the
// client under test is bound to for claiming upload jobs.
func workgroupID(t *testing.T, db *stores.Database, acc stores.Account) int {
	t.Helper()

	u, err := uuid.Parse(acc.Workgroup)
	if err != nil {
		t.Fatalf("parse workgroup UUID %q: %v", acc.Workgroup, err)
	}
	wg, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup(%s): %v", u, err)
	}
	return wg.ID
}

func mustNotFound(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, size int) {
	t.Helper()

	var buf bytes.Buffer
	err := c.Read(ctx, acc, path, 0, uint64(size), &buf)
	if !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("Read(%s) as %s: expected ErrNotFound, got %v", path, acc.Username, err)
	}
}

// TestIndexdClient_CrossWorkgroupIsolation verifies that files uploaded by an
// account in one workgroup are never visible to an account in a different
// workgroup, even when both are connected to the same share.
func TestIndexdClient_CrossWorkgroupIsolation(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg1 := newTestWorkgroup(t, db)
	wg2 := newTestWorkgroup(t, db)
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg1)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg2)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg1.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	content := []byte("alice's secret data")
	path := "secret.txt"

	uploadID, err := c.StartUpload(ctx, alice, path)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), path, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	waitForRead(t, ctx, c, alice, path, content)

	mustReadEquals(t, ctx, c, alice, path, content)
	mustNotFound(t, ctx, c, bob, path, len(content))
}

// TestIndexdClient_WithinWorkgroupPrivacy verifies that files in private
// directories (the default) are invisible to other accounts in the same
// workgroup, and that root-level files are always private.
func TestIndexdClient_WithinWorkgroupPrivacy(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroup(t, db)
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	content := []byte("alice's private content")

	// Upload to root (directory_id = NULL, always private).
	rootPath := "root.txt"
	uploadID, err := c.StartUpload(ctx, alice, rootPath)
	if err != nil {
		t.Fatalf("StartUpload(root): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), rootPath, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(root): %v", err)
	}
	if err := c.FinishUpload(ctx, rootPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(root): %v", err)
	}
	waitForRead(t, ctx, c, alice, rootPath, content)

	// Upload to a private directory (MakeDirectory always sets private=true).
	if err := c.MakeDirectory(ctx, alice, "private-dir"); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}
	dirPath := "private-dir/file.txt"
	uploadID, err = c.StartUpload(ctx, alice, dirPath)
	if err != nil {
		t.Fatalf("StartUpload(dir): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), dirPath, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(dir): %v", err)
	}
	if err := c.FinishUpload(ctx, dirPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(dir): %v", err)
	}
	waitForRead(t, ctx, c, alice, dirPath, content)

	mustReadEquals(t, ctx, c, alice, rootPath, content)
	mustReadEquals(t, ctx, c, alice, dirPath, content)

	mustNotFound(t, ctx, c, bob, rootPath, len(content))
	mustNotFound(t, ctx, c, bob, dirPath, len(content))
}

// TestIndexdClient_WithinWorkgroupPublicDirectory verifies that files in a
// public directory (private=false) are visible to all accounts in the same
// workgroup, while a private directory in the same workgroup remains hidden.
func TestIndexdClient_WithinWorkgroupPublicDirectory(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroup(t, db)
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	// Create a public directory directly via the store. This workgroup has no PublicDirs
	// configured, so MakeDirectory would produce a private dir; bypassing it lets the
	// test focus on the visibility SQL rather than the name-matching logic.
	if err := db.CreateDirectory(alice, share.Name, "/shared", false, false); err != nil {
		t.Fatalf("CreateDirectory(public): %v", err)
	}

	content := []byte("shared content")
	sharedPath := "shared/file.txt"

	uploadID, err := c.StartUpload(ctx, alice, sharedPath)
	if err != nil {
		t.Fatalf("StartUpload(shared): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), sharedPath, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(shared): %v", err)
	}
	if err := c.FinishUpload(ctx, sharedPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(shared): %v", err)
	}
	waitForRead(t, ctx, c, alice, sharedPath, content)

	mustReadEquals(t, ctx, c, alice, sharedPath, content)
	mustReadEquals(t, ctx, c, bob, sharedPath, content)

	// Verify a private directory in the same workgroup still hides its files from bob.
	if err := c.MakeDirectory(ctx, alice, "private-dir"); err != nil {
		t.Fatalf("MakeDirectory(private): %v", err)
	}
	privatePath := "private-dir/secret.txt"
	uploadID, err = c.StartUpload(ctx, alice, privatePath)
	if err != nil {
		t.Fatalf("StartUpload(private): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), privatePath, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(private): %v", err)
	}
	if err := c.FinishUpload(ctx, privatePath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(private): %v", err)
	}
	waitForRead(t, ctx, c, alice, privatePath, content)

	mustReadEquals(t, ctx, c, alice, privatePath, content)
	mustNotFound(t, ctx, c, bob, privatePath, len(content))
}

// TestIndexdClient_MakeDirectoryAutoPublic verifies that MakeDirectory creates a
// non-private directory when its name matches the workgroup's PublicDirs list,
// making files inside visible to all workgroup members, while a non-matching
// directory name still results in a private directory.
func TestIndexdClient_MakeDirectoryAutoPublic(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroupWithPublicDirs(t, db, stores.PublicDir{Path: "shared"})
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	// "shared" is in PublicDirs so MakeDirectory must create a non-private directory.
	if err := c.MakeDirectory(ctx, alice, "shared"); err != nil {
		t.Fatalf("MakeDirectory(shared): %v", err)
	}

	content := []byte("workgroup content")
	sharedPath := "shared/file.txt"
	uploadID, err := c.StartUpload(ctx, alice, sharedPath)
	if err != nil {
		t.Fatalf("StartUpload(shared): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), sharedPath, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(shared): %v", err)
	}
	if err := c.FinishUpload(ctx, sharedPath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(shared): %v", err)
	}
	waitForRead(t, ctx, c, alice, sharedPath, content)

	mustReadEquals(t, ctx, c, alice, sharedPath, content)
	mustReadEquals(t, ctx, c, bob, sharedPath, content)

	// "other" is not in PublicDirs, so it stays private.
	if err := c.MakeDirectory(ctx, alice, "other"); err != nil {
		t.Fatalf("MakeDirectory(other): %v", err)
	}
	privatePath := "other/secret.txt"
	uploadID, err = c.StartUpload(ctx, alice, privatePath)
	if err != nil {
		t.Fatalf("StartUpload(private): %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), privatePath, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(private): %v", err)
	}
	if err := c.FinishUpload(ctx, privatePath, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(private): %v", err)
	}
	waitForRead(t, ctx, c, alice, privatePath, content)

	mustReadEquals(t, ctx, c, alice, privatePath, content)
	mustNotFound(t, ctx, c, bob, privatePath, len(content))
}

// TestIndexdClient_PublicDirCaseSensitivity verifies that the CaseSensitive flag
// of a public folder controls whether the directory-name comparison is exact.
func TestIndexdClient_PublicDirCaseSensitivity(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	content := []byte("test data")

	t.Run("case-insensitive matches uppercase name", func(t *testing.T) {
		share := newTestShare(t, db, "share-ci")
		wg := newTestWorkgroupWithPublicDirs(t, db, stores.PublicDir{Path: "shared"})
		alice := newTestAccountInWorkgroup(t, db, "alice-ci", "secret", wg)
		bob := newTestAccountInWorkgroup(t, db, "bob-ci", "secret", wg)
		grantFullAccess(t, db, share, alice)
		grantFullAccess(t, db, share, bob)

		c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
		t.Cleanup(func() { _ = c.Close() })

		// "SHARED" should match "shared" in PublicDirs when case-insensitive.
		if err := c.MakeDirectory(ctx, alice, "SHARED"); err != nil {
			t.Fatalf("MakeDirectory: %v", err)
		}
		path := "SHARED/file.txt"
		uploadID, err := c.StartUpload(ctx, alice, path)
		if err != nil {
			t.Fatalf("StartUpload: %v", err)
		}
		if _, err := c.Write(ctx, bytes.NewReader(content), path, uploadID, 1, 0, uint64(len(content))); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
			t.Fatalf("FinishUpload: %v", err)
		}
		waitForRead(t, ctx, c, alice, path, content)

		mustReadEquals(t, ctx, c, bob, path, content)
	})

	t.Run("case-sensitive does not match uppercase name", func(t *testing.T) {
		share := newTestShare(t, db, "share-cs")
		wg := newTestWorkgroupWithPublicDirs(t, db, stores.PublicDir{Path: "shared", CaseSensitive: true})
		alice := newTestAccountInWorkgroup(t, db, "alice-cs", "secret", wg)
		bob := newTestAccountInWorkgroup(t, db, "bob-cs", "secret", wg)
		grantFullAccess(t, db, share, alice)
		grantFullAccess(t, db, share, bob)

		c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
		t.Cleanup(func() { _ = c.Close() })

		// "SHARED" must not match "shared" in PublicDirs when case-sensitive.
		if err := c.MakeDirectory(ctx, alice, "SHARED"); err != nil {
			t.Fatalf("MakeDirectory: %v", err)
		}
		path := "SHARED/file.txt"
		uploadID, err := c.StartUpload(ctx, alice, path)
		if err != nil {
			t.Fatalf("StartUpload: %v", err)
		}
		if _, err := c.Write(ctx, bytes.NewReader(content), path, uploadID, 1, 0, uint64(len(content))); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
			t.Fatalf("FinishUpload: %v", err)
		}
		waitForRead(t, ctx, c, alice, path, content)

		mustNotFound(t, ctx, c, bob, path, len(content))
	})
}

// TestIndexdClient_PublicDirCrossWorkgroupIsolation verifies that a public
// directory (created via PublicDirs name-matching) in one workgroup is invisible
// to accounts that belong to a different workgroup.
func TestIndexdClient_PublicDirCrossWorkgroupIsolation(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg1 := newTestWorkgroupWithPublicDirs(t, db, stores.PublicDir{Path: "shared"})
	wg2 := newTestWorkgroup(t, db)
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg1)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg2)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg1.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	if err := c.MakeDirectory(ctx, alice, "shared"); err != nil {
		t.Fatalf("MakeDirectory(shared): %v", err)
	}

	content := []byte("wg1 only content")
	path := "shared/file.txt"
	uploadID, err := c.StartUpload(ctx, alice, path)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), path, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	waitForRead(t, ctx, c, alice, path, content)

	mustReadEquals(t, ctx, c, alice, path, content)
	// Bob belongs to a different workgroup and must not see Alice's public dir.
	mustNotFound(t, ctx, c, bob, path, len(content))
}

// uploadFile uploads content to path as acc and waits until it can be read back.
func uploadFile(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, content []byte) {
	t.Helper()

	uploadID, err := c.StartUpload(ctx, acc, path)
	if err != nil {
		t.Fatalf("StartUpload(%s) as %s: %v", path, acc.Username, err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), path, uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write(%s) as %s: %v", path, acc.Username, err)
	}
	if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(%s) as %s: %v", path, acc.Username, err)
	}
	waitForRead(t, ctx, c, acc, path, content)
}

// TestIndexdClient_ReadOnlyPublicDir verifies that a file in a read-only public
// folder stays readable for the whole workgroup but may only be deleted,
// renamed over, or overwritten by the account that owns it.
func TestIndexdClient_ReadOnlyPublicDir(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroupWithPublicDirs(t, db, stores.PublicDir{Path: "readonly", ReadOnly: true})
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	if err := c.MakeDirectory(ctx, alice, "readonly"); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	content := []byte("alice's shared report")
	path := "readonly/report.txt"
	uploadFile(t, ctx, c, alice, path, content)

	// The folder is public, so bob sees the file.
	mustReadEquals(t, ctx, c, bob, path, content)

	// The folder is read-only, so bob may neither delete nor overwrite the file.
	if err := c.Delete(ctx, bob, path, false); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("Delete as bob: expected ErrNotFound, got %v", err)
	}
	if _, err := c.StartUpload(ctx, bob, path); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("StartUpload as bob: expected ErrNotFound, got %v", err)
	}
	mustReadEquals(t, ctx, c, bob, path, content)

	// Alice, who owns the file, may overwrite it.
	updated := []byte("alice's revised report")
	uploadFile(t, ctx, c, alice, path, updated)
	mustReadEquals(t, ctx, c, bob, path, updated)
	content = updated

	// Bob may still place his own file into the folder, and alice can read it.
	bobContent := []byte("bob's own notes")
	bobPath := "readonly/notes.txt"
	uploadFile(t, ctx, c, bob, bobPath, bobContent)
	mustReadEquals(t, ctx, c, alice, bobPath, bobContent)

	// Bob may not rename his own file over alice's; both files stay intact.
	if err := c.Rename(ctx, bob, bobPath, path, false, true); !errors.Is(err, stores.ErrAccessDenied) {
		t.Errorf("Rename as bob over alice's file: expected ErrAccessDenied, got %v", err)
	}
	mustReadEquals(t, ctx, c, alice, path, content)
	mustReadEquals(t, ctx, c, bob, bobPath, bobContent)

	// Alice, who owns the file, may delete it.
	if err := c.Delete(ctx, alice, path, false); err != nil {
		t.Fatalf("Delete as alice: %v", err)
	}
	mustNotFound(t, ctx, c, alice, path, len(content))
}

// TestIndexdClient_RewritablePublicDir verifies that in a public folder that is
// not read-only, any member of the workgroup may delete and rename over the
// files of the other members.
func TestIndexdClient_RewritablePublicDir(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroupWithPublicDirs(t, db, stores.PublicDir{Path: "shared"})
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	if err := c.MakeDirectory(ctx, alice, "shared"); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	content := []byte("alice's draft")
	path := "shared/draft.txt"
	uploadFile(t, ctx, c, alice, path, content)
	mustReadEquals(t, ctx, c, bob, path, content)

	// Bob overwrites alice's file.
	bobContent := []byte("bob's revision")
	uploadFile(t, ctx, c, bob, path, bobContent)
	mustReadEquals(t, ctx, c, alice, path, bobContent)

	// Bob also renames another of his files over it.
	renamed := []byte("bob's second revision")
	bobPath := "shared/revision.txt"
	uploadFile(t, ctx, c, bob, bobPath, renamed)
	if err := c.Rename(ctx, bob, bobPath, path, false, true); err != nil {
		t.Fatalf("Rename as bob: %v", err)
	}
	mustReadEquals(t, ctx, c, alice, path, renamed)
	bobContent = renamed

	// Bob deletes the file, too.
	if err := c.Delete(ctx, bob, path, false); err != nil {
		t.Fatalf("Delete as bob: %v", err)
	}
	mustNotFound(t, ctx, c, alice, path, len(bobContent))
}

// TestIndexdClient_OverwriteOwnFile verifies that a file can be uploaded again
// after a previous upload to the same path has been finalized. The uploads entry
// of the finished upload lingers until its buffered slabs reach the Sia network,
// and must not be mistaken for an upload that is still in flight.
func TestIndexdClient_OverwriteOwnFile(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	path := "file.txt"
	uploadFile(t, ctx, c, acc, path, []byte("first revision"))

	second := []byte("second revision")
	uploadFile(t, ctx, c, acc, path, second)
	mustReadEquals(t, ctx, c, acc, path, second)

	// A second upload to a path that already has one in flight is still refused.
	inFlight, err := c.StartUpload(ctx, acc, "pending.txt")
	if err != nil {
		t.Fatalf("StartUpload(pending): %v", err)
	}
	if _, err := c.StartUpload(ctx, acc, "pending.txt"); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("StartUpload(pending) while in flight: expected ErrNotFound, got %v", err)
	}
	if err := c.AbortUpload(ctx, "pending.txt", inFlight); err != nil {
		t.Fatalf("AbortUpload(pending): %v", err)
	}
}

// TestIndexdClient_UpdateWorkgroupRestampsDirectories verifies that changing the
// public folders of a workgroup also re-applies them to the folders that already
// exist, so that a folder can be shared, switched to read-only, and made private
// again after it has been created.
func TestIndexdClient_UpdateWorkgroupRestampsDirectories(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroup(t, db)
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	// The workgroup has no public folders yet, so the directory starts private.
	if err := c.MakeDirectory(ctx, alice, "team"); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	content := []byte("team content")
	path := "team/notes.txt"
	uploadFile(t, ctx, c, alice, path, content)
	mustNotFound(t, ctx, c, bob, path, len(content))

	setPublicDirs := func(dirs ...stores.PublicDir) {
		t.Helper()
		wg.PublicDirs = dirs
		if err := db.UpdateWorkgroup(wg); err != nil {
			t.Fatalf("UpdateWorkgroup: %v", err)
		}
	}

	// Sharing the folder makes the existing file visible to the workgroup, and
	// rewritable, since the entry is not read-only.
	setPublicDirs(stores.PublicDir{Path: "team"})
	mustReadEquals(t, ctx, c, bob, path, content)

	// Switching the entry to read-only protects the existing file from bob.
	setPublicDirs(stores.PublicDir{Path: "team", ReadOnly: true})
	mustReadEquals(t, ctx, c, bob, path, content)
	if err := c.Delete(ctx, bob, path, false); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("Delete as bob: expected ErrNotFound, got %v", err)
	}

	// A case-sensitive entry that no longer matches makes the folder private.
	setPublicDirs(stores.PublicDir{Path: "TEAM", ReadOnly: true, CaseSensitive: true})
	mustNotFound(t, ctx, c, bob, path, len(content))

	// Dropping the list entirely leaves the folder private, too.
	setPublicDirs(stores.PublicDir{Path: "team"})
	mustReadEquals(t, ctx, c, bob, path, content)
	setPublicDirs()
	mustNotFound(t, ctx, c, bob, path, len(content))

	// Alice, who owns the folder, keeps her access throughout.
	mustReadEquals(t, ctx, c, alice, path, content)
}

// TestIndexdClient_RenamePublicDirMovesAllContents verifies that renaming a
// public folder also moves the entries the caller may not touch directly: the
// files of the other members and their read-only subfolders have to follow the
// renamed folder instead of keeping their old paths.
func TestIndexdClient_RenamePublicDirMovesAllContents(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroupWithPublicDirs(t, db,
		stores.PublicDir{Path: "shared"},
		stores.PublicDir{Path: "ro", ReadOnly: true},
	)
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	// Alice sets up a public folder holding one of her files and a read-only
	// subfolder with another one.
	if err := c.MakeDirectory(ctx, alice, "shared"); err != nil {
		t.Fatalf("MakeDirectory(shared): %v", err)
	}
	if err := c.MakeDirectory(ctx, alice, "shared/ro"); err != nil {
		t.Fatalf("MakeDirectory(shared/ro): %v", err)
	}
	doc := []byte("alice's document")
	report := []byte("alice's report")
	uploadFile(t, ctx, c, alice, "shared/doc.txt", doc)
	uploadFile(t, ctx, c, alice, "shared/ro/report.txt", report)

	// Bob may rename the folder, since it is public and not read-only.
	if err := c.Rename(ctx, bob, "shared", "team", true, false); err != nil {
		t.Fatalf("Rename as bob: %v", err)
	}

	// Everything inside moved along, for both members.
	mustReadEquals(t, ctx, c, alice, "team/doc.txt", doc)
	mustReadEquals(t, ctx, c, bob, "team/doc.txt", doc)
	mustReadEquals(t, ctx, c, alice, "team/ro/report.txt", report)
	mustReadEquals(t, ctx, c, bob, "team/ro/report.txt", report)
	mustNotFound(t, ctx, c, alice, "shared/doc.txt", len(doc))
	mustNotFound(t, ctx, c, alice, "shared/ro/report.txt", len(report))
}

// TestIndexdClient_DuplicatePublicDirEntries verifies that when the same path
// appears twice in the public folder list, the first entry decides the flags
// everywhere: in the stored list and on the directories it is applied to.
func TestIndexdClient_DuplicatePublicDirEntries(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	wg := newTestWorkgroup(t, db)
	alice := newTestAccountInWorkgroup(t, db, "alice", "secret", wg)
	bob := newTestAccountInWorkgroup(t, db, "bob", "secret", wg)

	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, alice)
	grantFullAccess(t, db, share, bob)

	c := newIndexdClient(db, newFakeBackend(), share.Name, wg.ID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	if err := c.MakeDirectory(ctx, alice, "dup"); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}
	content := []byte("alice's file")
	path := "dup/file.txt"
	uploadFile(t, ctx, c, alice, path, content)

	// The first entry is not read-only and wins over the second.
	wg.PublicDirs = []stores.PublicDir{
		{Path: "dup"},
		{Path: "dup", ReadOnly: true},
	}
	if err := db.UpdateWorkgroup(wg); err != nil {
		t.Fatalf("UpdateWorkgroup: %v", err)
	}

	stored, err := db.FindWorkgroup(wg.UUID)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if len(stored.PublicDirs) != 1 || stored.PublicDirs[0].ReadOnly {
		t.Fatalf("stored config: want [{dup}], got %+v", stored.PublicDirs)
	}

	// The folder is stamped rewritable accordingly, so bob may delete
	// alice's file.
	if err := c.Delete(ctx, bob, path, false); err != nil {
		t.Fatalf("Delete as bob: %v", err)
	}
	mustNotFound(t, ctx, c, alice, path, len(content))
}

// uploadBuffered uploads a file in one part and finalizes it. A file smaller
// than a slab stays buffered in the database until it is packed.
func uploadBuffered(t *testing.T, ctx context.Context, c Client, acc stores.Account, path string, data []byte) {
	t.Helper()

	uploadID, err := c.StartUpload(ctx, acc, path)
	if err != nil {
		t.Fatalf("StartUpload(%s): %v", path, err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(data), path, uploadID, 1, 0, uint64(len(data))); err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}
	if err := c.FinishUpload(ctx, path, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload(%s): %v", path, err)
	}
}

// waitForObjects waits until the backend holds the expected number of objects.
func waitForObjects(t *testing.T, fb *fakeBackend, want int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fb.mu.Lock()
		n := len(fb.objects)
		fb.mu.Unlock()

		if n == want {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	fb.mu.Lock()
	n := len(fb.objects)
	fb.mu.Unlock()
	t.Fatalf("timed out waiting for %d uploaded objects, got %d", want, n)
}

// TestIndexdClient_PackedSlab verifies that the pieces of several files that
// are each too small for a slab of their own are uploaded together as one
// packed slab, that every file still reads back whole, and that the slab is
// only unpinned once the last of those files is deleted.
func TestIndexdClient_PackedSlab(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	fb := newFakeBackend()
	c := newIndexdClient(db, fb, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	// With a single data shard, three files of this size stay buffered on
	// their own, while together they exceed one slab.
	slabSize := uint64(proto.SectorSize)
	size := slabSize/2 - slabSize/8

	names := []string{"a.bin", "b.bin", "c.bin"}
	contents := make(map[string][]byte, len(names))
	for _, name := range names {
		data := make([]byte, size)
		frand.Read(data)
		contents[name] = data
		uploadBuffered(t, ctx, c, acc, name, data)
	}

	// The three of them make exactly one packed slab, with the tail of the
	// last file left over.
	waitForObjects(t, fb, 1)

	fb.mu.Lock()
	var packed []byte
	for _, data := range fb.objects {
		packed = data
	}
	fb.mu.Unlock()

	if uint64(len(packed)) != slabSize {
		t.Fatalf("want a packed slab of %d bytes, got %d", slabSize, len(packed))
	}

	// Every file reads back whole, including the one that spans the packed
	// slab and the buffer holding its remainder.
	for _, name := range names {
		mustReadEquals(t, ctx, c, acc, name, contents[name])
	}

	// Deleting a file that shares the slab must not take the slab with it.
	for _, name := range names[:2] {
		if err := c.Delete(ctx, acc, name, false); err != nil {
			t.Fatalf("Delete(%s): %v", name, err)
		}
	}

	fb.mu.Lock()
	n := len(fb.objects)
	fb.mu.Unlock()
	if n != 1 {
		t.Fatalf("want the shared slab to survive, got %d objects", n)
	}
	mustReadEquals(t, ctx, c, acc, names[2], contents[names[2]])

	// Once the last file that references it is gone, so is the slab.
	if err := c.Delete(ctx, acc, names[2], false); err != nil {
		t.Fatalf("Delete(%s): %v", names[2], err)
	}
	waitForObjects(t, fb, 0)
}

// TestIndexdClient_PackedSlabUploadFailure verifies that a packed slab whose
// upload fails puts its pieces back in the queue, so that they are packed again
// once the storage backend recovers.
func TestIndexdClient_PackedSlabUploadFailure(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	fb := newFakeBackend()
	fb.failUploads(errors.New("backend is down"))

	c := newIndexdClient(db, fb, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	slabSize := uint64(proto.SectorSize)
	size := slabSize/2 - slabSize/8

	names := []string{"a.bin", "b.bin", "c.bin"}
	contents := make(map[string][]byte, len(names))
	for _, name := range names {
		data := make([]byte, size)
		frand.Read(data)
		contents[name] = data
		uploadBuffered(t, ctx, c, acc, name, data)
	}

	// Wait for the packer to have tried and failed at least once.
	deadline := time.Now().Add(10 * time.Second)
	for fb.uploadAttempts() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the packer to attempt an upload")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The files are readable throughout, straight from their buffers.
	for _, name := range names {
		mustReadEquals(t, ctx, c, acc, name, contents[name])
	}

	fb.failUploads(nil)
	waitForObjects(t, fb, 1)

	for _, name := range names {
		mustReadEquals(t, ctx, c, acc, name, contents[name])
	}
}

// TestIndexdClient_PackedSlabFilesDeletedDuringUpload verifies that a packed
// slab whose every file is deleted while it is being uploaded does not stay
// pinned, since nothing references it by the time it lands.
func TestIndexdClient_PackedSlabFilesDeletedDuringUpload(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	fb := newGatedFakeBackend()
	c := newIndexdClient(db, fb, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })

	slabSize := uint64(proto.SectorSize)
	size := slabSize/2 - slabSize/8

	names := []string{"a.bin", "b.bin", "c.bin"}
	for _, name := range names {
		data := make([]byte, size)
		frand.Read(data)
		uploadBuffered(t, ctx, c, acc, name, data)
	}

	// Hold the packer in the backend, its pieces already claimed.
	deadline := time.Now().Add(10 * time.Second)
	for fb.uploadAttempts() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the packer to start an upload")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, name := range names {
		if err := c.Delete(ctx, acc, name, false); err != nil {
			t.Fatalf("Delete(%s): %v", name, err)
		}
	}

	// Let the slab land on a share where none of its files exist any more.
	fb.allowUploads(1)

	deadline = time.Now().Add(10 * time.Second)
	for fb.deleteCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the orphaned packed slab to be deleted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	fb.mu.Lock()
	n := len(fb.objects)
	fb.mu.Unlock()
	if n != 0 {
		t.Fatalf("want no objects left, got %d", n)
	}
}

// syncBuffer collects log output written from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestIndexdClient_PackingOptionsWarning verifies that a minimum size which the
// leftover data can never reach without filling a slab first is reported, since
// it leaves the configured age with nothing to trigger on.
func TestIndexdClient_PackingOptionsWarning(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	newTestShare(t, db, "testshare")
	slabSize := uint64(proto.SectorSize)

	tests := []struct {
		name    string
		packing PackingOptions
		warn    bool
	}{
		{name: "default", packing: PackingOptions{}},
		{name: "usable minimum", packing: PackingOptions{MinSize: slabSize / 2, MaxAge: time.Hour}},
		{name: "minimum without an age", packing: PackingOptions{MinSize: slabSize * 2}},
		{name: "minimum at the slab size", packing: PackingOptions{MinSize: slabSize, MaxAge: time.Hour}, warn: true},
		{name: "minimum past the slab size", packing: PackingOptions{MinSize: slabSize * 2, MaxAge: time.Hour}, warn: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out syncBuffer
			log.SetOutput(&out)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			c := newIndexdClient(db, newFakeBackend(), "testshare", 1, 1, 0, tc.packing)
			_ = c.Close()

			if got := strings.Contains(out.String(), "will never upload anything"); got != tc.warn {
				t.Fatalf("want warning %v, got %q", tc.warn, out.String())
			}
		})
	}
}

// TestCompleteWithRetry verifies that recording an uploaded slab is retried on
// transient failures, while a definite ErrNotFound is returned right away. A
// batch that fell out of the queue at claim time depends on this to not be
// abandoned over a database hiccup.
func TestCompleteWithRetry(t *testing.T) {
	t.Run("first try", func(t *testing.T) {
		var calls int
		if err := completeWithRetry(func() error {
			calls++
			return nil
		}); err != nil || calls != 1 {
			t.Fatalf("want success after 1 call, got err %v after %d calls", err, calls)
		}
	})

	t.Run("not found is definite", func(t *testing.T) {
		var calls int
		if err := completeWithRetry(func() error {
			calls++
			return stores.ErrNotFound
		}); !errors.Is(err, stores.ErrNotFound) || calls != 1 {
			t.Fatalf("want ErrNotFound after 1 call, got err %v after %d calls", err, calls)
		}
	})

	t.Run("transient failure", func(t *testing.T) {
		var calls int
		if err := completeWithRetry(func() error {
			calls++
			if calls == 1 {
				return errors.New("hiccup")
			}
			return nil
		}); err != nil || calls != 2 {
			t.Fatalf("want success after 2 calls, got err %v after %d calls", err, calls)
		}
	})

	t.Run("persistent failure", func(t *testing.T) {
		fail := errors.New("database is down")
		var calls int
		if err := completeWithRetry(func() error {
			calls++
			return fail
		}); !errors.Is(err, fail) || calls != completionAttempts {
			t.Fatalf("want %v after %d calls, got err %v after %d calls", fail, completionAttempts, err, calls)
		}
	})
}

// TestIndexdClient_LateCompletionSharedSlab verifies that a slab whose own
// files disappeared during the upload is only unpinned when no other file
// references its key. Slabs are content-addressed, so a duplicate file may
// legitimately hold the key of an upload whose file is already gone, and
// unpinning it then would destroy that file's data.
func TestIndexdClient_LateCompletionSharedSlab(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	fb := newFakeBackend()
	c := newIndexdClient(db, fb, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })
	ic := c.(*IndexdClient)

	// Upload a full-slab file and wait until its slab key is recorded.
	content := make([]byte, proto.SectorSize)
	frand.Read(content)
	uploadBuffered(t, ctx, c, acc, "a.bin", content)
	key := waitForSlabKey(t, db, acc, share.Name, "a.bin", uint64(len(content)))

	// A late completion of some other upload that produced the same key has
	// to leave the slab alone: a.bin references it.
	ic.dropSlabIfUnreferenced(ctx, key)
	if n := fb.deleteCount(); n != 0 {
		t.Fatalf("dropped a slab that a live file references")
	}
	mustReadEquals(t, ctx, c, acc, "a.bin", content)

	// A key that no file references is deleted as before.
	orphan, err := fb.Upload(ctx, bytes.NewReader([]byte("orphaned")), 1, 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	ic.dropSlabIfUnreferenced(ctx, orphan)
	if n := fb.deleteCount(); n != 1 {
		t.Fatalf("want the orphaned slab deleted, got %d deletions", n)
	}
}

// waitForSlabKey waits until the file's single slab has been uploaded and
// recorded, and returns its key.
func waitForSlabKey(t *testing.T, db *stores.Database, acc stores.Account, share, path string, size uint64) types.Hash256 {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		slabs, err := db.GetMetadata(acc, share, path, 0, size)
		if err == nil && len(slabs) == 1 && slabs[0].Key != (types.Hash256{}) {
			return slabs[0].Key
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the slab of %s to be recorded", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestIndexdClient_UnpinRetry verifies that a slab whose unpin could not reach
// the storage backend stays staged and is unpinned by the periodic retry, and
// that a staged slab whose key a live file references again is unstaged
// instead of unpinned.
func TestIndexdClient_UnpinRetry(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	wgID := workgroupID(t, db, acc)
	fb := newFakeBackend()
	c := newIndexdClient(db, fb, share.Name, wgID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })
	ic := c.(*IndexdClient)

	content := make([]byte, proto.SectorSize)
	frand.Read(content)
	uploadBuffered(t, ctx, c, acc, "a.bin", content)
	waitForSlabKey(t, db, acc, share.Name, "a.bin", uint64(len(content)))

	// The backend is down when the file is deleted: the file is gone, but
	// its slab stays pinned and staged for retry.
	fb.failDeletes(errors.New("backend is down"))
	if err := c.Delete(ctx, acc, "a.bin", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	waitForObjects(t, fb, 1)
	staged, err := db.PendingUnpins(share.Name, wgID)
	if err != nil || len(staged) != 1 {
		t.Fatalf("want 1 staged unpin, got %v, %v", staged, err)
	}

	// Once the backend is back, the periodic retry unpins and confirms.
	fb.failDeletes(nil)
	ic.retryPendingUnpins(ctx)
	waitForObjects(t, fb, 0)
	if staged, err := db.PendingUnpins(share.Name, wgID); err != nil || len(staged) != 0 {
		t.Fatalf("want no staged unpins left, got %v, %v", staged, err)
	}

	// A staged slab whose key a live file references is unstaged untouched.
	content2 := make([]byte, proto.SectorSize)
	frand.Read(content2)
	uploadBuffered(t, ctx, c, acc, "b.bin", content2)
	key := waitForSlabKey(t, db, acc, share.Name, "b.bin", uint64(len(content2)))

	if err := db.StageUnpin(share.Name, wgID, key); err != nil {
		t.Fatalf("StageUnpin: %v", err)
	}
	ic.retryPendingUnpins(ctx)
	if staged, err := db.PendingUnpins(share.Name, wgID); err != nil || len(staged) != 0 {
		t.Fatalf("want the referenced slab unstaged, got %v, %v", staged, err)
	}
	mustReadEquals(t, ctx, c, acc, "b.bin", content2)
}

// TestIndexdClient_StrandedPieceRecovery verifies that a piece claimed by a
// previous incarnation of the process, which died before completing it, is
// requeued by the janitor and then uploaded normally. The piece is only
// requeued on the second sweep that sees it stranded, so that a claim that is
// merely slow is not queued a second time.
func TestIndexdClient_StrandedPieceRecovery(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)
	wgID := workgroupID(t, db, acc)

	// The previous incarnation of the process buffers a full-slab file,
	// claims it, and dies before completing the upload.
	content := make([]byte, proto.SectorSize)
	frand.Read(content)
	uploadID, err := db.CreateUpload(acc, share.Name, "a.bin")
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if err := db.AddBufferedSlab(uploadID, 0, content); err != nil {
		t.Fatalf("AddBufferedSlab: %v", err)
	}
	if err := db.FinalizeUpload(uploadID); err != nil {
		t.Fatalf("FinalizeUpload: %v", err)
	}
	if _, err := db.ClaimUploadJob(share.Name, wgID, uint64(proto.SectorSize)); err != nil {
		t.Fatalf("ClaimUploadJob: %v", err)
	}

	fb := newFakeBackend()
	c := newIndexdClient(db, fb, share.Name, wgID, 1, 0, PackingOptions{})
	t.Cleanup(func() { _ = c.Close() })
	ic := c.(*IndexdClient)

	// The first sweep only takes note of the stranded piece.
	suspects := ic.requeueStrandedPieces(nil)
	if len(suspects) != 1 {
		t.Fatalf("want 1 suspect after the first sweep, got %d", len(suspects))
	}
	if ids, err := db.StrandedPieces(share.Name, wgID); err != nil || len(ids) != 1 {
		t.Fatalf("want the piece still stranded after the first sweep, got %v, %v", ids, err)
	}

	// The second sweep requeues it, and an upload worker takes it from there.
	ic.requeueStrandedPieces(suspects)
	waitForSlabKey(t, db, acc, share.Name, "a.bin", uint64(len(content)))
	mustReadEquals(t, ctx, c, acc, "a.bin", content)
}
