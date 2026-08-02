package main

import (
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// The indexes answer for the global open table, so they have to follow every way an open
// comes, goes, or changes its name. Each test here drives the real dispatcher and then asks
// the index the question the grant logic would ask.

func TestIntegrationRenameMovesTheOpenBetweenBuckets(t *testing.T) {
	h := newSMBTest(t)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create failed with %#x", status)
	}
	fid := createdFileID(held)

	if got := len(h.srv.opensOn(h.share, "dir/file", nil)); got != 1 {
		t.Fatalf("%d open(s) on the old name before the rename, want 1", got)
	}

	if _, err := alice.rename(fid, "dir/renamed"); err != nil {
		t.Fatalf("the rename failed: %v", err)
	}

	// A create on either name has to see the truth: the old name stands free, the new one is
	// taken.
	if got := len(h.srv.opensOn(h.share, "dir/file", nil)); got != 0 {
		t.Errorf("%d open(s) left on the old name, want none", got)
	}
	if got := len(h.srv.opensOn(h.share, "dir/renamed", nil)); got != 1 {
		t.Errorf("%d open(s) on the new name, want 1", got)
	}
}

// A rename of a file the backend already holds goes through the other branch of the handler,
// which used to leave the open pointing at the old name: reads on the handle reached for an
// object the backend no longer had, and the old name went on blocking creates while the new
// one stood unguarded.
func TestIntegrationBackendRenameMovesTheOpen(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create failed with %#x", status)
	}

	buf, err := alice.rename(createdFileID(held), "dir/renamed")
	if err != nil {
		t.Fatalf("the rename failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the rename was answered with %#x", status)
	}

	if got := len(h.srv.opensOn(h.share, "dir/file", nil)); got != 0 {
		t.Errorf("%d open(s) left on the old name, want none", got)
	}
	opens := h.srv.opensOn(h.share, "dir/renamed", nil)
	if len(opens) != 1 {
		t.Fatalf("%d open(s) on the new name, want 1", len(opens))
	}

	opens[0].mu.Lock()
	pathName := opens[0].pathName
	opens[0].mu.Unlock()
	if pathName != "dir/renamed" {
		t.Errorf("the open answers to %q, want dir/renamed", pathName)
	}
}

func TestIntegrationCloseAndRestoreKeepTheIndexRight(t *testing.T) {
	h := newSMBTest(t)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create failed with %#x", status)
	}

	if _, err := alice.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close failed: %v", err)
	}
	if got := len(h.srv.opensOn(h.share, "dir/file", nil)); got != 0 {
		t.Errorf("%d open(s) after the close, want none", got)
	}

	// Reopening a file created during the session restores the persisted open, and the index
	// entry with it.
	again, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	if status := smb2.Header(again).Status(); status != smb2.STATUS_OK {
		t.Fatalf("reopening failed with %#x", status)
	}
	if got := len(h.srv.opensOn(h.share, "dir/file", nil)); got != 1 {
		t.Errorf("%d open(s) after the restore, want 1", got)
	}
}

func TestIntegrationSweepClearsTheIndexes(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.createDurable("dir/file", testCreateGuid, false)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the durable create failed with %#x", status)
	}

	guid := [16]byte(alice.conn.clientGuid)
	op := h.srv.findReplayableOpen(guid, testCreateGuid)
	if op == nil {
		t.Fatal("the durable create was not filed as replayable")
	}
	if got := len(h.srv.opensOn(h.share, "dir/file", nil)); got != 1 {
		t.Fatalf("%d open(s) on the file, want 1", got)
	}

	// The connection is lost, and the open waits out its whole grant with nobody coming back.
	if n := alice.ss.orphanDurableOpens(); n != 1 {
		t.Fatalf("%d opens were set aside, want 1", n)
	}
	op.mu.Lock()
	op.disconnectTime = time.Now().Add(-op.durableTimeout - time.Second)
	op.mu.Unlock()

	h.srv.sweepDurableOpens()

	if h.srv.findReplayableOpen(guid, testCreateGuid) != nil {
		t.Error("a swept open can still answer for a replay")
	}
	if got := len(h.srv.opensOn(h.share, "dir/file", nil)); got != 0 {
		t.Errorf("%d open(s) left on the file after the sweep, want none", got)
	}
}
