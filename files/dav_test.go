package files

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mike76-dev/sombrero/client"
)

// davHandler returns the WebDAV handler over the docs share with readme.txt
// and the folder Projects in it.
func davHandler() http.Handler {
	mc := readingClient()
	mc.list = func(p string) ([]client.ObjectInfo, error) {
		switch p {
		case "":
			return []client.ObjectInfo{{Key: "/readme.txt", Size: uint64(len(readmeContent)), ModifiedAt: readmeModified}, {Key: "/Projects/"}}, nil
		case "Projects/":
			return []client.ObjectInfo{{Key: "/Projects/plan.pdf", Size: 5 << 20}}, nil
		}
		return nil, nil
	}
	return New(&mockStore{}, &mockServer{client: mc, partSize: 64}).DAV()
}

// dav sends a WebDAV request with the credentials, if any are given.
func dav(h http.Handler, method, p, user, password string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, p, nil)
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestDAVAuthentication verifies that the share credentials open WebDAV in
// each spelling of the workgroup, and nothing else does.
func TestDAVAuthentication(t *testing.T) {
	h := davHandler()
	depth := map[string]string{"Depth": "0"}

	w := dav(h, "PROPFIND", "/dav/", "", "", depth)
	if w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Errorf("without credentials: want %d asking for Basic, got %d %q", http.StatusUnauthorized, w.Code, w.Header().Get("WWW-Authenticate"))
	}
	for _, tt := range []struct {
		name, user, password string
		want                 int
	}{
		{"workgroup backslash user", `partners\alice`, "hunter2", http.StatusMultiStatus},
		{"workgroup slash user", "partners/alice", "hunter2", http.StatusMultiStatus},
		{"user at workgroup", "alice@partners", "hunter2", http.StatusMultiStatus},
		{"workgroup by UUID", testUUID.String() + `\alice`, "hunter2", http.StatusMultiStatus},
		{"wrong password", `partners\alice`, "letmein", http.StatusUnauthorized},
		{"no workgroup", "alice", "hunter2", http.StatusUnauthorized},
		{"unknown workgroup", `nobody\alice`, "hunter2", http.StatusUnauthorized},
		{"unknown user", `partners\mallory`, "hunter2", http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if w := dav(h, "PROPFIND", "/dav/", tt.user, tt.password, depth); w.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, w.Code, w.Body.String())
			}
		})
	}
}

// TestDAVListing verifies that the root lists the shares the account may
// read, a share its files and folders with their sizes and types, and that
// what is not there, or not the account's to see, is not found.
func TestDAVListing(t *testing.T) {
	h := davHandler()
	depth := map[string]string{"Depth": "1"}

	w := dav(h, "PROPFIND", "/dav/", `partners\alice`, "hunter2", depth)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("root: want %d, got %d (%s)", http.StatusMultiStatus, w.Code, w.Body.String())
	}
	for _, want := range []string{"<D:href>/dav/docs/</D:href>", "<D:href>/dav/pub/</D:href>", "<D:collection"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("root: want %s in the listing, got %s", want, w.Body.String())
		}
	}

	w = dav(h, "PROPFIND", "/dav/docs/", `partners\alice`, "hunter2", depth)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("share: want %d, got %d (%s)", http.StatusMultiStatus, w.Code, w.Body.String())
	}
	for _, want := range []string{
		"<D:href>/dav/docs/readme.txt</D:href>", "<D:href>/dav/docs/Projects/</D:href>",
		"<D:getcontentlength>100</D:getcontentlength>", "<D:getcontenttype>text/plain; charset=utf-8</D:getcontenttype>",
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("share: want %s in the listing, got %s", want, w.Body.String())
		}
	}

	for _, tt := range []struct {
		name string
		path string
		want int
	}{
		{"a folder", "/dav/docs/Projects/", http.StatusMultiStatus},
		{"a file", "/dav/docs/readme.txt", http.StatusMultiStatus},
		{"not there", "/dav/docs/gone.txt", http.StatusNotFound},
		{"a share the account may not read", "/dav/private/", http.StatusNotFound},
		{"a share that does not exist", "/dav/nothing/", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if w := dav(h, "PROPFIND", tt.path, `partners\alice`, "hunter2", depth); w.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, w.Code, w.Body.String())
			}
		})
	}
}

