package stores

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// describe writes a description of the given folders and files, which is what
// the applier is given.
func describe(t *testing.T, dirs []transfer.Directory, files []transfer.File) *transfer.Reader {
	t.Helper()

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{CreatedAt: time.Now(), Source: "renterd", Share: "idx"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, dir := range dirs {
		if err := w.Directory(dir); err != nil {
			t.Fatalf("Directory(%q): %v", dir.Path, err)
		}
	}
	for _, f := range files {
		if err := w.File(f); err != nil {
			t.Fatalf("File(%q): %v", f.Path, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := transfer.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	return r
}

// applyTarget sets up a share, a workgroup and an account for the rows to
// belong to, and returns what applying a description needs.
func applyTarget(t *testing.T, db *Database) (TransferTarget, Account) {
	t.Helper()

	db.WithShares(&recordingShares{})
	if err := db.RegisterShare(Share{Name: "idx", Type: "indexd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}
	wg := addWorkgroup(t, db, "acme")
	acc := addAccount(t, db, wg, "alice", "secret")

	return TransferTarget{Share: "idx", Workgroup: wg.ID, Owner: acc}, acc
}

// TestApplyTransfer verifies what a description comes to in the store: the
// folders and files it names, read back the way a client would see them.
func TestApplyTransfer(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	target, acc := applyTarget(t, db)
	now := time.Now().UTC().Truncate(time.Second)
	key := types.Hash256{1}

	dirs := []transfer.Directory{{Path: "/holiday", Private: true, CreatedAt: now, ModifiedAt: now}}
	files := []transfer.File{
		{
			Path: "/holiday/beach.raw", Size: 300, CreatedAt: now, ModifiedAt: now,
			Parts: []transfer.Part{
				{Offset: 0, DataOffset: 0, Length: 100, Object: key},
				{Offset: 100, DataOffset: 100, Length: 200, Object: types.Hash256{2}},
			},
		},
		{
			// The folders above a file are made where the description does not
			// name them, which is what an import from a flat listing needs.
			Path: "/deep/down/notes.txt", Size: 11, CreatedAt: now, ModifiedAt: now,
			Parts: []transfer.Part{{Offset: 0, Length: 11, Inline: []byte("hello there")}},
		},
		{
			// A file whose bytes are only to be had from somewhere else is left
			// for whoever can get at them.
			Path: "/holiday/foreign.bin", Size: 50, CreatedAt: now, ModifiedAt: now,
			Parts: []transfer.Part{{Offset: 0, Length: 50, Source: &transfer.Source{Kind: "renterd", Bucket: "default", Key: "x"}}},
		},
		{
			// A file the source had not finished writing, applied with the hole
			// it has rather than left out.
			Path: "/holiday/draft.bin", Size: 100, CreatedAt: now, ModifiedAt: now,
			Parts: []transfer.Part{{Offset: 0, Length: 40, Object: types.Hash256{3}}},
		},
	}

	stats, err := db.ApplyTransfer(ctx, describe(t, dirs, files), target)
	if err != nil {
		t.Fatalf("ApplyTransfer: %v", err)
	}
	if stats.Files != 3 || stats.Unresolved != 1 || stats.Incomplete != 1 {
		t.Errorf("stats: want 3 files, 1 unresolved and 1 incomplete, got %+v", stats)
	}
	if stats.Directories != 1 {
		t.Errorf("stats: want the one folder that was named, got %d", stats.Directories)
	}

	// The files are there to be listed, at the size the description gave them.
	beach, err := db.Object(acc, target.Share, "/holiday/beach.raw")
	if err != nil {
		t.Fatalf("Object: %v", err)
	}
	if beach.Size != 300 || beach.IsDir {
		t.Errorf("the applied file: want a file of 300 bytes, got %+v", beach)
	}
	if !beach.ModifiedAt.Equal(now) {
		t.Errorf("the applied file kept the time %v, want %v", beach.ModifiedAt, now)
	}

	// The folders above a file that named none of them were made on the way.
	if _, err := db.Object(acc, target.Share, "/deep/down"); err != nil {
		t.Errorf("the folder above the applied file: %v", err)
	}

	// The file nobody could get at was not made at all.
	if _, err := db.Object(acc, target.Share, "/holiday/foreign.bin"); err == nil {
		t.Error("the file whose bytes are elsewhere was applied")
	}

	// What the file is made of is what a read of it goes by.
	slabs, err := db.GetMetadata(acc, target.Share, "/holiday/beach.raw", 0, 300)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(slabs) != 2 {
		t.Fatalf("the metadata of the applied file: want 2 slices, got %d", len(slabs))
	}
	if slabs[0].Key != key || slabs[0].Length != 100 || slabs[1].At != 100 {
		t.Errorf("the metadata of the applied file: got %+v", slabs)
	}

	// The bytes the description carried went in as buffered data, which the
	// packer uploads the same as anything a client wrote.
	buffered, err := db.BufferedBytes(target.Share, target.Workgroup)
	if err != nil {
		t.Fatalf("BufferedBytes: %v", err)
	}
	if buffered != 11 {
		t.Errorf("buffered bytes: want the 11 that were carried, got %d", buffered)
	}
}

// TestApplyTransferTwice verifies that applying a description again takes up
// where it left off instead of making a second copy of everything.
func TestApplyTransferTwice(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	target, acc := applyTarget(t, db)
	dirs := []transfer.Directory{{Path: "/holiday"}}
	files := []transfer.File{{
		Path: "/holiday/beach.raw", Size: 100,
		Parts: []transfer.Part{{Offset: 0, Length: 100, Object: types.Hash256{1}}},
	}}

	if _, err := db.ApplyTransfer(ctx, describe(t, dirs, files), target); err != nil {
		t.Fatalf("the first run: %v", err)
	}

	stats, err := db.ApplyTransfer(ctx, describe(t, dirs, files), target)
	if err != nil {
		t.Fatalf("the second run: %v", err)
	}
	if stats.Files != 0 || stats.Directories != 0 || stats.AlreadyThere != 2 {
		t.Errorf("the second run: want the folder and the file left alone, got %+v", stats)
	}

	// One file, of one set of metadata: the second run added neither.
	objects, err := db.ListObjects(acc, target.Share, "/holiday")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("the folder holds %d file(s), want 1", len(objects))
	}
	slabs, err := db.GetMetadata(acc, target.Share, "/holiday/beach.raw", 0, 100)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(slabs) != 1 {
		t.Errorf("the metadata of the file: want the one slice, got %d", len(slabs))
	}
}

// TestApplyFileResults verifies what applying one file reports, which is what
// an import goes by when it decides what is left to do.
func TestApplyFileResults(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	target, _ := applyTarget(t, db)
	file := transfer.File{
		Path: "/x.bin", Size: 10,
		Parts: []transfer.Part{{Offset: 0, Length: 10, Object: types.Hash256{1}}},
	}

	if res, err := db.ApplyFile(target, file); err != nil || res != Applied {
		t.Fatalf("the first time: %v %v", res, err)
	}
	if res, err := db.ApplyFile(target, file); err != nil || res != AlreadyThere {
		t.Fatalf("the second time: %v %v", res, err)
	}

	foreign := transfer.File{
		Path: "/y.bin", Size: 10,
		Parts: []transfer.Part{{Offset: 0, Length: 10, Pin: &transfer.Pin{}}},
	}
	if res, err := db.ApplyFile(target, foreign); err != nil || res != Unresolved {
		t.Fatalf("a file that has to be fetched first: %v %v", res, err)
	}

	// A description a reader could not act on is refused rather than applied in
	// part.
	broken := transfer.File{Path: "bad", Size: 1, Parts: []transfer.Part{{Length: 1, Object: types.Hash256{1}}}}
	if _, err := db.ApplyFile(target, broken); err == nil {
		t.Error("a file with a path that is not from the root was applied")
	}
}

// TestRecover verifies what comes of cutting the files found inside a file out of
// it: the finds are files, the file is what lies between them, and a file with
// nothing left is gone.
func TestRecover(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	target, acc := applyTarget(t, db)
	key := types.Hash256{7}
	lost := transfer.File{
		Path: "/lost+found/x.bin", Size: 100,
		Parts: []transfer.Part{{Length: 100, Object: key}},
	}
	if _, err := db.ApplyFile(target, lost); err != nil {
		t.Fatalf("ApplyFile: %v", err)
	}

	// Two files found at 10 and 60, leaving the bytes before, between and after.
	found := []transfer.File{
		{Path: "/lost+found/recovered/a.pdf", Size: 30, Parts: []transfer.Part{{DataOffset: 10, Length: 30, Object: key}}},
		{Path: "/lost+found/recovered/b.png", Size: 20, Parts: []transfer.Part{{DataOffset: 60, Length: 20, Object: key}}},
	}
	remainder := []transfer.Part{
		{DataOffset: 0, Length: 10, Object: key},
		{DataOffset: 40, Length: 20, Object: key},
		{DataOffset: 80, Length: 20, Object: key},
	}
	if err := db.Recover(target, lost.Path, found, remainder); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	runs, err := db.GetMetadata(acc, "idx", lost.Path, 0, 100)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("the file is made of %d run(s), want the 3 left over", len(runs))
	}
	for i, want := range remainder {
		if runs[i].Key != key || runs[i].Offset != want.DataOffset || runs[i].Length != want.Length {
			t.Errorf("run %d: got %+v, want %+v", i, runs[i], want)
		}
	}
	if runs[1].At != 10 || runs[2].At != 30 {
		t.Errorf("the runs do not follow one another in the file: at %d and %d", runs[1].At, runs[2].At)
	}
	if size := fileSize(t, db, acc, lost.Path); size != 50 {
		t.Errorf("the file measures %d, want the 50 left over", size)
	}

	// Found again, the files are left alone; found in full, the file goes.
	rest := transfer.File{Path: "/lost+found/recovered/c.gif", Size: 50, Parts: []transfer.Part{{Length: 50, Object: key}}}
	if err := db.Recover(target, lost.Path, append(found, rest), nil); err != nil {
		t.Fatalf("Recover in full: %v", err)
	}
	entries, err := db.ListObjects(acc, "idx", "/lost+found")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir {
			t.Errorf("%s is still there with nothing left in it", entry.Path)
		}
	}
	if recovered, _ := db.ListObjects(acc, "idx", "/lost+found/recovered"); len(recovered) != 3 {
		t.Errorf("the recovered folder holds %d file(s), want 3", len(recovered))
	}

	// Nothing found and nothing left would lose the slab; it is refused.
	if err := db.Recover(target, rest.Path, nil, nil); err == nil {
		t.Error("a file was cut to nothing with nothing found in it")
	}
}

// fileSize is what the file is said to measure.
func fileSize(t *testing.T, db *Database, acc Account, path string) uint64 {
	t.Helper()

	dir, _ := splitPath(path)
	entries, err := db.ListObjects(acc, "idx", dir)
	if err != nil {
		t.Fatalf("ListObjects(%s): %v", dir, err)
	}
	for _, entry := range entries {
		if entry.Path == path {
			return entry.Size
		}
	}
	t.Fatalf("%s is not there", path)

	return 0
}

// TestApplyToTheWrongShare verifies that a description goes only where its rows
// would be read: a renterd share is listed by renterd, not from here.
func TestApplyToTheWrongShare(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	target, _ := applyTarget(t, db)
	renterd := addShare(t, db, "proxied") // registered as a renterd share
	file := transfer.File{
		Path: "/x.bin", Size: 10,
		Parts: []transfer.Part{{Offset: 0, Length: 10, Object: types.Hash256{1}}},
	}

	target.Share = renterd.Name
	if _, err := db.ApplyFile(target, file); err == nil {
		t.Error("a file was applied to a renterd share")
	}
	if _, err := db.ApplyDirectory(target, transfer.Directory{Path: "/holiday"}); err == nil {
		t.Error("a folder was applied to a renterd share")
	}

	target.Share = "no-such-share"
	if _, err := db.ApplyFile(target, file); err == nil {
		t.Error("a file was applied to a share that is not there")
	}
}
