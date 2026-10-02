package client

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
)

// lastTag returns what the object uploaded last was told to say about itself.
func lastTag(t *testing.T, fb *fakeBackend) objectTag {
	t.Helper()

	fb.mu.Lock()
	defer fb.mu.Unlock()

	if len(fb.tags) == 0 {
		t.Fatal("nothing was uploaded")
	}
	tag, ok := parseTag(fb.tags[len(fb.tags)-1])
	if !ok {
		t.Fatalf("the object says nothing this server understands: %s", fb.tags[len(fb.tags)-1])
	}

	return tag
}

// TestUploadTagsTheObject verifies that an object goes out saying which runs of
// which files are in it, which is what makes an account able to name them later.
func TestUploadTagsTheObject(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	// A slab's worth of data goes out as an object of its own.
	content := bytes.Repeat([]byte("s"), int(proto.SectorSize))
	uploadID, err := c.StartUpload(ctx, acc, "/big.bin")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), "/big.bin", uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.FinishUpload(ctx, "/big.bin", uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	waitForRead(t, ctx, c, acc, "/big.bin", content)

	tag := lastTag(t, backend)
	if len(tag.Pieces) != 1 {
		t.Fatalf("the object says it holds %d run(s), want 1", len(tag.Pieces))
	}

	piece := tag.Pieces[0]
	if piece.Share != share.Name || piece.Path != "/big.bin" {
		t.Errorf("the run names %q of %q", piece.Path, piece.Share)
	}
	if piece.Offset != 0 || piece.At != 0 || piece.Length != uint64(len(content)) {
		t.Errorf("the run is %+v, want the whole file at the start of the object", piece)
	}
}

