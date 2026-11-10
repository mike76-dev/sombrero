package files

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

var (
	versionContent  = bytes.Repeat([]byte("abcdefghij"), 5)
	versionModified = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
)

// versionedClient is a client whose readme.txt has two versions, the newer
// one readable, and whose deleted.txt is gone but for a version.
func versionedClient() *mockClient {
	mc := managedClient()
	mc.versions = func(p string) ([]client.VersionInfo, error) {
		switch p {
		case "readme.txt":
			return []client.VersionInfo{
				{ID: 7, Size: 50, ModifiedAt: versionModified, ReplacedAt: versionModified.Add(time.Hour), ReplacedBy: "alice"},
				{ID: 3, Size: 20, ModifiedAt: versionModified.Add(-24 * time.Hour), ReplacedAt: versionModified, Kept: true},
			}, nil
		case "deleted.txt":
			return []client.VersionInfo{{ID: 9, Size: 10, ModifiedAt: versionModified, ReplacedAt: versionModified.Add(time.Hour)}}, nil
		}
		return nil, nil
	}
	mc.readVersion = func(p string, id, offset, length uint64, w io.Writer) error {
		if p != "readme.txt" || id != 7 || offset+length > 50 {
			return stores.ErrNotFound
		}
		_, err := w.Write(versionContent[offset : offset+length])
		return err
	}
	return mc
}

