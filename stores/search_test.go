package stores

import (
	"context"
	"sort"
	"testing"

	"go.sia.tech/core/types"
)

// pathsOf lists the paths of the objects, sorted, folders marked with a slash.
func pathsOf(objects []ObjectMeta) []string {
	paths := make([]string, 0, len(objects))
	for _, o := range objects {
		p := o.Path
		if o.IsDir {
			p += "/"
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// TestSearchObjects verifies that a search finds the folders and files whose
// names contain the text, whatever the case, and only those the account may
// list: nothing of another workgroup but what is public.
func TestSearchObjects(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	alice, share, _ := newSlabTestFixture(t, db)
	bob, _ := newForeignAccount(t, db, "bob")
	if err := db.CreateDirectory(alice, share, "Plans", true, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	if err := db.CreateDirectory(alice, share, "Reports", true, false); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	plantPiece(t, db, share, alice, "Plans/report.docx", types.Hash256{1}, 0, 100)
	plantPiece(t, db, share, alice, "Plans/Report-final.pdf", types.Hash256{2}, 0, 100)
	plantPiece(t, db, share, alice, "notes.txt", types.Hash256{3}, 0, 100)
	plantAnonymousFile(t, db, share, "Drop", "dropped-report.txt")

	got, err := db.SearchObjects(alice, share, "REPORT")
	if err != nil {
		t.Fatalf("SearchObjects: %v", err)
	}
	want := []string{"/Drop/dropped-report.txt", "/Plans/Report-final.pdf", "/Plans/report.docx", "/Reports/"}
	if paths := pathsOf(got); len(paths) != len(want) || !equalStrings(paths, want) {
		t.Errorf("want %v, got %v", want, paths)
	}

	// The text matches the name, not the folder it is in.
	if got, err := db.SearchObjects(alice, share, "plans"); err != nil || !equalStrings(pathsOf(got), []string{"/Plans/"}) {
		t.Errorf("want the folder alone, got %v, %v", pathsOf(got), err)
	}

	// Another workgroup sees only what is public; nobody sees anything for no text.
	if got, err := db.SearchObjects(bob, share, "report"); err != nil || !equalStrings(pathsOf(got), []string{"/Drop/dropped-report.txt"}) {
		t.Errorf("another workgroup: want the public file alone, got %v, %v", pathsOf(got), err)
	}
	if got, err := db.SearchObjects(alice, share, "  "); err != nil || len(got) != 0 {
		t.Errorf("no text: want nothing, got %v, %v", got, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
