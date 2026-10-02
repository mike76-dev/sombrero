package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
	sdk "go.sia.tech/siastorage"
)

// fakePinner stands in for an indexer asked to take over objects it does not
// hold yet.
type fakePinner struct {
	pinned []types.Hash256
	err    error
}

// PinObject implements ObjectPinner.
func (fp *fakePinner) PinObject(ctx context.Context, obj sdk.Object) error {
	if fp.err != nil {
		return fp.err
	}
	fp.pinned = append(fp.pinned, obj.ID())

	return nil
}

// foreignSlab is a slab as another account describes one: a key of its own, the
// shards it takes, and the hosts that hold them.
func foreignSlab(n byte, length uint32) slabs.SlabSlice {
	return slabs.SlabSlice{
		Version:       1,
		EncryptionKey: slabs.EncryptionKey{n},
		MinShards:     2,
		Sectors: []slabs.PinnedSector{
			{Root: types.Hash256{n}, HostKey: types.PublicKey{n + 1}},
			{Root: types.Hash256{n + 2}, HostKey: types.PublicKey{n + 3}},
		},
		Length: length,
	}
}

// foreignFile is a file of another account's, with what it takes to pin it and
// where to fetch it from if that cannot be had.
func foreignFile(path string, length uint32) transfer.File {
	pin := &transfer.Pin{DataKey: [32]byte{9}, Slabs: []slabs.SlabSlice{foreignSlab(1, length)}}
	obj := sdk.NewUnsafeObject(pin.DataKey, pin.Slabs)

	return transfer.File{
		Path:       path,
		Size:       uint64(length),
		CreatedAt:  time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second),
		ModifiedAt: time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second),
		Parts: []transfer.Part{{
			Offset: 0, Length: uint64(length),
			Pin:    pin,
			Source: &transfer.Source{Kind: "indexd", Key: obj.ID().String()},
		}},
	}
}

// importTarget sets up a share, a workgroup and an account for an import to
// write its rows to.
func importTarget(t *testing.T, db *stores.Database, acc stores.Account, share string) stores.TransferTarget {
	t.Helper()

	return stores.TransferTarget{Share: share, Workgroup: workgroupID(t, db, acc), Owner: acc}
}

// TestImportPins verifies that what an indexer will take over is taken over
// where it lies: the rows are written, and not one byte is moved.
func TestImportPins(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	file := foreignFile("/taken/over.bin", 4096)
	src := &fakeSource{content: map[string][]byte{}}
	pinner := &fakePinner{}

	stats, err := Import(ctx, db, c, src, pinner,
		describeFiles(t, []transfer.File{file}, transfer.Directory{Path: "/taken", ReadOnly: true}),
		ImportOptions{CopyOptions: CopyOptions{Account: acc}, Target: importTarget(t, db, acc, share.Name)})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if stats.Pinned != 1 || stats.Copied != 0 || stats.Directories != 1 {
		t.Fatalf("stats: want the file pinned and nothing copied, got %+v", stats)
	}
	if stats.Bytes != 0 || src.reads != 0 {
		t.Errorf("a pinned file moved %d byte(s) in %d read(s)", stats.Bytes, src.reads)
	}

	// The object the account now holds is the one the description named.
	wanted := sdk.NewUnsafeObject(file.Parts[0].Pin.DataKey, file.Parts[0].Pin.Slabs)
	want := wanted.ID()
	if len(pinner.pinned) != 1 || pinner.pinned[0] != want {
		t.Errorf("pinned: want %s, got %v", want, pinner.pinned)
	}

	// The file is there, made of the object that was pinned, with the times the
	// description carried rather than the ones of the import.
	info, err := c.Object(ctx, acc, "/taken/over.bin")
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	if info.Size != 4096 {
		t.Errorf("the imported file: want 4096 bytes, got %d", info.Size)
	}
	if !info.ModifiedAt.Equal(file.ModifiedAt) {
		t.Errorf("the imported file was stamped %v, want the %v it came with", info.ModifiedAt, file.ModifiedAt)
	}

	metadata, err := db.GetMetadata(acc, share.Name, "/taken/over.bin", 0, 4096)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(metadata) != 1 || metadata[0].Key != want {
		t.Errorf("the metadata of the imported file: want the pinned object, got %+v", metadata)
	}
}

