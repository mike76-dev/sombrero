package files

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// The shares of the mock store: docs takes no guests, pub does.
var (
	docs = stores.Share{Name: "docs", Type: "indexd", Remark: "the documents", Password: "secret"}
	pub  = stores.Share{Name: "pub", Type: "renterd", AllowGuest: true}
)

// mockClient implements the parts of client.Client the listing uses. The
// embedded interface is nil, so any other call panics.
type mockClient struct {
	client.Client
	list    func(string) ([]client.ObjectInfo, error)
	object  func(string) (client.ObjectInfo, error)
	read    func(string, uint64, uint64, io.Writer) error
	storage func() (client.StorageInfo, error)
	listed  []string
	reads   [][2]uint64
}

func (m *mockClient) Read(_ context.Context, _ stores.Account, p string, offset, length uint64, w io.Writer) error {
	m.reads = append(m.reads, [2]uint64{offset, length})
	if m.read != nil {
		return m.read(p, offset, length, w)
	}
	return stores.ErrNotFound
}

func (m *mockClient) Storage(context.Context) (client.StorageInfo, error) {
	if m.storage != nil {
		return m.storage()
	}
	return client.StorageInfo{MinShards: 1, TotalShards: 3}, nil
}

func (m *mockClient) List(_ context.Context, _ stores.Account, p string) ([]client.ObjectInfo, error) {
	m.listed = append(m.listed, p)
	if m.list != nil {
		return m.list(p)
	}
	return nil, nil
}

func (m *mockClient) Object(_ context.Context, _ stores.Account, p string) (client.ObjectInfo, error) {
	if m.object != nil {
		return m.object(p)
	}
	return client.ObjectInfo{}, stores.ErrNotFound
}

// mockServer hands out one client for every share, or fails to.
type mockServer struct {
	client client.Client
	err    error
}

func (m *mockServer) ShareClient(stores.Share, string) (client.Client, error) {
	return m.client, m.err
}

// loggedIn returns a handler over the store and the server with a session of
// the named account, and the response that holds its cookie.
func loggedIn(t *testing.T, ms *mockStore, srv Server, username, password string) (*Handler, *httptest.ResponseRecorder) {
	t.Helper()
	h := New(ms, srv)
	w := login(h, "partners", username, password)
	if w.Code != http.StatusOK {
		t.Fatalf("login as %s: want %d, got %d (%s)", username, http.StatusOK, w.Code, w.Body.String())
	}
	return h, w
}

// TestSharesListsWhatTheAccountReaches verifies that the shares come with
// the account's rights on each, and that a guest sees only the shares that
// take guests.
func TestSharesListsWhatTheAccountReaches(t *testing.T) {
	h, w := loggedIn(t, &mockStore{}, &mockServer{}, "alice", "hunter2")
	r := call(h, http.MethodGet, "/shares", "", w)
	if r.Code != http.StatusOK {
		t.Fatalf("GET /shares: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var got []ShareInfo
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the shares: %v", err)
	}
	want := []ShareInfo{
		{Name: "docs", Type: "indexd", Remark: "the documents", Rights: Rights{Read: true, Write: true, Delete: true}},
		{Name: "pub", Type: "renterd", Rights: Rights{Read: true}},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d shares, got %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("share %d: want %+v, got %+v", i, want[i], got[i])
		}
	}

	h, w = loggedIn(t, &mockStore{}, &mockServer{}, "guest", "")
	r = call(h, http.MethodGet, "/shares", "", w)
	got = nil
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the guest's shares: %v", err)
	}
	if len(got) != 1 || got[0].Name != "pub" {
		t.Errorf("want the guest to see pub alone, got %+v", got)
	}

	if r := call(h, http.MethodGet, "/shares", "", nil); r.Code != http.StatusUnauthorized {
		t.Errorf("GET /shares without a session: want %d, got %d", http.StatusUnauthorized, r.Code)
	}
}

