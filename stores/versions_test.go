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

// TestDeleteFileKeepsTheVersions verifies that a share keeping versions deletes
// a file by making it one, and that a share keeping none releases it all.
func TestDeleteFileKeepsTheVersions(t *testing.T) {
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
	if len(slabs) != 0 {
		t.Errorf("want nothing released, got %v", slabs)
	}
	if _, err := db.Object(acc, share, "doc.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want the file gone from the share, got %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 {
		t.Errorf("want the deleted contents kept ahead of the version, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 300 {
		t.Errorf("want the deleted contents still buffered, got %d bytes", n)
	}
	if _, err := db.DeleteFile(acc, share, "doc.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting what is gone: want %v, got %v", ErrNotFound, err)
	}

	// A restored version left untouched is a version already: the file goes, the versions stay.
	versions, _ := db.Versions(share, "doc.txt")
	if _, err := db.RestoreVersion(acc, share, "doc.txt", versions[1].ID); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	if slabs, err := db.DeleteFile(acc, share, "doc.txt"); err != nil || len(slabs) != 0 {
		t.Fatalf("DeleteFile of the restored version: %v, %v", slabs, err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 {
		t.Errorf("want the versions as they were, got %v", sizes)
	}

	// With the policy off, the file and its versions go together.
	db.SetVersioning(VersionPolicy{})
	replaceFile(t, db, share, acc, "doc.txt", 200)
	slabs, err = db.DeleteFile(acc, share, "doc.txt")
	if err != nil {
		t.Fatalf("DeleteFile with the policy off: %v", err)
	}
	assertSlabs(t, "DeleteFile(doc.txt)", slabs, []types.Hash256{own})
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want the versions gone with the file, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 0 {
		t.Errorf("want every buffer gone, got %d bytes", n)
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
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE objects SET modified_at = $1 WHERE full_path = '/doc.txt' AND temporary = FALSE`, written)
		return err
	}); err != nil {
		t.Fatalf("dating the file: %v", err)
	}

	if _, err := db.RenameFile(acc, share, "doc.txt", "docs/notes.txt", false); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}
	versions, err := db.Versions(share, "docs/notes.txt")
	if err != nil || len(versions) != 1 || versions[0].Size != 400 {
		t.Fatalf("want the version under the new name, got %+v, %v", versions, err)
	}

	// A rename is no change to the contents, so the file keeps the time they were written.
	if o, err := db.Object(acc, share, "docs/notes.txt"); err != nil || !o.ModifiedAt.Equal(written) {
		t.Errorf("want the modified time kept through the rename, got %v, %v", o.ModifiedAt, err)
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

	// A file hidden as a version while its upload still lingers does not stand
	// in the way of writing the file in place.
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, Count: 10})
	ageSession(t, db, 30*time.Minute, 20*time.Minute)
	plantBufferedFile(t, db, share, acc, "tmp.txt", 75, false)
	if _, err := db.RenameFile(acc, share, "tmp.txt", "doc.txt", true); err != nil {
		t.Fatalf("RenameFile over the buffered file: %v", err)
	}
	replaceFile(t, db, share, acc, "doc.txt", 50)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 100 {
		t.Errorf("want the buffered file kept and the file written over, got %v", sizes)
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

// dateFile sets when the copy of the file with the size was created and
// modified, and when the version it is was replaced.
func dateFile(t *testing.T, db *Database, share, path string, size uint64, created, modified, replaced time.Time) {
	t.Helper()

	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		var id int64
		const date = `
			UPDATE objects
			SET created_at = $4, modified_at = $5
			WHERE share_name = $1 AND full_path = $2 AND size = $3
			RETURNING id
		`
		if err := tx.QueryRow(ctx, date, share, normalizePath(path), int64(size), created, modified).Scan(&id); err != nil {
			return err
		}
		if replaced.IsZero() {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE versions SET replaced_at = $2 WHERE object_id = $1`, id, replaced)
		return err
	})
	if err != nil {
		t.Fatalf("dating %s of %d bytes: %v", path, size, err)
	}
}

