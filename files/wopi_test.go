package files

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

const discoveryXML = `<?xml version="1.0" encoding="UTF-8"?>
<wopi-discovery><net-zone name="external-http"><app name="text/plain">
<action name="edit" default="true" ext="txt" urlsrc="http://office.example/browser/abc/cool.html?"/>
<action name="view" ext="txt" urlsrc="http://office.example/browser/abc/cool.html?"/>
</app></net-zone></wopi-discovery>`

// editingHandler returns a handler with alice logged in and an editor whose
// discovery document a test server answers.
func editingHandler(t *testing.T, mc *mockClient, ms *mockStore) (*Handler, *httptest.ResponseRecorder) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hosting/discovery" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, discoveryXML)
	}))
	t.Cleanup(ts.Close)
	h, w := loggedIn(t, ms, &mockServer{client: mc, partSize: 64}, "alice", "hunter2")
	if err := h.SetEditor(ts.URL, "https://files.example"); err != nil {
		t.Fatalf("SetEditor: %v", err)
	}
	return h, w
}

// editSession opens an editing session on the path.
func editSession(t *testing.T, h http.Handler, w *httptest.ResponseRecorder, p string) EditSession {
	t.Helper()
	r := manage(h, w, http.MethodGet, "edit?path="+p)
	if r.Code != http.StatusOK {
		t.Fatalf("edit %s: want %d, got %d (%s)", p, http.StatusOK, r.Code, r.Body.String())
	}
	var es EditSession
	json.NewDecoder(r.Body).Decode(&es)
	return es
}

