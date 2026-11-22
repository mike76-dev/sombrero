package files

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

var planModified = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

// treeClient is a client with readme.txt at the root and the folder Projects
// holding plan.txt, photo.jpg and the empty folder Empty.
func treeClient() *mockClient {
	contents := map[string][]byte{"readme.txt": readmeContent, "Projects/plan.txt": []byte("the plan"), "Projects/photo.jpg": []byte("jpeg")}
	file := func(key string) client.ObjectInfo {
		return client.ObjectInfo{Key: "/" + key, Size: uint64(len(contents[key])), ModifiedAt: planModified}
	}
	return &mockClient{
		object: func(p string) (client.ObjectInfo, error) {
			switch p {
			case "readme.txt", "Projects/plan.txt", "Projects/photo.jpg":
				return file(p), nil
			case "Projects":
				return client.ObjectInfo{Key: "/Projects/", ModifiedAt: planModified}, nil
			case "Projects/Empty":
				return client.ObjectInfo{Key: "/Projects/Empty/", ModifiedAt: planModified}, nil
			}
			return client.ObjectInfo{}, stores.ErrNotFound
		},
		list: func(p string) ([]client.ObjectInfo, error) {
			switch p {
			case "":
				return []client.ObjectInfo{file("readme.txt"), {Key: "/Projects/", ModifiedAt: planModified}}, nil
			case "Projects/":
				// The folder lists itself first, as the clients do.
				return []client.ObjectInfo{{Key: "/Projects/"}, file("Projects/plan.txt"), file("Projects/photo.jpg"), {Key: "/Projects/Empty/", ModifiedAt: planModified}}, nil
			case "Projects/Empty/":
				return []client.ObjectInfo{{Key: "/Projects/Empty/"}}, nil
			}
			return nil, stores.ErrNotFound
		},
		read: func(p string, offset, length uint64, w io.Writer) error {
			data, ok := contents[p]
			if !ok || offset+length > uint64(len(data)) {
				return stores.ErrNotFound
			}
			_, err := w.Write(data[offset : offset+length])
			return err
		},
	}
}

// archive asks for an archive, by query or by form.
func archive(h http.Handler, from *httptest.ResponseRecorder, method, params string) *httptest.ResponseRecorder {
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, "/share/docs/archive", strings.NewReader(params))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, "/share/docs/archive?"+params, nil)
	}
	for _, c := range from.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// entry is what the tests check of an archive's entry.
type entry struct {
	name    string
	content string
	method  uint16
}

// sameEntries fails the test unless the archive holds the entries, in order.
func sameEntries(t *testing.T, r *httptest.ResponseRecorder, want []entry) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(r.Body.Bytes()), int64(r.Body.Len()))
	if err != nil {
		t.Fatalf("opening the archive: %v", err)
	}
	if len(zr.File) != len(want) {
		names := make([]string, len(zr.File))
		for i, f := range zr.File {
			names[i] = f.Name
		}
		t.Fatalf("want the entries %v, got %v", want, names)
	}
	for i, f := range zr.File {
		if f.Name != want[i].name {
			t.Errorf("entry %d: want %q, got %q", i, want[i].name, f.Name)
			continue
		}
		if f.Method != want[i].method {
			t.Errorf("%s: want method %d, got %d", f.Name, want[i].method, f.Method)
		}
		if !f.Modified.Equal(planModified) {
			t.Errorf("%s: want modified %v, got %v", f.Name, planModified, f.Modified)
		}
		if f.FileInfo().IsDir() != strings.HasSuffix(f.Name, "/") {
			t.Errorf("%s: a folder entry where none is wanted, or the reverse", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("%s: opening: %v", f.Name, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(got) != want[i].content {
			t.Errorf("%s: want %q, got %q (%v)", f.Name, want[i].content, got, err)
		}
	}
}

// TestDownloadingAFolder verifies that a folder comes as an archive named
// after it, with its files and folders at any depth, the empty ones included,
// files already compressed stored as they are.
func TestDownloadingAFolder(t *testing.T) {
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: treeClient()}, "alice", "hunter2")

	r := archive(h, w, http.MethodGet, "path=/Projects")
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Type":        "application/zip",
		"Content-Disposition": `attachment; filename=Projects.zip`,
		"Cache-Control":       "private, no-cache",
	} {
		if got := r.Header().Get(name); got != want {
			t.Errorf("%s: want %q, got %q", name, want, got)
		}
	}
	sameEntries(t, r, []entry{
		{"Projects/", "", zip.Store},
		{"Projects/plan.txt", "the plan", zip.Deflate},
		{"Projects/photo.jpg", "jpeg", zip.Store},
		{"Projects/Empty/", "", zip.Store},
	})

	// The whole share is named after the share.
	r = archive(h, w, http.MethodGet, "path=/")
	if r.Code != http.StatusOK {
		t.Fatalf("the root: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if got := r.Header().Get("Content-Disposition"); got != `attachment; filename=docs.zip` {
		t.Errorf("the root: want the share's name, got %q", got)
	}
	sameEntries(t, r, []entry{
		{"readme.txt", string(readmeContent), zip.Deflate},
		{"Projects/", "", zip.Store},
		{"Projects/plan.txt", "the plan", zip.Deflate},
		{"Projects/photo.jpg", "jpeg", zip.Store},
		{"Projects/Empty/", "", zip.Store},
	})
}

