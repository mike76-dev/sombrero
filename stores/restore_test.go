package stores

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
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

	// A file whose data the account no longer holds is left out and named,
	// rather than made as an entry nothing can read.
	if err := db.UnregisterShare(fx.share.Name); err != nil {
		t.Fatalf("UnregisterShare: %v", err)
	}
	r, _ = transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	unpinned := types.Hash256{8}
	partial, err := db.Restore(ctx, r, RestoreOptions{Held: func(key types.Hash256) bool { return key != unpinned }})
	if err != nil {
		t.Fatalf("the restore with a slab gone: %v", err)
	}
	if partial.Files != 4 || partial.Missing != 1 || len(partial.MissingPaths) != 1 || partial.MissingPaths[0] != "/holiday/beach.raw" {
		t.Errorf("the restore with a slab gone: got %+v", partial)
	}
	if _, err := db.GetMetadata(alice, share.Name, "/holiday/beach.raw", 0, 300); err == nil {
		t.Error("the file whose data is gone was made all the same")
	}
}

// TestServerCatalog verifies that what belongs to no connection comes back from a
// catalog of the server: the shares, the workgroups with their accounts, and the
// bans, with what is there already left alone.
func TestServerCatalog(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	fx := plantCatalogFixture(t, db)
	addShare(t, db, "docs") // a renterd share nobody is connected to
	lonely := addWorkgroup(t, db, "lonely")
	if err := db.BanHost("192.168.1.100", "too many bad passwords"); err != nil {
		t.Fatalf("BanHost: %v", err)
	}

	var catalog bytes.Buffer
	stats, err := db.SnapshotServer(&catalog)
	if err != nil {
		t.Fatalf("SnapshotServer: %v", err)
	}
	if stats != (ServerStats{Shares: 2, Workgroups: 2, Accounts: 2, Bans: 1}) {
		t.Errorf("stats: got %+v", stats)
	}

	r, err := transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	server := r.Server()
	if server == nil || len(server.Shares) != 2 || len(server.Workgroups) != 2 || len(server.Bans) != 1 {
		t.Fatalf("the catalog: got %+v", server)
	}
	if server.Workgroups[0].Workgroup.Name != fx.wg.Name || len(server.Workgroups[0].Accounts) != 2 || len(server.Workgroups[0].Workgroup.PublicDirs) != 1 {
		t.Errorf("the first workgroup: got %+v", server.Workgroups[0])
	}

	// Everything goes, and the catalog brings it back, except the connection,
	// which is the business of the catalog of the connection.
	for _, name := range []string{fx.share.Name, "docs"} {
		if err := db.UnregisterShare(name); err != nil {
			t.Fatalf("UnregisterShare(%s): %v", name, err)
		}
	}
	for _, wg := range []Workgroup{fx.wg, lonely} {
		if err := db.RemoveWorkgroup(wg); err != nil {
			t.Fatalf("RemoveWorkgroup: %v", err)
		}
	}
	if err := db.ClearBans(); err != nil {
		t.Fatalf("ClearBans: %v", err)
	}

	r, _ = transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	restored, err := db.RestoreServer(ctx, r)
	if err != nil {
		t.Fatalf("RestoreServer: %v", err)
	}
	if restored != (ServerRestoreStats{Shares: 2, Workgroups: 2, Accounts: 2, Bans: 1}) {
		t.Errorf("restored: got %+v", restored)
	}
	if share, err := db.GetShare("docs"); err != nil || share.Type != "renterd" {
		t.Errorf("the renterd share: got %+v, %v", share, err)
	}
	wg, err := db.FindWorkgroup(fx.wg.UUID)
	if err != nil || wg.ID == 0 || len(wg.PublicDirs) != 1 {
		t.Errorf("the workgroup: got %+v, %v", wg, err)
	}
	if bob, err := db.FindAccount("bob", fx.wg.UUID.String()); err != nil || !bytes.Equal(bob.NTHash, ntHash("pw")) {
		t.Errorf("bob: got %+v, %v", bob, err)
	}
	if banned, _, err := db.IsBanned("192.168.1.100"); err != nil || !banned {
		t.Errorf("the ban: %v %v", banned, err)
	}
	if connected, _, err := db.IsConnected(wg, fx.share); err != nil || connected {
		t.Error("a catalog of the server made a connection")
	}

	// Again, and nothing is made twice.
	r, _ = transfer.NewReader(bytes.NewReader(catalog.Bytes()))
	if again, err := db.RestoreServer(ctx, r); err != nil || again != (ServerRestoreStats{}) {
		t.Errorf("the second restore: got %+v, %v", again, err)
	}
}