// TestImportCopiesWhatItCannotPin verifies that a file the indexer will not take
// over is brought across the long way instead of being left behind.
func TestImportCopiesWhatItCannotPin(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	content := []byte("the slab is too old to be taken over")
	file := foreignFile("/taken/over.bin", uint32(len(content)))
	src := &fakeSource{content: map[string][]byte{file.Parts[0].Source.Key: content}}

	// The indexer refuses, the way it does for a slab whose upload it considers
	// too old to pin.
	pinner := &fakePinner{err: errors.New("not enough redundancy: the slab is too old")}

	var refused []string
	stats, err := Import(ctx, db, c, src, pinner,
		describeFiles(t, []transfer.File{file}),
		ImportOptions{
			CopyOptions: CopyOptions{Account: acc, OnError: func(path string, err error) { refused = append(refused, path) }},
			Target:      importTarget(t, db, acc, share.Name),
		})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if stats.Copied != 1 || stats.Pinned != 0 || stats.Failed != 0 {
		t.Fatalf("stats: want the file copied after the refusal, got %+v", stats)
	}
	if stats.Bytes != uint64(len(content)) {
		t.Errorf("bytes copied: want %d, got %d", len(content), stats.Bytes)
	}
	if len(refused) != 1 || refused[0] != "/taken/over.bin" {
		t.Errorf("the refusal reported: got %v", refused)
	}

	waitForRead(t, ctx, c, acc, "/taken/over.bin", content)
}

// TestImportCopiesWhenAsked verifies that an import told to copy leaves the
// source's slabs alone even where they could have been taken over.
func TestImportCopiesWhenAsked(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	content := []byte("copied on purpose")
	file := foreignFile("/over.bin", uint32(len(content)))
	src := &fakeSource{content: map[string][]byte{file.Parts[0].Source.Key: content}}
	pinner := &fakePinner{}

	stats, err := Import(ctx, db, c, src, pinner,
		describeFiles(t, []transfer.File{file}),
		ImportOptions{
			CopyOptions: CopyOptions{Account: acc},
			Target:      importTarget(t, db, acc, share.Name),
			Copy:        true,
		})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if stats.Copied != 1 || len(pinner.pinned) != 0 {
		t.Errorf("stats: want the file copied and nothing pinned, got %+v and %v", stats, pinner.pinned)
	}

	waitForRead(t, ctx, c, acc, "/over.bin", content)
}

// TestImportLeavesWhatIsThere verifies that an import run again takes up where
// it left off, whether what is there was pinned or copied.
func TestImportLeavesWhatIsThere(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	file := foreignFile("/over.bin", 512)
	src := &fakeSource{content: map[string][]byte{}}
	pinner := &fakePinner{}
	target := importTarget(t, db, acc, share.Name)

	if _, err := Import(ctx, db, c, src, pinner, describeFiles(t, []transfer.File{file}),
		ImportOptions{CopyOptions: CopyOptions{Account: acc}, Target: target}); err != nil {
		t.Fatalf("the first import: %v", err)
	}

	stats, err := Import(ctx, db, c, src, pinner, describeFiles(t, []transfer.File{file}),
		ImportOptions{CopyOptions: CopyOptions{Account: acc}, Target: target})
	if err != nil {
		t.Fatalf("the second import: %v", err)
	}
	if stats.Skipped != 1 || stats.Pinned != 0 {
		t.Errorf("the second import: want the file left alone, got %+v", stats)
	}
}

