package stores

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
	"lukechampine.com/frand"
)

// replaceFile writes new buffered contents over the file at the path and
// returns the slabs the store released.
func replaceFile(t *testing.T, db *Database, share string, acc Account, path string, size int) []types.Hash256 {
	t.Helper()

	uploadID, err := db.CreateUpload(acc, share, path)
	if err != nil {
		t.Fatalf("CreateUpload(%s): %v", path, err)
	}
	if err := db.AddBufferedSlab(uploadID, 0, frand.Bytes(size)); err != nil {
		t.Fatalf("AddBufferedSlab(%s): %v", path, err)
	}
	slabs, err := db.FinalizeUpload(uploadID)
	if err != nil {
		t.Fatalf("FinalizeUpload(%s): %v", path, err)
	}
	return slabs
}

// versionSizes lists the sizes of the versions of the file, newest first.
func versionSizes(t *testing.T, db *Database, share, path string) []uint64 {
	t.Helper()

	versions, err := db.Versions(share, path)
	if err != nil {
		t.Fatalf("Versions(%s): %v", path, err)
	}
	sizes := make([]uint64, 0, len(versions))
	for _, v := range versions {
		sizes = append(sizes, v.Size)
	}
	return sizes
}

// TestFinalizeUploadKeepsAVersion verifies that a replaced file stays as a
// version holding its slabs, readable by itself, and in nobody's way.
func TestFinalizeUploadKeepsAVersion(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)

	if slabs := replaceFile(t, db, share, acc, "doc.txt", 300); len(slabs) != 0 {
		t.Errorf("the slab of the version was released: %v", slabs)
	}
	if n := pendingUnpins(t, db); n != 0 {
		t.Errorf("want nothing staged for unpinning, got %d", n)
	}

	versions, err := db.Versions(share, "doc.txt")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 1 || versions[0].Size != 400 || versions[0].ReplacedBy != acc.Username {
		t.Fatalf("want one version of 400 bytes replaced by %s, got %+v", acc.Username, versions)
	}
	if time.Since(versions[0].ReplacedAt) > time.Minute {
		t.Errorf("want the version replaced just now, got %v", versions[0].ReplacedAt)
	}

	md, err := db.VersionMetadata(share, "doc.txt", versions[0].ID, 0, 400)
	if err != nil {
		t.Fatalf("VersionMetadata: %v", err)
	}
	if len(md) != 1 || md[0].Key != own || md[0].Length != 400 {
		t.Errorf("want the version's slab, got %+v", md)
	}
	if _, err := db.VersionMetadata(share, "other.txt", versions[0].ID, 0, 400); !errors.Is(err, ErrNotFound) {
		t.Errorf("a version under another path: want %v, got %v", ErrNotFound, err)
	}

	// The file is the new contents, and the version is not in the way of the next upload.
	obj, err := db.Object(acc, share, "doc.txt")
	if err != nil || obj.Size != 300 {
		t.Fatalf("want the file at 300 bytes, got %+v, %v", obj, err)
	}
	replaceFile(t, db, share, acc, "doc.txt", 200)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 || sizes[1] != 400 {
		t.Errorf("want the versions newest first, got %v", sizes)
	}
	objects, err := db.ListObjects(acc, share, "/")
	if err != nil || len(objects) != 1 {
		t.Errorf("want the one file listed, got %+v, %v", objects, err)
	}

	// With the policy off, a replacement releases the file it replaces, as before.
	db.SetVersioning(VersionPolicy{})
	replaceFile(t, db, share, acc, "doc.txt", 100)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 {
		t.Errorf("want no version added, got %v", sizes)
	}
}

