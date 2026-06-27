package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/api/app"
	"go.sia.tech/indexd/slabs"
)

type fakeBackend struct {
	mu         sync.Mutex
	objects    map[types.Hash256][]byte
	nextID     uint64
	uploadGate chan struct{}
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
	if fb.uploadGate != nil {
		select {
		case <-ctx.Done():
			return types.Hash256{}, ctx.Err()
		case <-fb.uploadGate:
		}
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()

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

	c := newIndexdClient(db, newFakeBackend(), share.Name, 1, 0)
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

	err := db.SetAccessRights(stores.AccessRights{
		ShareName:     sh.Name,
		AccountID:     acc.ID,
		ReadAccess:    true,
		WriteAccess:   true,
		DeleteAccess:  true,
		ExecuteAccess: true,
	})
	if err != nil {
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
		slabs, err := db.GetMetadata(acc, share, path)
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
	c := newIndexdClient(db, backend, share.Name, 1, 0)
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

func TestIndexdClient_DeleteDuringMixedUpload(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newGatedFakeBackend()
	c := newIndexdClient(db, backend, share.Name, 1, 0)
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
	c := newIndexdClient(db, backend, share.Name, 1, 0)
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
	c := newIndexdClient(db, backend, share.Name, 1, 0)
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