// wopi sends the editor's call on the file with the access token.
func wopi(h http.Handler, method, file, token string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/wopi/files/"+file+"?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestEditSession verifies that an editing session names the editor's URL
// with this server's file in it, that the token answers CheckFileInfo with
// the file and the rights, and what is refused.
func TestEditSession(t *testing.T) {
	mc := readingClient()
	object := mc.object
	mc.object = func(p string) (client.ObjectInfo, error) {
		if p == "plan.pdf" {
			return client.ObjectInfo{Key: "/plan.pdf", Size: 5}, nil
		}
		return object(p)
	}
	h, w := editingHandler(t, mc, &mockStore{})

	es := editSession(t, h, w, "/readme.txt")
	id := wopiFileID("docs", "/readme.txt")
	want := "http://office.example/browser/abc/cool.html?WOPISrc=" + url.QueryEscape("https://files.example/files/wopi/files/"+id)
	if es.URL != want {
		t.Errorf("url: want %s, got %s", want, es.URL)
	}
	if es.Token == "" || es.TTL <= time.Now().UnixMilli() {
		t.Errorf("want a token with an expiry ahead, got %+v", es)
	}

	r := wopi(h, http.MethodGet, id, es.Token, nil, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("CheckFileInfo: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var info FileInfo
	json.NewDecoder(r.Body).Decode(&info)
	if info.BaseFileName != "readme.txt" || info.Size != uint64(len(readmeContent)) || !info.UserCanWrite || !info.SupportsLocks ||
		info.LastModifiedTime != wopiTime(readmeModified) || info.PostMessageOrigin != "https://files.example" || info.UserFriendlyName != "alice" {
		t.Errorf("want the file and the rights described, got %+v", info)
	}

	for _, tt := range []struct {
		name string
		call string
		want int
	}{
		{"a folder", "edit?path=/Projects", http.StatusBadRequest},
		{"not there", "edit?path=/gone.txt", http.StatusNotFound},
		{"not a document", "edit?path=/plan.pdf", http.StatusBadRequest},
		{"the root", "edit?path=/", http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if r := manage(h, w, http.MethodGet, tt.call); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}

	// Without an editor there is nothing to edit with.
	plain, pw := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")
	if r := manage(plain, pw, http.MethodGet, "edit?path=/readme.txt"); r.Code != http.StatusNotFound {
		t.Errorf("without an editor: want %d, got %d", http.StatusNotFound, r.Code)
	}

	// A read-only account views the file, and cannot save it.
	readOnly := &mockStore{getAccessRights: func(share stores.Share, acc stores.Account) (stores.AccessRights, error) {
		return stores.AccessRights{ShareName: share.Name, AccountID: acc.ID, ReadAccess: true}, nil
	}}
	rh, rw := editingHandler(t, readingClient(), readOnly)
	es = editSession(t, rh, rw, "/readme.txt")
	r = wopi(rh, http.MethodGet, id, es.Token, nil, nil)
	info = FileInfo{}
	json.NewDecoder(r.Body).Decode(&info)
	if r.Code != http.StatusOK || info.UserCanWrite {
		t.Errorf("read-only: want the file without the right to write, got %d %+v", r.Code, info)
	}
	if r := wopi(rh, http.MethodPost, id+"/contents", es.Token, []byte("x"), nil); r.Code != http.StatusForbidden {
		t.Errorf("read-only save: want %d, got %d", http.StatusForbidden, r.Code)
	}
}

// TestWOPITokens verifies that a call needs a live token on the very file.
func TestWOPITokens(t *testing.T) {
	h, w := editingHandler(t, readingClient(), &mockStore{})
	es := editSession(t, h, w, "/readme.txt")
	id := wopiFileID("docs", "/readme.txt")

	if r := wopi(h, http.MethodGet, id, "not-a-token", nil, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("unknown token: want %d, got %d", http.StatusUnauthorized, r.Code)
	}
	if r := wopi(h, http.MethodGet, wopiFileID("docs", "/other.txt"), es.Token, nil, nil); r.Code != http.StatusNotFound {
		t.Errorf("another file: want %d, got %d", http.StatusNotFound, r.Code)
	}
	h.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if r := wopi(h, http.MethodGet, id, es.Token, nil, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("expired token: want %d, got %d", http.StatusUnauthorized, r.Code)
	}
}

// TestWOPIGetFile verifies that the editor reads the file whole.
func TestWOPIGetFile(t *testing.T) {
	h, w := editingHandler(t, readingClient(), &mockStore{})
	es := editSession(t, h, w, "/readme.txt")
	id := wopiFileID("docs", "/readme.txt")

	r := wopi(h, http.MethodGet, id+"/contents", es.Token, nil, nil)
	if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), readmeContent) {
		t.Errorf("want the file's contents, got %d %q", r.Code, r.Body.String())
	}
	if ct := r.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type: want application/octet-stream, got %q", ct)
	}
}

// TestWOPIPutFile verifies that a save is stored over the file, that a save
// over a file changed since the editor read it is refused with the code the
// editor acts on, and that an empty save is refused.
func TestWOPIPutFile(t *testing.T) {
	mc := readingClient()
	h, w := editingHandler(t, mc, &mockStore{})
	es := editSession(t, h, w, "/readme.txt")
	id := wopiFileID("docs", "/readme.txt")

	r := wopi(h, http.MethodPost, id+"/contents", es.Token, []byte("new text"), map[string]string{"X-COOL-WOPI-Timestamp": wopiTime(readmeModified)})
	if r.Code != http.StatusOK {
		t.Fatalf("save: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var res map[string]string
	json.NewDecoder(r.Body).Decode(&res)
	if res["LastModifiedTime"] != wopiTime(readmeModified) {
		t.Errorf("want the file's time back, got %+v", res)
	}
	if len(mc.written) != 1 || string(mc.written[0].data) != "new text" || len(mc.finished) != 1 || len(mc.started) != 1 || mc.started[0] != "readme.txt" {
		t.Errorf("want the save stored as the file, got %+v started, %d parts, %d finished", mc.started, len(mc.written), len(mc.finished))
	}

	r = wopi(h, http.MethodPost, id+"/contents", es.Token, []byte("stale"), map[string]string{"X-COOL-WOPI-Timestamp": "2000-01-01T00:00:00Z"})
	if r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "1010") {
		t.Errorf("a stale save: want %d with the code 1010, got %d (%s)", http.StatusConflict, r.Code, r.Body.String())
	}
	if r := wopi(h, http.MethodPost, id+"/contents", es.Token, nil, nil); r.Code != http.StatusBadRequest {
		t.Errorf("an empty save: want %d, got %d", http.StatusBadRequest, r.Code)
	}
	if len(mc.written) != 1 {
		t.Errorf("want nothing else stored, got %d parts", len(mc.written))
	}
}

// TestWOPILocks verifies the lock operations: a lock is taken, held against
// another, refreshed, read, released by its holder only, and outlived.
func TestWOPILocks(t *testing.T) {
	h, w := editingHandler(t, readingClient(), &mockStore{})
	es := editSession(t, h, w, "/readme.txt")
	id := wopiFileID("docs", "/readme.txt")
	op := func(name, lock string) *httptest.ResponseRecorder {
		return wopi(h, http.MethodPost, id, es.Token, nil, map[string]string{"X-WOPI-Override": name, "X-WOPI-Lock": lock})
	}
	check := func(what string, r *httptest.ResponseRecorder, code int, lock string) {
		t.Helper()
		if r.Code != code || r.Header().Get("X-WOPI-Lock") != lock {
			t.Errorf("%s: want %d with lock %q, got %d with lock %q (%s)", what, code, lock, r.Code, r.Header().Get("X-WOPI-Lock"), r.Body.String())
		}
	}

	check("unlocked", op("GET_LOCK", ""), http.StatusOK, "")
	check("lock", op("LOCK", "a"), http.StatusOK, "")
	check("lock by another", op("LOCK", "b"), http.StatusConflict, "a")
	check("refresh", op("REFRESH_LOCK", "a"), http.StatusOK, "")
	check("refresh by another", op("REFRESH_LOCK", "b"), http.StatusConflict, "a")
	check("locked", op("GET_LOCK", ""), http.StatusOK, "a")
	check("save by another", wopi(h, http.MethodPost, id+"/contents", es.Token, []byte("x"), map[string]string{"X-WOPI-Lock": "b"}), http.StatusConflict, "a")
	if r := wopi(h, http.MethodPost, id+"/contents", es.Token, []byte("x"), map[string]string{"X-WOPI-Lock": "a"}); r.Code != http.StatusOK {
		t.Errorf("save by the holder: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	check("unlock by another", op("UNLOCK", "b"), http.StatusConflict, "a")
	check("unlock", op("UNLOCK", "a"), http.StatusOK, "")
	check("refresh after unlock", op("REFRESH_LOCK", "a"), http.StatusConflict, "")
	if r := op("LOCK", ""); r.Code != http.StatusBadRequest {
		t.Errorf("lock without a lock: want %d, got %d", http.StatusBadRequest, r.Code)
	}
	if r := op("PUT_RELATIVE", ""); r.Code != http.StatusNotImplemented {
		t.Errorf("an unsupported operation: want %d, got %d", http.StatusNotImplemented, r.Code)
	}

	check("lock again", op("LOCK", "c"), http.StatusOK, "")
	h.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	check("outlived", op("GET_LOCK", ""), http.StatusOK, "")
}