// TestTheDestinationPinsWhatIsImported verifies whose account the objects end up
// in: the share's own, since pinning them where they already are does nothing and
// would leave the rows naming objects this share does not hold.
func TestTheDestinationPinsWhatIsImported(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	pinner, ok := c.(ObjectPinner)
	if !ok {
		t.Fatal("the client of a share cannot pin what is imported into it")
	}

	file := foreignFile("/taken/over.bin", 2048)
	stats, err := Import(ctx, db, c, &fakeSource{}, pinner,
		describeFiles(t, []transfer.File{file}),
		ImportOptions{CopyOptions: CopyOptions{Account: acc}, Target: importTarget(t, db, acc, share.Name)})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if stats.Pinned != 1 {
		t.Fatalf("stats: want the file pinned, got %+v", stats)
	}

	// The object is the destination's now, under the key the description named.
	want := sdk.NewUnsafeObject(file.Parts[0].Pin.DataKey, file.Parts[0].Pin.Slabs)
	backend.mu.Lock()
	_, held := backend.pinned[want.ID()]
	backend.mu.Unlock()
	if !held {
		t.Errorf("the destination did not take over the object %s", want.ID())
	}
}

// TestImportReportsAsItGoes verifies that an import says how far it has got
// while it is running, rather than only once it has finished.
func TestImportReportsAsItGoes(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	files := []transfer.File{foreignFile("/taken/one.bin", 128), foreignFile("/taken/two.bin", 256)}
	var reports []ImportStats
	stats, err := Import(ctx, db, c, &fakeSource{}, &fakePinner{},
		describeFiles(t, files, transfer.Directory{Path: "/taken"}),
		ImportOptions{
			CopyOptions: CopyOptions{Account: acc},
			Target:      importTarget(t, db, acc, share.Name),
			Report:      func(s ImportStats) { reports = append(reports, s) },
		})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// One report per folder and file, each with the totals as they stood then.
	if len(reports) != 3 {
		t.Fatalf("want a report for each of the 3 records, got %d", len(reports))
	}
	if reports[0].Directories != 1 || reports[0].Pinned != 0 {
		t.Errorf("after the folder: got %+v", reports[0])
	}
	if reports[1].Pinned != 1 || reports[2].Pinned != 2 {
		t.Errorf("after the files: got %+v and %+v", reports[1], reports[2])
	}
	if reports[len(reports)-1] != stats {
		t.Errorf("the last report is %+v, want what the import came to, %+v", reports[len(reports)-1], stats)
	}
}

// TestPinFileRewritesTheParts verifies what pinning makes of a description: the
// parts name the objects this account holds, with nothing left to pin.
func TestPinFileRewritesTheParts(t *testing.T) {
	file := transfer.File{
		Path: "/x.bin", Size: 3072,
		Parts: []transfer.Part{
			{Offset: 0, Length: 1024, Pin: &transfer.Pin{DataKey: [32]byte{1}, Slabs: []slabs.SlabSlice{foreignSlab(1, 1024)}}},
			{Offset: 1024, Length: 2048, Pin: &transfer.Pin{DataKey: [32]byte{2}, Slabs: []slabs.SlabSlice{foreignSlab(4, 2048)}}},
		},
	}

	pinner := &fakePinner{}
	pinned, err := PinFile(context.Background(), pinner, file)
	if err != nil {
		t.Fatalf("PinFile: %v", err)
	}
	if len(pinner.pinned) != 2 {
		t.Fatalf("want both objects pinned, got %v", pinner.pinned)
	}

	for i, part := range pinned.Parts {
		if part.Pin != nil {
			t.Errorf("part %d still says what it takes to pin it", i)
		}
		if part.Object != pinner.pinned[i] {
			t.Errorf("part %d names %s, want the pinned %s", i, part.Object, pinner.pinned[i])
		}
	}

	// A file is taken over whole: one refusal leaves the description as it was,
	// for whoever copies it instead.
	refusing := &fakePinner{err: errors.New("no")}
	if _, err := PinFile(context.Background(), refusing, file); err == nil {
		t.Error("a file was taken over although its objects were refused")
	}
}