// TestListingAFolder verifies that a listing names what the client lists,
// folders told from files, and that the paths reach the client the way the
// SMB layer spells them.
func TestListingAFolder(t *testing.T) {
	created := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	mc := &mockClient{
		list: func(p string) ([]client.ObjectInfo, error) {
			switch p {
			case "":
				return []client.ObjectInfo{
					{Key: "/Projects/", CreatedAt: created, ModifiedAt: created},
					{Key: "/readme.txt", Size: 12, CreatedAt: created, ModifiedAt: created.Add(time.Hour)},
				}, nil
			case "Projects/":
				return []client.ObjectInfo{{Key: "/Projects/plan.pdf", Size: 5 << 20}}, nil
			}
			return nil, nil
		},
		object: func(p string) (client.ObjectInfo, error) {
			switch p {
			case "Projects":
				return client.ObjectInfo{Key: "/Projects/"}, nil
			case "readme.txt":
				return client.ObjectInfo{Key: "/readme.txt", Size: 12}, nil
			}
			return client.ObjectInfo{}, stores.ErrNotFound
		},
	}
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	r := call(h, http.MethodGet, "/share/docs/list", "", w)
	if r.Code != http.StatusOK {
		t.Fatalf("listing the root: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	var got Listing
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the listing: %v", err)
	}
	want := Listing{Share: "docs", Path: "/", Entries: []Entry{
		{Name: "Projects", Path: "/Projects", Dir: true, CreatedAt: created, ModifiedAt: created},
		{Name: "readme.txt", Path: "/readme.txt", Size: 12, CreatedAt: created, ModifiedAt: created.Add(time.Hour)},
	}}
	if got.Share != want.Share || got.Path != want.Path || len(got.Entries) != len(want.Entries) {
		t.Fatalf("want %+v, got %+v", want, got)
	}
	for i := range want.Entries {
		if !got.Entries[i].CreatedAt.Equal(want.Entries[i].CreatedAt) || !got.Entries[i].ModifiedAt.Equal(want.Entries[i].ModifiedAt) {
			t.Errorf("entry %d: want the times %v/%v, got %v/%v", i, want.Entries[i].CreatedAt, want.Entries[i].ModifiedAt, got.Entries[i].CreatedAt, got.Entries[i].ModifiedAt)
		}
		got.Entries[i].CreatedAt, got.Entries[i].ModifiedAt = want.Entries[i].CreatedAt, want.Entries[i].ModifiedAt
		if got.Entries[i] != want.Entries[i] {
			t.Errorf("entry %d: want %+v, got %+v", i, want.Entries[i], got.Entries[i])
		}
	}

	// A folder below the root, named with or without the leading slash.
	for _, p := range []string{"/Projects", "Projects", "/Projects/"} {
		r := call(h, http.MethodGet, "/share/docs/list?path="+p, "", w)
		if r.Code != http.StatusOK {
			t.Fatalf("listing %q: want %d, got %d (%s)", p, http.StatusOK, r.Code, r.Body.String())
		}
		var got Listing
		json.NewDecoder(r.Body).Decode(&got)
		if got.Path != "/Projects" || len(got.Entries) != 1 || got.Entries[0].Name != "plan.pdf" || got.Entries[0].Dir {
			t.Errorf("listing %q: got %+v", p, got)
		}
	}
	if len(mc.listed) != 4 || mc.listed[0] != "" || mc.listed[1] != "Projects/" {
		t.Errorf("want the client asked for \"\" and then \"Projects/\", got %q", mc.listed)
	}

	// An empty folder is an empty list, not a null.
	mc.list = func(string) ([]client.ObjectInfo, error) { return nil, nil }
	r = call(h, http.MethodGet, "/share/docs/list?path=/Projects", "", w)
	if body := r.Body.String(); r.Code != http.StatusOK || !strings.Contains(body, `"entries":[]`) {
		t.Errorf("an empty folder: want an empty entries list, got %d %s", r.Code, body)
	}
}

// TestListingRefusals verifies the answers for what cannot be listed: a share
// that is not there or not the account's, a path that is not a folder or
// climbs out, and a share whose storage cannot be reached.
func TestListingRefusals(t *testing.T) {
	mc := &mockClient{object: func(p string) (client.ObjectInfo, error) {
		if p == "readme.txt" {
			return client.ObjectInfo{Key: "/readme.txt"}, nil
		}
		return client.ObjectInfo{}, stores.ErrNotFound
	}}
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	tests := []struct {
		name string
		path string
		want int
	}{
		{"unknown share", "/share/nothing/list", http.StatusNotFound},
		{"share without rights", "/share/private/list", http.StatusForbidden},
		{"folder not there", "/share/docs/list?path=/gone", http.StatusNotFound},
		{"a file", "/share/docs/list?path=/readme.txt", http.StatusBadRequest},
		{"climbing path", "/share/docs/list?path=/Projects/../../etc", http.StatusBadRequest},
		{"no session", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from := w
			p := tt.path
			if p == "" {
				from, p = nil, "/share/docs/list"
			}
			if r := call(h, http.MethodGet, p, "", from); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}

	// A guest on a share that takes none is refused, where alice is not.
	g, gw := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "guest", "")
	if r := call(g, http.MethodGet, "/share/docs/list", "", gw); r.Code != http.StatusForbidden {
		t.Errorf("a guest on docs: want %d, got %d", http.StatusForbidden, r.Code)
	}
	if r := call(g, http.MethodGet, "/share/pub/list", "", gw); r.Code != http.StatusOK {
		t.Errorf("a guest on pub: want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}

	// A share whose storage cannot be reached, and a store that cannot answer.
	down, dw := loggedIn(t, &mockStore{}, &mockServer{err: errStore}, "alice", "hunter2")
	if r := call(down, http.MethodGet, "/share/docs/list", "", dw); r.Code != http.StatusServiceUnavailable {
		t.Errorf("storage down: want %d, got %d", http.StatusServiceUnavailable, r.Code)
	}
	broken, bw := loggedIn(t, &mockStore{getShare: func(string) (stores.Share, error) { return stores.Share{}, errStore }}, &mockServer{client: mc}, "alice", "hunter2")
	if r := call(broken, http.MethodGet, "/share/docs/list", "", bw); r.Code != http.StatusInternalServerError {
		t.Errorf("store failing: want %d, got %d", http.StatusInternalServerError, r.Code)
	}
}
