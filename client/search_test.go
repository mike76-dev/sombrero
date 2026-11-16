package client

import (
	"context"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
)

// keysOf lists the keys of the objects.
func keysOf(ois []ObjectInfo) []string {
	keys := make([]string, 0, len(ois))
	for _, oi := range ois {
		keys = append(keys, oi.Key)
	}
	return keys
}

// sameKeys fails the test unless the objects are those with the keys, in order.
func sameKeys(t *testing.T, what string, ois []ObjectInfo, want ...string) {
	t.Helper()
	got := keysOf(ois)
	if len(got) != len(want) {
		t.Fatalf("%s: want %v, got %v", what, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: want %v, got %v", what, want, got)
			return
		}
	}
}

// TestIndexdSearch verifies that an indexd client finds files and folders by
// a part of their names, whatever the case.
func TestIndexdSearch(t *testing.T) {
	ctx := context.Background()
	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)
	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)
	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	if err := c.MakeDirectory(ctx, acc, "Reports"); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}
	for _, p := range []string{"Reports/Q3-report.txt", "notes.txt"} {
		id, err := c.StartUpload(ctx, acc, p)
		if err != nil {
			t.Fatalf("StartUpload: %v", err)
		}
		if err := c.FinishUpload(ctx, p, id, nil); err != nil {
			t.Fatalf("FinishUpload: %v", err)
		}
	}

	ois, err := c.(Searcher).Search(ctx, acc, "REPORT")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	sameKeys(t, "a search", ois, "/Reports/", "/Reports/Q3-report.txt")
	if ois, err := c.(Searcher).Search(ctx, acc, "nothing"); err != nil || len(ois) != 0 {
		t.Errorf("no match: want nothing, got %v, %v", keysOf(ois), err)
	}
}

// TestRenterdSearch verifies the same of a renterd client, which walks the
// bucket, and that the server's folder stays out of the results.
func TestRenterdSearch(t *testing.T) {
	rv, bucket, acc := versionedRenterd(t, stores.VersionPolicy{Keep: true, Count: 10})
	ctx := context.Background()
	bucket.put("/Reports/Q3-report.txt", 400, time.Now())
	bucket.put("/Reports/", 0, time.Now())
	bucket.put("/notes.txt", 10, time.Now())
	bucket.put("/"+renterdVersionsFolder+"/report-copy", 400, time.Now())

	ois, err := rv.Search(ctx, acc, "report")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	sameKeys(t, "a search", ois, "/Reports/", "/Reports/Q3-report.txt")
	if ois, err := rv.Search(ctx, acc, " "); err != nil || len(ois) != 0 {
		t.Errorf("no text: want nothing, got %v, %v", keysOf(ois), err)
	}
}
