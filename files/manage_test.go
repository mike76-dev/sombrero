package files

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// manage sends a call without a body to the docs share.
func manage(h http.Handler, from *httptest.ResponseRecorder, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/share/docs/"+path, nil)
	for _, c := range from.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// managedClient is a client with readme.txt, the folder Projects holding
// plan.pdf, and the folder Archive that is read-only to the account.
func managedClient() *mockClient {
	mc := readingClient()
	mc.object = func(p string) (client.ObjectInfo, error) {
		switch p {
		case "readme.txt":
			return client.ObjectInfo{Key: "/readme.txt", Size: 100}, nil
		case "Projects", "Archive":
			return client.ObjectInfo{Key: "/" + p + "/"}, nil
		case "Projects/plan.pdf":
			return client.ObjectInfo{Key: "/Projects/plan.pdf", Size: 5 << 20}, nil
		}
		return client.ObjectInfo{}, stores.ErrNotFound
	}
	mc.pathAccess = func(p string) (stores.PathAccess, error) {
		switch {
		case p == "Archive" || len(p) > 8 && p[:8] == "Archive/":
			return stores.PathAccess{}, nil
		case len(p) > 5 && p[:5] == "gone/":
			return stores.PathAccess{}, stores.ErrNotFound
		}
		return stores.PathAccess{Write: true, Delete: true}, nil
	}
	mc.empty = func(p string) (bool, error) { return p != "Projects/", nil }
	return mc
}

// TestMakingAFolder verifies that a folder is made where the account may
// write, and refused where one is already there.
func TestMakingAFolder(t *testing.T) {
	mc := managedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	r := manage(h, w, http.MethodPost, "folder?path=/Projects/2026")
	if r.Code != http.StatusCreated {
		t.Fatalf("want %d, got %d (%s)", http.StatusCreated, r.Code, r.Body.String())
	}
	var e Entry
	json.NewDecoder(r.Body).Decode(&e)
	if e.Name != "2026" || e.Path != "/Projects/2026" {
		t.Errorf("want the folder described, got %+v", e)
	}
	if len(mc.made) != 1 || mc.made[0] != "Projects/2026" {
		t.Errorf("want Projects/2026 made, got %v", mc.made)
	}

	tests := []struct {
		name string
		path string
		want int
	}{
		{"already there", "/Projects", http.StatusConflict},
		{"a file is there", "/readme.txt", http.StatusConflict},
		{"read-only folder", "/Archive/new", http.StatusForbidden},
		{"folder not visible", "/gone/new", http.StatusNotFound},
		{"the root", "/", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodPost, "folder?path="+tt.path); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	if len(mc.made) != 1 {
		t.Errorf("want nothing else made, got %v", mc.made)
	}

	// A folder that appeared in the meantime is a conflict too.
	raced := managedClient()
	raced.mkdirErr = stores.ErrDirectoryExists
	rh, rw := loggedIn(t, &mockStore{}, &mockServer{client: raced}, "alice", "hunter2")
	if r := manage(rh, rw, http.MethodPost, "folder?path=/new"); r.Code != http.StatusConflict {
		t.Errorf("a folder made in the meantime: want %d, got %d", http.StatusConflict, r.Code)
	}
}

// TestDeleting verifies that files and folders go through their own calls,
// that a folder with something in it needs the call to say so, and that
// the right to delete is checked at the path.
func TestDeleting(t *testing.T) {
	mc := managedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	if r := manage(h, w, http.MethodDelete, "file?path=/readme.txt"); r.Code != http.StatusNoContent {
		t.Errorf("deleting a file: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if len(mc.versionsDropped) != 0 {
		t.Errorf("want the versions left by a plain deletion, got %v", mc.versionsDropped)
	}
	if r := manage(h, w, http.MethodDelete, "file?path=/readme.txt&purge=1"); r.Code != http.StatusNoContent {
		t.Errorf("deleting a file with its versions: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if len(mc.versionsDropped) != 1 || mc.versionsDropped[0] != (versionRef{"readme.txt", 0}) {
		t.Errorf("want every version of the file dropped, got %v", mc.versionsDropped)
	}
	if r := manage(h, w, http.MethodDelete, "folder?path=/Projects"); r.Code != http.StatusConflict {
		t.Errorf("deleting a full folder: want %d, got %d", http.StatusConflict, r.Code)
	}

	// A folder deleted recursively has its files deleted one by one, deepest
	// first, so that each leaves a version; with a purge they go with the
	// folder, and the versions under it after them.
	mc.list = func(p string) ([]client.ObjectInfo, error) {
		switch p {
		case "Projects/":
			return []client.ObjectInfo{{Key: "/Projects/plan.pdf"}, {Key: "/Projects/Old/"}}, nil
		case "Projects/Old/":
			return []client.ObjectInfo{{Key: "/Projects/Old/notes.txt"}}, nil
		}
		return nil, nil
	}
	if r := manage(h, w, http.MethodDelete, "folder?path=/Projects&recursive=1"); r.Code != http.StatusNoContent {
		t.Errorf("deleting a full folder recursively: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if r := manage(h, w, http.MethodDelete, "folder?path=/Projects&recursive=1&purge=1"); r.Code != http.StatusNoContent {
		t.Errorf("deleting a full folder with its versions: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	want := []deletion{
		{"readme.txt", false}, {"readme.txt", false},
		{"Projects/plan.pdf", false}, {"Projects/Old/notes.txt", false}, {"Projects/Old", true}, {"Projects", true},
		{"Projects", true},
	}
	if len(mc.deleted) != len(want) {
		t.Fatalf("want the deletions %v, got %v", want, mc.deleted)
	}
	for i := range want {
		if mc.deleted[i] != want[i] {
			t.Errorf("deletion %d: want %v, got %v", i, want[i], mc.deleted[i])
		}
	}
	if len(mc.versionsDropped) != 2 || mc.versionsDropped[1] != (versionRef{"Projects/", 0}) {
		t.Errorf("want the versions under the purged folder dropped, got %v", mc.versionsDropped)
	}

	tests := []struct {
		name string
		call string
		want int
	}{
		{"a folder as a file", "file?path=/Projects", http.StatusBadRequest},
		{"a file as a folder", "folder?path=/readme.txt", http.StatusBadRequest},
		{"not there", "file?path=/gone.txt", http.StatusNotFound},
		{"read-only folder", "folder?path=/Archive", http.StatusForbidden},
		{"the root", "folder?path=/", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodDelete, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	if len(mc.deleted) != len(want) {
		t.Errorf("want nothing else deleted, got %v", mc.deleted)
	}

	// The policy counts first: alice may not delete on pub.
	req := httptest.NewRequest(http.MethodDelete, "/share/pub/file?path=/x.txt", nil)
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("deleting on a read-only share: want %d, got %d", http.StatusForbidden, rec.Code)
	}
}

// TestRenaming verifies that a move takes the source's kind along, replaces
// only when told to, and is refused into a folder the account may not write.
func TestRenaming(t *testing.T) {
	mc := managedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	r := manage(h, w, http.MethodPost, "rename?path=/readme.txt&to=/Projects/readme.txt")
	if r.Code != http.StatusOK {
		t.Fatalf("moving a file: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var e Entry
	json.NewDecoder(r.Body).Decode(&e)
	if e.Path != "/Projects/readme.txt" || e.Name != "readme.txt" {
		t.Errorf("want the new place described, got %+v", e)
	}
	if r := manage(h, w, http.MethodPost, "rename?path=/Projects&to=/Work"); r.Code != http.StatusOK {
		t.Errorf("moving a folder: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if r := manage(h, w, http.MethodPost, "rename?path=/readme.txt&to=/Projects/plan.pdf&replace=1"); r.Code != http.StatusOK {
		t.Errorf("replacing a file: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	want := []renaming{
		{"readme.txt", "Projects/readme.txt", false, false},
		{"Projects", "Work", true, false},
		{"readme.txt", "Projects/plan.pdf", false, true},
	}
	if len(mc.renamed) != len(want) {
		t.Fatalf("want the moves %v, got %v", want, mc.renamed)
	}
	for i := range want {
		if mc.renamed[i] != want[i] {
			t.Errorf("move %d: want %v, got %v", i, want[i], mc.renamed[i])
		}
	}

	tests := []struct {
		name string
		call string
		want int
	}{
		{"over a file", "rename?path=/readme.txt&to=/Projects/plan.pdf", http.StatusConflict},
		{"a file over a folder", "rename?path=/readme.txt&to=/Projects&replace=1", http.StatusBadRequest},
		{"into itself", "rename?path=/Projects&to=/Projects/sub", http.StatusBadRequest},
		{"onto itself", "rename?path=/Projects&to=/Projects", http.StatusBadRequest},
		{"not there", "rename?path=/gone.txt&to=/x.txt", http.StatusNotFound},
		{"into a read-only folder", "rename?path=/readme.txt&to=/Archive/readme.txt", http.StatusForbidden},
		{"out of a read-only folder", "rename?path=/Archive&to=/Old", http.StatusForbidden},
		{"into a folder not visible", "rename?path=/readme.txt&to=/gone/readme.txt", http.StatusNotFound},
		{"to the root", "rename?path=/readme.txt&to=/", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodPost, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	if len(mc.renamed) != 3 {
		t.Errorf("want nothing else moved, got %v", mc.renamed)
	}

	// A backend that refuses the move over a protected file says so.
	refusing := managedClient()
	refusing.renameErr = stores.ErrAccessDenied
	rh, rw := loggedIn(t, &mockStore{}, &mockServer{client: refusing}, "alice", "hunter2")
	if r := manage(rh, rw, http.MethodPost, "rename?path=/readme.txt&to=/x.txt"); r.Code != http.StatusForbidden {
		t.Errorf("a move the store refuses: want %d, got %d", http.StatusForbidden, r.Code)
	}
}