// TestDAVDownload verifies that a file is read whole and by range, with its
// type and size, and that a folder is not a thing to download.
func TestDAVDownload(t *testing.T) {
	h := davHandler()

	w := dav(h, http.MethodGet, "/dav/docs/readme.txt", `partners\alice`, "hunter2", nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), readmeContent) {
		t.Errorf("want the file, got %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type: want text/plain, got %q", ct)
	}
	w = dav(h, http.MethodGet, "/dav/docs/readme.txt", `partners\alice`, "hunter2", map[string]string{"Range": "bytes=10-19"})
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), readmeContent[10:20]) {
		t.Errorf("a range: want %d with bytes 10-19, got %d %q", http.StatusPartialContent, w.Code, w.Body.String())
	}
	w = dav(h, http.MethodHead, "/dav/docs/readme.txt", `partners\alice`, "hunter2", nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != "100" || w.Body.Len() != 0 {
		t.Errorf("HEAD: want %d with the length and no body, got %d %q %d", http.StatusOK, w.Code, w.Header().Get("Content-Length"), w.Body.Len())
	}
	if w := dav(h, http.MethodGet, "/dav/docs/gone.txt", `partners\alice`, "hunter2", nil); w.Code != http.StatusNotFound {
		t.Errorf("a file not there: want %d, got %d", http.StatusNotFound, w.Code)
	}
	if w := dav(h, http.MethodGet, "/dav/docs/Projects/", `partners\alice`, "hunter2", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("a folder: want %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

// writingHandler is davHandler over the managed client, which knows the
// folder rights, with the mock to look at.
func writingHandler() (http.Handler, *mockClient) {
	mc := managedClient()
	mc.list = func(p string) ([]client.ObjectInfo, error) {
		switch p {
		case "":
			return []client.ObjectInfo{{Key: "/readme.txt", Size: uint64(len(readmeContent)), ModifiedAt: readmeModified}, {Key: "/Projects/"}}, nil
		case "Projects/":
			return []client.ObjectInfo{{Key: "/Projects/plan.pdf", Size: 5 << 20}}, nil
		}
		return nil, nil
	}
	return New(&mockStore{}, &mockServer{client: mc, partSize: 64}).DAV(), mc
}

// davBody sends a WebDAV request with a body.
func davBody(h http.Handler, method, p string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, p, bytes.NewReader(body))
	req.SetBasicAuth(`partners\alice`, "hunter2")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestDAVUpload verifies that an upload is stored in parts of the share's
// size, that an empty one stores nothing, and that a folder the account may
// not write refuses it.
func TestDAVUpload(t *testing.T) {
	h, mc := writingHandler()

	body := bytes.Repeat([]byte("x"), 150)
	w := davBody(h, http.MethodPut, "/dav/docs/Projects/new.txt", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT: want %d, got %d (%s)", http.StatusCreated, w.Code, w.Body.String())
	}
	if len(mc.started) != 1 || mc.started[0] != "Projects/new.txt" || len(mc.written) != 3 || len(mc.finished) != 3 {
		t.Errorf("want the upload started and sent in three parts, got %v, %d parts, %d finished", mc.started, len(mc.written), len(mc.finished))
	}
	var got []byte
	for _, part := range mc.written {
		got = append(got, part.data...)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("want the body stored, got %d bytes", len(got))
	}

	if w := davBody(h, http.MethodPut, "/dav/docs/empty.txt", nil, nil); w.Code != http.StatusCreated {
		t.Errorf("an empty PUT: want %d, got %d (%s)", http.StatusCreated, w.Code, w.Body.String())
	}
	if len(mc.started) != 1 {
		t.Errorf("want nothing started for an empty file, got %v", mc.started)
	}
	if w := davBody(h, http.MethodPut, "/dav/docs/Archive/new.txt", body, nil); w.Code < 400 {
		t.Errorf("a read-only folder: want a refusal, got %d", w.Code)
	}
	if w := davBody(h, http.MethodPut, "/dav/docs/Projects", body, nil); w.Code < 400 {
		t.Errorf("over a folder: want a refusal, got %d", w.Code)
	}
	if len(mc.started) != 1 {
		t.Errorf("want nothing else started, got %v", mc.started)
	}
}

// TestDAVManage verifies folders made, files and folders deleted, moved and
// copied, with the refusals of the rights.
func TestDAVManage(t *testing.T) {
	h, mc := writingHandler()

	if w := davBody(h, "MKCOL", "/dav/docs/Projects/2026", nil, nil); w.Code != http.StatusCreated {
		t.Errorf("MKCOL: want %d, got %d (%s)", http.StatusCreated, w.Code, w.Body.String())
	}
	if len(mc.made) != 1 || mc.made[0] != "Projects/2026" {
		t.Errorf("want the folder made, got %v", mc.made)
	}
	if w := davBody(h, "MKCOL", "/dav/docs/Projects", nil, nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("MKCOL over a folder: want %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
	if w := davBody(h, "MKCOL", "/dav/docs/Archive/new", nil, nil); w.Code < 400 {
		t.Errorf("MKCOL in a read-only folder: want a refusal, got %d", w.Code)
	}

	if w := davBody(h, http.MethodDelete, "/dav/docs/readme.txt", nil, nil); w.Code != http.StatusNoContent {
		t.Errorf("DELETE a file: want %d, got %d (%s)", http.StatusNoContent, w.Code, w.Body.String())
	}
	if w := davBody(h, http.MethodDelete, "/dav/docs/Projects/", nil, nil); w.Code != http.StatusNoContent {
		t.Errorf("DELETE a folder: want %d, got %d (%s)", http.StatusNoContent, w.Code, w.Body.String())
	}
	want := []deletion{{"readme.txt", false}, {"Projects/plan.pdf", false}, {"Projects", true}}
	if len(mc.deleted) != len(want) {
		t.Fatalf("want the deletions %v, got %v", want, mc.deleted)
	}
	for i := range want {
		if mc.deleted[i] != want[i] {
			t.Errorf("deletion %d: want %v, got %v", i, want[i], mc.deleted[i])
		}
	}
	if w := davBody(h, http.MethodDelete, "/dav/docs/gone.txt", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("DELETE what is not there: want %d, got %d", http.StatusNotFound, w.Code)
	}
	if w := davBody(h, http.MethodDelete, "/dav/docs/", nil, nil); w.Code < 400 {
		t.Errorf("DELETE a share: want a refusal, got %d", w.Code)
	}

	if w := davBody(h, "MOVE", "/dav/docs/readme.txt", nil, map[string]string{"Destination": "/dav/docs/Projects/readme.txt"}); w.Code != http.StatusCreated {
		t.Errorf("MOVE: want %d, got %d (%s)", http.StatusCreated, w.Code, w.Body.String())
	}
	if len(mc.renamed) != 1 || mc.renamed[0] != (renaming{"readme.txt", "Projects/readme.txt", false, false}) {
		t.Errorf("want the file moved, got %v", mc.renamed)
	}
	if w := davBody(h, "MOVE", "/dav/docs/readme.txt", nil, map[string]string{"Destination": "/dav/docs/Projects/plan.pdf", "Overwrite": "F"}); w.Code != http.StatusPreconditionFailed {
		t.Errorf("MOVE over a file without overwriting: want %d, got %d", http.StatusPreconditionFailed, w.Code)
	}
	if w := davBody(h, "MOVE", "/dav/docs/readme.txt", nil, map[string]string{"Destination": "/dav/pub/readme.txt"}); w.Code < 400 {
		t.Errorf("MOVE to another share: want a refusal, got %d", w.Code)
	}
	if len(mc.renamed) != 1 {
		t.Errorf("want nothing else moved, got %v", mc.renamed)
	}

	// A copy reads the file and stores it anew.
	mc.started, mc.written, mc.finished = nil, nil, nil
	if w := davBody(h, "COPY", "/dav/docs/readme.txt", nil, map[string]string{"Destination": "/dav/docs/Projects/copy.txt"}); w.Code != http.StatusCreated {
		t.Errorf("COPY: want %d, got %d (%s)", http.StatusCreated, w.Code, w.Body.String())
	}
	var got []byte
	for _, part := range mc.written {
		got = append(got, part.data...)
	}
	if len(mc.started) != 1 || mc.started[0] != "Projects/copy.txt" || !bytes.Equal(got, readmeContent) {
		t.Errorf("want the copy stored with the file's contents, got %v and %d bytes", mc.started, len(got))
	}
}

// TestDAVLocks verifies that a lock is taken and respected: a write without
// its token is refused until the lock is released.
func TestDAVLocks(t *testing.T) {
	h, _ := writingHandler()
	lockBody := []byte(`<?xml version="1.0" encoding="utf-8"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>alice</D:owner></D:lockinfo>`)

	w := davBody(h, "LOCK", "/dav/docs/readme.txt", lockBody, map[string]string{"Timeout": "Second-60"})
	if w.Code != http.StatusOK || w.Header().Get("Lock-Token") == "" {
		t.Fatalf("LOCK: want %d with a token, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
	token := w.Header().Get("Lock-Token")

	if w := davBody(h, http.MethodPut, "/dav/docs/readme.txt", []byte("x"), nil); w.Code != http.StatusLocked {
		t.Errorf("PUT without the token: want %d, got %d", http.StatusLocked, w.Code)
	}
	if w := davBody(h, http.MethodPut, "/dav/docs/readme.txt", []byte("x"), map[string]string{"If": "(" + token + ")"}); w.Code != http.StatusCreated {
		t.Errorf("PUT with the token: want %d, got %d (%s)", http.StatusCreated, w.Code, w.Body.String())
	}
	if w := davBody(h, "UNLOCK", "/dav/docs/readme.txt", nil, map[string]string{"Lock-Token": token}); w.Code != http.StatusNoContent {
		t.Errorf("UNLOCK: want %d, got %d (%s)", http.StatusNoContent, w.Code, w.Body.String())
	}
	if w := davBody(h, http.MethodPut, "/dav/docs/readme.txt", []byte("x"), nil); w.Code != http.StatusCreated {
		t.Errorf("PUT after the unlock: want %d, got %d", http.StatusCreated, w.Code)
	}
}

// TestDAVOptions verifies that the handler says what it supports.
func TestDAVOptions(t *testing.T) {
	h := davHandler()
	w := dav(h, http.MethodOptions, "/dav/docs/", `partners\alice`, "hunter2", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("DAV"), "1") {
		t.Errorf("OPTIONS: want %d with the DAV header, got %d %q", http.StatusOK, w.Code, w.Header().Get("DAV"))
	}
}
