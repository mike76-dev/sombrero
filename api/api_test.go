package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/core/types"
)

var errStore = errors.New("store error")

var testUUID = uuid.MustParse("12345678-1234-1234-1234-123456789abc")

// mockStore implements Store with optional per-method overrides.
type mockStore struct {
	isBanned         func(string) (bool, string, error)
	banHost          func(string, string) error
	unbanHost        func(string) error
	clearBans        func() error
	getAccountByID   func(int) (stores.Account, error)
	findAccount      func(string, string) (stores.Account, error)
	addAccount       func(stores.Account) error
	hasAccount       func(string, string) (bool, error)
	removeAccount    func(string, string) error
	findAccounts     func(string) ([]stores.Account, error)
	removeAccounts   func(string) error
	addWorkgroup          func(stores.Workgroup) error
	findWorkgroup         func(uuid.UUID) (stores.Workgroup, error)
	findWorkgroupByName   func(string) (stores.Workgroup, error)
	removeWorkgroup       func(stores.Workgroup) error
	getAccessRights  func(stores.Share, stores.Account) (stores.AccessRights, error)
	setAccessRights  func(stores.AccessRights) error
	removeAccess     func(stores.Share, stores.Account) error
	clearAccess      func(stores.Account) error
	registerShare    func(stores.Share) error
	unregisterShare  func(string) error
	getShare         func(string) (stores.Share, error)
	getShares        func(stores.Account) ([]stores.Share, error)
	getAccounts      func(stores.Share) ([]stores.AccessRights, error)
	addConnection    func(stores.Workgroup, stores.Share, types.PrivateKey) error
	removeConnection func(stores.Workgroup, stores.Share) error
}

func (m *mockStore) IsBanned(h string) (bool, string, error) {
	if m.isBanned != nil {
		return m.isBanned(h)
	}
	return false, "", nil
}
func (m *mockStore) BanHost(h, r string) error {
	if m.banHost != nil {
		return m.banHost(h, r)
	}
	return nil
}
func (m *mockStore) UnbanHost(h string) error {
	if m.unbanHost != nil {
		return m.unbanHost(h)
	}
	return nil
}
func (m *mockStore) ClearBans() error {
	if m.clearBans != nil {
		return m.clearBans()
	}
	return nil
}
func (m *mockStore) GetAccountByID(id int) (stores.Account, error) {
	if m.getAccountByID != nil {
		return m.getAccountByID(id)
	}
	return stores.Account{}, nil
}
func (m *mockStore) FindAccount(u, w string) (stores.Account, error) {
	if m.findAccount != nil {
		return m.findAccount(u, w)
	}
	return stores.Account{}, nil
}
func (m *mockStore) AddAccount(a stores.Account) error {
	if m.addAccount != nil {
		return m.addAccount(a)
	}
	return nil
}
func (m *mockStore) HasAccount(u, w string) (bool, error) {
	if m.hasAccount != nil {
		return m.hasAccount(u, w)
	}
	return false, nil
}
func (m *mockStore) RemoveAccount(u, w string) error {
	if m.removeAccount != nil {
		return m.removeAccount(u, w)
	}
	return nil
}
func (m *mockStore) FindAccounts(w string) ([]stores.Account, error) {
	if m.findAccounts != nil {
		return m.findAccounts(w)
	}
	return nil, nil
}
func (m *mockStore) RemoveAccounts(w string) error {
	if m.removeAccounts != nil {
		return m.removeAccounts(w)
	}
	return nil
}
func (m *mockStore) AddWorkgroup(wg stores.Workgroup) error {
	if m.addWorkgroup != nil {
		return m.addWorkgroup(wg)
	}
	return nil
}
func (m *mockStore) FindWorkgroup(u uuid.UUID) (stores.Workgroup, error) {
	if m.findWorkgroup != nil {
		return m.findWorkgroup(u)
	}
	return stores.Workgroup{}, nil
}
func (m *mockStore) FindWorkgroupByName(name string) (stores.Workgroup, error) {
	if m.findWorkgroupByName != nil {
		return m.findWorkgroupByName(name)
	}
	return stores.Workgroup{}, nil
}
func (m *mockStore) RemoveWorkgroup(wg stores.Workgroup) error {
	if m.removeWorkgroup != nil {
		return m.removeWorkgroup(wg)
	}
	return nil
}
func (m *mockStore) GetAccessRights(s stores.Share, a stores.Account) (stores.AccessRights, error) {
	if m.getAccessRights != nil {
		return m.getAccessRights(s, a)
	}
	return stores.AccessRights{}, nil
}
func (m *mockStore) SetAccessRights(ar stores.AccessRights) error {
	if m.setAccessRights != nil {
		return m.setAccessRights(ar)
	}
	return nil
}
func (m *mockStore) RemoveAccessRights(s stores.Share, a stores.Account) error {
	if m.removeAccess != nil {
		return m.removeAccess(s, a)
	}
	return nil
}
func (m *mockStore) ClearAccessRights(a stores.Account) error {
	if m.clearAccess != nil {
		return m.clearAccess(a)
	}
	return nil
}
func (m *mockStore) RegisterShare(s stores.Share) error {
	if m.registerShare != nil {
		return m.registerShare(s)
	}
	return nil
}
func (m *mockStore) UnregisterShare(n string) error {
	if m.unregisterShare != nil {
		return m.unregisterShare(n)
	}
	return nil
}
func (m *mockStore) GetShare(n string) (stores.Share, error) {
	if m.getShare != nil {
		return m.getShare(n)
	}
	return stores.Share{}, nil
}
func (m *mockStore) GetShares(a stores.Account) ([]stores.Share, error) {
	if m.getShares != nil {
		return m.getShares(a)
	}
	return nil, nil
}
func (m *mockStore) GetAccounts(s stores.Share) ([]stores.AccessRights, error) {
	if m.getAccounts != nil {
		return m.getAccounts(s)
	}
	return nil, nil
}
func (m *mockStore) AddConnection(wg stores.Workgroup, s stores.Share, k types.PrivateKey) error {
	if m.addConnection != nil {
		return m.addConnection(wg, s, k)
	}
	return nil
}
func (m *mockStore) RemoveConnection(wg stores.Workgroup, s stores.Share) error {
	if m.removeConnection != nil {
		return m.removeConnection(wg, s)
	}
	return nil
}

