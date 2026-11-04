package stores

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// newGrantFixture sets up a share with an owner account and a second,
// connected workgroup to grant folders to.
func newGrantFixture(t *testing.T, db *Database) (owner Account, share string, grantee Workgroup) {
	t.Helper()

	owner, share, _ = newSlabTestFixture(t, db)
	acc, _ := newForeignAccount(t, db, "bob")
	wg, err := db.FindWorkgroup(uuid.MustParse(acc.Workgroup))
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}

	sh, err := db.GetShare(share)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if err := db.AddConnection(wg, sh, nil); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}

	return owner, share, wg
}

// TestGrantRoundTrip verifies that a grant is stored, raised in place, listed
// from both ends and removed.
func TestGrantRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	owner, share, wg := newGrantFixture(t, db)
	if err := db.CreateDirectory(owner, share, "/folder", true, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}

	if err := db.SetGrant(share, "/folder", wg, LevelWrite); err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	grants, err := db.Grants(share, "/folder")
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 1 || grants[0].Workgroup.UUID != wg.UUID || grants[0].Level != LevelWrite {
		t.Fatalf("wrong grants: %+v", grants)
	}

	if err := db.SetGrant(share, "/folder", wg, LevelAdmin); err != nil {
		t.Fatalf("SetGrant again: %v", err)
	}
	grants, err = db.Grants(share, "/folder")
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 1 || grants[0].Level != LevelAdmin {
		t.Fatalf("the grant was not raised in place: %+v", grants)
	}

	mine, err := db.WorkgroupGrants(wg)
	if err != nil {
		t.Fatalf("WorkgroupGrants: %v", err)
	}
	if len(mine) != 1 || mine[0].ShareName != share || mine[0].Path != "/folder" || mine[0].Level != LevelAdmin {
		t.Fatalf("wrong workgroup grants: %+v", mine)
	}

	if err := db.RemoveGrant(share, "/folder", wg); err != nil {
		t.Fatalf("RemoveGrant: %v", err)
	}
	grants, err = db.Grants(share, "/folder")
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("the grant is still there: %+v", grants)
	}
}

// TestGrantIsRefused verifies the refusals: an unconnected workgroup, a folder
// that does not exist, and a level off the ladder.
func TestGrantIsRefused(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	owner, share, wg := newGrantFixture(t, db)
	if err := db.CreateDirectory(owner, share, "/folder", true, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}

	stranger, _ := newForeignAccount(t, db, "carol")
	strangerWG, err := db.FindWorkgroup(uuid.MustParse(stranger.Workgroup))
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if err := db.SetGrant(share, "/folder", strangerWG, LevelRead); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("an unconnected workgroup was granted: %v", err)
	}

	if err := db.SetGrant(share, "/nowhere", wg, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing folder was granted: %v", err)
	}

	if err := db.SetGrant(share, "/folder", wg, LevelAdmin+1); err == nil {
		t.Fatal("a level off the ladder was accepted")
	}
}

// TestEffectiveLevelCoversTheSubtree verifies that a grant reaches everything
// under its folder, takes the highest ancestor, and stops at name prefixes.
func TestEffectiveLevelCoversTheSubtree(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	owner, share, wg := newGrantFixture(t, db)
	for _, dir := range []string{"/a", "/a/b", "/ab", "/a_b"} {
		if err := db.CreateDirectory(owner, share, dir, true, false); err != nil {
			t.Fatalf("CreateDirectory(%s): %v", dir, err)
		}
	}

	if err := db.SetGrant(share, "/a", wg, LevelRead); err != nil {
		t.Fatalf("SetGrant(/a): %v", err)
	}
	if err := db.SetGrant(share, "/a_b", wg, LevelWrite); err != nil {
		t.Fatalf("SetGrant(/a_b): %v", err)
	}

	assertLevel := func(path string, want GrantLevel, granted bool) {
		t.Helper()
		level, ok, err := db.EffectiveLevel(share, path, wg)
		if err != nil {
			t.Fatalf("EffectiveLevel(%s): %v", path, err)
		}
		if ok != granted || (ok && level != want) {
			t.Fatalf("EffectiveLevel(%s) = %v, %v; want %v, %v", path, level, ok, want, granted)
		}
	}

	assertLevel("/a", LevelRead, true)
	assertLevel("/a/b", LevelRead, true)
	assertLevel("/a/b/deep/file.txt", LevelRead, true)
	// Similar names are not covered: /ab is no child of /a, and the _ in a
	// granted folder's name matches nothing but itself.
	assertLevel("/ab", 0, false)
	assertLevel("/axb", 0, false)
	assertLevel("/", 0, false)

	// The deeper grant raises the subtree, the shallow one keeps its own level.
	if err := db.SetGrant(share, "/a/b", wg, LevelDelete); err != nil {
		t.Fatalf("SetGrant(/a/b): %v", err)
	}
	assertLevel("/a/b", LevelDelete, true)
	assertLevel("/a/b/deep", LevelDelete, true)
	assertLevel("/a", LevelRead, true)
}

// TestGrantLevelNames verifies the level names parse back to themselves.
func TestGrantLevelNames(t *testing.T) {
	for l := LevelRead; l <= LevelAdmin; l++ {
		parsed, err := ParseGrantLevel(l.String())
		if err != nil || parsed != l {
			t.Fatalf("ParseGrantLevel(%s) = %v, %v", l, parsed, err)
		}
	}
	if _, err := ParseGrantLevel("owner"); err == nil {
		t.Fatal("an unknown name parsed")
	}
}
