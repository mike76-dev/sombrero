package stores

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/utils"
	"go.sia.tech/core/types"
	"golang.org/x/crypto/md4"
	"gopkg.in/yaml.v3"
)

// recordingShares is a share manager that records notifications and can be
// told to fail, so that rollbacks can be tested.
type recordingShares struct {
	registered   []string
	removed      []string
	updated      []AccessRights
	accessGone   []string // workgroup UUID + "/" + username, as keyed by the SMB server
	connected    []string
	disconnected []string
	fail         error
}

func (r *recordingShares) RegisterShare(sh Share) error {
	if r.fail != nil {
		return r.fail
	}
	r.registered = append(r.registered, sh.Name)
	return nil
}

func (r *recordingShares) RemoveShare(sh Share) error {
	if r.fail != nil {
		return r.fail
	}
	r.removed = append(r.removed, sh.Name)
	return nil
}

func (r *recordingShares) UpdateAccessRights(sh Share, ar AccessRights) error {
	if r.fail != nil {
		return r.fail
	}
	r.updated = append(r.updated, ar)
	return nil
}

func (r *recordingShares) RemoveAccess(acc Account) {
	r.accessGone = append(r.accessGone, acc.Workgroup+"/"+acc.Username)
}

func (r *recordingShares) AddConnection(wg Workgroup, sh Share, _ types.PrivateKey) error {
	if r.fail != nil {
		return r.fail
	}
	r.connected = append(r.connected, wg.UUID.String()+"/"+sh.Name)
	return nil
}

func (r *recordingShares) RemoveConnection(wg Workgroup, sh Share) error {
	if r.fail != nil {
		return r.fail
	}
	r.disconnected = append(r.disconnected, wg.UUID.String()+"/"+sh.Name)
	return nil
}

func newTestJSONStore(t *testing.T) (*JSONStore, *recordingShares) {
	t.Helper()
	js, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONStore: %v", err)
	}
	rs := &recordingShares{}
	js.WithShares(rs)
	return js, rs
}

