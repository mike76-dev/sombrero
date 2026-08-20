package stores

import (
	"context"
	"errors"
	"testing"

	"go.sia.tech/core/types"
)

// plantTree creates a directory holding one file, and returns the path of the
// file.
func plantTree(t *testing.T, db *Database, share string, acc Account, dir string, key byte) string {
	t.Helper()

	if err := db.CreateDirectory(acc, share, dir, false, false); err != nil {
		t.Fatalf("CreateDirectory(%s): %v", dir, err)
	}

	path := dir + "/file.txt"
	plantPiece(t, db, share, acc, path, types.Hash256{key}, 0, 100)

	return path
}

// assertPath checks whether a file or a directory is still there.
func assertPath(t *testing.T, db *Database, share string, acc Account, path string, want bool) {
	t.Helper()

	_, err := db.Object(acc, share, path)
	switch {
	case err == nil && !want:
		t.Fatalf("%s should be gone", path)
	case errors.Is(err, ErrNotFound) && want:
		t.Fatalf("%s should still be there", path)
	case err != nil && !errors.Is(err, ErrNotFound):
		t.Fatalf("Object(%s): %v", path, err)
	}
}

// TestDeleteDirectoryWildcardNames verifies that deleting a directory whose
// name holds a LIKE wildcard leaves the siblings that the wildcard would match
// alone.
func TestDeleteDirectoryWildcardNames(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, _ := newSlabTestFixture(t, db)

	// The _ matches a single character and the % any number of them, so each
	// of these names matches the sibling below it.
	underscore := plantTree(t, db, share, acc, "/a_b", 1)
	single := plantTree(t, db, share, acc, "/axb", 2)
	percent := plantTree(t, db, share, acc, "/c%d", 3)
	many := plantTree(t, db, share, acc, "/cxyzd", 4)

	if _, err := db.DeleteDirectory(acc, share, "/a_b"); err != nil {
		t.Fatalf("DeleteDirectory(/a_b): %v", err)
	}
	assertPath(t, db, share, acc, underscore, false)
	assertPath(t, db, share, acc, "/a_b", false)
	assertPath(t, db, share, acc, single, true)
	assertPath(t, db, share, acc, "/axb", true)

	if _, err := db.DeleteDirectory(acc, share, "/c%d"); err != nil {
		t.Fatalf("DeleteDirectory(/c%%d): %v", err)
	}
	assertPath(t, db, share, acc, percent, false)
	assertPath(t, db, share, acc, "/c%d", false)
	assertPath(t, db, share, acc, many, true)
	assertPath(t, db, share, acc, "/cxyzd", true)
}

// TestRenameDirectoryWildcardNames verifies that renaming a directory whose
// name holds a LIKE wildcard moves its own contents and nothing else.
func TestRenameDirectoryWildcardNames(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, _ := newSlabTestFixture(t, db)

	plantTree(t, db, share, acc, "/a_b", 1)
	sibling := plantTree(t, db, share, acc, "/axb", 2)

	if err := db.RenameDirectory(acc, share, "/a_b", "/renamed", false); err != nil {
		t.Fatalf("RenameDirectory: %v", err)
	}
	assertPath(t, db, share, acc, "/renamed/file.txt", true)
	assertPath(t, db, share, acc, "/a_b/file.txt", false)

	// The sibling kept both its name and its file.
	assertPath(t, db, share, acc, sibling, true)
	assertPath(t, db, share, acc, "/axb", true)
}

// TestRenameDirectoryIntoWildcardSibling verifies that a directory whose name
// holds a LIKE wildcard can be moved into a sibling the wildcard matches. Only
// a move into the directory's own subtree is refused.
func TestRenameDirectoryIntoWildcardSibling(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, _ := newSlabTestFixture(t, db)

	plantTree(t, db, share, acc, "/a_b", 1)
	plantTree(t, db, share, acc, "/axb", 2)

	if err := db.RenameDirectory(acc, share, "/a_b", "/axb/moved", false); err != nil {
		t.Fatalf("RenameDirectory: %v", err)
	}
	assertPath(t, db, share, acc, "/axb/moved/file.txt", true)
	assertPath(t, db, share, acc, "/axb/file.txt", true)
	assertPath(t, db, share, acc, "/a_b", false)

	// A directory still cannot be moved inside itself.
	err := db.RenameDirectory(acc, share, "/axb", "/axb/moved/nested", false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want a move into its own subtree refused, got %v", err)
	}
}
