package files

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mike76-dev/sombrero/client"
)

// TestSearching verifies that a search asks the share's client for the text
// and answers with the matches as entries, and that an empty text and an
// account without the right to read are refused.
func TestSearching(t *testing.T) {
	mc := readingClient()
	mc.search = func(text string) ([]client.ObjectInfo, error) {
		return []client.ObjectInfo{
			{Key: "/Reports/", ModifiedAt: readmeModified},
			{Key: "/Reports/Q3-report.txt", Size: 1200, ModifiedAt: readmeModified},
		}, nil
	}
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	get := func(h http.Handler, from *httptest.ResponseRecorder, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/share/docs/search?"+query, nil)
		for _, c := range from.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	r := get(h, w, "q=+Report+")
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var entries []Entry
	if err := json.Unmarshal(r.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decoding the matches: %v", err)
	}
	if len(entries) != 2 || !entries[0].Dir || entries[0].Path != "/Reports" || entries[1].Name != "Q3-report.txt" || entries[1].Path != "/Reports/Q3-report.txt" || entries[1].Size != 1200 {
		t.Errorf("want the folder and the file as entries, got %+v", entries)
	}
	if len(mc.searched) != 1 || mc.searched[0] != "Report" {
		t.Errorf("want the client asked for the trimmed text, got %v", mc.searched)
	}

	if r := get(h, w, "q=+"); r.Code != http.StatusBadRequest {
		t.Errorf("no text: want %d, got %d", http.StatusBadRequest, r.Code)
	}
	g, gw := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "guest", "")
	if r := get(g, gw, "q=report"); r.Code != http.StatusForbidden {
		t.Errorf("a guest on docs: want %d, got %d", http.StatusForbidden, r.Code)
	}
}
