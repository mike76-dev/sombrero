package client

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	proto "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/api/app"
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
	for _, name := range []string{"/one.txt", "/two.txt"} {
		content := halfSlab(name)
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

// TestRetagFollowsAnOverwrite verifies that a file written over the one that was
// there leaves no object claiming to hold what the old one had.
func TestRetagFollowsAnOverwrite(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	// Two halves of a slab from two files, so the slab outlives either of them.
	for _, name := range []string{"/kept.txt", "/replaced.txt"} {
		content := halfSlab(name)
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

	key := awaitSlab(t, db, acc, c, "/kept.txt")
	if other := awaitSlab(t, db, acc, c, "/replaced.txt"); other != key {
		t.Fatalf("the two files went into different slabs, %s and %s", key, other)
	}

	// The second file is written again, which is a new object of its own and
	// leaves the old slab holding one run fewer.
	again := []byte("a shorter file altogether")
	uploadID, err := c.StartUpload(ctx, acc, "/replaced.txt")
	if err != nil {
		t.Fatalf("StartUpload again: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(again), "/replaced.txt", uploadID, 1, 0, uint64(len(again))); err != nil {
		t.Fatalf("Write again: %v", err)
	}
	if err := c.FinishUpload(ctx, "/replaced.txt", uploadID, nil); err != nil {
		t.Fatalf("FinishUpload again: %v", err)
	}
	waitForRead(t, ctx, c, acc, "/replaced.txt", again)

	tag := awaitRetag(t, backend, key, func(tag objectTag) bool {
		return len(tag.Pieces) == 1
	})
	if tag.Pieces[0].Path != "/kept.txt" {
		t.Errorf("the run left in the object: got %+v", tag.Pieces[0])
	}
}

// TestRetagIsTriedAgain verifies that a backend which would not take a tag is
// waited out, rather than leaving the object saying the wrong thing for good.
func TestRetagIsTriedAgain(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	backend.mu.Lock()
	backend.retagErr = errors.New("the indexer is not answering")
	backend.mu.Unlock()

	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	ic := c.(*IndexdClient)
	ic.slabRetryDelays = []time.Duration{time.Millisecond}
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

	// The slab stays on the list for as long as the backend refuses it.
	waitFor(t, 5*time.Second, func() bool {
		ic.mu.Lock()
		defer ic.mu.Unlock()
		_, waiting := ic.retagging[key]

		return waiting
	}, "the slab that could not be retagged was dropped")

	// Once it answers, the tag is written without anything else happening.
	backend.mu.Lock()
	backend.retagErr = nil
	backend.mu.Unlock()

	awaitRetag(t, backend, key, func(tag objectTag) bool {
		return len(tag.Pieces) == 1 && tag.Pieces[0].Path == "/after.bin"
	})
}

// TestRetagIsNotForcedOnTheIndexer verifies that a tag the indexer turns down is
// not asked about again and again: an answer is not a failure to answer.
func TestRetagIsNotForcedOnTheIndexer(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	backend.mu.Lock()
	backend.retagErr = &app.HTTPError{StatusCode: http.StatusBadRequest, Body: "object metadata size limit (1024) exceeded"}
	backend.mu.Unlock()

	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	ic := c.(*IndexdClient)
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

	// The indexer is asked once, and the slab is then off the list for good.
	waitFor(t, 5*time.Second, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()

		return backend.retagAttempts >= 1
	}, "the indexer was never asked")
	waitFor(t, 5*time.Second, func() bool {
		ic.mu.Lock()
		defer ic.mu.Unlock()
		_, waiting := ic.retagging[key]

		return !waiting
	}, "the slab the indexer refused is still waiting to be retagged")

	time.Sleep(3 * retryDelay)
	backend.mu.Lock()
	attempts := backend.retagAttempts
	backend.mu.Unlock()
	if attempts != 1 {
		t.Errorf("the indexer was asked %d times, want once", attempts)
	}
}

// TestSilentSlabsAreTaggedOnStart verifies that a slab an older server uploaded
// without a tag is told what it holds once a server that tags starts up, with
// nothing having to touch the file.
func TestSilentSlabsAreTaggedOnStart(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	// The fake's objects carry no metadata when asked after, which is what a
	// slab written before tagging looks like.
	backend := newFakeBackend()
	old := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)

	content := bytes.Repeat([]byte("o"), int(proto.SectorSize))
	uploadID, err := old.StartUpload(ctx, acc, "/old.bin")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := old.Write(ctx, bytes.NewReader(content), "/old.bin", uploadID, 1, 0, uint64(len(content))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := old.FinishUpload(ctx, "/old.bin", uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	waitForRead(t, ctx, old, acc, "/old.bin", content)
	key := awaitSlab(t, db, acc, old, "/old.bin")
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, tagged := backend.retagged(key); tagged {
		t.Fatal("the slab was retagged before the restart")
	}

	// The next server to start finds the slab saying nothing, and tells it.
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	awaitRetag(t, backend, key, func(tag objectTag) bool {
		return len(tag.Pieces) == 1 && tag.Pieces[0].Path == "/old.bin" && tag.Pieces[0].Share == share.Name
	})
}

// TestTagKeepsTheCatalog verifies that a slab packed with more files than its tag
// can name still names the catalog in it, wherever that was packed, since the
// catalog is what a rescue of the account starts from.
func TestTagKeepsTheCatalog(t *testing.T) {
	tag := manyPieces(400)
	last := tag.Pieces[len(tag.Pieces)-1]
	tag.Pieces[len(tag.Pieces)-1] = objectPiece{
		Share: "shared", Path: transfer.CatalogFolder + "/20261005T000000.000000000Z.catalog",
		At: last.At, Length: last.Length, Size: last.Length,
	}

	got, ok := parseTag(tag.encode())
	if !ok || got.Omitted == 0 {
		t.Fatalf("the tag holds everything, so nothing is being tested: %+v", got)
	}
	if !isCatalog(got.Pieces[0]) || got.Pieces[0].At != last.At {
		t.Errorf("the catalog is not the first piece named: %+v", got.Pieces[0])
	}
	if tag.Pieces[0].Path == got.Pieces[0].Path {
		t.Error("the caller's pieces were reordered")
	}
}

// TestWantsTag verifies which slabs a server tags when it starts: the silent
// ones and the ones tagged by an older server, not the ones it tagged itself.
func TestWantsTag(t *testing.T) {
	old, err := json.Marshal(objectTag{Version: tagJSON, Pieces: manyPieces(2).Pieces})
	if err != nil {
		t.Fatal(err)
	}
	for _, meta := range []json.RawMessage{nil, json.RawMessage(`{"theirs":true}`), old} {
		if !wantsTag(meta) {
			t.Errorf("%q would be left as it is", meta)
		}
	}
	if wantsTag(manyPieces(2).pack(2)) {
		t.Error("a tag in the current layout would be rewritten")
	}
}

// TestTagFitsTheIndexer verifies that an object packed with more files than its
// metadata has room for names the first of them and counts the rest, since the
// indexer takes a tag of a kilobyte or nothing.
func TestTagFitsTheIndexer(t *testing.T) {
	// Files named by a camera pack so well that hundreds fit; these are named
	// by a person, which is what fills a tag.
	const many = 300
	r := rand.New(rand.NewSource(1))
	jobs := make([]stores.UploadJob, 0, many)
	owners := make(map[uint64]stores.PieceOwner, many)
	for i := range many {
		name := make([]byte, 8+r.Intn(16))
		for j := range name {
			name[j] = byte('a' + r.Intn(26))
		}
		id := uint64(i + 1)
		jobs = append(jobs, stores.UploadJob{MetadataID: id, Data: bytes.Repeat([]byte{'x'}, 1000)})
		owners[id] = stores.PieceOwner{Share: "shared", Path: fmt.Sprintf("/documents/%s.pdf", name), Size: 1000}
	}

	meta := tagPieces(jobs, owners)
	if len(meta) > maxTagSize {
		t.Fatalf("the tag encodes to %d bytes, more than the %d the indexer takes", len(meta), maxTagSize)
	}

	tag, ok := parseTag(meta)
	if !ok {
		t.Fatal("the tag says nothing this server understands")
	}
	if len(tag.Pieces) == 0 || tag.Omitted == 0 || len(tag.Pieces)+tag.Omitted != many {
		t.Fatalf("the tag names %d piece(s) and counts %d left out, want %d between them", len(tag.Pieces), tag.Omitted, many)
	}
	for i, piece := range tag.Pieces {
		if piece.Path != owners[uint64(i+1)].Path || piece.At != uint64(i*1000) {
			t.Errorf("piece %d is %+v, want the %dth file in object order", i, piece, i)
		}
	}

	// One piece more would not have fit.
	full := objectTag{Version: tagBinary, Pieces: make([]objectPiece, 0, many)}
	for i := range many {
		full.Pieces = append(full.Pieces, objectPiece{Share: "shared", Path: owners[uint64(i+1)].Path, At: uint64(i * 1000), Length: 1000, Size: 1000})
	}
	if more := full.pack(len(tag.Pieces) + 1); len(more) <= maxTagSize {
		t.Errorf("a tag of %d pieces encodes to %d bytes and would have fit", len(tag.Pieces)+1, len(more))
	}

	// Even so, the kilobyte holds a good few of them.
	if len(tag.Pieces) < 25 {
		t.Errorf("the tag names %d pieces, want at least 25 in a kilobyte", len(tag.Pieces))
	}

	// A tag that fits is left whole, and says nothing was left out.
	few, ok := parseTag(tagPieces(jobs[:3], owners))
	if !ok || len(few.Pieces) != 3 || few.Omitted != 0 {
		t.Errorf("a tag of 3 pieces reads as %+v", few)
	}
}

// halfSlab is exactly half a slab of the name's own bytes. Two of them fill one
// slab, which is what has the packer put both files in one object; a pair that
// came to a byte less than a slab would never be packed at all.
func halfSlab(name string) []byte {
	content := make([]byte, proto.SectorSize/2)
	for i := range content {
		content[i] = name[i%len(name)]
	}

	return content
}

// waitFor waits for the condition to hold, failing with the given complaint if
// it does not.
func waitFor(t *testing.T, within time.Duration, cond func() bool, complaint string) {
	t.Helper()

	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal(complaint)
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

// manyPieces is a tag of this many pieces across two shares, with the odd offsets
// and sizes a real packed slab has.
func manyPieces(n int) objectTag {
	tag := objectTag{Version: tagBinary}
	var at uint64
	for i := range n {
		share := "shared"
		if i%7 == 0 {
			share = "other"
		}
		length := uint64(1000 + i*37)
		tag.Pieces = append(tag.Pieces, objectPiece{
			Share:  share,
			Path:   fmt.Sprintf("/photos/%d/IMG_%04d.jpg", 2020+i%5, i),
			Offset: uint64(i%3) << 22,
			At:     at,
			Length: length,
			Size:   length + uint64(i%3)<<22,
		})
		at += length
	}

	return tag
}

// TestTagRoundTrip verifies that a tag reads back as what was written, in the
// plain layout and in the compressed one, and that version 1 is still read.
func TestTagRoundTrip(t *testing.T) {
	for _, n := range []int{1, 3, 50, 400} {
		want := manyPieces(n)
		meta := want.pack(n)
		got, ok := parseTag(meta)
		if !ok {
			t.Fatalf("a tag of %d piece(s) was not taken for one", n)
		}
		if got.Omitted != 0 || !slices.Equal(got.Pieces, want.Pieces) {
			t.Errorf("a tag of %d piece(s) read back as %+v", n, got)
		}
		if n < 3 && meta[1] != 0 || n >= 50 && meta[1] != tagCompressed {
			t.Errorf("a tag of %d piece(s) was written with flags %d", n, meta[1])
		}
	}

	// What was cut counts the rest.
	cut, ok := parseTag(manyPieces(100).pack(40))
	if !ok || len(cut.Pieces) != 40 || cut.Omitted != 60 {
		t.Errorf("a tag cut to 40 of 100 reads as %d piece(s) and %d omitted", len(cut.Pieces), cut.Omitted)
	}

	old, err := json.Marshal(objectTag{Version: tagJSON, Pieces: manyPieces(5).Pieces})
	if err != nil {
		t.Fatal(err)
	}
	if tag, ok := parseTag(old); !ok || !slices.Equal(tag.Pieces, manyPieces(5).Pieces) {
		t.Errorf("a version-1 tag reads as %+v", tag)
	}
}

// TestParseTag verifies what is taken for a tag and what is not, since an object
// may carry anything at all, or nothing, and a tag from another account may be
// made to hurt.
func TestParseTag(t *testing.T) {
	flipped := manyPieces(20).pack(20)
	flipped[len(flipped)/2] ^= 0x01

	var bomb bytes.Buffer
	bomb.Write([]byte{tagBinary, tagCompressed})
	w, _ := flate.NewWriter(&bomb, flate.BestCompression)
	_, _ = w.Write(make([]byte, 4<<20))
	_ = w.Close()

	// A tag claiming more pieces than anyone has, with nothing behind the claim.
	claim := binary.AppendUvarint([]byte{tagBinary, 0}, 0)
	claim = binary.AppendUvarint(claim, 0)
	claim = binary.AppendUvarint(claim, 1<<40)
	claim = binary.BigEndian.AppendUint32(claim, crc32.ChecksumIEEE(claim[2:]))

	for _, meta := range []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`not json`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"sombrero":1}`), // nothing in it
		json.RawMessage(`{"sombrero":99,"pieces":[{"at":0}]}`), // written by a newer server
		json.RawMessage(`{"pieces":[{"at":0}]}`),               // somebody else's metadata
		{tagBinary},
		{tagBinary, 0},
		{tagBinary, 0, 1, 2, 3, 4},
		flipped,
		bomb.Bytes(),
		claim,
	} {
		if _, ok := parseTag(meta); ok {
			t.Errorf("%q was taken for a tag", meta[:min(16, len(meta))])
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

// FuzzParseTag verifies that nothing an object may carry makes the reader fall
// over: a tag is taken or it is not.
func FuzzParseTag(f *testing.F) {
	f.Add([]byte(manyPieces(3).pack(3)))
	f.Add([]byte(manyPieces(200).pack(200)))
	f.Add([]byte(`{"sombrero":1,"pieces":[{"share":"s","path":"/x","offset":0,"at":0,"length":4}]}`))
	f.Fuzz(func(t *testing.T, meta []byte) {
		if tag, ok := parseTag(meta); ok && len(tag.Pieces) == 0 {
			t.Error("a tag of nothing was taken")
		}
	})
}
