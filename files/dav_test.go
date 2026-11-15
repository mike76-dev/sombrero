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

// TestDAVReadOnly verifies that this version refuses to write, and says what
// it supports.
func TestDAVReadOnly(t *testing.T) {
	h := davHandler()
	for _, method := range []string{http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "PROPPATCH"} {
		if w := dav(h, method, "/dav/docs/new.txt", `partners\alice`, "hunter2", map[string]string{"Destination": "/dav/docs/other.txt"}); w.Code != http.StatusForbidden {
			t.Errorf("%s: want %d, got %d (%s)", method, http.StatusForbidden, w.Code, w.Body.String())
		}
	}
	w := dav(h, http.MethodOptions, "/dav/docs/", `partners\alice`, "hunter2", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("DAV"), "1") {
		t.Errorf("OPTIONS: want %d with the DAV header, got %d %q", http.StatusOK, w.Code, w.Header().Get("DAV"))
	}
}