// TestDownloadingASelection verifies that several files and folders, named
// by a form as a selection too long for a query would be, come in one archive
// named after their folder.
func TestDownloadingASelection(t *testing.T) {
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: treeClient()}, "alice", "hunter2")

	r := archive(h, w, http.MethodPost, "path=%2FProjects%2Fphoto.jpg&path=%2FProjects%2FEmpty")
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if got := r.Header().Get("Content-Disposition"); got != `attachment; filename=Projects.zip` {
		t.Errorf("want the folder's name, got %q", got)
	}
	sameEntries(t, r, []entry{
		{"photo.jpg", "jpeg", zip.Store},
		{"Empty/", "", zip.Store},
	})

	// A selection at the root is named after the share, and a single file
	// after its folder.
	r = archive(h, w, http.MethodGet, "path=/readme.txt&path=/Projects")
	if r.Code != http.StatusOK {
		t.Fatalf("at the root: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if got := r.Header().Get("Content-Disposition"); got != `attachment; filename=docs.zip` {
		t.Errorf("at the root: want the share's name, got %q", got)
	}
	sameEntries(t, r, []entry{
		{"readme.txt", string(readmeContent), zip.Deflate},
		{"Projects/", "", zip.Store},
		{"Projects/plan.txt", "the plan", zip.Deflate},
		{"Projects/photo.jpg", "jpeg", zip.Store},
		{"Projects/Empty/", "", zip.Store},
	})
	r = archive(h, w, http.MethodGet, "path=/Projects/plan.txt")
	if got := r.Header().Get("Content-Disposition"); r.Code != http.StatusOK || got != `attachment; filename=Projects.zip` {
		t.Errorf("a single file: want %d and the folder's name, got %d and %q", http.StatusOK, r.Code, got)
	}
}

// TestArchiveRefusals verifies the answers for what cannot be archived, all
// given before any of the archive is sent.
func TestArchiveRefusals(t *testing.T) {
	mc := treeClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	for _, tt := range []struct {
		name   string
		params string
		want   int
	}{
		{"no path", "", http.StatusBadRequest},
		{"climbing path", "path=/../readme.txt", http.StatusBadRequest},
		{"the root among others", "path=/&path=/Projects", http.StatusBadRequest},
		{"not there", "path=/gone", http.StatusNotFound},
		{"one of them not there", "path=/Projects&path=/gone", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := archive(h, w, http.MethodGet, tt.params)
			if r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
			if got := r.Header().Get("Content-Type"); got == "application/zip" {
				t.Errorf("want the refusal in the answer, got an archive")
			}
		})
	}
	if len(mc.reads) != 0 {
		t.Errorf("want nothing read for a refusal, got %v", mc.reads)
	}

	g, gw := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "guest", "")
	if r := archive(g, gw, http.MethodGet, "path=/Projects"); r.Code != http.StatusForbidden {
		t.Errorf("a guest on docs: want %d, got %d", http.StatusForbidden, r.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/share/docs/archive?path=/Projects", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: want %d, got %d", http.StatusUnauthorized, rec.Code)
	}
}