// awaitRetag waits for the worker to tell the object what it holds, since a
// client waiting on a rename does not wait on the indexer.
func awaitRetag(t *testing.T, fb *fakeBackend, key types.Hash256, want func(objectTag) bool) objectTag {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if meta, ok := fb.retagged(key); ok {
			if tag, ok := parseTag(meta); ok && want(tag) {
				return tag
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	meta, _ := fb.retagged(key)
	t.Fatalf("the object was not told what it holds; the last it was told was %s", meta)

	return objectTag{}
}

// TestRetagFollowsARename verifies that a file which moved is still the file its
// object names, rather than one that is no longer anywhere.
func TestRetagFollowsARename(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	content := bytes.Repeat([]byte("s"), int(proto.SectorSize))
	uploadID, err := c.StartUpload(ctx, acc, "/before.bin")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(content), "/before.bin", uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.FinishUpload(ctx, "/before.bin", uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	waitForRead(t, ctx, c, acc, "/before.bin", content)

	key := awaitSlab(t, db, acc, c, "/before.bin")
	if err := c.Rename(ctx, acc, "/before.bin", "/after.bin", false, false); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	tag := awaitRetag(t, backend, key, func(tag objectTag) bool {
		return len(tag.Pieces) == 1 && tag.Pieces[0].Path == "/after.bin"
	})
	if tag.Pieces[0].Length != uint64(len(content)) {
		t.Errorf("the run after the rename: got %+v", tag.Pieces[0])
	}
}

// TestRetagFollowsADelete verifies that a file deleted out of a slab it shared
// leaves an object that no longer claims to hold it.
func TestRetagFollowsADelete(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	// Nothing packs on age here, so two halves of a slab are packed when the
	// second of them makes a slab's worth: one object, two files.
	half := int(proto.SectorSize / 2)
	for _, name := range []string{"/one.txt", "/two.txt"} {
		content := bytes.Repeat([]byte(name), half/len(name))
		uploadID, err := c.StartUpload(ctx, acc, name)
		if err != nil {
			t.Fatalf("StartUpload(%s): %v", name, err)
		}
		if _, err := c.Write(ctx, bytes.NewReader(content), name, uploadID, 1, 0, uint64(len(content))); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
		if err := c.FinishUpload(ctx, name, uploadID, nil); err != nil {
			t.Fatalf("FinishUpload(%s): %v", name, err)
		}
		waitForRead(t, ctx, c, acc, name, content)
	}

	key := awaitSlab(t, db, acc, c, "/one.txt")
	if other := awaitSlab(t, db, acc, c, "/two.txt"); other != key {
		t.Fatalf("the two files went into different slabs, %s and %s", key, other)
	}

	if err := c.Delete(ctx, acc, "/one.txt", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The slab is still the other file's, so it stays pinned and says so.
	tag := awaitRetag(t, backend, key, func(tag objectTag) bool {
		return len(tag.Pieces) == 1
	})
	if tag.Pieces[0].Path != "/two.txt" {
		t.Errorf("the run left in the object: got %+v", tag.Pieces[0])
	}
}

// awaitSlab waits until the file is made of one slab on the network, rather than
// of bytes still waiting in the database, and returns its key. A file is
// readable long before that, so what it reads back says nothing about this.
func awaitSlab(t *testing.T, db *stores.Database, acc stores.Account, c Client, path string) types.Hash256 {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		info, err := c.Object(context.Background(), acc, path)
		if err != nil {
			t.Fatalf("Object(%s): %v", path, err)
		}
		slices, err := db.GetMetadata(acc, "testshare", path, 0, info.Size)
		if err != nil {
			t.Fatalf("GetMetadata(%s): %v", path, err)
		}
		if len(slices) == 1 && slices[0].Key != (types.Hash256{}) {
			return slices[0].Key
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("the file %s was not uploaded as one slab", path)

	return types.Hash256{}
}

// TestTagPiecesLaysOutThePackedSlab verifies what a packed object says of itself:
// one run per piece, at the offset the piece was packed to.
func TestTagPiecesLaysOutThePackedSlab(t *testing.T) {
	jobs := []stores.UploadJob{
		{MetadataID: 1, ObjOffset: 0, Data: []byte("hello there")},
		{MetadataID: 2, ObjOffset: 4096, Data: []byte("second")},
		{MetadataID: 3, ObjOffset: 0, Data: []byte("third")},
	}
	owners := map[uint64]stores.PieceOwner{
		1: {Share: "s", Path: "/one.txt", Size: 11},
		2: {Share: "s", Path: "/big.bin", Size: 4102},
		// The file of the third piece was deleted while it waited, so there is
		// nobody to name it.
	}

	tag, ok := parseTag(tagPieces(jobs, owners))
	if !ok {
		t.Fatal("the tag says nothing this server understands")
	}
	if len(tag.Pieces) != 2 {
		t.Fatalf("the tag holds %d run(s), want the 2 that could be named", len(tag.Pieces))
	}

	if got := tag.Pieces[0]; got.Path != "/one.txt" || got.At != 0 || got.Length != 11 {
		t.Errorf("the first run: got %+v", got)
	}

	// The second piece starts where the first one ended, even though the piece
	// between them could not be named: the object is laid out as it was packed.
	if got := tag.Pieces[1]; got.Path != "/big.bin" || got.At != 11 || got.Offset != 4096 || got.Length != 6 {
		t.Errorf("the second run: got %+v", got)
	}
}

// TestParseTag verifies what is taken for a tag and what is not, since an object
// may carry anything at all, or nothing.
func TestParseTag(t *testing.T) {
	for _, meta := range []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`not json`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"sombrero":1}`), // nothing in it
		json.RawMessage(`{"sombrero":99,"pieces":[{"at":0}]}`), // written by a newer server
		json.RawMessage(`{"pieces":[{"at":0}]}`),               // somebody else's metadata
	} {
		if _, ok := parseTag(meta); ok {
			t.Errorf("%q was taken for a tag", meta)
		}
	}

	good := json.RawMessage(`{"sombrero":1,"pieces":[{"share":"s","path":"/x","offset":0,"at":0,"length":4}]}`)
	tag, ok := parseTag(good)
	if !ok {
		t.Fatalf("%q was not taken for a tag", good)
	}
	if len(tag.Pieces) != 1 || tag.Pieces[0].Path != "/x" {
		t.Errorf("the tag reads as %+v", tag)
	}
}
