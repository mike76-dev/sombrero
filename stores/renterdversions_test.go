package stores

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// renterdSave records a save over the renterd file and returns the ID of the
// version it left, zero where it left none, and the IDs pruned by the count.
func renterdSave(t *testing.T, db *Database, share, path string, acc Account, size uint64, deliberate bool) (uint64, []uint64) {
	t.Helper()

	id, pruned, err := db.KeepRenterdFile(RenterdReplacement{Share: share, Path: path, Account: acc, Size: size, ModifiedAt: time.Now(), Deliberate: deliberate})
	if err != nil {
		t.Fatalf("KeepRenterdFile(%s): %v", path, err)
	}
	return id, pruned
}

// renterdSizes lists the sizes of the versions of the renterd file, newest first.
func renterdSizes(t *testing.T, db *Database, share, path string) []uint64 {
	t.Helper()

	versions, err := db.RenterdVersions(share, path)
	if err != nil {
		t.Fatalf("RenterdVersions(%s): %v", path, err)
	}
	sizes := make([]uint64, 0, len(versions))
	for _, v := range versions {
		sizes = append(sizes, v.Size)
	}
	return sizes
}

// TestRenterdFileKeepsAVersion verifies the session and restore rules on a
// renterd file: one version per session, a deliberate replacement kept unless
// the file holds a restored version untouched.
func TestRenterdFileKeepsAVersion(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, MaxSession: time.Hour, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	first, pruned := renterdSave(t, db, share, "docs/doc.txt", acc, 400, false)
	if first == 0 || len(pruned) != 0 {
		t.Fatalf("want the first save kept, nothing pruned, got %d, %v", first, pruned)
	}
	if id, _ := renterdSave(t, db, share, "docs/doc.txt", acc, 300, false); id != 0 {
		t.Errorf("want the save within the session not kept, got version %d", id)
	}
	deleted, _ := renterdSave(t, db, share, "/docs/doc.txt", acc, 200, true)
	if deleted == 0 {
		t.Fatal("want the deliberate replacement kept whatever the session")
	}
	if sizes := renterdSizes(t, db, share, "docs/doc.txt"); len(sizes) != 2 || sizes[0] != 200 || sizes[1] != 400 {
		t.Errorf("want the two versions newest first, got %v", sizes)
	}

	v, err := db.RenterdVersion(share, "docs/doc.txt", first)
	if err != nil || v.Size != 400 || v.ReplacedBy != acc.Username || v.Kept {
		t.Errorf("want the first version by its ID, got %+v, %v", v, err)
	}
	if _, err := db.RenterdVersion(share, "docs/other.txt", first); !errors.Is(err, ErrNotFound) {
		t.Errorf("a version under another path: want ErrNotFound, got %v", err)
	}

	// The file written back from the newest version is not kept again by a restore.
	if err := db.MarkRenterdRestored(share, "docs/doc.txt", 200, []uint64{0}); err != nil {
		t.Fatalf("MarkRenterdRestored: %v", err)
	}
	if id, _ := renterdSave(t, db, share, "docs/doc.txt", acc, 200, true); id != 0 {
		t.Errorf("want the restored version not kept again, got version %d", id)
	}
	if err := db.MarkRenterdRestored(share, "docs/doc.txt", 999, []uint64{first, deleted}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no version of the size: want ErrNotFound, got %v", err)
	}
	if id, _ := renterdSave(t, db, share, "docs/doc.txt", acc, 100, true); id == 0 {
		t.Error("want the next deliberate replacement kept, the note being gone")
	}
}