func TestServerModeYAML(t *testing.T) {
	tests := []struct {
		in   string
		want ServerMode
		err  bool
	}{
		{"mode: normal", ModeNormal, false},
		{"mode: lite", ModeLite, false},
		{"mode: Lite", ModeLite, false},
		{"mode: \"\"", ModeNormal, false},
		{"mode: full", ModeNormal, true},
	}
	for _, tt := range tests {
		var cfg Config
		err := yaml.Unmarshal([]byte(tt.in), &cfg)
		if tt.err {
			if err == nil {
				t.Errorf("%q: expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tt.in, err)
		} else if cfg.Mode != tt.want {
			t.Errorf("%q: want %v, got %v", tt.in, tt.want, cfg.Mode)
		}
	}

	// Omitted mode defaults to normal.
	var cfg Config
	if err := yaml.Unmarshal([]byte("debug: true"), &cfg); err != nil {
		t.Fatal(err)
	} else if cfg.Mode != ModeNormal {
		t.Errorf("default mode: want normal, got %v", cfg.Mode)
	}

	// Round-trip.
	out, err := yaml.Marshal(Config{Mode: ModeLite})
	if err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(out), "mode: lite") {
		t.Errorf("marshalled config missing mode: %s", out)
	}
}

func TestJSONStoreWorkgroups(t *testing.T) {
	js, _ := newTestJSONStore(t)

	u1, u2 := uuid.New(), uuid.New()
	if err := js.AddWorkgroup(Workgroup{UUID: u1, Name: "first"}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	if err := js.AddWorkgroup(Workgroup{UUID: u2}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	if err := js.AddWorkgroup(Workgroup{UUID: u1}); err == nil {
		t.Fatal("expected duplicate UUID error")
	}
	if err := js.AddWorkgroup(Workgroup{UUID: uuid.New(), Name: "first"}); err == nil {
		t.Fatal("expected duplicate name error")
	}

	wg, err := js.FindWorkgroup(u1)
	if err != nil || wg.ID != 1 || wg.Name != "first" {
		t.Fatalf("FindWorkgroup: %v %+v", err, wg)
	}
	wg, err = js.FindWorkgroupByName("first")
	if err != nil || wg.UUID != u1 {
		t.Fatalf("FindWorkgroupByName: %v %+v", err, wg)
	}
	wg, err = js.GetWorkgroupByID(2)
	if err != nil || wg.UUID != u2 {
		t.Fatalf("GetWorkgroupByID: %v %+v", err, wg)
	}
	if wg, err := js.FindWorkgroup(uuid.New()); err != nil || wg.ID != 0 {
		t.Fatalf("missing workgroup should be zero: %v %+v", err, wg)
	}

	wg = Workgroup{ID: 1, PublicDirs: []string{"pub"}, CaseSensitive: true}
	if err := js.UpdateWorkgroup(wg); err != nil {
		t.Fatalf("UpdateWorkgroup: %v", err)
	}
	if got, _ := js.GetWorkgroupByID(1); len(got.PublicDirs) != 1 || !got.CaseSensitive {
		t.Fatalf("update not applied: %+v", got)
	}
	if err := js.UpdateWorkgroup(Workgroup{ID: 42}); err == nil {
		t.Fatal("expected workgroup not found error")
	}

	wgs, err := js.GetWorkgroups()
	if err != nil || len(wgs) != 2 || wgs[0].ID != 1 || wgs[1].ID != 2 {
		t.Fatalf("GetWorkgroups: %v %+v", err, wgs)
	}

	if err := js.RemoveWorkgroup(Workgroup{ID: 1}); err != nil {
		t.Fatalf("RemoveWorkgroup: %v", err)
	}
	if wgs, _ := js.GetWorkgroups(); len(wgs) != 1 {
		t.Fatalf("workgroup not removed: %+v", wgs)
	}

	// IDs are not reused.
	if err := js.AddWorkgroup(Workgroup{UUID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if wg, _ := js.FindWorkgroupByName(""); wg.ID != 0 {
		t.Fatal("empty name must not match unnamed workgroups")
	}
	wgs, _ = js.GetWorkgroups()
	if wgs[len(wgs)-1].ID != 3 {
		t.Fatalf("expected ID 3, got %d", wgs[len(wgs)-1].ID)
	}
}

func TestJSONStoreAccounts(t *testing.T) {
	js, rs := newTestJSONStore(t)

	u := uuid.New()
	if err := js.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatal(err)
	}

	if err := js.AddAccount(Account{Username: "user", Password: "secret", Workgroup: uuid.New().String()}); err == nil {
		t.Fatal("expected missing workgroup error")
	}
	if err := js.AddAccount(Account{Username: "user", Password: "secret", Workgroup: "not-a-uuid"}); err == nil {
		t.Fatal("expected invalid UUID error")
	}
	if err := js.AddAccount(Account{Username: "user", Password: "secret", Workgroup: u.String()}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := js.AddAccount(Account{Username: "user", Password: "other", Workgroup: u.String()}); err == nil {
		t.Fatal("expected duplicate account error")
	}

	h := md4.New()
	h.Write(utils.EncodeStringToBytes("secret"))
	wantHash := h.Sum(nil)

	acc, err := js.FindAccount("user", u.String())
	if err != nil || acc.ID != 1 || !bytes.Equal(acc.NTHash, wantHash) {
		t.Fatalf("FindAccount: %v %+v", err, acc)
	}
	if acc.Password != "" {
		t.Fatal("plaintext password must not be returned")
	}
	if got, _ := js.GetAccountByID(1); got.Username != "user" || got.Workgroup != u.String() {
		t.Fatalf("GetAccountByID: %+v", got)
	}
	if ok, _ := js.HasAccount("user", u.String()); !ok {
		t.Fatal("HasAccount: want true")
	}
	if ok, _ := js.HasAccount("other", u.String()); ok {
		t.Fatal("HasAccount: want false")
	}
	if accs, _ := js.FindAccounts(u.String()); len(accs) != 1 {
		t.Fatalf("FindAccounts: %+v", accs)
	}

	if err := js.RemoveAccount("user", u.String()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := js.HasAccount("user", u.String()); ok {
		t.Fatal("account not removed")
	}
	if len(rs.accessGone) != 1 || rs.accessGone[0] != u.String()+"/user" {
		t.Fatalf("RemoveAccess not called on RemoveAccount: %+v", rs.accessGone)
	}

	// RemoveAccounts clears the whole workgroup.
	js.AddAccount(Account{Username: "a", Workgroup: u.String()})
	js.AddAccount(Account{Username: "b", Workgroup: u.String()})
	if err := js.RemoveAccounts(u.String()); err != nil {
		t.Fatal(err)
	}
	if accs, _ := js.FindAccounts(u.String()); len(accs) != 0 {
		t.Fatalf("accounts not removed: %+v", accs)
	}
	if len(rs.accessGone) != 3 {
		t.Fatalf("RemoveAccess not called on RemoveAccounts: %+v", rs.accessGone)
	}
}

func TestJSONStoreShares(t *testing.T) {
	js, rs := newTestJSONStore(t)

	if err := js.RegisterShare(Share{Name: "idx", Type: "indexd", ServerName: "srv"}); !errors.Is(err, ErrLiteMode) {
		t.Fatalf("expected ErrLiteMode, got %v", err)
	}
	if err := js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv", Password: "pw", Bucket: "b"}); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}
	if err := js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"}); err == nil {
		t.Fatal("expected duplicate share error")
	}
	if len(rs.registered) != 1 || rs.registered[0] != "s1" {
		t.Fatalf("share manager not notified: %+v", rs.registered)
	}

	sh, err := js.GetShare("s1")
	if err != nil || sh.Type != "renterd" || sh.Password != "pw" || sh.CreatedAt.IsZero() {
		t.Fatalf("GetShare: %v %+v", err, sh)
	}
	if sh, _ := js.GetShare("nope"); sh.Name != "" {
		t.Fatalf("missing share should be zero: %+v", sh)
	}

	js.RegisterShare(Share{Name: "a1", Type: "renterd", ServerName: "srv"})
	shares, err := js.GetAllShares()
	if err != nil || len(shares) != 2 || shares[0].Name != "a1" || shares[1].Name != "s1" {
		t.Fatalf("GetAllShares: %v %+v", err, shares)
	}

	if err := js.UnregisterShare("s1"); err != nil {
		t.Fatal(err)
	}
	if len(rs.removed) != 1 || rs.removed[0] != "s1" {
		t.Fatalf("share manager not notified of removal: %+v", rs.removed)
	}
	if shares, _ := js.GetAllShares(); len(shares) != 1 {
		t.Fatalf("share not removed: %+v", shares)
	}
	if err := js.UnregisterShare("nope"); err != nil {
		t.Fatalf("removing a missing share must not fail: %v", err)
	}
}

func TestJSONStoreShareRollback(t *testing.T) {
	js, rs := newTestJSONStore(t)

	rs.fail = errors.New("renterd unreachable")
	if err := js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"}); err == nil {
		t.Fatal("expected error from share manager")
	}
	rs.fail = nil
	if shares, _ := js.GetAllShares(); len(shares) != 0 {
		t.Fatalf("failed registration must be rolled back: %+v", shares)
	}

	// The file on disk must not contain the share either.
	js2, err := NewJSONStore(filepath.Dir(js.path))
	if err != nil {
		t.Fatal(err)
	}
	if shares, _ := js2.GetAllShares(); len(shares) != 0 {
		t.Fatalf("rollback not persisted: %+v", shares)
	}
}

func TestJSONStorePolicies(t *testing.T) {
	js, rs := newTestJSONStore(t)

	u := uuid.New()
	js.AddWorkgroup(Workgroup{UUID: u})
	js.AddAccount(Account{Username: "user", Password: "pw", Workgroup: u.String()})
	js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"})
	acc, _ := js.FindAccount("user", u.String())
	wg, _ := js.FindWorkgroup(u)
	sh, _ := js.GetShare("s1")

	ar := AccessRights{ShareName: "s1", AccountID: acc.ID, ReadAccess: true, WriteAccess: true}

	// No connection yet.
	if err := js.SetAccessRights(ar); err == nil {
		t.Fatal("expected error without a connection")
	}
	if err := js.AddConnection(wg, sh, nil); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}
	if err := js.SetAccessRights(ar); err != nil {
		t.Fatalf("SetAccessRights: %v", err)
	}
	if len(rs.updated) != 1 {
		t.Fatalf("share manager not notified: %+v", rs.updated)
	}
	if err := js.SetAccessRights(AccessRights{ShareName: "s1", AccountID: 42, ReadAccess: true}); err == nil {
		t.Fatal("expected error for a missing account")
	}
	if err := js.SetAccessRights(AccessRights{ShareName: "nope", AccountID: acc.ID}); err == nil {
		t.Fatal("expected error for a missing share")
	}

	got, err := js.GetAccessRights(sh, acc)
	if err != nil || !got.ReadAccess || !got.WriteAccess || got.DeleteAccess {
		t.Fatalf("GetAccessRights: %v %+v", err, got)
	}

	// Upsert.
	ar.WriteAccess = false
	if err := js.SetAccessRights(ar); err != nil {
		t.Fatal(err)
	}
	if got, _ := js.GetAccessRights(sh, acc); got.WriteAccess {
		t.Fatal("policy not updated")
	}

	if shares, _ := js.GetShares(acc); len(shares) != 1 || shares[0].Name != "s1" {
		t.Fatalf("GetShares: %+v", shares)
	}
	if ars, _ := js.GetAccounts(sh); len(ars) != 1 || ars[0].AccountID != acc.ID {
		t.Fatalf("GetAccounts: %+v", ars)
	}

	if err := js.RemoveAccessRights(sh, acc); err != nil {
		t.Fatal(err)
	}
	if got, _ := js.GetAccessRights(sh, acc); got.AccountID != 0 {
		t.Fatalf("policy not removed: %+v", got)
	}

	// ClearAccessRights notifies the share manager.
	js.SetAccessRights(AccessRights{ShareName: "s1", AccountID: acc.ID, ReadAccess: true})
	if err := js.ClearAccessRights(acc); err != nil {
		t.Fatal(err)
	}
	if len(rs.accessGone) != 1 || rs.accessGone[0] != u.String()+"/user" {
		t.Fatalf("RemoveAccess not called: %+v", rs.accessGone)
	}
	if ars, _ := js.GetAccounts(sh); len(ars) != 0 {
		t.Fatalf("policies not cleared: %+v", ars)
	}
}

func TestJSONStoreConnections(t *testing.T) {
	js, rs := newTestJSONStore(t)

	u := uuid.New()
	js.AddWorkgroup(Workgroup{UUID: u})
	js.AddAccount(Account{Username: "user", Workgroup: u.String()})
	js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"})
	wg, _ := js.FindWorkgroup(u)
	sh, _ := js.GetShare("s1")
	acc, _ := js.FindAccount("user", u.String())

	if err := js.AddConnection(wg, Share{Name: "nope"}, nil); err == nil {
		t.Fatal("expected error for a missing share")
	}
	if err := js.AddConnection(Workgroup{UUID: uuid.New()}, sh, nil); err == nil {
		t.Fatal("expected error for a missing workgroup")
	}

	key := types.GeneratePrivateKey()
	if err := js.AddConnection(wg, sh, key); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}
	connected, gotKey, err := js.IsConnected(wg, sh)
	if err != nil || !connected || !bytes.Equal(gotKey, key) {
		t.Fatalf("IsConnected: %v %v", err, connected)
	}

	// Adding again keeps the existing key but still notifies the manager.
	if err := js.AddConnection(wg, sh, nil); err != nil {
		t.Fatal(err)
	}
	if _, gotKey, _ := js.IsConnected(wg, sh); !bytes.Equal(gotKey, key) {
		t.Fatal("existing app key must be kept")
	}
	if len(rs.connected) != 2 {
		t.Fatalf("share manager notifications: %+v", rs.connected)
	}

	newKey := types.GeneratePrivateKey()
	if err := js.SetAppKey(wg, sh, newKey); err != nil {
		t.Fatal(err)
	}
	if _, gotKey, _ := js.IsConnected(wg, sh); !bytes.Equal(gotKey, newKey) {
		t.Fatal("app key not updated")
	}

	// Removing the connection cascades the policies.
	if err := js.SetAccessRights(AccessRights{ShareName: "s1", AccountID: acc.ID, ReadAccess: true}); err != nil {
		t.Fatal(err)
	}
	if err := js.RemoveConnection(wg, sh); err != nil {
		t.Fatalf("RemoveConnection: %v", err)
	}
	if connected, _, _ := js.IsConnected(wg, sh); connected {
		t.Fatal("connection not removed")
	}
	if ars, _ := js.GetAccounts(sh); len(ars) != 0 {
		t.Fatalf("policies not cascaded: %+v", ars)
	}
	if len(rs.disconnected) != 1 {
		t.Fatalf("share manager not notified: %+v", rs.disconnected)
	}
}

func TestJSONStoreBans(t *testing.T) {
	js, _ := newTestJSONStore(t)

	if banned, _, err := js.IsBanned("1.2.3.4"); err != nil || banned {
		t.Fatalf("IsBanned: %v %v", err, banned)
	}
	if err := js.BanHost("1.2.3.4", "abuse"); err != nil {
		t.Fatal(err)
	}
	// A repeated ban keeps the original reason.
	if err := js.BanHost("1.2.3.4", "other"); err != nil {
		t.Fatal(err)
	}
	banned, reason, err := js.IsBanned("1.2.3.4")
	if err != nil || !banned || reason != "abuse" {
		t.Fatalf("IsBanned: %v %v %q", err, banned, reason)
	}

	if err := js.UnbanHost("1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if banned, _, _ := js.IsBanned("1.2.3.4"); banned {
		t.Fatal("host not unbanned")
	}

	js.BanHost("1.1.1.1", "")
	js.BanHost("2.2.2.2", "")
	if err := js.ClearBans(); err != nil {
		t.Fatal(err)
	}
	if banned, _, _ := js.IsBanned("1.1.1.1"); banned {
		t.Fatal("ban list not cleared")
	}
}

func TestJSONStoreWorkgroupCascade(t *testing.T) {
	js, rs := newTestJSONStore(t)

	u := uuid.New()
	js.AddWorkgroup(Workgroup{UUID: u})
	js.AddAccount(Account{Username: "user", Workgroup: u.String()})
	js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"})
	wg, _ := js.FindWorkgroup(u)
	sh, _ := js.GetShare("s1")
	acc, _ := js.FindAccount("user", u.String())
	js.AddConnection(wg, sh, nil)
	js.SetAccessRights(AccessRights{ShareName: "s1", AccountID: acc.ID, ReadAccess: true})

	if err := js.RemoveWorkgroup(wg); err != nil {
		t.Fatal(err)
	}
	if accs, _ := js.FindAccounts(u.String()); len(accs) != 0 {
		t.Fatalf("accounts not cascaded: %+v", accs)
	}
	if connected, _, _ := js.IsConnected(wg, sh); connected {
		t.Fatal("connection not cascaded")
	}
	if ars, _ := js.GetAccounts(sh); len(ars) != 0 {
		t.Fatalf("policies not cascaded: %+v", ars)
	}
	// The share itself stays.
	if shares, _ := js.GetAllShares(); len(shares) != 1 {
		t.Fatalf("share must not be removed: %+v", shares)
	}
	// The share manager is told to drop the connection and the account entries.
	if len(rs.disconnected) != 1 || rs.disconnected[0] != u.String()+"/s1" {
		t.Fatalf("RemoveConnection not called: %+v", rs.disconnected)
	}
	if len(rs.accessGone) != 1 || rs.accessGone[0] != u.String()+"/user" {
		t.Fatalf("RemoveAccess not called: %+v", rs.accessGone)
	}
}

func TestJSONStorePersistence(t *testing.T) {
	dir := t.TempDir()
	js, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	js.WithShares(&recordingShares{})

	u := uuid.New()
	js.AddWorkgroup(Workgroup{UUID: u, Name: "wg"})
	js.AddAccount(Account{Username: "user", Password: "pw", Workgroup: u.String()})
	js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv", Password: "apipw"})
	wg, _ := js.FindWorkgroup(u)
	sh, _ := js.GetShare("s1")
	acc, _ := js.FindAccount("user", u.String())
	js.AddConnection(wg, sh, nil)
	js.SetAccessRights(AccessRights{ShareName: "s1", AccountID: acc.ID, ReadAccess: true, ExecuteAccess: true})
	js.BanHost("1.2.3.4", "abuse")
	js.Close()

	js2, err := NewJSONStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	js2.WithShares(&recordingShares{})

	wg2, err := js2.FindWorkgroup(u)
	if err != nil || wg2.ID != wg.ID || wg2.Name != "wg" {
		t.Fatalf("workgroup not persisted: %v %+v", err, wg2)
	}
	acc2, err := js2.FindAccount("user", u.String())
	if err != nil || acc2.ID != acc.ID || !bytes.Equal(acc2.NTHash, acc.NTHash) {
		t.Fatalf("account not persisted: %v %+v", err, acc2)
	}
	sh2, err := js2.GetShare("s1")
	if err != nil || sh2.Password != "apipw" || !sh2.CreatedAt.Equal(sh.CreatedAt) {
		t.Fatalf("share not persisted: %v %+v", err, sh2)
	}
	if connected, _, _ := js2.IsConnected(wg2, sh2); !connected {
		t.Fatal("connection not persisted")
	}
	if ar, _ := js2.GetAccessRights(sh2, acc2); !ar.ReadAccess || !ar.ExecuteAccess || ar.WriteAccess {
		t.Fatalf("policy not persisted: %+v", ar)
	}
	if banned, reason, _ := js2.IsBanned("1.2.3.4"); !banned || reason != "abuse" {
		t.Fatal("ban not persisted")
	}

	// ID counters continue where they left off.
	u2 := uuid.New()
	js2.AddWorkgroup(Workgroup{UUID: u2})
	if wg3, _ := js2.FindWorkgroup(u2); wg3.ID != wg.ID+1 {
		t.Fatalf("workgroup ID counter not persisted: %d", wg3.ID)
	}
	js2.AddAccount(Account{Username: "user2", Workgroup: u2.String()})
	if acc3, _ := js2.FindAccount("user2", u2.String()); acc3.ID != acc.ID+1 {
		t.Fatalf("account ID counter not persisted: %d", acc3.ID)
	}

	// The persisted file must not contain plaintext account passwords.
	b, err := os.ReadFile(filepath.Join(dir, jsonStoreFilename))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"pw"`)) {
		t.Fatal("plaintext password persisted")
	}
}
