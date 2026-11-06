package files

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// upload sends the body as the file at the query's path.
func upload(h http.Handler, from *httptest.ResponseRecorder, query string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/share/docs/file?"+query, bytes.NewReader(body))
	for _, c := range from.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestUploadingAFile verifies that the body reaches the share in parts of
// the share's size, numbered from one, and that the upload is finished with
// every part in order.
func TestUploadingAFile(t *testing.T) {
	mc := readingClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc, partSize: 100}, "alice", "hunter2")

	body := bytes.Repeat([]byte("abcdefghij"), 25)
	r := upload(h, w, "path=/Projects/new.bin", body)
	if r.Code != http.StatusCreated {
		t.Fatalf("want %d, got %d (%s)", http.StatusCreated, r.Code, r.Body.String())
	}
	var entry Entry
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		t.Fatalf("decoding the entry: %v", err)
	}
	if entry.Name != "new.bin" || entry.Path != "/Projects/new.bin" || entry.Size != 250 || entry.Dir {
		t.Errorf("want the new file described, got %+v", entry)
	}

	if len(mc.started) != 1 || mc.started[0] != "Projects/new.bin" {
		t.Errorf("want one upload started for Projects/new.bin, got %v", mc.started)
	}
	if len(mc.written) != 3 {
		t.Fatalf("want 3 parts, got %d", len(mc.written))
	}
	var joined []byte
	for i, part := range mc.written {
		if part.number != i+1 || part.offset != uint64(100*i) {
			t.Errorf("part %d: want number %d at %d, got number %d at %d", i, i+1, 100*i, part.number, part.offset)
		}
		joined = append(joined, part.data...)
	}
	if !bytes.Equal(joined, body) {
		t.Error("the parts do not add up to the body")
	}
	if len(mc.finished) != 3 || mc.finished[0].PartNumber != 1 || mc.finished[2].ETag != "etag-3" {
		t.Errorf("want the upload finished with the three parts, got %v", mc.finished)
	}
	if len(mc.aborted) != 0 {
		t.Errorf("want nothing aborted, got %v", mc.aborted)
	}

	// A body smaller than a part is one part.
	mc.written, mc.finished = nil, nil
	if r := upload(h, w, "path=/small.txt", []byte("hi")); r.Code != http.StatusCreated {
		t.Fatalf("a small file: want %d, got %d (%s)", http.StatusCreated, r.Code, r.Body.String())
	}
	if len(mc.written) != 1 || string(mc.written[0].data) != "hi" || len(mc.finished) != 1 {
		t.Errorf("a small file: want one part, got %v finished as %v", mc.written, mc.finished)
	}
}

// TestUploadRefusals verifies what is refused before anything is stored, and
// that an upload that fails on the way is aborted.
func TestUploadRefusals(t *testing.T) {
	newClient := func() *mockClient {
		mc := readingClient()
		mc.pathAccess = func(p string) (stores.PathAccess, error) {
			switch p {
			case "gone/x.txt":
				return stores.PathAccess{}, stores.ErrNotFound
			case "Projects/locked.txt":
				return stores.PathAccess{}, nil
			}
			return stores.PathAccess{Write: true, Delete: true}, nil
		}
		return mc
	}
	mc := newClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc, partSize: 100}, "alice", "hunter2")

	tests := []struct {
		name  string
		query string
		body  string
		want  int
	}{
		{"the root", "path=/", "x", http.StatusBadRequest},
		{"climbing path", "path=/../x.txt", "x", http.StatusBadRequest},
		{"a folder", "path=/Projects", "x", http.StatusBadRequest},
		{"an empty body", "path=/empty.txt", "", http.StatusBadRequest},
		{"folder not visible", "path=/gone/x.txt", "x", http.StatusNotFound},
		{"folder not writable", "path=/Projects/locked.txt", "x", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := upload(h, w, tt.query, []byte(tt.body)); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	if len(mc.started) != 0 {
		t.Errorf("want no upload started by a refused request, got %v", mc.started)
	}

	// The share's policy counts first: alice may only read pub.
	req := httptest.NewRequest(http.MethodPut, "/share/pub/file?path=/x.txt", strings.NewReader("x"))
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("writing to a read-only share: want %d, got %d", http.StatusForbidden, rec.Code)
	}

	// Somebody else is writing the file.
	busy := newClient()
	busy.startErr = stores.ErrNotFound
	bh, bw := loggedIn(t, &mockStore{}, &mockServer{client: busy, partSize: 100}, "alice", "hunter2")
	if r := upload(bh, bw, "path=/x.txt", []byte("x")); r.Code != http.StatusConflict {
		t.Errorf("a file being written: want %d, got %d", http.StatusConflict, r.Code)
	}

	// A part that cannot be stored, and a finish that fails, both abort.
	failing := newClient()
	failing.writeErr = errStore
	fh, fw := loggedIn(t, &mockStore{}, &mockServer{client: failing, partSize: 100}, "alice", "hunter2")
	if r := upload(fh, fw, "path=/x.txt", []byte("x")); r.Code != http.StatusInternalServerError {
		t.Errorf("a failing part: want %d, got %d", http.StatusInternalServerError, r.Code)
	}
	if len(failing.aborted) != 1 || failing.aborted[0] != "x.txt" {
		t.Errorf("a failing part: want the upload aborted, got %v", failing.aborted)
	}
	unfinished := newClient()
	unfinished.finishErr = errStore
	uh, uw := loggedIn(t, &mockStore{}, &mockServer{client: unfinished, partSize: 100}, "alice", "hunter2")
	if r := upload(uh, uw, "path=/x.txt", []byte("x")); r.Code != http.StatusInternalServerError {
		t.Errorf("a failing finish: want %d, got %d", http.StatusInternalServerError, r.Code)
	}
	if len(unfinished.aborted) != 1 {
		t.Errorf("a failing finish: want the upload aborted, got %v", unfinished.aborted)
	}
}

// TestUploadWithoutFolderRights verifies that a backend that cannot say what
// the account may do at a path is still written to on the share's policy.
func TestUploadWithoutFolderRights(t *testing.T) {
	mc := readingClient()
	plain := struct{ client.Client }{mc}
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: plain, partSize: 100}, "alice", "hunter2")
	if r := upload(h, w, "path=/x.txt", []byte("x")); r.Code != http.StatusCreated {
		t.Errorf("want %d, got %d (%s)", http.StatusCreated, r.Code, r.Body.String())
	}
	if len(mc.started) != 1 {
		t.Errorf("want the upload started, got %v", mc.started)
	}
}
