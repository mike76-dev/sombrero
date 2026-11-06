package files

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/utils"
	"golang.org/x/crypto/md4"
)

var testUUID = uuid.MustParse("12345678-1234-1234-1234-123456789abc")

var errStore = errors.New("store error")

// ntHashOf hashes a password the way the accounts are stored.
func ntHashOf(password string) []byte {
	h := md4.New()
	h.Write(utils.EncodeStringToBytes(password))
	return h.Sum(nil)
}

// mockStore implements Store with optional per-method overrides. Without
// one, it holds the workgroup "partners" with the accounts alice and a guest,
// and the shares docs (alice: read, write, delete) and pub (everybody: read).
type mockStore struct {
	findAccount         func(string, string) (stores.Account, error)
	findWorkgroup       func(uuid.UUID) (stores.Workgroup, error)
	findWorkgroupByName func(string) (stores.Workgroup, error)
	getShare            func(string) (stores.Share, error)
	getShares           func(stores.Account) ([]stores.Share, error)
	getAccessRights     func(stores.Share, stores.Account) (stores.AccessRights, error)
}

var partners = stores.Workgroup{ID: 7, UUID: testUUID, Name: "partners"}

func (m *mockStore) GetShare(name string) (stores.Share, error) {
	if m.getShare != nil {
		return m.getShare(name)
	}
	switch name {
	case "docs":
		return docs, nil
	case "pub":
		return pub, nil
	case "private":
		return stores.Share{Name: "private", Type: "indexd"}, nil
	}
	return stores.Share{}, nil
}

func (m *mockStore) GetShares(acc stores.Account) ([]stores.Share, error) {
	if m.getShares != nil {
		return m.getShares(acc)
	}
	return []stores.Share{docs, pub}, nil
}

func (m *mockStore) GetAccessRights(share stores.Share, acc stores.Account) (stores.AccessRights, error) {
	if m.getAccessRights != nil {
		return m.getAccessRights(share, acc)
	}
	ar := stores.AccessRights{ShareName: share.Name, AccountID: acc.ID}
	switch {
	case share.Name == "docs" && acc.Username == "alice":
		ar.ReadAccess, ar.WriteAccess, ar.DeleteAccess = true, true, true
	case share.Name == "docs", share.Name == "pub":
		ar.ReadAccess = true
	default:
		return stores.AccessRights{}, nil
	}
	return ar, nil
}

func (m *mockStore) FindAccount(u, w string) (stores.Account, error) {
	if m.findAccount != nil {
		return m.findAccount(u, w)
	}
	if w != testUUID.String() {
		return stores.Account{}, stores.ErrAccountNotFound
	}
	switch u {
	case "alice":
		return stores.Account{ID: 1, Username: "alice", NTHash: ntHashOf("hunter2"), Workgroup: w}, nil
	case "guest":
		return stores.Account{ID: 2, Username: "guest", NTHash: ntHashOf(""), Workgroup: w}, nil
	}
	return stores.Account{}, stores.ErrAccountNotFound
}

func (m *mockStore) FindWorkgroup(u uuid.UUID) (stores.Workgroup, error) {
	if m.findWorkgroup != nil {
		return m.findWorkgroup(u)
	}
	if u == testUUID {
		return partners, nil
	}
	return stores.Workgroup{}, nil
}

func (m *mockStore) FindWorkgroupByName(name string) (stores.Workgroup, error) {
	if m.findWorkgroupByName != nil {
		return m.findWorkgroupByName(name)
	}
	if name == "partners" {
		return partners, nil
	}
	return stores.Workgroup{}, nil
}

