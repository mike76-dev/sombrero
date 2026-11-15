package files

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
)

// linkStore is a mock store keeping links in memory, made by whoever asks.
type linkStore struct {
	mockStore
	made map[string]stores.Link
}

func newLinkStore() *linkStore {
	ls := &linkStore{made: make(map[string]stores.Link)}
	ls.createLink = func(share, path string, acc stores.Account, expires time.Time) (stores.Link, error) {
		l := stores.Link{Token: "token" + string(rune('0'+len(ls.made))), Share: share, Path: path, Account: acc, CreatedAt: time.Now(), ExpiresAt: expires}
		ls.made[l.Token] = l
		return l, nil
	}
	ls.link = func(token string) (stores.Link, error) {
		if l, ok := ls.made[token]; ok {
			return l, nil
		}
		return stores.Link{}, stores.ErrNotFound
	}
	ls.links = func(share, path string, acc stores.Account) ([]stores.Link, error) {
		var links []stores.Link
		for _, l := range ls.made {
			if l.Share == share && l.Path == path && l.Account.Workgroup == acc.Workgroup {
				links = append(links, l)
			}
		}
		return links, nil
	}
	ls.deleteLink = func(token string, acc stores.Account) error {
		if l, ok := ls.made[token]; ok && l.Account.Workgroup == acc.Workgroup {
			delete(ls.made, token)
			return nil
		}
		return stores.ErrNotFound
	}
	return ls
}