// TestListObjectsAt verifies that a folder is listed as it was at a time:
// with the contents the files held then, the files deleted since, the folders
// made by then, and nothing made later.
func TestListObjectsAt(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	for _, dir := range []string{"docs", "docs/sub", "docs/late"} {
		if err := db.CreateDirectory(acc, share, dir, false, false); err != nil {
			t.Fatalf("CreateDirectory(%s): %v", dir, err)
		}
	}
	base := time.Now().Truncate(time.Second)
	ago := func(hours int) time.Time {
		return base.Add(-time.Duration(hours) * time.Hour)
	}
	var none time.Time

	// doc.txt held 400 bytes until 6 hours ago, 300 until 2 hours ago, and 200 since.
	plantPiece(t, db, share, acc, "docs/doc.txt", types.Hash256{1}, 0, 400)
	replaceFile(t, db, share, acc, "docs/doc.txt", 300)
	replaceFile(t, db, share, acc, "docs/doc.txt", 200)
	dateFile(t, db, share, "docs/doc.txt", 400, ago(10), ago(10), ago(6))
	dateFile(t, db, share, "docs/doc.txt", 300, ago(10), ago(6), ago(2))
	dateFile(t, db, share, "docs/doc.txt", 200, ago(10), ago(2), none)

	// gone.txt was made 8 hours ago and deleted 4 hours ago.
	plantPiece(t, db, share, acc, "docs/gone.txt", types.Hash256{2}, 0, 500)
	if _, err := db.DeleteFile(acc, share, "docs/gone.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	dateFile(t, db, share, "docs/gone.txt", 500, ago(8), ago(8), ago(4))

	// quick.txt was made 9 hours ago and last saved 3 hours ago, keeping no version.
	plantPiece(t, db, share, acc, "docs/quick.txt", types.Hash256{3}, 0, 50)
	dateFile(t, db, share, "docs/quick.txt", 50, ago(9), ago(3), none)

	// new.txt and the folder late appeared an hour ago, the folder sub 7 hours ago.
	plantPiece(t, db, share, acc, "docs/new.txt", types.Hash256{4}, 0, 100)
	dateFile(t, db, share, "docs/new.txt", 100, ago(1), ago(1), none)
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE directories SET created_at = $1 WHERE full_path = '/docs/sub'`, ago(7)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE directories SET created_at = $1 WHERE full_path = '/docs/late'`, ago(1))
		return err
	})
	if err != nil {
		t.Fatalf("dating the folders: %v", err)
	}

	// An entry is a file with its size and modified time, or a folder with neither.
	type entry struct {
		path     string
		size     uint64
		modified time.Time
	}
	tests := []struct {
		name string
		at   time.Time
		want []entry
	}{
		{"before most of it", ago(9), []entry{{"/docs/doc.txt", 400, ago(10)}, {"/docs/quick.txt", 50, ago(3)}}},
		{"halfway", ago(5), []entry{{"/docs/doc.txt", 300, ago(6)}, {"/docs/gone.txt", 500, ago(8)}, {"/docs/quick.txt", 50, ago(3)}, {"/docs/sub", 0, none}}},
		{"after the deletion", ago(3), []entry{{"/docs/doc.txt", 300, ago(6)}, {"/docs/quick.txt", 50, ago(3)}, {"/docs/sub", 0, none}}},
		{"now", base, []entry{{"/docs/doc.txt", 200, ago(2)}, {"/docs/late", 0, none}, {"/docs/new.txt", 100, ago(1)}, {"/docs/quick.txt", 50, ago(3)}, {"/docs/sub", 0, none}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects, err := db.ListObjectsAt(acc, share, "docs", tt.at)
			if err != nil {
				t.Fatalf("ListObjectsAt: %v", err)
			}
			if len(objects) != len(tt.want) {
				t.Fatalf("want %d entries %v, got %v", len(tt.want), tt.want, objects)
			}
			for i, want := range tt.want {
				got := objects[i]
				if got.Path != want.path || got.Size != want.size || got.IsDir != want.modified.IsZero() {
					t.Errorf("entry %d: want %+v, got %+v", i, want, got)
				}
				if !want.modified.IsZero() && !got.ModifiedAt.Equal(want.modified) {
					t.Errorf("%s: want modified %v, got %v", want.path, want.modified, got.ModifiedAt)
				}
			}
		})
	}

	// A folder the account does not see, or that is not there, lists nothing.
	if objects, err := db.ListObjectsAt(acc, share, "elsewhere", ago(5)); err != nil || len(objects) != 0 {
		t.Errorf("listing a folder not there: want nothing, got %v, %v", objects, err)
	}

	// A file resolves at a time to the same copy the listing shows: a version, or the file itself.
	v, err := db.ObjectAt(acc, share, "docs/doc.txt", ago(5))
	if err != nil || v.ID == 0 || v.Size != 300 || !v.ReplacedAt.Equal(ago(2)) {
		t.Errorf("doc.txt halfway: want the version of 300 bytes replaced 2 hours ago, got %+v, %v", v, err)
	}
	v, err = db.ObjectAt(acc, share, "docs/doc.txt", base)
	if err != nil || v.ID != 0 || v.Size != 200 {
		t.Errorf("doc.txt now: want the file itself, got %+v, %v", v, err)
	}
	if v, err := db.ObjectAt(acc, share, "docs/gone.txt", ago(5)); err != nil || v.Size != 500 {
		t.Errorf("gone.txt halfway: want the deleted contents, got %+v, %v", v, err)
	}
	if _, err := db.ObjectAt(acc, share, "docs/gone.txt", ago(3)); !errors.Is(err, ErrNotFound) {
		t.Errorf("gone.txt after the deletion: want %v, got %v", ErrNotFound, err)
	}
	if _, err := db.ObjectAt(acc, share, "docs/new.txt", ago(5)); !errors.Is(err, ErrNotFound) {
		t.Errorf("new.txt before it was made: want %v, got %v", ErrNotFound, err)
	}

	// The folder has a previous state at every time a version under it was written, by the second.
	want := []time.Time{ago(6), ago(8), ago(10)}
	for _, dir := range []string{"docs", ""} {
		times, err := db.Snapshots(share, dir)
		if err != nil {
			t.Fatalf("Snapshots(%q): %v", dir, err)
		}
		if len(times) != len(want) {
			t.Fatalf("Snapshots(%q): want %v, got %v", dir, want, times)
		}
		for i := range want {
			if !times[i].Equal(want[i]) {
				t.Errorf("Snapshots(%q)[%d]: want %v, got %v", dir, i, want[i], times[i])
			}
		}
	}
	if times, err := db.Snapshots(share, "docs/sub"); err != nil || len(times) != 0 {
		t.Errorf("Snapshots(docs/sub): want none, got %v, %v", times, err)
	}
}