// call sends a request to the handler, with the cookies of an earlier
// response if one is given.
func call(h http.Handler, method, path, body string, from *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if from != nil {
		for _, c := range from.Result().Cookies() {
			req.AddCookie(c)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func login(h http.Handler, workgroup, username, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(LoginRequest{Workgroup: workgroup, Username: username, Password: password})
	return call(h, http.MethodPost, "/login", string(body), nil)
}

func decodeIdentity(t *testing.T, w *httptest.ResponseRecorder) Identity {
	t.Helper()
	var id Identity
	if err := json.NewDecoder(w.Body).Decode(&id); err != nil {
		t.Fatalf("decoding the identity: %v", err)
	}
	return id
}

// TestLoginStartsASession verifies the life of a session: the login names
// the account, the cookie it sets answers GET /me, and the logout ends it.
func TestLoginStartsASession(t *testing.T) {
	h := New(&mockStore{}, &mockServer{})

	w := login(h, "partners", "alice", "hunter2")
	if w.Code != http.StatusOK {
		t.Fatalf("login: want %d, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
	want := Identity{Username: "alice", Workgroup: WorkgroupIdentity{UUID: testUUID, Name: "partners"}}
	if got := decodeIdentity(t, w); got != want {
		t.Errorf("login: want %+v, got %+v", want, got)
	}

	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("want one session cookie, got %v", cookies)
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.Path != "/" {
		t.Errorf("want an HttpOnly, SameSite=Strict cookie for /, got %s", c.String())
	}

	me := call(h, http.MethodGet, "/me", "", w)
	if me.Code != http.StatusOK {
		t.Fatalf("GET /me: want %d, got %d (%s)", http.StatusOK, me.Code, me.Body.String())
	}
	if got := decodeIdentity(t, me); got != want {
		t.Errorf("GET /me: want %+v, got %+v", want, got)
	}

	// Without the cookie there is no session to speak of.
	if r := call(h, http.MethodGet, "/me", "", nil); r.Code != http.StatusUnauthorized {
		t.Errorf("GET /me without a cookie: want %d, got %d", http.StatusUnauthorized, r.Code)
	}

	out := call(h, http.MethodPost, "/logout", "", w)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout: want %d, got %d", http.StatusNoContent, out.Code)
	}
	if cs := out.Result().Cookies(); len(cs) != 1 || cs[0].MaxAge >= 0 {
		t.Errorf("want the logout to clear the cookie, got %v", cs)
	}
	if r := call(h, http.MethodGet, "/me", "", w); r.Code != http.StatusUnauthorized {
		t.Errorf("GET /me after the logout: want %d, got %d", http.StatusUnauthorized, r.Code)
	}
}

// TestLoginNamesTheWorkgroupEitherWay verifies that the UUID does as well as
// the name, and that a login through TLS gets a Secure cookie.
func TestLoginNamesTheWorkgroupEitherWay(t *testing.T) {
	h := New(&mockStore{}, &mockServer{})
	w := login(h, testUUID.String(), "alice", "hunter2")
	if w.Code != http.StatusOK {
		t.Fatalf("login by UUID: want %d, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
	if got := decodeIdentity(t, w); got.Workgroup.Name != "partners" {
		t.Errorf("want the workgroup named in the identity, got %+v", got)
	}

	body, _ := json.Marshal(LoginRequest{Workgroup: "partners", Username: "alice", Password: "hunter2"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if cs := rec.Result().Cookies(); rec.Code != http.StatusOK || len(cs) != 1 || !cs[0].Secure {
		t.Errorf("login behind TLS: want a Secure cookie, got %d %v", rec.Code, cs)
	}
}

// TestLoginRefusals verifies that every wrong credential is refused with the
// same answer, and that a request that is no login at all is told so.
func TestLoginRefusals(t *testing.T) {
	h := New(&mockStore{}, &mockServer{})
	tests := []struct {
		name string
		body string
		want int
	}{
		{"unknown workgroup", `{"workgroup":"nobody","username":"alice","password":"hunter2"}`, http.StatusUnauthorized},
		{"unknown UUID", `{"workgroup":"` + uuid.NewString() + `","username":"alice","password":"hunter2"}`, http.StatusUnauthorized},
		{"anonymous workgroup", `{"workgroup":"` + stores.AnonymousWorkgroup.String() + `","username":"alice","password":""}`, http.StatusUnauthorized},
		{"unknown account", `{"workgroup":"partners","username":"bob","password":"hunter2"}`, http.StatusUnauthorized},
		{"wrong password", `{"workgroup":"partners","username":"alice","password":"hunter3"}`, http.StatusUnauthorized},
		{"empty password", `{"workgroup":"partners","username":"alice","password":""}`, http.StatusUnauthorized},
		{"no username", `{"workgroup":"partners","password":"hunter2"}`, http.StatusBadRequest},
		{"no workgroup", `{"username":"alice","password":"hunter2"}`, http.StatusBadRequest},
		{"not JSON", `workgroup=partners`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := call(h, http.MethodPost, "/login", tt.body, nil)
			if w.Code != tt.want {
				t.Fatalf("want %d, got %d (%s)", tt.want, w.Code, w.Body.String())
			}
			if tt.want == http.StatusUnauthorized && strings.TrimSpace(w.Body.String()) != `"login failure"` {
				t.Errorf("want the one refusal message, got %s", w.Body.String())
			}
			if len(w.Result().Cookies()) != 0 {
				t.Error("a refused login set a cookie")
			}
		})
	}
}

// TestLoginReportsStoreErrors verifies that a store that cannot answer is an
// internal error rather than a refusal.
func TestLoginReportsStoreErrors(t *testing.T) {
	byName := New(&mockStore{findWorkgroupByName: func(string) (stores.Workgroup, error) { return stores.Workgroup{}, errStore }}, &mockServer{})
	if w := login(byName, "partners", "alice", "hunter2"); w.Code != http.StatusInternalServerError {
		t.Errorf("workgroup lookup failing: want %d, got %d", http.StatusInternalServerError, w.Code)
	}
	account := New(&mockStore{findAccount: func(string, string) (stores.Account, error) { return stores.Account{}, errStore }}, &mockServer{})
	if w := login(account, "partners", "alice", "hunter2"); w.Code != http.StatusInternalServerError {
		t.Errorf("account lookup failing: want %d, got %d", http.StatusInternalServerError, w.Code)
	}
}

// TestGuestLogsInWithoutAPassword verifies that a guest account is one over
// the file API too: the empty password, and only that, gets in.
func TestGuestLogsInWithoutAPassword(t *testing.T) {
	h := New(&mockStore{}, &mockServer{})
	if w := login(h, "partners", "guest", ""); w.Code != http.StatusOK {
		t.Errorf("guest login: want %d, got %d (%s)", http.StatusOK, w.Code, w.Body.String())
	}
	if w := login(h, "partners", "guest", "x"); w.Code != http.StatusUnauthorized {
		t.Errorf("guest login with a password: want %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

// TestSessionsExpire verifies that a session is good for its lifetime and
// no longer, and that an expired one is forgotten.
func TestSessionsExpire(t *testing.T) {
	h := New(&mockStore{}, &mockServer{})
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	w := login(h, "partners", "alice", "hunter2")
	if w.Code != http.StatusOK {
		t.Fatalf("login: want %d, got %d", http.StatusOK, w.Code)
	}

	now = now.Add(sessionLifetime - time.Second)
	if r := call(h, http.MethodGet, "/me", "", w); r.Code != http.StatusOK {
		t.Errorf("GET /me within the lifetime: want %d, got %d", http.StatusOK, r.Code)
	}

	now = now.Add(time.Second)
	if r := call(h, http.MethodGet, "/me", "", w); r.Code != http.StatusUnauthorized {
		t.Errorf("GET /me past the lifetime: want %d, got %d", http.StatusUnauthorized, r.Code)
	}
	h.mu.Lock()
	left := len(h.sessions)
	h.mu.Unlock()
	if left != 0 {
		t.Errorf("want the expired session dropped, %d left", left)
	}
}
