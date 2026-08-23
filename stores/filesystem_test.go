package stores

import (
	"context"
	"errors"
	"strings"
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

// TestCurrentAndParent verifies that a directory is described along with the one
// above it, rather than with the one it is itself inside.
func TestCurrentAndParent(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, _ := newSlabTestFixture(t, db)

	for _, dir := range []string{"/a", "/a/b", "/a/b/c"} {
		if err := db.CreateDirectory(acc, share, dir, false, false); err != nil {
			t.Fatalf("CreateDirectory(%s): %v", dir, err)
		}
	}

	current, parent, err := db.CurrentAndParent(acc, share, "/a/b/c")
	if err != nil {
		t.Fatalf("CurrentAndParent: %v", err)
	}
	if current.Path != "/a/b/c" || parent.Path != "/a/b" {
		t.Fatalf("want /a/b/c and /a/b, got %q and %q", current.Path, parent.Path)
	}

	// A directory of the root has no parent to name: the share stands in for it.
	current, parent, err = db.CurrentAndParent(acc, share, "/a")
	if err != nil {
		t.Fatalf("CurrentAndParent(/a): %v", err)
	}
	if current.Path != "/a" || parent.Path != "" {
		t.Fatalf("want /a and the root, got %q and %q", current.Path, parent.Path)
	}

	// The root is neither of them.
	current, parent, err = db.CurrentAndParent(acc, share, "/")
	if err != nil {
		t.Fatalf("CurrentAndParent(/): %v", err)
	}
	if current.Path != "" || parent.Path != "" {
		t.Fatalf("want the root twice, got %q and %q", current.Path, parent.Path)
	}

	if _, _, err := db.CurrentAndParent(acc, share, "/a/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want a directory that is not there reported, got %v", err)
	}
}

// plantAnonymousFile puts a file owned by the reserved identity into the public
// folder, as an anonymous session's upload leaves it.
func plantAnonymousFile(t *testing.T, db *Database, share, folder, name string) Account {
	t.Helper()

	acc, err := db.EnsureAnonymous()
	if err != nil {
		t.Fatalf("EnsureAnonymous: %v", err)
	}
	if err := db.EnsurePublicDir(share, folder); err != nil {
		t.Fatalf("EnsurePublicDir: %v", err)
	}
	plantPiece(t, db, share, acc, folder+"/"+name, types.Hash256{7}, 0, 100)

	return acc
}

// TestAnonymousDropsAreSharedWithEverybody verifies the rule the public folder
// rests on: what the reserved identity owns is visible to every member of the
// share, whatever workgroup they are in, while what they own stays as private
// from an anonymous session as it is from each other.
func TestAnonymousDropsAreSharedWithEverybody(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, _ := newSlabTestFixture(t, db)
	bob, _ := newForeignAccount(t, db, "bob")

	anon := plantAnonymousFile(t, db, share, "Drop", "dropped.txt")

	// A member of one workgroup and a member of another both see the drop.
	for _, who := range []struct {
		name string
		acc  Account
	}{{"the share's own workgroup", acc}, {"another workgroup", bob}} {
		if _, err := db.Object(who.acc, share, "Drop/dropped.txt"); err != nil {
			t.Errorf("%s cannot see the drop: %v", who.name, err)
		}
		if _, err := db.Object(who.acc, share, "Drop"); err != nil {
			t.Errorf("%s cannot see the public folder: %v", who.name, err)
		}
	}

	// What a real user puts on the share is not visible to an anonymous
	// session, which is the half that must not become symmetric.
	if err := db.CreateDirectory(acc, share, "Private", true, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	plantPiece(t, db, share, acc, "Private/secret.txt", types.Hash256{8}, 0, 100)
	if _, err := db.Object(anon, share, "Private/secret.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an anonymous session can see another user's file: %v", err)
	}
	if _, err := db.Object(anon, share, "Private"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an anonymous session can see another user's folder: %v", err)
	}
}

// TestPublicDirIsRefusedToSomebodyElsesFolder verifies that a folder of that
// name which another workgroup already made is left where it is: taking it over
// would hand one workgroup's directory to every member of the share.
func TestPublicDirIsRefusedToSomebodyElsesFolder(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, _ := newSlabTestFixture(t, db)
	if _, err := db.EnsureAnonymous(); err != nil {
		t.Fatalf("EnsureAnonymous: %v", err)
	}

	if err := db.CreateDirectory(acc, share, "Drop", false, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}

	err := db.EnsurePublicDir(share, "Drop")
	if err == nil {
		t.Fatal("EnsurePublicDir took over a folder of another workgroup")
	}
	if !strings.Contains(err.Error(), "belongs to another workgroup") {
		t.Fatalf("EnsurePublicDir: want the owner named, got %v", err)
	}
}

// TestAnonymousDropsAreListedToEverybody is the drop as a user of the share
// finds it: by listing the share, rather than by asking for the path. The two
// go through different queries, and a folder that answers a lookup but never
// appears in a listing is a share that looks empty to everyone but the
// anonymous session that filled it.
func TestAnonymousDropsAreListedToEverybody(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	acc, share, _ := newSlabTestFixture(t, db)
	bob, _ := newForeignAccount(t, db, "bob")

	plantAnonymousFile(t, db, share, "Drop", "dropped.txt")

	for _, who := range []struct {
		name string
		acc  Account
	}{{"the share's own workgroup", acc}, {"another workgroup", bob}} {
		root, err := db.ListObjects(who.acc, share, "/")
		if err != nil {
			t.Fatalf("%s: ListObjects(/): %v", who.name, err)
		}
		var found bool
		for _, oi := range root {
			if oi.Path == "/Drop" && oi.IsDir {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not see the public folder in the share: %+v", who.name, root)
		}

		inside, err := db.ListObjects(who.acc, share, "/Drop")
		if err != nil {
			t.Fatalf("%s: ListObjects(/Drop): %v", who.name, err)
		}
		found = false
		for _, oi := range inside {
			if oi.Path == "/Drop/dropped.txt" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not see the drop in the folder: %+v", who.name, inside)
		}
	}
}
