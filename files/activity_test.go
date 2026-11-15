package files

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
)

// activity asks for the feed of docs with the query, as the session.
func activity(h http.Handler, from *httptest.ResponseRecorder, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/share/docs/activity?"+query, nil)
	for _, c := range from.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestActivityFeed verifies that the feed of a share comes from the store for
// the account's workgroup, a page at a time from where the caller says, and
// that it is refused to whoever may not read the share.
func TestActivityFeed(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var asked []any
	store := &mockStore{activity: func(share string, acc stores.Account, before uint64, limit int) ([]stores.Activity, error) {
		asked = []any{share, acc.Username, acc.Workgroup, before, limit}
		return []stores.Activity{
			{ID: 9, Share: share, Username: "bob", Action: stores.ActivityRename, Path: "/plan.pdf", Target: "/Projects/plan.pdf", At: at},
			{ID: 8, Share: share, Username: "alice", Action: stores.ActivityUpload, Path: "/plan.pdf", Size: 1200, At: at.Add(-time.Minute)},
		}, nil
	}}
	h, w := loggedIn(t, store, &mockServer{client: readingClient()}, "alice", "hunter2")

	r := activity(h, w, "")
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var entries []ActivityEntry
	if err := json.Unmarshal(r.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decoding the feed: %v", err)
	}
	if len(entries) != 2 || entries[0].ID != 9 || entries[0].Action != "rename" || entries[0].Target != "/Projects/plan.pdf" || entries[1].Size != 1200 || !entries[1].At.Equal(at.Add(-time.Minute)) {
		t.Errorf("want the two entries as the store gave them, got %+v", entries)
	}
	if want := []any{"docs", "alice", testUUID.String(), uint64(0), activityPage}; len(asked) != 5 || asked[0] != want[0] || asked[1] != want[1] || asked[2] != want[2] || asked[3] != want[3] || asked[4] != want[4] {
		t.Errorf("want the store asked %v, got %v", want, asked)
	}

	// The next page starts before the entry named, as long as the caller says.
	activity(h, w, "before=8&limit=10")
	if asked[3] != uint64(8) || asked[4] != 10 {
		t.Errorf("want the store asked for 10 before 8, got %v", asked)
	}
	for _, q := range []string{"before=x", "limit=0", "limit=many"} {
		if r := activity(h, w, q); r.Code != http.StatusBadRequest {
			t.Errorf("%s: want %d, got %d", q, http.StatusBadRequest, r.Code)
		}
	}

	g, gw := loggedIn(t, store, &mockServer{client: readingClient()}, "guest", "")
	if r := activity(g, gw, ""); r.Code != http.StatusForbidden {
		t.Errorf("a guest on docs: want %d, got %d", http.StatusForbidden, r.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/share/docs/activity", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: want %d, got %d", http.StatusUnauthorized, rec.Code)
	}
}
