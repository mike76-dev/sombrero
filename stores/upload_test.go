package stores

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
)

// sliceLength is the size of every metadata entry planted by plantObject.
const sliceLength = 10

// newSlabTestFixture registers a share and an account that owns the objects
// planted by plantObject.
func newSlabTestFixture(t *testing.T, db *Database) (Account, string) {
	t.Helper()

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}

	if err := db.AddAccount(Account{
		Username:  "alice",
		Password:  "secret123",
		Workgroup: u.String(),
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	acc, err := db.FindAccount("alice", u.String())
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}

	sh := Share{
		Name:         "testshare",
		Type:         "indexd",
		ServerName:   "test-server",
		DataShards:   1,
		ParityShards: 0,
	}
	if err := db.RegisterShare(sh); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}

	return acc, sh.Name
}

// plantObject inserts a visible object whose metadata references the given slab
// keys, one entry per key. Passing the same key to more than one call makes the
// files share a slab, as they will once slabs are packed. Any directory in the
// path has to exist already.
func plantObject(t *testing.T, db *Database, share string, acc Account, path string, keys ...types.Hash256) {
	t.Helper()

	path = normalizePath(path)
	dir, name := splitPath(path)

	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		// The join on directories yields no row for a file in the root
		// folder, leaving directory_id NULL as the schema expects.
		const insertObject = `
			INSERT INTO objects (
				share_name,
				directory_id,
				name,
				full_path,
				size,
				account,
				workgroup,
				temporary
			)
			SELECT $1, d.id, $2, $3, $4, a.id, a.workgroup, FALSE
			FROM accounts a
			LEFT JOIN directories d
				ON d.share_name = $1
				AND d.full_path = $6
			WHERE a.id = $5
			RETURNING id
		`

		var oid uint64
		if err := tx.QueryRow(ctx, insertObject, share, name, path, len(keys)*sliceLength, acc.ID, dir).Scan(&oid); err != nil {
			return err
		}

		const insertMetadata = `
			INSERT INTO metadata (object_id, obj_offset, slab_key, data_offset, data_length)
			VALUES ($1, $2, $3, 0, $4)
		`

		for i, key := range keys {
			if _, err := tx.Exec(ctx, insertMetadata, oid, i*sliceLength, key[:], sliceLength); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("plant object %s: %v", path, err)
	}
}

// assertSlabs compares the slab keys reported by a delete against the expected
// set, ignoring their order.
func assertSlabs(t *testing.T, what string, got, want []types.Hash256) {
	t.Helper()

	sortKeys := func(keys []types.Hash256) {
		sort.Slice(keys, func(i, j int) bool {
			return keys[i].String() < keys[j].String()
		})
	}
	sortKeys(got)
	sortKeys(want)

	if len(got) != len(want) {
		t.Fatalf("%s: want %d unreferenced slabs %v, got %d %v", what, len(want), want, len(got), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: want unreferenced slabs %v, got %v", what, want, got)
		}
	}
}

// TestDeleteFileSharedSlab verifies that deleting a file only reports the slabs
// that no other file references any more. A slab shared with a surviving file
// must stay pinned, or deleting one small file would destroy the data of every
// other file packed into the same slab.
func TestDeleteFileSharedSlab(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	shared := types.Hash256{1}
	sole := types.Hash256{2}

	plantObject(t, db, share, acc, "a.txt", shared)
	plantObject(t, db, share, acc, "b.txt", shared, sole)

	// The first delete leaves the shared slab referenced by b.txt.
	slabs, err := db.DeleteFile(acc, share, "a.txt")
	if err != nil {
		t.Fatalf("DeleteFile(a.txt): %v", err)
	}
	assertSlabs(t, "DeleteFile(a.txt)", slabs, nil)

	// The last reference is gone, so both of b.txt's slabs can be unpinned.
	slabs, err = db.DeleteFile(acc, share, "b.txt")
	if err != nil {
		t.Fatalf("DeleteFile(b.txt): %v", err)
	}
	assertSlabs(t, "DeleteFile(b.txt)", slabs, []types.Hash256{shared, sole})
}

// TestDeleteDirectorySharedSlab verifies that deleting a directory keeps a slab
// pinned while a file outside of it still references that slab, and reports it
// once that file is deleted too.
func TestDeleteDirectorySharedSlab(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share := newSlabTestFixture(t, db)

	if err := db.CreateDirectory(acc, share, "docs", true, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}

	shared := types.Hash256{1}
	inner := types.Hash256{2}

	plantObject(t, db, share, acc, "docs/inside.txt", shared, inner)
	plantObject(t, db, share, acc, "outside.txt", shared)

	slabs, err := db.DeleteDirectory(acc, share, "docs")
	if err != nil {
		t.Fatalf("DeleteDirectory(docs): %v", err)
	}
	assertSlabs(t, "DeleteDirectory(docs)", slabs, []types.Hash256{inner})

	slabs, err = db.DeleteFile(acc, share, "outside.txt")
	if err != nil {
		t.Fatalf("DeleteFile(outside.txt): %v", err)
	}
	assertSlabs(t, "DeleteFile(outside.txt)", slabs, []types.Hash256{shared})
}