// TestEmptiedFolderIsEmpty verifies that a folder whose files were deleted
// counts as empty for all the versions they left, and goes with them.
func TestEmptiedFolderIsEmpty(t *testing.T) {
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
	if empty, err := db.DirectoryEmpty(acc, share, "docs"); err != nil || empty {
		t.Fatalf("a folder with a file: want not empty, got %v, %v", empty, err)
	}
	if _, err := db.DeleteFile(acc, share, "docs/doc.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if empty, err := db.DirectoryEmpty(acc, share, "docs"); err != nil || !empty {
		t.Fatalf("a folder with a deleted file: want empty, got %v, %v", empty, err)
	}

	slabs, err := db.DeleteDirectory(acc, share, "docs")
	if err != nil {
		t.Fatalf("DeleteDirectory: %v", err)
	}
	assertSlabs(t, "DeleteDirectory(docs)", slabs, []types.Hash256{own})
	if sizes := versionSizes(t, db, share, "docs/doc.txt"); len(sizes) != 0 {
		t.Errorf("want the versions gone with the folder, got %v", sizes)
	}
}

// TestExpireVersions verifies that the versions past the age go, on every
// share, with their slabs staged for unpinning, and none with no age set.
func TestExpireVersions(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	replaceFile(t, db, share, acc, "doc.txt", 200)

	versions, err := db.Versions(share, "doc.txt")
	if err != nil || len(versions) != 2 {
		t.Fatalf("want two versions, got %+v, %v", versions, err)
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE versions SET replaced_at = NOW() - INTERVAL '40 days' WHERE object_id = $1`, int64(versions[1].ID))
		return err
	})
	if err != nil {
		t.Fatalf("aging the version: %v", err)
	}

	if n, err := db.ExpireVersions(0); err != nil || n != 0 {
		t.Errorf("ExpireVersions(0): want nothing dropped, got %d, %v", n, err)
	}
	n, err := db.ExpireVersions(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("ExpireVersions: %v", err)
	}
	if n != 1 {
		t.Errorf("want one version dropped, got %d", n)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 || sizes[0] != 300 {
		t.Errorf("want the newer version left, got %v", sizes)
	}
	if n := pendingUnpins(t, db); n != 1 {
		t.Errorf("want the expired version's slab staged for unpinning, got %d staged", n)
	}
}

// TestKeptVersionsStay verifies that a kept version is neither counted nor
// dropped by the limits until it is released, and goes on an explicit delete.
func TestKeptVersionsStay(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 2})

	acc, share, _ := newSlabTestFixture(t, db)
	plantPiece(t, db, share, acc, "doc.txt", types.Hash256{1}, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	replaceFile(t, db, share, acc, "doc.txt", 200)

	versions, err := db.Versions(share, "doc.txt")
	if err != nil || len(versions) != 2 || versions[0].Kept || versions[1].Kept {
		t.Fatalf("want two versions, neither kept, got %+v, %v", versions, err)
	}
	oldest := versions[1].ID
	if err := db.KeepVersion(share, "doc.txt", oldest+1000, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("keeping a version that is not there: want ErrNotFound, got %v", err)
	}
	if err := db.KeepVersion(share, "other.txt", oldest, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("keeping a version under another path: want ErrNotFound, got %v", err)
	}
	if err := db.KeepVersion(share, "doc.txt", oldest, true); err != nil {
		t.Fatalf("KeepVersion: %v", err)
	}
	if versions, _ = db.Versions(share, "doc.txt"); len(versions) != 2 || !versions[1].Kept || versions[0].Kept {
		t.Errorf("want the oldest version listed as kept, got %+v", versions)
	}

	replaceFile(t, db, share, acc, "doc.txt", 100)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 3 || sizes[2] != 400 {
		t.Errorf("want the kept version left out of the count, got %v", sizes)
	}
	replaceFile(t, db, share, acc, "doc.txt", 50)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 3 || sizes[0] != 100 || sizes[1] != 200 || sizes[2] != 400 {
		t.Errorf("want the version beyond the count dropped around the kept one, got %v", sizes)
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE versions SET replaced_at = NOW() - INTERVAL '40 days'`)
		return err
	})
	if err != nil {
		t.Fatalf("aging the versions: %v", err)
	}
	if n, err := db.ExpireVersions(30 * 24 * time.Hour); err != nil || n != 2 {
		t.Errorf("want the two unkept versions expired, got %d, %v", n, err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 || sizes[0] != 400 {
		t.Errorf("want the kept version left, got %v", sizes)
	}

	if err := db.KeepVersion(share, "doc.txt", oldest, false); err != nil {
		t.Fatalf("KeepVersion(false): %v", err)
	}
	if n, err := db.ExpireVersions(30 * 24 * time.Hour); err != nil || n != 1 {
		t.Errorf("want the released version expired, got %d, %v", n, err)
	}

	replaceFile(t, db, share, acc, "doc.txt", 40)
	versions, _ = db.Versions(share, "doc.txt")
	if len(versions) != 1 {
		t.Fatalf("want one version, got %+v", versions)
	}
	if err := db.KeepVersion(share, "doc.txt", versions[0].ID, true); err != nil {
		t.Fatalf("KeepVersion: %v", err)
	}
	if _, err := db.DeleteVersions(share, "doc.txt", versions[0].ID); err != nil {
		t.Fatalf("DeleteVersions: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want the kept version deleted on request, got %v", sizes)
	}
}

// TestRestoreVersion verifies that a restored version becomes the file again,
// sharing its slabs and copying its buffers, with what it replaced kept.
func TestRestoreVersion(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	own := types.Hash256{1}
	plantPiece(t, db, share, acc, "doc.txt", own, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	versions, err := db.Versions(share, "doc.txt")
	if err != nil || len(versions) != 1 {
		t.Fatalf("want the one version, got %+v, %v", versions, err)
	}
	jobs := pendingJobs(t, db)

	// The slab version comes back sharing its slab, and the buffered contents become a version.
	slabs, err := db.RestoreVersion(acc, share, "doc.txt", versions[0].ID)
	if err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	if len(slabs) != 0 {
		t.Errorf("want nothing released, got %v", slabs)
	}
	if obj, err := db.Object(acc, share, "doc.txt"); err != nil || obj.Size != 400 {
		t.Errorf("want the file at 400 bytes, got %+v, %v", obj, err)
	}
	if md, err := db.GetMetadata(acc, share, "doc.txt", 0, 400); err != nil || len(md) != 1 || md[0].Key != own {
		t.Errorf("want the file made of the version's slab, got %+v, %v", md, err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 || sizes[1] != 400 {
		t.Errorf("want the replaced contents kept ahead of the version, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 300 {
		t.Errorf("want the buffers as they were, got %d bytes", n)
	}

	// A buffered version comes back as a copy of its bytes, queued for upload.
	versions, _ = db.Versions(share, "doc.txt")
	if _, err := db.RestoreVersion(acc, share, "doc.txt", versions[0].ID); err != nil {
		t.Fatalf("RestoreVersion of the buffered version: %v", err)
	}
	if obj, err := db.Object(acc, share, "doc.txt"); err != nil || obj.Size != 300 {
		t.Errorf("want the file at 300 bytes, got %+v, %v", obj, err)
	}
	if n := bufferedBytes(t, db); n != 600 {
		t.Errorf("want the version's buffer copied, got %d bytes", n)
	}
	if n := pendingJobs(t, db); n != jobs+1 {
		t.Errorf("want the copy queued for upload, got %d jobs rather than %d", n, jobs+1)
	}
	// The file held the slab version unchanged, which is kept already, so nothing is added.
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 {
		t.Errorf("want no version added for a restored version, got %v", sizes)
	}

	// A version the file does not have, and a file somebody else is writing.
	if _, err := db.RestoreVersion(acc, share, "doc.txt", 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown version: want %v, got %v", ErrNotFound, err)
	}
	if _, err := db.RestoreVersion(acc, share, "other.txt", versions[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a version under another path: want %v, got %v", ErrNotFound, err)
	}
	if _, err := db.CreateUpload(acc, share, "doc.txt"); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, err := db.RestoreVersion(acc, share, "doc.txt", versions[0].ID); !errors.Is(err, ErrUploadInFlight) {
		t.Errorf("a file being written: want %v, got %v", ErrUploadInFlight, err)
	}
}

// TestRestoredVersionIsNotKeptAgain verifies that a file holding a restored
// version unchanged adds no version when replaced deliberately, until it is
// edited or the version is gone.
func TestRestoredVersionIsNotKeptAgain(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	plantPiece(t, db, share, acc, "doc.txt", types.Hash256{1}, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	versions, err := db.Versions(share, "doc.txt")
	if err != nil || len(versions) != 1 {
		t.Fatalf("want the one version, got %+v, %v", versions, err)
	}
	first := versions[0].ID

	// Written over SMB, the file is marked with the one of the versions that is of its size.
	if err := db.MarkRestored(share, "doc.txt", []uint64{first}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkRestored at another size: want %v, got %v", ErrNotFound, err)
	}
	if _, err := db.EmptyFile(acc, share, "doc.txt", true); err != nil {
		t.Fatalf("EmptyFile: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 {
		t.Errorf("want the 300 bytes kept, being no version's, got %v", sizes)
	}
	replaceFile(t, db, share, acc, "doc.txt", 400)
	both, _ := db.Versions(share, "doc.txt")
	if err := db.MarkRestored(share, "doc.txt", []uint64{both[0].ID, both[1].ID}); err != nil {
		t.Fatalf("MarkRestored among two versions: %v", err)
	}
	if _, err := db.EmptyFile(acc, share, "doc.txt", true); err != nil {
		t.Fatalf("EmptyFile of the restored version: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 {
		t.Errorf("want nothing added for the restored version, got %v", sizes)
	}

	// Restored through the API and edited since, the file is kept again.
	if _, err := db.RestoreVersion(acc, share, "doc.txt", first); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	replaceFile(t, db, share, acc, "doc.txt", 200)
	if _, err := db.RestoreVersion(acc, share, "doc.txt", first); err != nil {
		t.Fatalf("RestoreVersion after an edit: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 3 || sizes[0] != 200 {
		t.Errorf("want the edited contents kept, got %v", sizes)
	}

	// With the version it holds gone, the file is the only copy, and is kept.
	if _, err := db.DeleteVersions(share, "doc.txt", first); err != nil {
		t.Fatalf("DeleteVersions: %v", err)
	}
	if _, err := db.EmptyFile(acc, share, "doc.txt", true); err != nil {
		t.Fatalf("EmptyFile with the version gone: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 3 || sizes[0] != 400 {
		t.Errorf("want the only copy kept, got %v", sizes)
	}

	// Written back from the copy that stood as the file, which the emptying made the newest
	// version, the file is marked with that version by a zero ID.
	replaceFile(t, db, share, acc, "doc.txt", 400)
	if err := db.MarkRestored(share, "doc.txt", []uint64{0}); err != nil {
		t.Fatalf("MarkRestored with the newest version: %v", err)
	}
	if _, err := db.EmptyFile(acc, share, "doc.txt", true); err != nil {
		t.Fatalf("EmptyFile of the copy written back: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 3 {
		t.Errorf("want nothing added for the copy written back, got %v", sizes)
	}
	replaceFile(t, db, share, acc, "doc.txt", 100)
	if err := db.MarkRestored(share, "doc.txt", []uint64{0}); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkRestored with the newest version of another size: want %v, got %v", ErrNotFound, err)
	}
}

// TestRestoreVersionKeepsTheLatestSave verifies that a restore within the
// editing session keeps the save it replaces, where another save would not.
func TestRestoreVersionKeepsTheLatestSave(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, MaxSession: time.Hour, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	plantPiece(t, db, share, acc, "doc.txt", types.Hash256{1}, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	replaceFile(t, db, share, acc, "doc.txt", 200)
	versions, err := db.Versions(share, "doc.txt")
	if err != nil || len(versions) != 1 {
		t.Fatalf("want the one version from before the session, got %+v, %v", versions, err)
	}

	if _, err := db.RestoreVersion(acc, share, "doc.txt", versions[0].ID); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 200 {
		t.Errorf("want the latest save kept by the restore, got %v", sizes)
	}

	// The session continues from the restore: the next save leaves no version.
	replaceFile(t, db, share, acc, "doc.txt", 100)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 200 {
		t.Errorf("want the save after the restore within the session, got %v", sizes)
	}
}

// TestSwapSaveKeepsAVersion verifies that an editor saving by renaming the file
// away, renaming a new one into its place and deleting the old one leaves a
// version, under the session rule, and that the history survives the swap.
func TestSwapSaveKeepsAVersion(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, MaxSession: time.Hour, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	plantPiece(t, db, share, acc, "doc.txt", types.Hash256{1}, 0, 400)

	swapSave := func(size int) []types.Hash256 {
		t.Helper()
		plantBufferedFile(t, db, share, acc, "~new.tmp", size, false)
		if _, err := db.RenameFile(acc, share, "doc.txt", "~old.tmp", false); err != nil {
			t.Fatalf("RenameFile away: %v", err)
		}
		if _, err := db.RenameFile(acc, share, "~new.tmp", "doc.txt", false); err != nil {
			t.Fatalf("RenameFile into place: %v", err)
		}
		slabs, err := db.DeleteFile(acc, share, "~old.tmp")
		if err != nil {
			t.Fatalf("DeleteFile of the old file: %v", err)
		}
		return slabs
	}

	if slabs := swapSave(300); len(slabs) != 0 {
		t.Errorf("the slab of the version was released: %v", slabs)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 || sizes[0] != 400 {
		t.Errorf("want the file swapped out kept, got %v", sizes)
	}
	if obj, err := db.Object(acc, share, "doc.txt"); err != nil || obj.Size != 300 {
		t.Errorf("want the file at 300 bytes, got %+v, %v", obj, err)
	}
	if _, err := db.Object(acc, share, "~old.tmp"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want the old name gone, got %v", err)
	}

	// The next save of the session adds no version, but the history stays.
	swapSave(200)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 1 || sizes[0] != 400 {
		t.Errorf("want the session's saves collapsed, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 200 {
		t.Errorf("want the replaced contents gone, got %d bytes buffered", n)
	}

	// A save after a pause leaves a version of its own.
	ageSession(t, db, 30*time.Minute, 20*time.Minute)
	swapSave(100)
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 200 {
		t.Errorf("want the state before the pause kept, got %v", sizes)
	}

	// Deleted long after it was renamed away, the file is kept under its own name, with its history.
	plantBufferedFile(t, db, share, acc, "~new.tmp", 50, false)
	if _, err := db.RenameFile(acc, share, "doc.txt", "~old.tmp", false); err != nil {
		t.Fatalf("RenameFile away: %v", err)
	}
	if _, err := db.RenameFile(acc, share, "~new.tmp", "doc.txt", false); err != nil {
		t.Fatalf("RenameFile into place: %v", err)
	}
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE displaced SET at = NOW() - interval '10 minutes'`)
		return err
	})
	if err != nil {
		t.Fatalf("ageing the displaced rows: %v", err)
	}
	if _, err := db.DeleteFile(acc, share, "~old.tmp"); err != nil {
		t.Fatalf("DeleteFile long after: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want no versions on the new file, got %v", sizes)
	}
	if sizes := versionSizes(t, db, share, "~old.tmp"); len(sizes) != 3 || sizes[0] != 100 {
		t.Errorf("want the deleted file kept with its history, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 350 {
		t.Errorf("want the new file and the kept contents buffered, got %d bytes", n)
	}

	// A file renamed away from a name nothing has taken is deleted like any other: kept.
	if _, err := db.RenameFile(acc, share, "doc.txt", "gone.txt", false); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}
	if _, err := db.DeleteFile(acc, share, "gone.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := db.Object(acc, share, "doc.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want nothing under the old name, got %v", err)
	}
	if sizes := versionSizes(t, db, share, "gone.txt"); len(sizes) != 1 || sizes[0] != 50 {
		t.Errorf("want the deleted file kept, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 350 {
		t.Errorf("want nothing released, got %d bytes buffered", n)
	}
}

// TestEmptyFileKeepsTheVersions verifies that emptying a file for an overwrite
// keeps its contents as a version under the session rule, and its versions.
func TestEmptyFileKeepsTheVersions(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	db.SetVersioning(VersionPolicy{Keep: true, Gap: 10 * time.Minute, MaxSession: time.Hour, Count: 10})

	acc, share, _ := newSlabTestFixture(t, db)
	plantPiece(t, db, share, acc, "doc.txt", types.Hash256{1}, 0, 400)
	replaceFile(t, db, share, acc, "doc.txt", 300)
	ageSession(t, db, 30*time.Minute, 20*time.Minute)

	slabs, err := db.EmptyFile(acc, share, "doc.txt", false)
	if err != nil {
		t.Fatalf("EmptyFile: %v", err)
	}
	if len(slabs) != 0 {
		t.Errorf("want nothing released, got %v", slabs)
	}
	if _, err := db.Object(acc, share, "doc.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want the file gone from the share, got %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 {
		t.Errorf("want the contents kept ahead of the version, got %v", sizes)
	}

	// The versions are read while the file is gone, by the right the path grants.
	if _, err := db.PathAccess(acc, share, "doc.txt"); err != nil {
		t.Errorf("PathAccess with the file emptied: %v", err)
	}

	// The file written afterwards has the versions; emptied again within the
	// session, its contents go, and the versions stay.
	replaceFile(t, db, share, acc, "doc.txt", 200)
	if _, err := db.EmptyFile(acc, share, "doc.txt", false); err != nil {
		t.Fatalf("EmptyFile within the session: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 2 || sizes[0] != 300 {
		t.Errorf("want the versions as they were, got %v", sizes)
	}
	if n := bufferedBytes(t, db); n != 300 {
		t.Errorf("want the emptied contents gone, got %d bytes buffered", n)
	}
	if _, err := db.EmptyFile(acc, share, "doc.txt", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("emptying what is not there: want %v, got %v", ErrNotFound, err)
	}

	// A deliberate emptying, which a restore makes, keeps the contents within the session too.
	replaceFile(t, db, share, acc, "doc.txt", 100)
	if _, err := db.EmptyFile(acc, share, "doc.txt", true); err != nil {
		t.Fatalf("EmptyFile deliberately: %v", err)
	}
	if sizes := versionSizes(t, db, share, "doc.txt"); len(sizes) != 3 || sizes[0] != 100 {
		t.Errorf("want the contents kept whatever the session, got %v", sizes)
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