// TestRenterdVersionLimits verifies the count and age limits on renterd
// versions, the kept ones exempt, and the explicit deletions.
func TestRenterdVersionLimits(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 2})

	acc, share, _ := newSlabTestFixture(t, db)
	a, _ := renterdSave(t, db, share, "doc.txt", acc, 1, false)
	b, _ := renterdSave(t, db, share, "doc.txt", acc, 2, false)
	if _, pruned := renterdSave(t, db, share, "doc.txt", acc, 3, false); len(pruned) != 1 || pruned[0] != a {
		t.Errorf("want the oldest version pruned, got %v", pruned)
	}
	if err := db.KeepRenterdVersion(share, "doc.txt", b, true); err != nil {
		t.Fatalf("KeepRenterdVersion: %v", err)
	}
	if err := db.KeepRenterdVersion(share, "doc.txt", a, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("keeping a version that went: want ErrNotFound, got %v", err)
	}
	if _, pruned := renterdSave(t, db, share, "doc.txt", acc, 4, false); len(pruned) != 0 {
		t.Errorf("want the kept version left out of the count, got %v pruned", pruned)
	}
	if _, pruned := renterdSave(t, db, share, "doc.txt", acc, 5, false); len(pruned) != 1 {
		t.Errorf("want the version beyond the count pruned around the kept one, got %v", pruned)
	}
	if sizes := renterdSizes(t, db, share, "doc.txt"); len(sizes) != 3 || sizes[0] != 5 || sizes[1] != 4 || sizes[2] != 2 {
		t.Errorf("want the two newest and the kept version, got %v", sizes)
	}

	if dropped, err := db.ExpireRenterdVersions(share, 0); err != nil || len(dropped) != 0 {
		t.Errorf("ExpireRenterdVersions(0): want nothing dropped, got %v, %v", dropped, err)
	}
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE renterd_versions SET replaced_at = NOW() - INTERVAL '40 days'`)
		return err
	})
	if err != nil {
		t.Fatalf("aging the versions: %v", err)
	}
	if dropped, err := db.ExpireRenterdVersions(share, 30*24*time.Hour); err != nil || len(dropped) != 2 {
		t.Errorf("want the two unkept versions expired, got %v, %v", dropped, err)
	}
	if err := db.KeepRenterdVersion(share, "doc.txt", b, false); err != nil {
		t.Fatalf("KeepRenterdVersion(false): %v", err)
	}
	if dropped, err := db.ExpireRenterdVersions(share, 30*24*time.Hour); err != nil || len(dropped) != 1 || dropped[0] != b {
		t.Errorf("want the released version expired, got %v, %v", dropped, err)
	}

	c, _ := renterdSave(t, db, share, "doc.txt", acc, 6, false)
	d, _ := renterdSave(t, db, share, "doc.txt", acc, 7, false)
	renterdSave(t, db, share, "sub/deep.txt", acc, 8, false)
	if err := db.ForgetRenterdVersion(d); err != nil {
		t.Fatalf("ForgetRenterdVersion: %v", err)
	}
	if dropped, err := db.DeleteRenterdVersions(share, "doc.txt", c); err != nil || len(dropped) != 1 || dropped[0] != c {
		t.Errorf("want the one version dropped by its ID, got %v, %v", dropped, err)
	}
	if sizes := renterdSizes(t, db, share, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want no versions left, got %v", sizes)
	}
	renterdSave(t, db, share, "doc.txt", acc, 9, false)
	if dropped, err := db.DeleteRenterdVersionsUnder(share, ""); err != nil || len(dropped) != 2 {
		t.Errorf("want every version under the root dropped, got %v, %v", dropped, err)
	}
}

// TestRenterdVersionsFollowTheFile verifies that the versions move with a
// renamed file or folder, that the swap save is recognised, and that the
// folder lists its versions and snapshots.
func TestRenterdVersionsFollowTheFile(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	renterdSave(t, db, share, "a/x.txt", acc, 1, false)
	renterdSave(t, db, share, "a/sub/y.txt", acc, 2, false)
	renterdSave(t, db, share, "elsewhere.txt", acc, 3, false)

	if err := db.RenterdFileRenamed(share, "a/x.txt", "a/z.txt", false); err != nil {
		t.Fatalf("RenterdFileRenamed: %v", err)
	}
	if sizes := renterdSizes(t, db, share, "a/z.txt"); len(sizes) != 1 || sizes[0] != 1 {
		t.Errorf("want the version under the new name, got %v", sizes)
	}
	if origin, err := db.RenterdDisplacedOrigin(share, "a/z.txt"); err != nil || origin != "/a/x.txt" {
		t.Errorf("want the old name noted, got %q, %v", origin, err)
	}

	if err := db.RenterdFileRenamed(share, "a", "b", true); err != nil {
		t.Fatalf("RenterdFileRenamed(dir): %v", err)
	}
	under, err := db.RenterdVersionsUnder(share, "b")
	if err != nil || len(under) != 2 || under[0].Path != "/b/sub/y.txt" || under[1].Path != "/b/z.txt" {
		t.Fatalf("want the two versions under the renamed folder, got %+v, %v", under, err)
	}
	if origin, _ := db.RenterdDisplacedOrigin(share, "b/z.txt"); origin != "" {
		t.Errorf("want the old name forgotten with the folder, got %q", origin)
	}
	if under, _ := db.RenterdVersionsUnder(share, ""); len(under) != 3 {
		t.Errorf("want every version under the root, got %+v", under)
	}

	times, err := db.RenterdSnapshots(share, "b")
	if err != nil || len(times) == 0 || len(times) > 2 {
		t.Fatalf("want the modification times of the versions, got %v, %v", times, err)
	}
	for i := 1; i < len(times); i++ {
		if !times[i].Before(times[i-1]) {
			t.Errorf("want the times newest first, got %v", times)
		}
	}

	// The swap save: the saved copy takes the name, the old file is deleted
	// under a temporary one and becomes the version.
	renterdSave(t, db, share, "b/tmp.txt", acc, 4, false)
	if err := db.RenterdFileRenamed(share, "b/z.txt", "b/old.txt", false); err != nil {
		t.Fatalf("RenterdFileRenamed: %v", err)
	}
	if err := db.RenterdFileRenamed(share, "b/tmp.txt", "b/z.txt", false); err != nil {
		t.Fatalf("RenterdFileRenamed: %v", err)
	}
	origin, err := db.RenterdDisplacedOrigin(share, "b/old.txt")
	if err != nil || origin != "/b/z.txt" {
		t.Fatalf("want the old file's origin, got %q, %v", origin, err)
	}
	id, _, err := db.AbsorbRenterdFile(RenterdReplacement{Share: share, Path: "b/old.txt", Account: acc, Size: 5, ModifiedAt: time.Now()}, origin)
	if err != nil || id == 0 {
		t.Fatalf("want the old file kept as a version of the origin, got %d, %v", id, err)
	}
	if sizes := renterdSizes(t, db, share, "b/z.txt"); len(sizes) != 3 || sizes[0] != 5 {
		t.Errorf("want the versions of both files under the name, the old file newest, got %v", sizes)
	}
	if sizes := renterdSizes(t, db, share, "b/old.txt"); len(sizes) != 0 {
		t.Errorf("want nothing left under the temporary name, got %v", sizes)
	}
}
