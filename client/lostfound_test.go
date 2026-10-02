package client

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
)

// TestSortLostAndFound verifies what comes of looking inside an object nobody
// can name: the files that were packed into it, as runs of the object itself.
func TestSortLostAndFound(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	// An object as an import of an untagged account leaves one: a slab's worth
	// of whatever was packed into it, with two files in the middle of it.
	one, two := pdf(4096), png(8192)
	object := make([]byte, proto.SectorSize)
	copy(object[1000:], one)
	copy(object[1000+len(one):], two)

	const lost = "/lost+found/0f1e2d3c.bin"
	if err := c.MakeDirectory(ctx, acc, LostAndFound); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	uploadID, err := c.StartUpload(ctx, acc, lost)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(object), lost, uploadID, 1, 0, uint64(len(object))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.FinishUpload(ctx, lost, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	waitForRead(t, ctx, c, acc, lost, object)
	key := awaitSlab(t, db, acc, c, lost)

	ic := c.(*IndexdClient)
	report, err := ic.SortLostAndFound(ctx, acc, LostAndFound, "", 0)
	if err != nil {
		t.Fatalf("SortLostAndFound: %v", err)
	}
	if report.Objects != 1 || report.Recovered != 2 {
		t.Fatalf("the report: want 2 files out of the one object, got %+v", report)
	}
	if report.Bytes != uint64(len(one)+len(two)) {
		t.Errorf("the recovered bytes: want %d, got %d", len(one)+len(two), report.Bytes)
	}
	if report.Leftover != proto.SectorSize-uint64(len(one)+len(two)) {
		t.Errorf("the leftover bytes: got %d", report.Leftover)
	}

	// Each file is there to be read, and reads back as what was packed into the
	// object: the data was never moved, only named.
	for _, want := range []struct {
		at   int
		ext  string
		data []byte
	}{
		{1000, "pdf", one},
		{1000 + len(one), "png", two},
	} {
		path := fmt.Sprintf("/lost+found/%s/%s-%d.%s", RecoveredFolder, key.String()[:8], want.at, want.ext)
		info, err := c.Object(ctx, acc, path)
		if err != nil {
			t.Fatalf("the recovered file %s: %v", path, err)
		}
		if info.Size != uint64(len(want.data)) {
			t.Errorf("%s measures %d, want %d", path, info.Size, len(want.data))
		}

		var got bytes.Buffer
		if err := c.Read(ctx, acc, path, 0, info.Size, &got); err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !bytes.Equal(got.Bytes(), want.data) {
			t.Errorf("%s does not read back as what was packed into the object", path)
		}
	}

	// Sorting the same objects again comes to the same files, and leaves them
	// as they are.
	again, err := ic.SortLostAndFound(ctx, acc, LostAndFound, "", 0)
	if err != nil {
		t.Fatalf("the second sort: %v", err)
	}
	if again.Recovered != 2 {
		t.Errorf("the second sort found %d file(s), want the same 2", again.Recovered)
	}

	entries, err := db.ListObjects(acc, share.Name, "/lost+found/"+RecoveredFolder)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("the recovered folder holds %d file(s), want 2", len(entries))
	}
}

// TestSortLostAndFoundGoesInRounds verifies that a sort of more objects than a
// round looks at says where it stopped, since each object is downloaded whole.
func TestSortLostAndFoundGoesInRounds(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	if err := c.MakeDirectory(ctx, acc, LostAndFound); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	// Three objects, each holding one file of its own.
	for i := range 3 {
		object := make([]byte, proto.SectorSize)
		copy(object[16:], pdf(2048))

		name := fmt.Sprintf("/lost+found/%02d.bin", i)
		uploadID, err := c.StartUpload(ctx, acc, name)
		if err != nil {
			t.Fatalf("StartUpload(%s): %v", name, err)
		}
		if _, err := c.Write(ctx, bytes.NewReader(object), name, uploadID, 1, 0, uint64(len(object))); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
		if err := c.FinishUpload(ctx, name, uploadID, nil); err != nil {
			t.Fatalf("FinishUpload(%s): %v", name, err)
		}
		waitForRead(t, ctx, c, acc, name, object)
		awaitSlab(t, db, acc, c, name)
	}

	ic := c.(*IndexdClient)

	// A round of two leaves one for the next, and says where to take it up.
	first, err := ic.SortLostAndFound(ctx, acc, LostAndFound, "", 2)
	if err != nil {
		t.Fatalf("the first round: %v", err)
	}
	if first.Objects != 2 || first.Recovered != 2 || !first.More {
		t.Fatalf("the first round: want 2 objects and more to come, got %+v", first)
	}
	if first.Last != "/lost+found/01.bin" {
		t.Errorf("the first round stopped at %q, want the second object", first.Last)
	}

	second, err := ic.SortLostAndFound(ctx, acc, LostAndFound, first.Last, 2)
	if err != nil {
		t.Fatalf("the second round: %v", err)
	}
	if second.Objects != 1 || second.Recovered != 1 || second.More {
		t.Errorf("the second round: want the last object and nothing after it, got %+v", second)
	}
}

// TestSortLostAndFoundLeavesWhatItCannotRead verifies that the sorter looks only
// inside what it is meant to: one whole object of this account's, and nothing
// that is still waiting in the database.
func TestSortLostAndFoundLeavesWhatItCannotRead(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	backend := newFakeBackend()
	c := newIndexdClient(db, backend, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	// A small file stays buffered with nothing to pack it with, so there is no
	// object to look inside yet.
	const waiting = "/lost+found/waiting.bin"
	if err := c.MakeDirectory(ctx, acc, LostAndFound); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}

	uploadID, err := c.StartUpload(ctx, acc, waiting)
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(pdf(2048)), waiting, uploadID, 1, 0, 2048); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.FinishUpload(ctx, waiting, uploadID, nil); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}

	report, err := c.(*IndexdClient).SortLostAndFound(ctx, acc, LostAndFound, "", 0)
	if err != nil {
		t.Fatalf("SortLostAndFound: %v", err)
	}
	if report.Objects != 0 || report.Recovered != 0 || report.Skipped != 1 {
		t.Errorf("the report: want the buffered file left alone, got %+v", report)
	}
}