func newTestAPI(ms *mockStore) *API {
	return NewAPI(context.Background(), ms, stores.IndexdConfig{})
}

func doRequest(api *API, method, path string, body any) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	return w
}

func checkStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Errorf("status: want %d, got %d (body: %s)", want, w.Code, w.Body.String())
	}
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(w.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

const testWorkgroupName = "acme"

func foundWorkgroup() func(uuid.UUID) (stores.Workgroup, error) {
	return func(u uuid.UUID) (stores.Workgroup, error) {
		if u == testUUID {
			return stores.Workgroup{ID: 1, UUID: testUUID, Name: testWorkgroupName}, nil
		}
		return stores.Workgroup{}, nil
	}
}

func foundWorkgroupByName() func(string) (stores.Workgroup, error) {
	return func(name string) (stores.Workgroup, error) {
		if name == testWorkgroupName {
			return stores.Workgroup{ID: 1, UUID: testUUID, Name: testWorkgroupName}, nil
		}
		return stores.Workgroup{}, nil
	}
}

func foundShare(name, typ string) func(string) (stores.Share, error) {
	return func(n string) (stores.Share, error) {
		if n == name {
			return stores.Share{Name: name, Type: typ, Password: "secret"}, nil
		}
		return stores.Share{}, nil
	}
}

func foundAccount(username, workgroup string) func(string, string) (stores.Account, error) {
	return func(u, w string) (stores.Account, error) {
		if u == username {
			return stores.Account{ID: 1, Username: u, Workgroup: workgroup}, nil
		}
		return stores.Account{}, nil
	}
}

func TestBans(t *testing.T) {
	t.Run("GET not banned", func(t *testing.T) {
		ms := &mockStore{
			isBanned: func(h string) (bool, string, error) { return false, "", nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/ban/1.2.3.4", nil)
		checkStatus(t, w, http.StatusOK)
		resp := decodeJSON[IsBannedResponse](t, w)
		if resp.Banned {
			t.Error("expected not banned")
		}
	})

	t.Run("GET banned", func(t *testing.T) {
		ms := &mockStore{
			isBanned: func(h string) (bool, string, error) { return true, "spam", nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/ban/1.2.3.4", nil)
		checkStatus(t, w, http.StatusOK)
		resp := decodeJSON[IsBannedResponse](t, w)
		if !resp.Banned {
			t.Error("expected banned")
		}
		if resp.Reason != "spam" {
			t.Errorf("reason: want %q, got %q", "spam", resp.Reason)
		}
	})

	t.Run("GET store error", func(t *testing.T) {
		ms := &mockStore{isBanned: func(string) (bool, string, error) { return false, "", errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/ban/1.2.3.4", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("PUT bans host with reason", func(t *testing.T) {
		var gotHost, gotReason string
		ms := &mockStore{
			banHost: func(h, r string) error { gotHost, gotReason = h, r; return nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodPut, "/ban/1.2.3.4?reason=spam", nil)
		checkStatus(t, w, http.StatusNoContent)
		if gotHost != "1.2.3.4" {
			t.Errorf("host: want %q, got %q", "1.2.3.4", gotHost)
		}
		if gotReason != "spam" {
			t.Errorf("reason: want %q, got %q", "spam", gotReason)
		}
	})

	t.Run("PUT store error", func(t *testing.T) {
		ms := &mockStore{banHost: func(string, string) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodPut, "/ban/1.2.3.4", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE unbans host", func(t *testing.T) {
		var gotHost string
		ms := &mockStore{unbanHost: func(h string) error { gotHost = h; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/ban/1.2.3.4", nil)
		checkStatus(t, w, http.StatusNoContent)
		if gotHost != "1.2.3.4" {
			t.Errorf("host: want %q, got %q", "1.2.3.4", gotHost)
		}
	})

	t.Run("DELETE store error", func(t *testing.T) {
		ms := &mockStore{unbanHost: func(string) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/ban/1.2.3.4", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE /bans clears all", func(t *testing.T) {
		called := false
		ms := &mockStore{clearBans: func() error { called = true; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/bans", nil)
		checkStatus(t, w, http.StatusNoContent)
		if !called {
			t.Error("ClearBans not called")
		}
	})

	t.Run("DELETE /bans store error", func(t *testing.T) {
		ms := &mockStore{clearBans: func() error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/bans", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

func TestAccount(t *testing.T) {
	t.Run("GET by username", func(t *testing.T) {
		ms := &mockStore{findAccount: foundAccount("alice", testUUID.String())}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/account?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusOK)
		acc := decodeJSON[stores.Account](t, w)
		if acc.Username != "alice" {
			t.Errorf("username: want %q, got %q", "alice", acc.Username)
		}
	})

	t.Run("GET by ID", func(t *testing.T) {
		ms := &mockStore{
			getAccountByID: func(id int) (stores.Account, error) {
				return stores.Account{ID: id, Username: "alice"}, nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/account?id=1", nil)
		checkStatus(t, w, http.StatusOK)
		acc := decodeJSON[stores.Account](t, w)
		if acc.ID != 1 {
			t.Errorf("id: want 1, got %d", acc.ID)
		}
	})

	t.Run("GET missing username and ID returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodGet, "/account", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("GET invalid ID returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodGet, "/account?id=0", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("GET by username store error", func(t *testing.T) {
		ms := &mockStore{findAccount: func(string, string) (stores.Account, error) { return stores.Account{}, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/account?username=alice", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("GET by ID store error", func(t *testing.T) {
		ms := &mockStore{getAccountByID: func(int) (stores.Account, error) { return stores.Account{}, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/account?id=1", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("POST creates account", func(t *testing.T) {
		var got stores.Account
		ms := &mockStore{addAccount: func(a stores.Account) error { got = a; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/account", stores.Account{
			Username:  "Alice",
			Password:  "secret",
			Workgroup: testUUID.String(),
		})
		checkStatus(t, w, http.StatusNoContent)
		if got.Username != "alice" {
			t.Errorf("username lowercased: want %q, got %q", "alice", got.Username)
		}
	})

	t.Run("POST invalid JSON returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/account", bytes.NewBufferString("not-json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		newTestAPI(&mockStore{}).ServeHTTP(w, req)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST store error", func(t *testing.T) {
		ms := &mockStore{addAccount: func(stores.Account) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/account", stores.Account{Username: "alice"})
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE removes account", func(t *testing.T) {
		var gotUser, gotWG string
		ms := &mockStore{removeAccount: func(u, w string) error { gotUser, gotWG = u, w; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/account?username=Alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusNoContent)
		if gotUser != "alice" {
			t.Errorf("username lowercased: want %q, got %q", "alice", gotUser)
		}
		if gotWG != testUUID.String() {
			t.Errorf("workgroup: want %q, got %q", testUUID.String(), gotWG)
		}
	})

	t.Run("DELETE missing username returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodDelete, "/account", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("DELETE store error", func(t *testing.T) {
		ms := &mockStore{removeAccount: func(string, string) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/account?username=alice", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

func TestAccounts(t *testing.T) {
	accs := []stores.Account{
		{ID: 1, Username: "alice", Workgroup: testUUID.String()},
		{ID: 2, Username: "bob", Workgroup: testUUID.String()},
	}

	t.Run("GET returns accounts for workgroup", func(t *testing.T) {
		ms := &mockStore{
			findAccounts: func(wg string) ([]stores.Account, error) { return accs, nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/accounts?workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusOK)
		got := decodeJSON[[]stores.Account](t, w)
		if len(got) != 2 {
			t.Errorf("want 2 accounts, got %d", len(got))
		}
	})

	t.Run("GET store error", func(t *testing.T) {
		ms := &mockStore{findAccounts: func(string) ([]stores.Account, error) { return nil, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/accounts", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE removes accounts for workgroup", func(t *testing.T) {
		var gotWG string
		ms := &mockStore{removeAccounts: func(wg string) error { gotWG = wg; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/accounts?workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusNoContent)
		if gotWG != testUUID.String() {
			t.Errorf("workgroup: want %q, got %q", testUUID.String(), gotWG)
		}
	})

	t.Run("DELETE store error", func(t *testing.T) {
		ms := &mockStore{removeAccounts: func(string) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/accounts", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

func TestShares(t *testing.T) {
	t.Run("POST registers renterd share", func(t *testing.T) {
		var got stores.Share
		ms := &mockStore{registerShare: func(s stores.Share) error { got = s; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/share", stores.Share{
			Name: "MyShare", Type: "renterd", ServerName: "srv",
		})
		checkStatus(t, w, http.StatusNoContent)
		if got.Name != "myshare" {
			t.Errorf("name lowercased: want %q, got %q", "myshare", got.Name)
		}
		if got.Type != "renterd" {
			t.Errorf("type: want %q, got %q", "renterd", got.Type)
		}
	})

	t.Run("POST registers indexd share", func(t *testing.T) {
		ms := &mockStore{registerShare: func(s stores.Share) error { return nil }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/share", stores.Share{Name: "s2", Type: "indexd"})
		checkStatus(t, w, http.StatusNoContent)
	})

	t.Run("POST wrong type returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodPost, "/share", stores.Share{Name: "s", Type: "unknown"})
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST invalid JSON returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/share", bytes.NewBufferString("{bad}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		newTestAPI(&mockStore{}).ServeHTTP(w, req)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST store error", func(t *testing.T) {
		ms := &mockStore{registerShare: func(stores.Share) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/share", stores.Share{Name: "s", Type: "renterd"})
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("GET returns share without password", func(t *testing.T) {
		ms := &mockStore{getShare: foundShare("myshare", "renterd")}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare", nil)
		checkStatus(t, w, http.StatusOK)
		sh := decodeJSON[stores.Share](t, w)
		if sh.Name != "myshare" {
			t.Errorf("name: want %q, got %q", "myshare", sh.Name)
		}
		if sh.Password != "" {
			t.Error("password must not be exposed")
		}
	})

	t.Run("GET store error", func(t *testing.T) {
		ms := &mockStore{getShare: func(string) (stores.Share, error) { return stores.Share{}, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE unregisters share", func(t *testing.T) {
		var gotName string
		ms := &mockStore{unregisterShare: func(n string) error { gotName = n; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/share/myshare", nil)
		checkStatus(t, w, http.StatusNoContent)
		if gotName != "myshare" {
			t.Errorf("name: want %q, got %q", "myshare", gotName)
		}
	})

	t.Run("DELETE store error", func(t *testing.T) {
		ms := &mockStore{unregisterShare: func(string) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/share/myshare", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

func TestShareAccounts(t *testing.T) {
	ars := []stores.AccessRights{
		{ShareName: "myshare", AccountID: 1, ReadAccess: true},
	}

	t.Run("GET returns access rights", func(t *testing.T) {
		ms := &mockStore{
			getShare:    foundShare("myshare", "renterd"),
			getAccounts: func(stores.Share) ([]stores.AccessRights, error) { return ars, nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare/accounts", nil)
		checkStatus(t, w, http.StatusOK)
		got := decodeJSON[[]stores.AccessRights](t, w)
		if len(got) != 1 {
			t.Errorf("want 1, got %d", len(got))
		}
	})

	t.Run("GET getShare store error", func(t *testing.T) {
		ms := &mockStore{getShare: func(string) (stores.Share, error) { return stores.Share{}, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare/accounts", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("GET getAccounts store error", func(t *testing.T) {
		ms := &mockStore{
			getShare:    foundShare("myshare", "renterd"),
			getAccounts: func(stores.Share) ([]stores.AccessRights, error) { return nil, errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare/accounts", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

func TestPolicy(t *testing.T) {
	t.Run("GET returns access rights", func(t *testing.T) {
		ms := &mockStore{
			findAccount: foundAccount("alice", testUUID.String()),
			getShare:    foundShare("myshare", "renterd"),
			getAccessRights: func(stores.Share, stores.Account) (stores.AccessRights, error) {
				return stores.AccessRights{ShareName: "myshare", AccountID: 1, ReadAccess: true}, nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodGet,
			"/share/myshare/policy?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusOK)
		ar := decodeJSON[stores.AccessRights](t, w)
		if !ar.ReadAccess {
			t.Error("expected ReadAccess true")
		}
	})

	t.Run("GET missing username returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodGet, "/share/myshare/policy", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("GET findAccount store error", func(t *testing.T) {
		ms := &mockStore{
			findAccount: func(string, string) (stores.Account, error) { return stores.Account{}, errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare/policy?username=alice", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("GET getShare store error", func(t *testing.T) {
		ms := &mockStore{
			findAccount: foundAccount("alice", testUUID.String()),
			getShare:    func(string) (stores.Share, error) { return stores.Share{}, errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare/policy?username=alice", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("PUT sets access rights", func(t *testing.T) {
		var gotAR stores.AccessRights
		ms := &mockStore{
			getShare:        foundShare("myshare", "renterd"),
			findAccount:     foundAccount("alice", testUUID.String()),
			setAccessRights: func(ar stores.AccessRights) error { gotAR = ar; return nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/policy?username=alice&workgroup="+testUUID.String()+"&read=true&write=true", nil)
		checkStatus(t, w, http.StatusNoContent)
		if !gotAR.ReadAccess {
			t.Error("expected ReadAccess true")
		}
		if !gotAR.WriteAccess {
			t.Error("expected WriteAccess true")
		}
		if gotAR.DeleteAccess {
			t.Error("expected DeleteAccess false")
		}
	})

	t.Run("PUT share not found returns 400", func(t *testing.T) {
		ms := &mockStore{getShare: func(string) (stores.Share, error) { return stores.Share{}, nil }}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/missing/policy?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT missing username returns 400", func(t *testing.T) {
		ms := &mockStore{getShare: foundShare("myshare", "renterd")}
		w := doRequest(newTestAPI(ms), http.MethodPut, "/share/myshare/policy", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT setAccessRights store error", func(t *testing.T) {
		ms := &mockStore{
			getShare:        foundShare("myshare", "renterd"),
			findAccount:     foundAccount("alice", testUUID.String()),
			setAccessRights: func(stores.AccessRights) error { return errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/policy?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE removes access rights", func(t *testing.T) {
		called := false
		ms := &mockStore{
			findAccount:  foundAccount("alice", testUUID.String()),
			getShare:     foundShare("myshare", "renterd"),
			removeAccess: func(stores.Share, stores.Account) error { called = true; return nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete,
			"/share/myshare/policy?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusNoContent)
		if !called {
			t.Error("RemoveAccessRights not called")
		}
	})

	t.Run("DELETE missing username returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodDelete, "/share/myshare/policy", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("DELETE store error", func(t *testing.T) {
		ms := &mockStore{
			findAccount:  foundAccount("alice", testUUID.String()),
			getShare:     foundShare("myshare", "renterd"),
			removeAccess: func(stores.Share, stores.Account) error { return errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete,
			"/share/myshare/policy?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

func TestAccountShares(t *testing.T) {
	shares := []stores.Share{
		{Name: "s1", Type: "renterd", Password: "secret1"},
		{Name: "s2", Type: "indexd", Password: "secret2"},
	}

	t.Run("GET returns shares without passwords", func(t *testing.T) {
		ms := &mockStore{
			findAccount: foundAccount("alice", testUUID.String()),
			getShares:   func(stores.Account) ([]stores.Share, error) { return shares, nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet,
			"/account/shares?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusOK)
		got := decodeJSON[[]stores.Share](t, w)
		if len(got) != 2 {
			t.Errorf("want 2 shares, got %d", len(got))
		}
		for _, sh := range got {
			if sh.Password != "" {
				t.Errorf("share %q: password must not be exposed", sh.Name)
			}
		}
	})

	t.Run("GET missing username returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodGet, "/account/shares", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("GET getShares store error", func(t *testing.T) {
		ms := &mockStore{
			findAccount: foundAccount("alice", testUUID.String()),
			getShares:   func(stores.Account) ([]stores.Share, error) { return nil, errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/account/shares?username=alice", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE /account/policy clears all access rights", func(t *testing.T) {
		called := false
		ms := &mockStore{
			findAccount: foundAccount("alice", testUUID.String()),
			clearAccess: func(stores.Account) error { called = true; return nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete,
			"/account/policy?username=alice&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusNoContent)
		if !called {
			t.Error("ClearAccessRights not called")
		}
	})

	t.Run("DELETE /account/policy missing username returns 400", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodDelete, "/account/policy", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("DELETE /account/policy store error", func(t *testing.T) {
		ms := &mockStore{
			findAccount: foundAccount("alice", testUUID.String()),
			clearAccess: func(stores.Account) error { return errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/account/policy?username=alice", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

func TestWorkgroups(t *testing.T) {
	t.Run("POST creates workgroup without name", func(t *testing.T) {
		var got stores.Workgroup
		ms := &mockStore{addWorkgroup: func(wg stores.Workgroup) error { got = wg; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/workgroup", nil)
		checkStatus(t, w, http.StatusOK)
		resp := decodeJSON[WorkgroupResponse](t, w)
		if resp.UUID == (uuid.UUID{}) {
			t.Error("expected non-zero UUID in response")
		}
		if got.UUID != resp.UUID {
			t.Errorf("stored UUID %v does not match response UUID %v", got.UUID, resp.UUID)
		}
		if resp.Name != "" {
			t.Errorf("expected empty name, got %q", resp.Name)
		}
	})

	t.Run("POST creates workgroup with name", func(t *testing.T) {
		var got stores.Workgroup
		ms := &mockStore{addWorkgroup: func(wg stores.Workgroup) error { got = wg; return nil }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/workgroup", map[string]string{"name": "acme"})
		checkStatus(t, w, http.StatusOK)
		resp := decodeJSON[WorkgroupResponse](t, w)
		if resp.UUID == (uuid.UUID{}) {
			t.Error("expected non-zero UUID in response")
		}
		if resp.Name != "acme" {
			t.Errorf("name: want %q, got %q", "acme", resp.Name)
		}
		if got.Name != "acme" {
			t.Errorf("stored name: want %q, got %q", "acme", got.Name)
		}
	})

	t.Run("POST store error", func(t *testing.T) {
		ms := &mockStore{addWorkgroup: func(stores.Workgroup) error { return errStore }}
		w := doRequest(newTestAPI(ms), http.MethodPost, "/workgroup", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("GET returns workgroup by UUID", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: foundWorkgroup()}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/workgroup/"+testUUID.String(), nil)
		checkStatus(t, w, http.StatusOK)
		wg := decodeJSON[stores.Workgroup](t, w)
		if wg.UUID != testUUID {
			t.Errorf("uuid: want %v, got %v", testUUID, wg.UUID)
		}
	})

	t.Run("GET returns workgroup by name", func(t *testing.T) {
		ms := &mockStore{findWorkgroupByName: foundWorkgroupByName()}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/workgroup/"+testWorkgroupName, nil)
		checkStatus(t, w, http.StatusOK)
		wg := decodeJSON[stores.Workgroup](t, w)
		if wg.Name != testWorkgroupName {
			t.Errorf("name: want %q, got %q", testWorkgroupName, wg.Name)
		}
	})

	t.Run("GET not found by UUID returns 404", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: func(uuid.UUID) (stores.Workgroup, error) { return stores.Workgroup{}, nil }}
		other := uuid.MustParse("00000000-0000-0000-0000-000000000001")
		w := doRequest(newTestAPI(ms), http.MethodGet, "/workgroup/"+other.String(), nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("GET not found by name returns 404", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodGet, "/workgroup/unknown-name", nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("GET store error by UUID", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: func(uuid.UUID) (stores.Workgroup, error) { return stores.Workgroup{}, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/workgroup/"+testUUID.String(), nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("GET store error by name", func(t *testing.T) {
		ms := &mockStore{findWorkgroupByName: func(string) (stores.Workgroup, error) { return stores.Workgroup{}, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/workgroup/unknown-name", nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE removes workgroup by UUID", func(t *testing.T) {
		called := false
		ms := &mockStore{
			findWorkgroup:   foundWorkgroup(),
			removeWorkgroup: func(stores.Workgroup) error { called = true; return nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/workgroup/"+testUUID.String(), nil)
		checkStatus(t, w, http.StatusNoContent)
		if !called {
			t.Error("RemoveWorkgroup not called")
		}
	})

	t.Run("DELETE removes workgroup by name", func(t *testing.T) {
		called := false
		ms := &mockStore{
			findWorkgroupByName: foundWorkgroupByName(),
			removeWorkgroup:     func(stores.Workgroup) error { called = true; return nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/workgroup/"+testWorkgroupName, nil)
		checkStatus(t, w, http.StatusNoContent)
		if !called {
			t.Error("RemoveWorkgroup not called")
		}
	})

	t.Run("DELETE not found returns 404", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: func(uuid.UUID) (stores.Workgroup, error) { return stores.Workgroup{}, nil }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/workgroup/"+testUUID.String(), nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("DELETE store error", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup:   foundWorkgroup(),
			removeWorkgroup: func(stores.Workgroup) error { return errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete, "/workgroup/"+testUUID.String(), nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}

// TestConnectRequest tests POST /connect/:workgroup/:share.
// The happy path (which calls the real indexd SDK) requires a live indexd server
// and is therefore not covered here; only the validation paths are tested.
func TestConnectRequest(t *testing.T) {
	requestPath := "/connect/" + testUUID.String() + "/myshare"

	t.Run("POST unknown workgroup returns 404", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodPost, "/connect/unknown-name/myshare", nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("POST workgroup not found by UUID returns 404", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: func(uuid.UUID) (stores.Workgroup, error) { return stores.Workgroup{}, nil }}
		w := doRequest(newTestAPI(ms), http.MethodPost, requestPath, nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("POST workgroup store error returns 500", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: func(uuid.UUID) (stores.Workgroup, error) { return stores.Workgroup{}, errStore }}
		w := doRequest(newTestAPI(ms), http.MethodPost, requestPath, nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("POST share not found returns 404", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      func(string) (stores.Share, error) { return stores.Share{}, nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodPost, requestPath, nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("POST share store error returns 500", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      func(string) (stores.Share, error) { return stores.Share{}, errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodPost, requestPath, nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("POST renterd share returns 400", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "renterd"),
		}
		w := doRequest(newTestAPI(ms), http.MethodPost, requestPath, nil)
		checkStatus(t, w, http.StatusBadRequest)
	})
}

func TestConnect(t *testing.T) {
	path := "/connect/" + testUUID.String() + "/myshare"

	t.Run("PUT connects workgroup to renterd share (no body)", func(t *testing.T) {
		called := false
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "renterd"),
			addConnection: func(stores.Workgroup, stores.Share, types.PrivateKey) error {
				called = true
				return nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodPut, path, nil)
		checkStatus(t, w, http.StatusNoContent)
		if !called {
			t.Error("AddConnection not called")
		}
	})

	t.Run("PUT reconnects to indexd share with existing app key", func(t *testing.T) {
		appKey := make([]byte, 64)
		for i := range appKey {
			appKey[i] = byte(i)
		}
		var gotKey types.PrivateKey
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
			addConnection: func(_ stores.Workgroup, _ stores.Share, k types.PrivateKey) error {
				gotKey = k
				return nil
			},
		}
		body := map[string]string{"appKey": hex.EncodeToString(appKey)}
		w := doRequest(newTestAPI(ms), http.MethodPut, path, body)
		checkStatus(t, w, http.StatusOK)
		resp := decodeJSON[ConnectResponse](t, w)
		if resp.AppKey != hex.EncodeToString(appKey) {
			t.Errorf("appKey in response: want %q, got %q", hex.EncodeToString(appKey), resp.AppKey)
		}
		if len(gotKey) != 64 {
			t.Errorf("app key length stored: want 64, got %d", len(gotKey))
		}
		if gotKey[1] != 1 {
			t.Errorf("app key[1]: want 1, got %d", gotKey[1])
		}
	})

	t.Run("PUT unknown workgroup returns 404", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodPut, "/connect/unknown-name/myshare", nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("PUT workgroup not found by UUID returns 404", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: func(uuid.UUID) (stores.Workgroup, error) { return stores.Workgroup{}, nil }}
		w := doRequest(newTestAPI(ms), http.MethodPut, path, nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("PUT share not found returns 404", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      func(string) (stores.Share, error) { return stores.Share{}, nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodPut, path, nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("PUT invalid appKey hex returns 400", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
		}
		w := doRequest(newTestAPI(ms), http.MethodPut, path, map[string]string{"appKey": "not-hex!!"})
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT invalid JSON body returns 400", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
		}
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString("{bad}"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		newTestAPI(ms).ServeHTTP(w, req)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT indexd share without pending builder returns 400", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
		}
		w := doRequest(newTestAPI(ms), http.MethodPut, path, nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT store error", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "renterd"),
			addConnection: func(stores.Workgroup, stores.Share, types.PrivateKey) error { return errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodPut, path, nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})

	t.Run("DELETE disconnects workgroup from share", func(t *testing.T) {
		called := false
		ms := &mockStore{
			findWorkgroup:    foundWorkgroup(),
			getShare:         foundShare("myshare", "renterd"),
			removeConnection: func(stores.Workgroup, stores.Share) error { called = true; return nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete, path, nil)
		checkStatus(t, w, http.StatusNoContent)
		if !called {
			t.Error("RemoveConnection not called")
		}
	})

	t.Run("DELETE unknown workgroup returns 404", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodDelete, "/connect/unknown-name/myshare", nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("DELETE workgroup not found returns 404", func(t *testing.T) {
		ms := &mockStore{findWorkgroup: func(uuid.UUID) (stores.Workgroup, error) { return stores.Workgroup{}, nil }}
		w := doRequest(newTestAPI(ms), http.MethodDelete, path, nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("DELETE share not found returns 404", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      func(string) (stores.Share, error) { return stores.Share{}, nil },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete, path, nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("DELETE store error", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup:    foundWorkgroup(),
			getShare:         foundShare("myshare", "renterd"),
			removeConnection: func(stores.Workgroup, stores.Share) error { return errStore },
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete, path, nil)
		checkStatus(t, w, http.StatusInternalServerError)
	})
}