// TestListingVersions verifies that a file's versions are listed newest
// first, and that a share keeping none lists none.
func TestListingVersions(t *testing.T) {
	mc := versionedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	r := manage(h, w, http.MethodGet, "versions?path=/readme.txt")
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var got []VersionEntry
	json.NewDecoder(r.Body).Decode(&got)
	if len(got) != 2 || got[0].ID != 7 || got[0].Size != 50 || got[0].ReplacedBy != "alice" || got[0].Kept || got[1].ID != 3 || !got[1].Kept {
		t.Errorf("want the two versions newest first, the older kept, got %+v", got)
	}

	// A deleted file is reached through the versions it left.
	r = manage(h, w, http.MethodGet, "versions?path=/deleted.txt")
	got = nil
	json.NewDecoder(r.Body).Decode(&got)
	if r.Code != http.StatusOK || len(got) != 1 || got[0].ID != 9 {
		t.Errorf("a deleted file: want its one version, got %d %+v", r.Code, got)
	}

	tests := []struct {
		name string
		call string
		want int
	}{
		{"a file without versions", "versions?path=/Projects/plan.pdf", http.StatusOK},
		{"a folder", "versions?path=/Projects", http.StatusBadRequest},
		{"the root", "versions?path=/", http.StatusBadRequest},
		{"not there", "versions?path=/gone.txt", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodGet, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}

	// A share whose client keeps no versions has none to list.
	plain := struct{ client.Client }{mc}
	ph, pw := loggedIn(t, &mockStore{}, &mockServer{client: plain}, "alice", "hunter2")
	r = manage(ph, pw, http.MethodGet, "versions?path=/readme.txt")
	if r.Code != http.StatusOK || r.Body.String() != "[]\n" {
		t.Errorf("without versions: want an empty list, got %d %s", r.Code, r.Body.String())
	}
}

// TestDownloadingAVersion verifies that a version comes back whole and
// ranged, under the file's name, and that an unknown one is not found.
func TestDownloadingAVersion(t *testing.T) {
	mc := versionedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")
	h.chunkSize = func(uint64) uint64 { return 32 }

	r := manage(h, w, http.MethodGet, "version?path=/readme.txt&id=7")
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if !bytes.Equal(r.Body.Bytes(), versionContent) {
		t.Errorf("want the version's content, got %q", r.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Length":      "50",
		"Content-Disposition": `attachment; filename=readme.txt`,
		"Last-Modified":       versionModified.Format(http.TimeFormat),
	} {
		if got := r.Header().Get(name); got != want {
			t.Errorf("%s: want %q, got %q", name, want, got)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/share/docs/version?path=/readme.txt&id=7", nil)
	req.Header.Set("Range", "bytes=10-19")
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), versionContent[10:20]) {
		t.Errorf("a range: want %d with bytes 10-19, got %d %q", http.StatusPartialContent, rec.Code, rec.Body.String())
	}

	tests := []struct {
		name string
		call string
		want int
	}{
		{"unknown version", "version?path=/readme.txt&id=9", http.StatusNotFound},
		{"no version", "version?path=/readme.txt", http.StatusBadRequest},
		{"malformed version", "version?path=/readme.txt&id=x", http.StatusBadRequest},
		{"a folder", "version?path=/Projects&id=7", http.StatusBadRequest},
		{"not there", "version?path=/gone.txt&id=7", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodGet, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
}

// TestRestoringAVersion verifies that a restore reaches the client with the
// version named, under the right to write, and answers for what went wrong.
func TestRestoringAVersion(t *testing.T) {
	mc := versionedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	r := manage(h, w, http.MethodPost, "restore?path=/readme.txt&id=3")
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var e Entry
	json.NewDecoder(r.Body).Decode(&e)
	if e.Path != "/readme.txt" || e.Size != 100 {
		t.Errorf("want the file described, got %+v", e)
	}
	if len(mc.restored) != 1 || mc.restored[0] != (versionRef{"readme.txt", 3}) {
		t.Errorf("want version 3 of readme.txt restored, got %v", mc.restored)
	}

	tests := []struct {
		name string
		call string
		err  error
		want int
	}{
		{"no version", "restore?path=/readme.txt", nil, http.StatusBadRequest},
		{"a folder", "restore?path=/Projects&id=3", nil, http.StatusBadRequest},
		{"not there", "restore?path=/gone.txt&id=3", nil, http.StatusNotFound},
		{"unknown version", "restore?path=/readme.txt&id=9", stores.ErrNotFound, http.StatusNotFound},
		{"read-only folder", "restore?path=/readme.txt&id=3", stores.ErrAccessDenied, http.StatusForbidden},
		{"being written", "restore?path=/readme.txt&id=3", stores.ErrUploadInFlight, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc.restoreErr = tt.err
			if r := manage(h, w, http.MethodPost, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	mc.restoreErr = nil
	if len(mc.restored) != 1 {
		t.Errorf("want nothing else restored, got %v", mc.restored)
	}

	// The policy counts first: alice may not write on pub.
	req := httptest.NewRequest(http.MethodPost, "/share/pub/restore?path=/x.txt&id=3", nil)
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("restoring on a read-only share: want %d, got %d", http.StatusForbidden, rec.Code)
	}
}

// TestDeletingVersions verifies that one version or all of them go on
// request, under the right to delete.
func TestDeletingVersions(t *testing.T) {
	mc := versionedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	if r := manage(h, w, http.MethodDelete, "versions?path=/readme.txt&id=3"); r.Code != http.StatusNoContent {
		t.Errorf("deleting one version: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if r := manage(h, w, http.MethodDelete, "versions?path=/readme.txt"); r.Code != http.StatusNoContent {
		t.Errorf("deleting every version: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if r := manage(h, w, http.MethodDelete, "versions?path=/deleted.txt"); r.Code != http.StatusNoContent {
		t.Errorf("deleting the versions of a deleted file: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	want := []versionRef{{"readme.txt", 3}, {"readme.txt", 0}, {"deleted.txt", 0}}
	if len(mc.versionsDropped) != 3 || mc.versionsDropped[0] != want[0] || mc.versionsDropped[1] != want[1] || mc.versionsDropped[2] != want[2] {
		t.Errorf("want the deletions %v, got %v", want, mc.versionsDropped)
	}

	tests := []struct {
		name string
		call string
		err  error
		want int
	}{
		{"malformed version", "versions?path=/readme.txt&id=0", nil, http.StatusBadRequest},
		{"a folder", "versions?path=/Projects", nil, http.StatusBadRequest},
		{"not there", "versions?path=/gone.txt", nil, http.StatusNotFound},
		{"read-only folder", "versions?path=/readme.txt", stores.ErrAccessDenied, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc.dropVersionsErr = tt.err
			if r := manage(h, w, http.MethodDelete, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	if len(mc.versionsDropped) != 3 {
		t.Errorf("want nothing else deleted, got %v", mc.versionsDropped)
	}
}

// TestKeepingAVersion verifies that a version is kept and released on request,
// under the right to write, and that the call needs a version to name.
func TestKeepingAVersion(t *testing.T) {
	mc := versionedClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	if r := manage(h, w, http.MethodPost, "keep?path=/readme.txt&id=3"); r.Code != http.StatusNoContent {
		t.Errorf("keeping a version: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if r := manage(h, w, http.MethodDelete, "keep?path=/readme.txt&id=3"); r.Code != http.StatusNoContent {
		t.Errorf("releasing a version: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if r := manage(h, w, http.MethodPost, "keep?path=/deleted.txt&id=9"); r.Code != http.StatusNoContent {
		t.Errorf("keeping a version of a deleted file: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	want := []keeping{{versionRef{"readme.txt", 3}, true}, {versionRef{"readme.txt", 3}, false}, {versionRef{"deleted.txt", 9}, true}}
	if len(mc.kept) != 3 || mc.kept[0] != want[0] || mc.kept[1] != want[1] || mc.kept[2] != want[2] {
		t.Errorf("want the markings %v, got %v", want, mc.kept)
	}

	tests := []struct {
		name string
		call string
		err  error
		want int
	}{
		{"no version", "keep?path=/readme.txt", nil, http.StatusBadRequest},
		{"malformed version", "keep?path=/readme.txt&id=x", nil, http.StatusBadRequest},
		{"a folder", "keep?path=/Projects&id=3", nil, http.StatusBadRequest},
		{"not there", "keep?path=/gone.txt&id=3", nil, http.StatusNotFound},
		{"no such version", "keep?path=/readme.txt&id=4", stores.ErrNotFound, http.StatusNotFound},
		{"read-only folder", "keep?path=/readme.txt&id=3", stores.ErrAccessDenied, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc.keepErr = tt.err
			if r := manage(h, w, http.MethodPost, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	if len(mc.kept) != 3 {
		t.Errorf("want nothing else marked, got %v", mc.kept)
	}

	plain := struct{ client.Client }{mc}
	ph, pw := loggedIn(t, &mockStore{}, &mockServer{client: plain}, "alice", "hunter2")
	if r := manage(ph, pw, http.MethodPost, "keep?path=/readme.txt&id=3"); r.Code != http.StatusNotFound {
		t.Errorf("without versions: want %d, got %d (%s)", http.StatusNotFound, r.Code, r.Body.String())
	}
}
