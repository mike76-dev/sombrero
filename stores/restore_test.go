package stores

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mike76-dev/sombrero/transfer"
)

// TestRestore verifies that a connection comes back from its catalog as it was:
// the share, the workgroup, its accounts and what they may do, the app key, and
// every folder and file to the account that owned it.
func TestRestore(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	fx := plantCatalogFixture(t, db)
	var catalog bytes.Buffer
	if _, err := db.Snapshot(&catalog, fx.share.Name, fx.target.Workgroup, 1000); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Everything the catalog describes goes, as if the database were lost.
	if err := db.UnregisterShare(fx.share.Name); err != nil {
		t.Fatalf("UnregisterShare: %v", err)
	}
	if err := db.RemoveWorkgroup(fx.wg); err != nil {
		t.Fatalf("RemoveWorkgroup: %v", err)
	}
	if wg, _ := db.FindWorkgroup(fx.wg.UUID); wg.ID != 0 {
		t.Fatal("the workgroup is still there")
	}

	r, err := transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	stats, err := db.Restore(ctx, r, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if stats.Share != fx.share.Name || stats.Workgroup != fx.wg.UUID || stats.Accounts != 2 || stats.Policies != 1 {
		t.Errorf("stats: got %+v", stats)
	}
	if stats.Directories != 1 || stats.Files != 5 || stats.Incomplete != 1 || stats.AlreadyThere != 0 {
		t.Errorf("the rows: got %+v", stats.ApplyStats)
	}

	// The share, the workgroup with its public folder, and the connection with
	// its key.
	share, err := db.GetShare(fx.share.Name)
	if err != nil || share.Type != "indexd" || share.ServerName != fx.share.ServerName {
		t.Errorf("the share: got %+v, %v", share, err)
	}
	wg, err := db.FindWorkgroup(fx.wg.UUID)
	if err != nil || wg.Name != fx.wg.Name || len(wg.PublicDirs) != 1 || !wg.PublicDirs[0].ReadOnly {
		t.Errorf("the workgroup: got %+v, %v", wg, err)
	}
	connected, key, err := db.IsConnected(wg, share)
	if err != nil || !connected || !bytes.Equal(key, fx.key) {
		t.Errorf("the connection: connected %v, key %x, %v", connected, key, err)
	}

	// The accounts authenticate as before, and bob may do what he could.
	bob, err := db.FindAccount("bob", wg.UUID.String())
	if err != nil || bob.ID == 0 || !bytes.Equal(bob.NTHash, ntHash("pw")) {
		t.Fatalf("bob: got %+v, %v", bob, err)
	}
	rights, err := db.GetAccessRights(share, bob)
	if err != nil || !rights.ReadAccess || rights.WriteAccess || !rights.ExecuteAccess {
		t.Errorf("bob's rights: got %+v, %v", rights, err)
	}
	alice, err := db.FindAccount(fx.alice.Username, wg.UUID.String())
	if err != nil || alice.ID == 0 {
		t.Fatalf("alice: got %+v, %v", alice, err)
	}

	// The folders and files are back where they were and whose they were: the
	// private folder is alice's and bob does not see it.
	entries, err := db.ListObjects(alice, share.Name, "/holiday")
	if err != nil {
		t.Fatalf("ListObjects(alice): %v", err)
	}
	if len(entries) != 4 {
		t.Errorf("alice's folder holds %d entries, want 4: %+v", len(entries), entries)
	}
	if entries, err := db.ListObjects(bob, share.Name, "/holiday"); err == nil && len(entries) > 0 {
		t.Errorf("bob sees into alice's private folder: %+v", entries)
	}
	slices, err := db.GetMetadata(alice, share.Name, "/holiday/beach.raw", 0, 300)
	if err != nil || len(slices) != 2 || slices[0].Key != fx.object || slices[0].Offset != 1000 {
		t.Errorf("the file on the network: got %+v, %v", slices, err)
	}
	slices, err = db.GetMetadata(alice, share.Name, "/holiday/small.txt", 0, 100)
	if err != nil || len(slices) != 1 || !bytes.Equal(slices[0].Data, fx.small) {
		t.Errorf("the small file is not back in a buffer: got %+v, %v", slices, err)
	}
	if entries, err := db.ListObjects(bob, share.Name, "/"); err != nil || len(entries) != 1 || entries[0].Path != "/bob.bin" {
		t.Errorf("bob's view of the root: want his file alone, got %+v, %v", entries, err)
	}

	// Restoring again is refused, unless forced, and then changes nothing.
	r, _ = transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	if _, err := db.Restore(ctx, r, RestoreOptions{}); !errors.Is(err, ErrConnectionExists) {
		t.Errorf("a restore over a live connection: got %v", err)
	}
	r, _ = transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	again, err := db.Restore(ctx, r, RestoreOptions{Force: true})
	if err != nil {
		t.Fatalf("the forced restore: %v", err)
	}
	if again.Accounts != 0 || again.Directories != 0 || again.Files != 0 || again.AlreadyThere != 6 {
		t.Errorf("the forced restore: got %+v", again)
	}
}

// TestRestoreRefusals verifies what a restore will not do: apply a description
// that is not a catalog, or recreate a share over a different one.
func TestRestoreRefusals(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	var plain bytes.Buffer
	w, _ := transfer.NewWriter(&plain, transfer.Header{Source: "renterd"})
	_ = w.Close()
	r, err := transfer.NewReader(bytes.NewReader(plain.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := db.Restore(ctx, r, RestoreOptions{}); !errors.Is(err, ErrNotACatalog) {
		t.Errorf("a plain description: got %v", err)
	}

	// A renterd share of the catalog's name is already there.
	db.WithShares(&recordingShares{})
	addShare(t, db, "idx")
	var catalog bytes.Buffer
	w, _ = transfer.NewWriter(&catalog, transfer.Header{Source: "sombrero"})
	_ = w.Connection(transfer.Connection{Share: transfer.Share{Name: "idx", Type: "indexd", Server: "https://indexer"}, Workgroup: transfer.Workgroup{UUID: [16]byte{1}}})
	_ = w.Close()
	r, _ = transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	if _, err := db.Restore(ctx, r, RestoreOptions{}); !errors.Is(err, ErrShareMismatch) {
		t.Errorf("a share serving something else: got %v", err)
	}
}