// TestRemoveAccountReleasesItsStorage verifies that deleting an account does for
// its files what deleting them one by one would: the slabs only they referenced
// are staged for unpinning, the ones another account's file still uses are not,
// and what was still buffered is dropped.
func TestRemoveAccountReleasesItsStorage(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	// Alice owns a file on two slabs, one of which bob's file is on too, and two
	// files still in buffers.
	fx := plantCatalogFixture(t, db)
	buffers := func() int {
		var n int
		err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COUNT(*) FROM buffers`).Scan(&n)
		})
		if err != nil {
			t.Fatalf("counting the buffers: %v", err)
		}
		return n
	}
	if got := buffers(); got != 2 {
		t.Fatalf("the fixture holds %d buffer(s), want 2", got)
	}

	if err := db.RemoveAccount(fx.alice.Username, fx.wg.UUID.String()); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}

	staged, err := db.PendingUnpins(fx.share.Name, fx.wg.ID)
	if err != nil {
		t.Fatalf("PendingUnpins: %v", err)
	}
	if len(staged) != 1 || staged[0] != (types.Hash256{8}) {
		t.Errorf("staged for unpinning: got %v, want the one slab only alice's file was on", staged)
	}
	if got := buffers(); got != 0 {
		t.Errorf("%d buffer(s) are left with nothing referring to them", got)
	}

	// Bob's file is as it was, on the slab the two shared.
	slices, err := db.GetMetadata(fx.bob, fx.share.Name, "/bob.bin", 0, 10)
	if err != nil || len(slices) != 1 || slices[0].Key != fx.object {
		t.Errorf("bob's file: got %+v, %v", slices, err)
	}

	// Removing every account of the workgroup releases the rest.
	if err := db.RemoveAccounts(fx.wg.UUID.String()); err != nil {
		t.Fatalf("RemoveAccounts: %v", err)
	}
	staged, err = db.PendingUnpins(fx.share.Name, fx.wg.ID)
	if err != nil {
		t.Fatalf("PendingUnpins: %v", err)
	}
	if len(staged) != 2 {
		t.Errorf("staged for unpinning after the last account went: got %v, want both slabs", staged)
	}
}

// TestRemoveWorkgroupReleasesItsStorage verifies that deleting a workgroup does
// for the files of its accounts what deleting the accounts would, and hands the
// slabs nothing references any more to the share manager while the connection
// that pinned them can still drop them.
func TestRemoveWorkgroupReleasesItsStorage(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	fx := plantCatalogFixture(t, db)
	rec := &recordingShares{}
	db.WithShares(rec)

	if err := db.RemoveWorkgroup(fx.wg); err != nil {
		t.Fatalf("RemoveWorkgroup: %v", err)
	}

	assertSlabs(t, "UnpinSlabs", rec.unpinned, []types.Hash256{fx.object, {8}})
	for _, name := range rec.unpinnedOn {
		if name != fx.share.Name {
			t.Errorf("slabs unpinned on %q, want %q", name, fx.share.Name)
		}
	}
	if len(rec.disconnected) != 1 || rec.disconnected[0] != fx.wg.UUID.String()+"/"+fx.share.Name {
		t.Errorf("disconnected: got %v, want the one connection", rec.disconnected)
	}
	if n := storedBuffers(t, db); n != 0 {
		t.Errorf("%d buffer(s) are left with nothing referring to them", n)
	}
	if wg, err := db.FindWorkgroup(fx.wg.UUID); err != nil || wg.ID != 0 {
		t.Errorf("the workgroup is still there: %+v, %v", wg, err)
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