// asSession sends the request with the session's cookies.
func asSession(h http.Handler, from *httptest.ResponseRecorder, method, target string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for _, c := range from.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestSharingLinks verifies that a link is made to a file for the account,
// listed and deleted by the workgroup, and that whoever holds it reads the
// file without a session, shown or saved, until it expires or goes.
func TestSharingLinks(t *testing.T) {
	ls := newLinkStore()
	h, w := loggedIn(t, &ls.mockStore, &mockServer{client: readingClient()}, "alice", "hunter2")
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	expires := now.Add(24 * time.Hour)
	body, _ := json.Marshal(LinkRequest{ExpiresAt: &expires})
	r := asSession(h, w, http.MethodPost, "/share/docs/link?path=/readme.txt", body)
	if r.Code != http.StatusCreated {
		t.Fatalf("want %d, got %d (%s)", http.StatusCreated, r.Code, r.Body.String())
	}
	var made LinkEntry
	if err := json.Unmarshal(r.Body.Bytes(), &made); err != nil {
		t.Fatalf("decoding the link: %v", err)
	}
	if made.Token == "" || !strings.HasSuffix(made.URL, "/files/link/"+made.Token) || made.Path != "/readme.txt" || made.CreatedBy != "alice" || made.ExpiresAt == nil || !made.ExpiresAt.Equal(expires) {
		t.Errorf("want a link to readme.txt by alice expiring tomorrow at /files/link/<token>, got %+v", made)
	}
	if l := ls.made[made.Token]; l.Share != "docs" || l.Account.Username != "alice" {
		t.Errorf("want the store asked for alice's link on docs, got %+v", l)
	}

	// Without a body the link lasts; a past expiry, a file that is not there
	// and a share the account may not read are refused.
	if r := asSession(h, w, http.MethodPost, "/share/docs/link?path=/readme.txt", nil); r.Code != http.StatusCreated || strings.Contains(r.Body.String(), "expiresAt") {
		t.Errorf("a lasting link: want %d with no expiry, got %d (%s)", http.StatusCreated, r.Code, r.Body.String())
	}
	past := now.Add(-time.Minute)
	body, _ = json.Marshal(LinkRequest{ExpiresAt: &past})
	if r := asSession(h, w, http.MethodPost, "/share/docs/link?path=/readme.txt", body); r.Code != http.StatusBadRequest {
		t.Errorf("a past expiry: want %d, got %d", http.StatusBadRequest, r.Code)
	}
	if r := asSession(h, w, http.MethodPost, "/share/docs/link?path=/gone.txt", nil); r.Code != http.StatusNotFound {
		t.Errorf("a file not there: want %d, got %d", http.StatusNotFound, r.Code)
	}
	g, gw := loggedIn(t, &ls.mockStore, &mockServer{client: readingClient()}, "guest", "")
	if r := asSession(g, gw, http.MethodPost, "/share/docs/link?path=/readme.txt", nil); r.Code != http.StatusForbidden {
		t.Errorf("a guest on docs: want %d, got %d", http.StatusForbidden, r.Code)
	}

	// The links on the file are listed for the workgroup.
	r = asSession(h, w, http.MethodGet, "/share/docs/links?path=/readme.txt", nil)
	var listed []LinkEntry
	if err := json.Unmarshal(r.Body.Bytes(), &listed); r.Code != http.StatusOK || err != nil || len(listed) != 2 {
		t.Errorf("want the two links listed, got %d %s", r.Code, r.Body.String())
	}

	// Whoever holds the link reads the file, shown unless asked to be saved.
	public := func(target string, method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	r = public("/link/"+made.Token, http.MethodGet)
	if r.Code != http.StatusOK || !bytes.Equal(r.Body.Bytes(), readmeContent) {
		t.Fatalf("the link: want the file, got %d %q", r.Code, r.Body.String())
	}
	if got := r.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("the link: want the file shown, got %q", got)
	}
	if r := public("/link/"+made.Token+"?download=1", http.MethodGet); !strings.HasPrefix(r.Header().Get("Content-Disposition"), "attachment;") {
		t.Errorf("download: want the file saved, got %q", r.Header().Get("Content-Disposition"))
	}
	if r := public("/link/"+made.Token, http.MethodHead); r.Code != http.StatusOK || r.Body.Len() != 0 || r.Header().Get("Content-Length") != "100" {
		t.Errorf("HEAD: want %d with the length and no body, got %d %q", http.StatusOK, r.Code, r.Body.String())
	}
	if r := public("/link/nothing", http.MethodGet); r.Code != http.StatusNotFound {
		t.Errorf("an unknown token: want %d, got %d", http.StatusNotFound, r.Code)
	}
	now = expires
	if r := public("/link/"+made.Token, http.MethodGet); r.Code != http.StatusNotFound {
		t.Errorf("an expired link: want %d, got %d", http.StatusNotFound, r.Code)
	}
	now = now.Add(-time.Hour)

	// A link made by an account that may no longer read the share is dead.
	ls.made["dead"] = stores.Link{Token: "dead", Share: "private", Path: "/readme.txt", Account: stores.Account{Username: "alice", Workgroup: testUUID.String()}}
	if r := public("/link/dead", http.MethodGet); r.Code != http.StatusNotFound {
		t.Errorf("a link without rights: want %d, got %d", http.StatusNotFound, r.Code)
	}

	// Deleting takes the token on its share; another share or an unknown token is not found.
	if r := asSession(h, w, http.MethodDelete, "/share/pub/link?token="+made.Token, nil); r.Code != http.StatusNotFound {
		t.Errorf("deleting under another share: want %d, got %d", http.StatusNotFound, r.Code)
	}
	if r := asSession(h, w, http.MethodDelete, "/share/docs/link?token="+made.Token, nil); r.Code != http.StatusNoContent {
		t.Errorf("deleting: want %d, got %d (%s)", http.StatusNoContent, r.Code, r.Body.String())
	}
	if r := public("/link/"+made.Token, http.MethodGet); r.Code != http.StatusNotFound {
		t.Errorf("a deleted link: want %d, got %d", http.StatusNotFound, r.Code)
	}
	if r := asSession(h, w, http.MethodDelete, "/share/docs/link?token="+made.Token, nil); r.Code != http.StatusNotFound {
		t.Errorf("deleting again: want %d, got %d", http.StatusNotFound, r.Code)
	}
}
