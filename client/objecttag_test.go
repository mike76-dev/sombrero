package client

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
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
