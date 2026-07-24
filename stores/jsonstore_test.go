package stores

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// The behavior the JSON store shares with the PostgreSQL store is tested by
// the suite in store_suite_test.go. Only what is specific to the Lite mode is
// tested here.

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

// TestJSONStoreShareTypes verifies that the Lite mode only supports renterd
// shares.
func TestJSONStoreShareTypes(t *testing.T) {
	js, rs := newTestJSONStore(t)

	if err := js.RegisterShare(Share{Name: "idx", Type: "indexd", ServerName: "srv"}); !errors.Is(err, ErrLiteMode) {
		t.Fatalf("expected ErrLiteMode, got %v", err)
	}
	if len(rs.registered) != 0 {
		t.Fatalf("share manager notified of a rejected share: %+v", rs.registered)
	}
	if shares, _ := js.GetAllShares(); len(shares) != 0 {
		t.Fatalf("rejected share was stored: %+v", shares)
	}
}

// TestJSONStoreIDs verifies that the store assigns the workgroup and account
// IDs itself and never reuses them.
func TestJSONStoreIDs(t *testing.T) {
	js, _ := newTestJSONStore(t)

	u1, u2 := uuid.New(), uuid.New()
	if err := js.AddWorkgroup(Workgroup{UUID: u1}); err != nil {
		t.Fatal(err)
	}
	if err := js.AddWorkgroup(Workgroup{UUID: u2}); err != nil {
		t.Fatal(err)
	}
	wg1, _ := js.FindWorkgroup(u1)
	wg2, _ := js.FindWorkgroup(u2)
	if wg1.ID != 1 || wg2.ID != 2 {
		t.Fatalf("workgroup IDs: got %d and %d", wg1.ID, wg2.ID)
	}

	if err := js.RemoveWorkgroup(wg1); err != nil {
		t.Fatal(err)
	}
	if err := js.AddWorkgroup(Workgroup{UUID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	wgs, _ := js.GetWorkgroups()
	if wgs[len(wgs)-1].ID != 3 {
		t.Fatalf("workgroup ID reused: %d", wgs[len(wgs)-1].ID)
	}

	acc1 := Account{Username: "a", Password: "pw", Workgroup: u2.String()}
	acc2 := Account{Username: "b", Password: "pw", Workgroup: u2.String()}
	if err := js.AddAccount(acc1); err != nil {
		t.Fatal(err)
	}
	if err := js.AddAccount(acc2); err != nil {
		t.Fatal(err)
	}
	a1, _ := js.FindAccount("a", u2.String())
	a2, _ := js.FindAccount("b", u2.String())
	if a1.ID != 1 || a2.ID != 2 {
		t.Fatalf("account IDs: got %d and %d", a1.ID, a2.ID)
	}
	if err := js.RemoveAccount("a", u2.String()); err != nil {
		t.Fatal(err)
	}
	if err := js.AddAccount(Account{Username: "c", Password: "pw", Workgroup: u2.String()}); err != nil {
		t.Fatal(err)
	}
	if a3, _ := js.FindAccount("c", u2.String()); a3.ID != 3 {
		t.Fatalf("account ID reused: %d", a3.ID)
	}
}

// TestJSONStoreRollbackPersisted verifies that a rolled back mutation is not
// left behind in the file on disk.
func TestJSONStoreRollbackPersisted(t *testing.T) {
	js, rs := newTestJSONStore(t)

	rs.fail = errors.New("renterd unreachable")
	if err := js.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"}); err == nil {
		t.Fatal("expected error from share manager")
	}
	rs.fail = nil

	js2, err := NewJSONStore(filepath.Dir(js.path))
	if err != nil {
		t.Fatal(err)
	}
	if shares, _ := js2.GetAllShares(); len(shares) != 0 {
		t.Fatalf("rollback not persisted: %+v", shares)
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
