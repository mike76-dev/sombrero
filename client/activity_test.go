package client

import (
	"context"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
)

// act does the same round of work through any client: a folder, a file saved
// twice, moved into the folder and deleted, then the folder deleted.
func act(t *testing.T, c Client, acc stores.Account) {
	t.Helper()
	ctx := context.Background()
	if err := c.MakeDirectory(ctx, acc, "Plans"); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}
	for range 2 {
		id, err := c.StartUpload(ctx, acc, "report.txt")
		if err != nil {
			t.Fatalf("StartUpload: %v", err)
		}
		if err := c.FinishUpload(ctx, "report.txt", id, nil); err != nil {
			t.Fatalf("FinishUpload: %v", err)
		}
	}
	if err := c.Rename(ctx, acc, "report.txt", "Plans/report.txt", false, false); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := c.Delete(ctx, acc, "Plans/report.txt", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := c.Delete(ctx, acc, "Plans", true); err != nil {
		t.Fatalf("Delete of the folder: %v", err)
	}
}

// wantFeed fails the test unless the feed of the share holds the round of
// work act does, newest first, in the account's name; the two saves, moments
// apart, are one entry.
func wantFeed(t *testing.T, db *stores.Database, share string, acc stores.Account) {
	t.Helper()
	feed, err := db.Activity(share, acc, 0, 0)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	want := []stores.Activity{
		{Action: stores.ActivityRmdir, Path: "/Plans"},
		{Action: stores.ActivityDelete, Path: "/Plans/report.txt"},
		{Action: stores.ActivityRename, Path: "/report.txt", Target: "/Plans/report.txt"},
		{Action: stores.ActivityUpload, Path: "/report.txt"},
		{Action: stores.ActivityMkdir, Path: "/Plans"},
	}
	if len(feed) != len(want) {
		t.Fatalf("want %d entries, got %+v", len(want), feed)
	}
	for i, w := range want {
		got := feed[i]
		if got.Action != w.Action || got.Path != w.Path || got.Target != w.Target || got.Username != acc.Username || got.Workgroup != acc.Workgroup {
			t.Errorf("entry %d: want %s %s %s by %s, got %+v", i, w.Action, w.Path, w.Target, acc.Username, got)
		}
	}
}

// TestIndexdRecordsActivity verifies that an indexd client records what the
// account does for the feed.
func TestIndexdRecordsActivity(t *testing.T) {
	ctx := context.Background()
	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)
	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	act(t, c, acc)
	wantFeed(t, db, share.Name, acc)
}

// TestRenterdRecordsActivity verifies the same of a renterd client keeping versions.
func TestRenterdRecordsActivity(t *testing.T) {
	rv, _, acc := versionedRenterd(t, stores.VersionPolicy{Keep: true, Count: 10})
	act(t, rv, acc)
	wantFeed(t, rv.db, rv.share, acc)
}