// ageSession moves the newest version's session back in time: its start by
// started, and its last save by saved.
func ageSession(t *testing.T, db *Database, started, saved time.Duration) {
	t.Helper()

	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			UPDATE versions
			SET replaced_at = NOW() - make_interval(secs => $1),
				session_until = NOW() - make_interval(secs => $2)
			WHERE object_id = (SELECT object_id FROM versions ORDER BY replaced_at DESC LIMIT 1)
		`
		_, err := tx.Exec(ctx, query, started.Seconds(), saved.Seconds())
		return err
	})
	if err != nil {
		t.Fatalf("ageSession: %v", err)
	}
}

// TestVersionGapCollapsesASession verifies that saves within the gap of one
// another leave one version, while a pause, a long session or another
// account's save leave another.
func TestVersionGapCollapsesASession(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, MaxSession: time.Hour, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	plantPiece(t, db, share, acc, "doc.txt", types.Hash256{1}, 0, 400)

	replaceFile(t, db, share, acc, "doc.txt", 300)
	replaceFile(t, db, share, acc, "doc.txt", 200)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 || sizes[0] != 400 {
		t.Errorf("want the one version from before the session, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 200 {
		t.Errorf("want the contents the session wrote over gone, got %d bytes buffered", n)
	}

	// A save after a pause longer than the gap starts a session of its own.
	ageSession(t, db, 30*time.Minute, 20*time.Minute)
	replaceFile(t, db, share, acc, "doc.txt", 100)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 200 {
		t.Errorf("want the state before the pause kept, got %v", sizes)
	}

	// A session saving steadily for longer than the cap leaves a version too.
	ageSession(t, db, 2*time.Hour, time.Minute)
	replaceFile(t, db, share, acc, "doc.txt", 50)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 3 || sizes[0] != 100 {
		t.Errorf("want the long session cut, got %v", sizes)
	}

	// Another account's save within the gap is a session of its own.
	bob, _ := newForeignAccount(t, db, "bob")
	replaceFile(t, db, share, bob, "doc.txt", 25)
	versions, err := db.Versions(share, "doc.txt")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 4 || versions[0].Size != 50 || versions[0].ReplacedBy != "bob" {
		t.Errorf("want another account's replacement kept, got %+v", versions)
	}
}

// TestVersionCountDropsTheOldest verifies that the versions beyond the count
// go, oldest first, releasing the slabs only they held.
func TestVersionCountDropsTheOldest(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 2})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)

	replaceFile(t, db, share, acc, "doc.txt", 300)
	replaceFile(t, db, share, acc, "doc.txt", 200)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 {
		t.Fatalf("want two versions kept, got %v", sizes)
	}

	slabs := replaceFile(t, db, share, acc, "doc.txt", 100)
	assertSlabs(t, "FinalizeUpload(doc.txt)", slabs, []types.Hash256{own})
	if n := pendingUnpins(t, db); n != 1 {
		t.Errorf("want the dropped version's slab staged for unpinning, got %d staged", n)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 200 || sizes[1] != 300 {
		t.Errorf("want the two newest versions, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 600 {
		t.Errorf("want the file and its two versions buffered, got %d bytes", n)
	}
}

// TestDeleteFileTakesTheVersions verifies that deleting a file releases what
// its versions held too.
func TestDeleteFileTakesTheVersions(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)

	slabs, err := db.DeleteFile(acc, share, "doc.txt")
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	assertSlabs(t, "DeleteFile(doc.txt)", slabs, []types.Hash256{own})
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want the versions gone with the file, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 0 {
		t.Errorf("want the file's buffer gone, got %d bytes", n)
	}
}

// TestRenameFileMovesTheVersions verifies that the versions of a renamed file
// are found under its new name.
func TestRenameFileMovesTheVersions(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	if err := db.CreateDirectory(acc, share, "docs", false, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}

	if _, err := db.RenameFile(acc, share, "doc.txt", "docs/notes.txt", false); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}
	versions, err := db.Versions(share, "docs/notes.txt")
	if err != nil || len(versions) != 1 || versions[0].Size != 400 {
		t.Fatalf("want the version under the new name, got %+v, %v", versions, err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want nothing left under the old name, got %v", sizes)
	}
	if md, err := db.VersionMetadata(share, "docs/notes.txt", versions[0].ID, 0, 400); err != nil || len(md) != 1 || md[0].Key != own {
		t.Errorf("want the version readable under the new name, got %+v, %v", md, err)
	}
	if objects, err := db.ListObjects(acc, share, "docs"); err != nil || len(objects) != 1 {
		t.Errorf("want the one file listed in the folder, got %+v, %v", objects, err)
	}
}

// TestRenameOverKeepsAVersion verifies that a file renamed over another, the
// way editors save, keeps the other as a version under the session rule.
func TestRenameOverKeepsAVersion(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)

	plantBufferedFile(t, db, share, acc, "tmp.txt", 300, false)
	slabs, err := db.RenameFile(acc, share, "tmp.txt", "doc.txt", true)
	if err != nil {
		t.Fatalf("RenameFile over doc.txt: %v", err)
	}
	if len(slabs) != 0 {
		t.Errorf("the slab of the version was released: %v", slabs)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 || sizes[0] != 400 {
		t.Errorf("want the file renamed over kept, got %v", sizes)
	}
	if obj, err := db.Object(acc, share, "doc.txt"); err != nil || obj.Size != 300 {
		t.Errorf("want the file at 300 bytes, got %+v, %v", obj, err)
	}
	if _, err := db.Object(acc, share, "tmp.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want the temporary name gone, got %v", err)
	}

	// The next save of the session adds no version; what it replaces goes.
	plantBufferedFile(t, db, share, acc, "tmp.txt", 200, false)
	if _, err := db.RenameFile(acc, share, "tmp.txt", "doc.txt", true); err != nil {
		t.Fatalf("RenameFile over doc.txt again: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 {
		t.Errorf("want the session's saves collapsed, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 200 {
		t.Errorf("want the replaced contents gone, got %d bytes buffered", n)
	}

	// With the policy off, the file renamed over goes, as before.
	db.SetVersioning(VersionPolicy{})
	plantBufferedFile(t, db, share, acc, "tmp.txt", 100, false)
	if _, err := db.RenameFile(acc, share, "tmp.txt", "doc.txt", true); err != nil {
		t.Fatalf("RenameFile with versioning off: %v", err)
	}
	if n := bufferedBytes(t, db); n != 100 {
		t.Errorf("want only the new contents buffered, got %d bytes", n)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 {
		t.Errorf("want the earlier version untouched, got %v", sizes)
	}
}

// TestFoldersCarryTheVersions verifies that versions move with their folder
// and go with it.
func TestFoldersCarryTheVersions(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	if err := db.CreateDirectory(acc, share, "docs", false, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "docs/doc.txt", own, 0, 400)
	replaceFile(t, db, share, acc, "docs/doc.txt", 300)

	if _, err := db.RenameDirectory(acc, share, "docs", "old", false); err != nil {
		t.Fatalf("RenameDirectory: %v", err)
	}
	if sizes := versionSizes(t, db, share, "old/doc.txt"); len(sizes) != 1 || sizes[0] != 400 {
		t.Errorf("want the version under the folder's new name, got %v", sizes)
	}

	slabs, err := db.DeleteDirectory(acc, share, "old")
	if err != nil {
		t.Fatalf("DeleteDirectory: %v", err)
	}
	assertSlabs(t, "DeleteDirectory(old)", slabs, []types.Hash256{own})
	if sizes := versionSizes(t, db, share, "old/doc.txt"); len(sizes) != 0 {
		t.Errorf("want the versions gone with the folder, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 0 {
		t.Errorf("want nothing left buffered, got %d bytes", n)
	}
}

// TestDeleteVersions verifies that one version goes by its ID and the rest at
// once, each releasing what only it held, and the file stays.
func TestDeleteVersions(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	replaceFile(t, db, share, acc, "doc.txt", 200)
	replaceFile(t, db, share, acc, "doc.txt", 100)

	versions, err := db.Versions(share, "doc.txt")
	if err != nil || len(versions) != 3 {
		t.Fatalf("want three versions, got %+v, %v", versions, err)
	}

	// The newest alone, which held a buffer and no slab.
	slabs, err := db.DeleteVersions(share, "doc.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("DeleteVersions(newest): %v", err)
	}
	if len(slabs) != 0 {
		t.Errorf("want no slab released, got %v", slabs)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 || sizes[1] != 400 {
		t.Errorf("want the two older versions left, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 400 {
		t.Errorf("want the deleted version's buffer gone, got %d bytes", n)
	}

	// A version of another file is not reachable under this path.
	if _, err := db.DeleteVersions(share, "other.txt", versions[1].ID); err != nil {
		t.Fatalf("DeleteVersions under another path: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 {
		t.Errorf("want the versions untouched, got %v", sizes)
	}

	// All of them, which releases the slab of the original.
	slabs, err = db.DeleteVersions(share, "doc.txt", 0)
	if err != nil {
		t.Fatalf("DeleteVersions(all): %v", err)
	}
	assertSlabs(t, "DeleteVersions(doc.txt)", slabs, []types.Hash256{own})
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want no versions left, got %v", sizes)
	}
	if obj, err := db.Object(acc, share, "doc.txt"); err != nil || obj.Size != 100 {
		t.Errorf("want the file untouched at 100 bytes, got %+v, %v", obj, err)
	}
}
