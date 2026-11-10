package files

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// snapshotClient is a versioned client whose Projects folder was made a day
// before the version, held plan.pdf and a deleted file at the time of it, and
// whose readme.txt stood then as it is now.
func snapshotClient() *mockClient {
	mc := versionedClient()
	made := versionModified.Add(-24 * time.Hour)
	mc.object = func(p string) (client.ObjectInfo, error) {
		switch p {
		case "readme.txt":
			return client.ObjectInfo{Key: "/readme.txt", Size: uint64(len(readmeContent)), ModifiedAt: made}, nil
		case "Projects", "Archive":
			return client.ObjectInfo{Key: "/" + p + "/", CreatedAt: made}, nil
		case "Projects/plan.pdf":
			return client.ObjectInfo{Key: "/Projects/plan.pdf", Size: 5 << 20}, nil
		}
		return client.ObjectInfo{}, stores.ErrNotFound
	}
	mc.snapshots = func(p string) ([]time.Time, error) {
		if p != "Projects" && p != "" {
			return nil, nil
		}
		local := time.FixedZone("CET", 3600)
		return []time.Time{versionModified.In(local), made.In(local)}, nil
	}
	mc.listAt = func(p string, at time.Time) ([]client.ObjectInfo, error) {
		if p != "Projects" || !at.Equal(versionModified) {
			return nil, nil
		}
		return []client.ObjectInfo{
			{Key: "/Projects/plan.pdf", Size: 1000, ModifiedAt: made},
			{Key: "/Projects/gone.txt", Size: 10, ModifiedAt: made},
		}, nil
	}
	mc.objectAt = func(p string, at time.Time) (client.VersionInfo, error) {
		switch {
		case p == "readme.txt" && at.Equal(versionModified):
			return client.VersionInfo{ID: 7, Size: 50, ModifiedAt: versionModified}, nil
		case p == "readme.txt" && at.After(versionModified):
			return client.VersionInfo{Size: uint64(len(readmeContent)), ModifiedAt: made}, nil
		}
		return client.VersionInfo{}, stores.ErrNotFound
	}
	mc.read = func(p string, offset, length uint64, w io.Writer) error {
		_, err := w.Write(readmeContent[offset : offset+length])
		return err
	}
	return mc
}

// TestFolderSnapshots verifies that a folder lists the times of its previous
// states, and that a file, a missing folder and a share keeping none do not.
func TestFolderSnapshots(t *testing.T) {
	mc := snapshotClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	for _, p := range []string{"/Projects", "/"} {
		r := manage(h, w, http.MethodGet, "snapshots?path="+p)
		if r.Code != http.StatusOK {
			t.Fatalf("snapshots of %s: want %d, got %d (%s)", p, http.StatusOK, r.Code, r.Body.String())
		}
		var got []string
		json.NewDecoder(r.Body).Decode(&got)
		want := []string{versionModified.UTC().Format(time.RFC3339), versionModified.Add(-24 * time.Hour).UTC().Format(time.RFC3339)}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("snapshots of %s: want the two times newest first in UTC %v, got %v", p, want, got)
		}
	}

	tests := []struct {
		name string
		call string
		want int
	}{
		{"a file", "snapshots?path=/readme.txt", http.StatusBadRequest},
		{"not there", "snapshots?path=/gone", http.StatusNotFound},
		{"a folder not visible", "snapshots?path=/gone/sub", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodGet, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}

	plain := struct{ client.Client }{mc}
	ph, pw := loggedIn(t, &mockStore{}, &mockServer{client: plain}, "alice", "hunter2")
	if r := manage(ph, pw, http.MethodGet, "snapshots?path=/Projects"); r.Code != http.StatusOK || r.Body.String() != "[]\n" {
		t.Errorf("without versions: want an empty list, got %d %s", r.Code, r.Body.String())
	}
}

// TestListingAFolderAtATime verifies that a listing with a time shows the
// folder as it was then, read-only, and nothing of a folder made later.
func TestListingAFolderAtATime(t *testing.T) {
	mc := snapshotClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	at := versionModified.Format(time.RFC3339)
	r := manage(h, w, http.MethodGet, "list?path=/Projects&at="+at)
	if r.Code != http.StatusOK {
		t.Fatalf("listing at a time: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var got Listing
	json.NewDecoder(r.Body).Decode(&got)
	if got.Path != "/Projects" || got.Rights != (FolderRights{}) || len(got.Entries) != 2 {
		t.Fatalf("want the folder as it was, read-only, got %+v", got)
	}
	if got.Entries[1].Name != "gone.txt" || got.Entries[1].Size != 10 || got.Entries[1].Dir {
		t.Errorf("want the deleted file listed, got %+v", got.Entries[1])
	}
	if len(mc.listed) != 0 {
		t.Errorf("want the folder as it is left alone, got %v listed", mc.listed)
	}

	tests := []struct {
		name string
		call string
		want int
	}{
		{"before the folder was made", "list?path=/Projects&at=" + versionModified.Add(-48*time.Hour).Format(time.RFC3339), http.StatusNotFound},
		{"a malformed time", "list?path=/Projects&at=yesterday", http.StatusBadRequest},
		{"a file", "list?path=/readme.txt&at=" + at, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodGet, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}

	plain := struct{ client.Client }{mc}
	ph, pw := loggedIn(t, &mockStore{}, &mockServer{client: plain}, "alice", "hunter2")
	if r := manage(ph, pw, http.MethodGet, "list?path=/Projects&at="+at); r.Code != http.StatusNotFound {
		t.Errorf("without versions: want %d, got %d (%s)", http.StatusNotFound, r.Code, r.Body.String())
	}
}

// TestAFileAtATime verifies that a file downloads and restores as it was at a
// time: a version where one stood then, the file itself where it did.
func TestAFileAtATime(t *testing.T) {
	mc := snapshotClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	at := versionModified.Format(time.RFC3339)
	r := manage(h, w, http.MethodGet, "version?path=/readme.txt&at="+at)
	if r.Code != http.StatusOK || r.Body.String() != string(versionContent) {
		t.Errorf("the version of the time: want %d with its contents, got %d %q", http.StatusOK, r.Code, r.Body.String())
	}
	later := versionModified.Add(time.Hour).Format(time.RFC3339)
	r = manage(h, w, http.MethodGet, "version?path=/readme.txt&at="+later)
	if r.Code != http.StatusOK || r.Body.String() != string(readmeContent) {
		t.Errorf("the file itself at a time: want %d with its contents, got %d %q", http.StatusOK, r.Code, r.Body.String())
	}

	if r := manage(h, w, http.MethodPost, "restore?path=/readme.txt&at="+at); r.Code != http.StatusOK {
		t.Errorf("restoring the version of the time: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if r := manage(h, w, http.MethodPost, "restore?path=/readme.txt&at="+later); r.Code != http.StatusOK {
		t.Errorf("restoring what the file holds: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if len(mc.restored) != 1 || mc.restored[0] != (versionRef{"readme.txt", 7}) {
		t.Errorf("want the one version restored, got %v", mc.restored)
	}

	tests := []struct {
		name   string
		method string
		call   string
		want   int
	}{
		{"before the file was made", http.MethodGet, "version?path=/readme.txt&at=" + versionModified.Add(-48*time.Hour).Format(time.RFC3339), http.StatusNotFound},
		{"a malformed time", http.MethodGet, "version?path=/readme.txt&at=yesterday", http.StatusBadRequest},
		{"neither id nor time", http.MethodGet, "version?path=/readme.txt", http.StatusBadRequest},
		{"neither id nor time to restore", http.MethodPost, "restore?path=/readme.txt", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, tt.method, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
}
