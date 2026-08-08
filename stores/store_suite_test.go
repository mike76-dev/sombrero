package stores

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/utils"
	"go.sia.tech/core/types"
	"golang.org/x/crypto/md4"
)

// This file holds the tests that every Store implementation must pass. They
// only use the Store interface, so that the PostgreSQL-backed store used in
// the Normal mode and the JSON-backed store used in the Lite mode are held to
// the same contract. Behavior that is specific to one backend is tested in
// stores_test.go and jsonstore_test.go respectively.

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

// storeBackend is one implementation of the Store interface, together with the
// share manager it notifies.
type storeBackend struct {
	name string
	open func(t *testing.T) (Store, *recordingShares)
}

// storeBackends lists every implementation the shared suite runs against.
var storeBackends = []storeBackend{
	{
		name: "postgres",
		open: func(t *testing.T) (Store, *recordingShares) {
			t.Helper()
			db := NewTestStore(t, context.Background())
			t.Cleanup(db.Close)
			rs := &recordingShares{}
			db.WithShares(rs)
			return db, rs
		},
	},
	{
		name: "json",
		open: func(t *testing.T) (Store, *recordingShares) {
			t.Helper()
			js, rs := newTestJSONStore(t)
			t.Cleanup(js.Close)
			return js, rs
		},
	},
}

// forEachStore runs fn against every Store implementation as a subtest, each
// one on a freshly created, empty store.
func forEachStore(t *testing.T, fn func(t *testing.T, st Store, rs *recordingShares)) {
	t.Helper()
	for _, b := range storeBackends {
		t.Run(b.name, func(t *testing.T) {
			st, rs := b.open(t)
			fn(t, st, rs)
		})
	}
}

// addWorkgroup adds a workgroup and returns it as it was stored.
func addWorkgroup(t *testing.T, st Store, name string) Workgroup {
	t.Helper()
	u := uuid.New()
	if err := st.AddWorkgroup(Workgroup{UUID: u, Name: name}); err != nil {
		t.Fatalf("AddWorkgroup(%q): %v", name, err)
	}
	wg, err := st.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup(%q): %v", name, err)
	}
	if wg.ID == 0 {
		t.Fatalf("FindWorkgroup(%q): expected a stored workgroup", name)
	}
	return wg
}

// addAccount adds an account to the workgroup and returns it as it was stored.
func addAccount(t *testing.T, st Store, wg Workgroup, username, password string) Account {
	t.Helper()
	if err := st.AddAccount(Account{Username: username, Password: password, Workgroup: wg.UUID.String()}); err != nil {
		t.Fatalf("AddAccount(%q): %v", username, err)
	}
	acc, err := st.FindAccount(username, wg.UUID.String())
	if err != nil {
		t.Fatalf("FindAccount(%q): %v", username, err)
	}
	if acc.ID == 0 {
		t.Fatalf("FindAccount(%q): expected a stored account", username)
	}
	return acc
}

// addShare registers a renterd share, the only type both backends support,
// and returns it as it was stored.
func addShare(t *testing.T, st Store, name string) Share {
	t.Helper()
	if err := st.RegisterShare(Share{Name: name, Type: "renterd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare(%q): %v", name, err)
	}
	sh, err := st.GetShare(name)
	if err != nil {
		t.Fatalf("GetShare(%q): %v", name, err)
	}
	if sh.Name == "" {
		t.Fatalf("GetShare(%q): expected a stored share", name)
	}
	return sh
}

// shareNames returns the sorted names of the shares; the order in which a
// store returns them is not part of the contract.
func shareNames(shares []Share) []string {
	names := make([]string, 0, len(shares))
	for _, sh := range shares {
		names = append(names, sh.Name)
	}
	sort.Strings(names)
	return names
}

// accountIDs returns the sorted account IDs of the access rights.
func accountIDs(ars []AccessRights) []int {
	ids := make([]int, 0, len(ars))
	for _, ar := range ars {
		ids = append(ids, ar.AccountID)
	}
	sort.Ints(ids)
	return ids
}

// usernames returns the sorted user names of the accounts.
func usernames(accs []Account) []string {
	names := make([]string, 0, len(accs))
	for _, acc := range accs {
		names = append(names, acc.Username)
	}
	sort.Strings(names)
	return names
}

func TestStoreWorkgroups(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, _ *recordingShares) {
		u1, u2 := uuid.New(), uuid.New()
		if err := st.AddWorkgroup(Workgroup{UUID: u1, Name: "first"}); err != nil {
			t.Fatalf("AddWorkgroup: %v", err)
		}
		if err := st.AddWorkgroup(Workgroup{UUID: u2}); err != nil {
			t.Fatalf("AddWorkgroup unnamed: %v", err)
		}

		// UUIDs and names are unique.
		if err := st.AddWorkgroup(Workgroup{UUID: u1}); err == nil {
			t.Fatal("AddWorkgroup: expected duplicate UUID error")
		}
		if err := st.AddWorkgroup(Workgroup{UUID: uuid.New(), Name: "first"}); err == nil {
			t.Fatal("AddWorkgroup: expected duplicate name error")
		}

		// FindWorkgroup by UUID.
		wg1, err := st.FindWorkgroup(u1)
		if err != nil {
			t.Fatalf("FindWorkgroup: %v", err)
		}
		if wg1.ID == 0 || wg1.UUID != u1 || wg1.Name != "first" {
			t.Fatalf("FindWorkgroup: got %+v", wg1)
		}

		// The unnamed workgroup has an empty name.
		wg2, err := st.FindWorkgroup(u2)
		if err != nil {
			t.Fatalf("FindWorkgroup unnamed: %v", err)
		}
		if wg2.ID == 0 || wg2.Name != "" {
			t.Fatalf("FindWorkgroup unnamed: got %+v", wg2)
		}

		// FindWorkgroupByName.
		byName, err := st.FindWorkgroupByName("first")
		if err != nil {
			t.Fatalf("FindWorkgroupByName: %v", err)
		}
		if byName.ID != wg1.ID || byName.UUID != u1 {
			t.Fatalf("FindWorkgroupByName: got %+v", byName)
		}

		// GetWorkgroupByID.
		byID, err := st.GetWorkgroupByID(wg2.ID)
		if err != nil {
			t.Fatalf("GetWorkgroupByID: %v", err)
		}
		if byID.UUID != u2 {
			t.Fatalf("GetWorkgroupByID: want UUID %v, got %+v", u2, byID)
		}

		// Unknown workgroups are returned as zero values, not as errors.
		if wg, err := st.FindWorkgroup(uuid.New()); err != nil || wg.ID != 0 {
			t.Fatalf("FindWorkgroup missing: %v %+v", err, wg)
		}
		if wg, err := st.FindWorkgroupByName("unknown"); err != nil || wg.ID != 0 {
			t.Fatalf("FindWorkgroupByName missing: %v %+v", err, wg)
		}
		if wg, err := st.GetWorkgroupByID(999999); err != nil || wg.ID != 0 {
			t.Fatalf("GetWorkgroupByID missing: %v %+v", err, wg)
		}

		// An empty name must not match the unnamed workgroup.
		if wg, err := st.FindWorkgroupByName(""); err != nil || wg.ID != 0 {
			t.Fatalf("FindWorkgroupByName(\"\"): %v %+v", err, wg)
		}

		// GetWorkgroups lists all workgroups ordered by ID.
		wgs, err := st.GetWorkgroups()
		if err != nil {
			t.Fatalf("GetWorkgroups: %v", err)
		}
		if len(wgs) != 2 {
			t.Fatalf("GetWorkgroups: want 2, got %+v", wgs)
		}
		if wgs[0].ID > wgs[1].ID {
			t.Fatalf("GetWorkgroups: not ordered by ID: %+v", wgs)
		}
		if wgs[0].UUID != u1 || wgs[1].UUID != u2 {
			t.Fatalf("GetWorkgroups: got %+v", wgs)
		}

		// RemoveWorkgroup leaves the other workgroup alone.
		if err := st.RemoveWorkgroup(wg1); err != nil {
			t.Fatalf("RemoveWorkgroup: %v", err)
		}
		if wg, err := st.FindWorkgroup(u1); err != nil || wg.ID != 0 {
			t.Fatalf("FindWorkgroup after remove: %v %+v", err, wg)
		}
		if wgs, err := st.GetWorkgroups(); err != nil || len(wgs) != 1 || wgs[0].UUID != u2 {
			t.Fatalf("GetWorkgroups after remove: %v %+v", err, wgs)
		}
	})
}

// A workgroup name is what a client sends as the NTLM domain, and a domain name is not
// case-sensitive: WRG\test and wrg\test are the same login, and neither client can be asked to
// know the case the workgroup was created with. So the name has to be found however it is spelled,
// and a second workgroup must not be able to take the same name in another case, which would leave
// two workgroups that no client could choose between.
func TestStoreWorkgroupNamesAreCaseInsensitive(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, _ *recordingShares) {
		u := uuid.New()
		if err := st.AddWorkgroup(Workgroup{UUID: u, Name: "WrG"}); err != nil {
			t.Fatalf("AddWorkgroup: %v", err)
		}

		// The name is folded on the way in, so it reads back in the one form however it was
		// given. Anything keyed by the name, the SMB server included, sees only that form.
		wg, err := st.FindWorkgroup(u)
		if err != nil {
			t.Fatalf("FindWorkgroup: %v", err)
		}
		if wg.Name != "wrg" {
			t.Fatalf("stored name: want %q, got %q", "wrg", wg.Name)
		}
		if byID, err := st.GetWorkgroupByID(wg.ID); err != nil || byID.Name != "wrg" {
			t.Fatalf("GetWorkgroupByID: %v %+v", err, byID)
		}

		for _, name := range []string{"wrg", "WRG", "Wrg", "wRG"} {
			byName, err := st.FindWorkgroupByName(name)
			if err != nil {
				t.Fatalf("FindWorkgroupByName(%q): %v", name, err)
			}
			if byName.ID != wg.ID || byName.UUID != u {
				t.Fatalf("FindWorkgroupByName(%q): got %+v", name, byName)
			}
		}

		if err := st.AddWorkgroup(Workgroup{UUID: uuid.New(), Name: "WRG"}); err == nil {
			t.Fatal("AddWorkgroup: a name differing only in case was accepted")
		}
	})
}

// samePublicDirs compares two public folder lists, order included.
func samePublicDirs(a, b []PublicDir) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStoreWorkgroupSettings(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, _ *recordingShares) {
		wg := addWorkgroup(t, st, "")

		// A new workgroup has no public folders.
		if len(wg.PublicDirs) != 0 {
			t.Fatalf("PublicDirs: want empty, got %v", wg.PublicDirs)
		}

		// UpdateWorkgroup persists the folders together with their flags.
		want := []PublicDir{
			{Path: "shared"},
			{Path: "public", ReadOnly: true, CaseSensitive: true},
		}
		wg.PublicDirs = want
		if err := st.UpdateWorkgroup(wg); err != nil {
			t.Fatalf("UpdateWorkgroup: %v", err)
		}

		// Every getter reflects the update.
		updated, err := st.FindWorkgroup(wg.UUID)
		if err != nil {
			t.Fatalf("FindWorkgroup after update: %v", err)
		}
		if !samePublicDirs(updated.PublicDirs, want) {
			t.Fatalf("PublicDirs: want %+v, got %+v", want, updated.PublicDirs)
		}
		byID, err := st.GetWorkgroupByID(wg.ID)
		if err != nil {
			t.Fatalf("GetWorkgroupByID after update: %v", err)
		}
		if !samePublicDirs(byID.PublicDirs, want) {
			t.Fatalf("GetWorkgroupByID after update: got %+v", byID)
		}

		// A folder can be flipped between read-only and rewritable.
		wg.PublicDirs = []PublicDir{{Path: "shared", ReadOnly: true}}
		if err := st.UpdateWorkgroup(wg); err != nil {
			t.Fatalf("UpdateWorkgroup flip: %v", err)
		}
		flipped, err := st.FindWorkgroup(wg.UUID)
		if err != nil {
			t.Fatalf("FindWorkgroup after flip: %v", err)
		}
		if !samePublicDirs(flipped.PublicDirs, []PublicDir{{Path: "shared", ReadOnly: true}}) {
			t.Fatalf("after flip: got %+v", flipped.PublicDirs)
		}

		// Clearing the folders sets them back to empty.
		wg.PublicDirs = nil
		if err := st.UpdateWorkgroup(wg); err != nil {
			t.Fatalf("UpdateWorkgroup clear: %v", err)
		}
		cleared, err := st.FindWorkgroup(wg.UUID)
		if err != nil {
			t.Fatalf("FindWorkgroup after clear: %v", err)
		}
		if len(cleared.PublicDirs) != 0 {
			t.Fatalf("after clear: got %+v", cleared)
		}

		// UpdateWorkgroup on a missing ID returns an error.
		if err := st.UpdateWorkgroup(Workgroup{ID: 999999, PublicDirs: []PublicDir{{Path: "x"}}}); err == nil {
			t.Fatal("UpdateWorkgroup missing ID: expected error, got nil")
		}

		// The folders can also be passed to AddWorkgroup directly.
		u := uuid.New()
		reports := []PublicDir{{Path: "reports", ReadOnly: true, CaseSensitive: true}}
		if err := st.AddWorkgroup(Workgroup{UUID: u, Name: "labeled", PublicDirs: reports}); err != nil {
			t.Fatalf("AddWorkgroup with dirs: %v", err)
		}
		added, err := st.FindWorkgroup(u)
		if err != nil {
			t.Fatalf("FindWorkgroup with dirs: %v", err)
		}
		if !samePublicDirs(added.PublicDirs, reports) {
			t.Fatalf("FindWorkgroup with dirs: got %+v", added)
		}
		byName, err := st.FindWorkgroupByName("labeled")
		if err != nil {
			t.Fatalf("FindWorkgroupByName with dirs: %v", err)
		}
		if !samePublicDirs(byName.PublicDirs, reports) {
			t.Fatalf("FindWorkgroupByName with dirs: got %+v", byName)
		}
	})
}

// TestStoreDuplicatePublicDirs verifies that entries with an empty path are
// dropped and that duplicate paths collapse to the first entry, matching the
// first-match-wins rule of FindPublicDir and the directory restamping.
func TestStoreDuplicatePublicDirs(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, _ *recordingShares) {
		dirty := []PublicDir{
			{Path: "dup"},
			{Path: ""},
			{Path: "dup", ReadOnly: true},
			{Path: "other", CaseSensitive: true},
		}
		want := []PublicDir{
			{Path: "dup"},
			{Path: "other", CaseSensitive: true},
		}

		wg := addWorkgroup(t, st, "")
		wg.PublicDirs = dirty
		if err := st.UpdateWorkgroup(wg); err != nil {
			t.Fatalf("UpdateWorkgroup: %v", err)
		}
		updated, err := st.FindWorkgroup(wg.UUID)
		if err != nil {
			t.Fatalf("FindWorkgroup after update: %v", err)
		}
		if !samePublicDirs(updated.PublicDirs, want) {
			t.Fatalf("UpdateWorkgroup: want %+v, got %+v", want, updated.PublicDirs)
		}

		u := uuid.New()
		if err := st.AddWorkgroup(Workgroup{UUID: u, PublicDirs: dirty}); err != nil {
			t.Fatalf("AddWorkgroup: %v", err)
		}
		added, err := st.FindWorkgroup(u)
		if err != nil {
			t.Fatalf("FindWorkgroup after add: %v", err)
		}
		if !samePublicDirs(added.PublicDirs, want) {
			t.Fatalf("AddWorkgroup: want %+v, got %+v", want, added.PublicDirs)
		}
	})
}

func TestPublicDirMatches(t *testing.T) {
	tests := []struct {
		dir  PublicDir
		name string
		want bool
	}{
		{PublicDir{Path: "shared"}, "shared", true},
		{PublicDir{Path: "shared"}, "SHARED", true},
		{PublicDir{Path: "shared"}, "other", false},
		{PublicDir{Path: "shared", CaseSensitive: true}, "shared", true},
		{PublicDir{Path: "shared", CaseSensitive: true}, "SHARED", false},
	}
	for _, tt := range tests {
		if got := tt.dir.Matches(tt.name); got != tt.want {
			t.Errorf("%+v.Matches(%q) = %v, want %v", tt.dir, tt.name, got, tt.want)
		}
	}
}

func TestStoreAccounts(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, rs *recordingShares) {
		wg := addWorkgroup(t, st, "")
		u := wg.UUID.String()

		// The workgroup must exist and its UUID must be valid.
		if err := st.AddAccount(Account{Username: "alice", Password: "secret", Workgroup: uuid.New().String()}); err == nil {
			t.Fatal("AddAccount: expected missing workgroup error")
		}
		if err := st.AddAccount(Account{Username: "alice", Password: "secret", Workgroup: "not-a-uuid"}); err == nil {
			t.Fatal("AddAccount: expected invalid UUID error")
		}

		if err := st.AddAccount(Account{Username: "alice", Password: "secret", Workgroup: u}); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
		if err := st.AddAccount(Account{Username: "alice", Password: "other", Workgroup: u}); err == nil {
			t.Fatal("AddAccount: expected duplicate account error")
		}

		// The password is stored as an NT hash and never returned in plaintext.
		h := md4.New()
		h.Write(utils.EncodeStringToBytes("secret"))
		wantHash := h.Sum(nil)

		acc, err := st.FindAccount("alice", u)
		if err != nil {
			t.Fatalf("FindAccount: %v", err)
		}
		if acc.ID == 0 || acc.Username != "alice" || acc.Workgroup != u {
			t.Fatalf("FindAccount: got %+v", acc)
		}
		if !bytes.Equal(acc.NTHash, wantHash) {
			t.Fatalf("FindAccount: NT hash mismatch, got %x", acc.NTHash)
		}
		if acc.Password != "" {
			t.Fatal("FindAccount: plaintext password must not be returned")
		}

		// GetAccountByID returns the same account.
		byID, err := st.GetAccountByID(acc.ID)
		if err != nil {
			t.Fatalf("GetAccountByID: %v", err)
		}
		if byID.Username != "alice" || byID.Workgroup != u || !bytes.Equal(byID.NTHash, wantHash) {
			t.Fatalf("GetAccountByID: got %+v", byID)
		}
		if byID.Password != "" {
			t.Fatal("GetAccountByID: plaintext password must not be returned")
		}

		// An unknown account is reported as an error and never as a zero value. A zero
		// Account looks usable to a caller that only checks the error, and its nil NTHash
		// authenticates anybody who computes NTOWFv2 over it.
		if got, err := st.FindAccount("nobody", u); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("FindAccount missing: got %v, %+v, want ErrAccountNotFound", err, got)
		}
		if got, err := st.GetAccountByID(999999); !errors.Is(err, ErrAccountNotFound) {
			t.Fatalf("GetAccountByID missing: got %v, %+v, want ErrAccountNotFound", err, got)
		}

		// HasAccount.
		if has, err := st.HasAccount("alice", u); err != nil || !has {
			t.Fatalf("HasAccount: %v %v", err, has)
		}
		if has, err := st.HasAccount("nobody", u); err != nil || has {
			t.Fatalf("HasAccount missing: %v %v", err, has)
		}

		// Accounts are scoped to their workgroup.
		other := addWorkgroup(t, st, "other")
		addAccount(t, st, other, "alice", "secret")
		if has, err := st.HasAccount("alice", other.UUID.String()); err != nil || !has {
			t.Fatalf("HasAccount in other workgroup: %v %v", err, has)
		}

		// FindAccounts lists the accounts of one workgroup only.
		addAccount(t, st, wg, "bob", "pw")
		accs, err := st.FindAccounts(u)
		if err != nil {
			t.Fatalf("FindAccounts: %v", err)
		}
		if got := usernames(accs); len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
			t.Fatalf("FindAccounts: want [alice bob], got %v", got)
		}
		for _, a := range accs {
			if a.Workgroup != u {
				t.Fatalf("FindAccounts: want workgroup %q, got %q", u, a.Workgroup)
			}
			if a.Password != "" {
				t.Fatal("FindAccounts: plaintext password must not be returned")
			}
		}

		// RemoveAccount removes a single account and notifies the share manager.
		if err := st.RemoveAccount("alice", u); err != nil {
			t.Fatalf("RemoveAccount: %v", err)
		}
		if has, err := st.HasAccount("alice", u); err != nil || has {
			t.Fatalf("HasAccount after remove: %v %v", err, has)
		}
		if len(rs.accessGone) != 1 || rs.accessGone[0] != u+"/alice" {
			t.Fatalf("RemoveAccess not called on RemoveAccount: %+v", rs.accessGone)
		}
		// The account of the same name in the other workgroup is untouched.
		if has, err := st.HasAccount("alice", other.UUID.String()); err != nil || !has {
			t.Fatalf("HasAccount in other workgroup after remove: %v %v", err, has)
		}

		// RemoveAccounts clears the whole workgroup and notifies for each account.
		rs.accessGone = nil
		addAccount(t, st, wg, "carol", "pw")
		if err := st.RemoveAccounts(u); err != nil {
			t.Fatalf("RemoveAccounts: %v", err)
		}
		if accs, err := st.FindAccounts(u); err != nil || len(accs) != 0 {
			t.Fatalf("FindAccounts after RemoveAccounts: %v %+v", err, accs)
		}
		if len(rs.accessGone) != 2 {
			t.Fatalf("RemoveAccess not called for every account: %+v", rs.accessGone)
		}
		if accs, err := st.FindAccounts(other.UUID.String()); err != nil || len(accs) != 1 {
			t.Fatalf("RemoveAccounts must not touch other workgroups: %v %+v", err, accs)
		}

		// Every lookup rejects an invalid workgroup UUID.
		if _, err := st.FindAccount("alice", "not-a-uuid"); err == nil {
			t.Fatal("FindAccount: expected invalid UUID error")
		}
		if _, err := st.HasAccount("alice", "not-a-uuid"); err == nil {
			t.Fatal("HasAccount: expected invalid UUID error")
		}
		if _, err := st.FindAccounts("not-a-uuid"); err == nil {
			t.Fatal("FindAccounts: expected invalid UUID error")
		}
		if err := st.RemoveAccount("alice", "not-a-uuid"); err == nil {
			t.Fatal("RemoveAccount: expected invalid UUID error")
		}
		if err := st.RemoveAccounts("not-a-uuid"); err == nil {
			t.Fatal("RemoveAccounts: expected invalid UUID error")
		}
	})
}

func TestStoreShares(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, rs *recordingShares) {
		share := Share{
			Name:         "mybucket",
			Type:         "renterd",
			ServerName:   "localhost",
			Password:     "apipass",
			Bucket:       "files",
			Remark:       "test share",
			DataShards:   10,
			ParityShards: 4,
		}
		if err := st.RegisterShare(share); err != nil {
			t.Fatalf("RegisterShare: %v", err)
		}
		if len(rs.registered) != 1 || rs.registered[0] != share.Name {
			t.Fatalf("share manager not notified: %+v", rs.registered)
		}

		// Share names are unique.
		if err := st.RegisterShare(share); err == nil {
			t.Fatal("RegisterShare: expected duplicate share error")
		}

		// GetShare round-trips every field.
		got, err := st.GetShare(share.Name)
		if err != nil {
			t.Fatalf("GetShare: %v", err)
		}
		if got.Name != share.Name || got.Type != share.Type || got.ServerName != share.ServerName {
			t.Fatalf("GetShare: got %+v", got)
		}
		if got.Password != share.Password || got.Bucket != share.Bucket || got.Remark != share.Remark {
			t.Fatalf("GetShare: got %+v", got)
		}
		if got.DataShards != share.DataShards || got.ParityShards != share.ParityShards {
			t.Fatalf("GetShare: want %d/%d shards, got %d/%d", share.DataShards, share.ParityShards, got.DataShards, got.ParityShards)
		}
		if got.CreatedAt.IsZero() {
			t.Fatal("GetShare: expected a creation timestamp")
		}

		// Unknown and empty share names are returned as zero values.
		if empty, err := st.GetShare("nosuchshare"); err != nil || empty.Name != "" {
			t.Fatalf("GetShare missing: %v %+v", err, empty)
		}
		if empty, err := st.GetShare(""); err != nil || empty.Name != "" {
			t.Fatalf("GetShare(\"\"): %v %+v", err, empty)
		}

		// GetAllShares lists the shares ordered by name.
		addShare(t, st, "another")
		shares, err := st.GetAllShares()
		if err != nil {
			t.Fatalf("GetAllShares: %v", err)
		}
		if len(shares) != 2 || shares[0].Name != "another" || shares[1].Name != "mybucket" {
			t.Fatalf("GetAllShares: want [another mybucket], got %v", shareNames(shares))
		}

		// UnregisterShare removes the share and notifies the share manager.
		if err := st.UnregisterShare(share.Name); err != nil {
			t.Fatalf("UnregisterShare: %v", err)
		}
		if len(rs.removed) != 1 || rs.removed[0] != share.Name {
			t.Fatalf("share manager not notified of removal: %+v", rs.removed)
		}
		if gone, err := st.GetShare(share.Name); err != nil || gone.Name != "" {
			t.Fatalf("GetShare after unregister: %v %+v", err, gone)
		}
		if shares, err := st.GetAllShares(); err != nil || len(shares) != 1 {
			t.Fatalf("GetAllShares after unregister: %v %v", err, shareNames(shares))
		}

		// Unregistering an unknown or empty share name is not an error.
		if err := st.UnregisterShare("nosuchshare"); err != nil {
			t.Fatalf("UnregisterShare missing: %v", err)
		}
		if err := st.UnregisterShare(""); err != nil {
			t.Fatalf("UnregisterShare(\"\"): %v", err)
		}
		if len(rs.removed) != 1 {
			t.Fatalf("share manager notified for a missing share: %+v", rs.removed)
		}
	})
}

// TestStoreShareRollback verifies that a share is not persisted if the share
// manager refuses to open it.
func TestStoreShareRollback(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, rs *recordingShares) {
		rs.fail = errors.New("renterd unreachable")
		if err := st.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"}); err == nil {
			t.Fatal("RegisterShare: expected error from the share manager")
		}
		rs.fail = nil

		if shares, err := st.GetAllShares(); err != nil || len(shares) != 0 {
			t.Fatalf("failed registration must be rolled back: %v %v", err, shareNames(shares))
		}
		if sh, err := st.GetShare("s1"); err != nil || sh.Name != "" {
			t.Fatalf("failed registration must be rolled back: %v %+v", err, sh)
		}
	})
}

// TestStoreShareCascade verifies that unregistering a share also drops the
// connections and the access policies that refer to it.
func TestStoreShareCascade(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, _ *recordingShares) {
		wg := addWorkgroup(t, st, "")
		acc := addAccount(t, st, wg, "user", "pw")
		sh := addShare(t, st, "s1")
		kept := addShare(t, st, "s2")

		for _, s := range []Share{sh, kept} {
			if err := st.AddConnection(wg, s, types.GeneratePrivateKey()); err != nil {
				t.Fatalf("AddConnection %s: %v", s.Name, err)
			}
			if err := st.SetAccessRights(AccessRights{ShareName: s.Name, AccountID: acc.ID, ReadAccess: true}); err != nil {
				t.Fatalf("SetAccessRights %s: %v", s.Name, err)
			}
		}

		if err := st.UnregisterShare(sh.Name); err != nil {
			t.Fatalf("UnregisterShare: %v", err)
		}
		if connected, _, err := st.IsConnected(wg, sh); err != nil || connected {
			t.Fatalf("connection not cascaded: %v %v", err, connected)
		}
		if ars, err := st.GetAccounts(sh); err != nil || len(ars) != 0 {
			t.Fatalf("policies not cascaded: %v %+v", err, ars)
		}
		if ar, err := st.GetAccessRights(sh, acc); err != nil || ar.AccountID != 0 {
			t.Fatalf("policy not cascaded: %v %+v", err, ar)
		}

		// The other share keeps its connection and its policy.
		if connected, _, err := st.IsConnected(wg, kept); err != nil || !connected {
			t.Fatalf("unrelated connection removed: %v %v", err, connected)
		}
		if ar, err := st.GetAccessRights(kept, acc); err != nil || !ar.ReadAccess {
			t.Fatalf("unrelated policy removed: %v %+v", err, ar)
		}
		if shares, err := st.GetShares(acc); err != nil || len(shares) != 1 || shares[0].Name != kept.Name {
			t.Fatalf("GetShares after unregister: %v %v", err, shareNames(shares))
		}
	})
}

func TestStoreConnections(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, rs *recordingShares) {
		wg := addWorkgroup(t, st, "")
		sh := addShare(t, st, "s1")

		// Both ends of the connection must exist.
		if err := st.AddConnection(wg, Share{Name: "nosuchshare"}, nil); err == nil {
			t.Fatal("AddConnection: expected error for a missing share")
		}
		if err := st.AddConnection(Workgroup{ID: 999999, UUID: uuid.New()}, sh, nil); err == nil {
			t.Fatal("AddConnection: expected error for a missing workgroup")
		}

		// Not connected initially.
		if connected, _, err := st.IsConnected(wg, sh); err != nil || connected {
			t.Fatalf("IsConnected initial: %v %v", err, connected)
		}

		key := types.GeneratePrivateKey()
		if err := st.AddConnection(wg, sh, key); err != nil {
			t.Fatalf("AddConnection: %v", err)
		}
		connected, gotKey, err := st.IsConnected(wg, sh)
		if err != nil {
			t.Fatalf("IsConnected: %v", err)
		}
		if !connected {
			t.Fatal("IsConnected: expected true")
		}
		if !bytes.Equal(gotKey, key) {
			t.Fatal("IsConnected: app key mismatch")
		}

		// Connecting again keeps the existing key, but still notifies the manager.
		if err := st.AddConnection(wg, sh, types.GeneratePrivateKey()); err != nil {
			t.Fatalf("AddConnection idempotent: %v", err)
		}
		if _, gotKey, _ := st.IsConnected(wg, sh); !bytes.Equal(gotKey, key) {
			t.Fatal("AddConnection idempotent: existing app key must be kept")
		}
		if len(rs.connected) != 2 {
			t.Fatalf("share manager notifications: %+v", rs.connected)
		}

		// SetAppKey replaces the key.
		newKey := types.GeneratePrivateKey()
		if err := st.SetAppKey(wg, sh, newKey); err != nil {
			t.Fatalf("SetAppKey: %v", err)
		}
		if _, gotKey, _ := st.IsConnected(wg, sh); !bytes.Equal(gotKey, newKey) {
			t.Fatal("SetAppKey: app key not updated")
		}

		// A different workgroup is not connected to the same share.
		other := addWorkgroup(t, st, "other")
		if connected, _, err := st.IsConnected(other, sh); err != nil || connected {
			t.Fatalf("IsConnected for another workgroup: %v %v", err, connected)
		}

		// RemoveConnection notifies the share manager and cascades the policies.
		acc := addAccount(t, st, wg, "user", "pw")
		if err := st.SetAccessRights(AccessRights{ShareName: sh.Name, AccountID: acc.ID, ReadAccess: true}); err != nil {
			t.Fatalf("SetAccessRights: %v", err)
		}
		if err := st.RemoveConnection(wg, sh); err != nil {
			t.Fatalf("RemoveConnection: %v", err)
		}
		if connected, _, err := st.IsConnected(wg, sh); err != nil || connected {
			t.Fatalf("IsConnected after remove: %v %v", err, connected)
		}
		if ars, err := st.GetAccounts(sh); err != nil || len(ars) != 0 {
			t.Fatalf("policies not cascaded: %v %+v", err, ars)
		}
		if len(rs.disconnected) != 1 || rs.disconnected[0] != wg.UUID.String()+"/"+sh.Name {
			t.Fatalf("share manager not notified: %+v", rs.disconnected)
		}
	})
}

func TestStorePolicies(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, rs *recordingShares) {
		wg := addWorkgroup(t, st, "")
		acc := addAccount(t, st, wg, "user", "pw")
		sh := addShare(t, st, "s1")

		// GetAccessRights returns a zero value when no policy exists.
		if ar, err := st.GetAccessRights(sh, acc); err != nil || ar.AccountID != 0 {
			t.Fatalf("GetAccessRights pre-set: %v %+v", err, ar)
		}

		// A policy requires a connection between the workgroup and the share.
		ar := AccessRights{ShareName: sh.Name, AccountID: acc.ID, ReadAccess: true, WriteAccess: true}
		if err := st.SetAccessRights(ar); err == nil {
			t.Fatal("SetAccessRights without a connection: expected error, got nil")
		}
		if err := st.AddConnection(wg, sh, types.GeneratePrivateKey()); err != nil {
			t.Fatalf("AddConnection: %v", err)
		}
		if err := st.SetAccessRights(ar); err != nil {
			t.Fatalf("SetAccessRights: %v", err)
		}
		if len(rs.updated) != 1 {
			t.Fatalf("share manager not notified: %+v", rs.updated)
		}

		// Both the account and the share must exist.
		if err := st.SetAccessRights(AccessRights{ShareName: sh.Name, AccountID: 999999, ReadAccess: true}); err == nil {
			t.Fatal("SetAccessRights: expected error for a missing account")
		}
		if err := st.SetAccessRights(AccessRights{ShareName: "nosuchshare", AccountID: acc.ID, ReadAccess: true}); err == nil {
			t.Fatal("SetAccessRights: expected error for a missing share")
		}

		got, err := st.GetAccessRights(sh, acc)
		if err != nil {
			t.Fatalf("GetAccessRights: %v", err)
		}
		if !got.ReadAccess || !got.WriteAccess || got.DeleteAccess || got.ExecuteAccess {
			t.Fatalf("GetAccessRights: unexpected flags %+v", got)
		}
		if got.ShareName != sh.Name || got.AccountID != acc.ID {
			t.Fatalf("GetAccessRights: got %+v", got)
		}

		// SetAccessRights acts as an upsert.
		ar.WriteAccess = false
		ar.DeleteAccess = true
		if err := st.SetAccessRights(ar); err != nil {
			t.Fatalf("SetAccessRights upsert: %v", err)
		}
		if got, err := st.GetAccessRights(sh, acc); err != nil || got.WriteAccess || !got.DeleteAccess {
			t.Fatalf("GetAccessRights after upsert: %v %+v", err, got)
		}

		// GetAccounts lists the accounts with a policy on the share.
		ars, err := st.GetAccounts(sh)
		if err != nil {
			t.Fatalf("GetAccounts: %v", err)
		}
		if ids := accountIDs(ars); len(ids) != 1 || ids[0] != acc.ID {
			t.Fatalf("GetAccounts: want [%d], got %+v", acc.ID, ars)
		}
		if ars, err := st.GetAccounts(Share{}); err != nil || len(ars) != 0 {
			t.Fatalf("GetAccounts for an empty share: %v %+v", err, ars)
		}

		// GetShares lists the shares the account has any access to.
		if shares, err := st.GetShares(acc); err != nil || len(shares) != 1 || shares[0].Name != sh.Name {
			t.Fatalf("GetShares: %v %v", err, shareNames(shares))
		}

		// A policy without any access rights doesn't grant the share.
		other := addShare(t, st, "s2")
		if err := st.AddConnection(wg, other, nil); err != nil {
			t.Fatalf("AddConnection s2: %v", err)
		}
		if err := st.SetAccessRights(AccessRights{ShareName: other.Name, AccountID: acc.ID}); err != nil {
			t.Fatalf("SetAccessRights s2: %v", err)
		}
		if shares, err := st.GetShares(acc); err != nil || len(shares) != 1 || shares[0].Name != sh.Name {
			t.Fatalf("GetShares with an empty policy: %v %v", err, shareNames(shares))
		}

		// An account without policies has no shares.
		stranger := addAccount(t, st, wg, "stranger", "pw")
		if shares, err := st.GetShares(stranger); err != nil || len(shares) != 0 {
			t.Fatalf("GetShares for an account without policies: %v %v", err, shareNames(shares))
		}

		// RemoveAccessRights removes a single policy.
		if err := st.RemoveAccessRights(sh, acc); err != nil {
			t.Fatalf("RemoveAccessRights: %v", err)
		}
		if got, err := st.GetAccessRights(sh, acc); err != nil || got.AccountID != 0 {
			t.Fatalf("GetAccessRights after remove: %v %+v", err, got)
		}
		if ars, err := st.GetAccounts(other); err != nil || len(ars) != 1 {
			t.Fatalf("RemoveAccessRights removed an unrelated policy: %v %+v", err, ars)
		}

		// ClearAccessRights removes all policies of the account and notifies
		// the share manager.
		rs.accessGone = nil
		if err := st.SetAccessRights(ar); err != nil {
			t.Fatalf("SetAccessRights before clear: %v", err)
		}
		if err := st.ClearAccessRights(acc); err != nil {
			t.Fatalf("ClearAccessRights: %v", err)
		}
		if ars, err := st.GetAccounts(sh); err != nil || len(ars) != 0 {
			t.Fatalf("GetAccounts after clear: %v %+v", err, ars)
		}
		if ars, err := st.GetAccounts(other); err != nil || len(ars) != 0 {
			t.Fatalf("GetAccounts after clear: %v %+v", err, ars)
		}
		if len(rs.accessGone) != 1 {
			t.Fatalf("RemoveAccess not called on ClearAccessRights: %+v", rs.accessGone)
		}
	})
}

// TestStoreAccountCascade verifies that removing an account also removes its
// access policies.
func TestStoreAccountCascade(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, _ *recordingShares) {
		wg := addWorkgroup(t, st, "")
		acc := addAccount(t, st, wg, "user", "pw")
		kept := addAccount(t, st, wg, "keeper", "pw")
		sh := addShare(t, st, "s1")
		if err := st.AddConnection(wg, sh, nil); err != nil {
			t.Fatalf("AddConnection: %v", err)
		}
		for _, a := range []Account{acc, kept} {
			if err := st.SetAccessRights(AccessRights{ShareName: sh.Name, AccountID: a.ID, ReadAccess: true}); err != nil {
				t.Fatalf("SetAccessRights: %v", err)
			}
		}

		if err := st.RemoveAccount("user", wg.UUID.String()); err != nil {
			t.Fatalf("RemoveAccount: %v", err)
		}
		if ar, err := st.GetAccessRights(sh, acc); err != nil || ar.AccountID != 0 {
			t.Fatalf("policy not cascaded: %v %+v", err, ar)
		}
		if ids, err := st.GetAccounts(sh); err != nil || len(ids) != 1 || ids[0].AccountID != kept.ID {
			t.Fatalf("GetAccounts after RemoveAccount: %v %+v", err, ids)
		}

		// RemoveAccounts drops the policies of the remaining accounts too.
		if err := st.RemoveAccounts(wg.UUID.String()); err != nil {
			t.Fatalf("RemoveAccounts: %v", err)
		}
		if ars, err := st.GetAccounts(sh); err != nil || len(ars) != 0 {
			t.Fatalf("policies not cascaded on RemoveAccounts: %v %+v", err, ars)
		}
	})
}

// TestStoreWorkgroupCascade verifies that removing a workgroup removes its
// accounts, connections, and policies, but keeps the shares themselves.
func TestStoreWorkgroupCascade(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, rs *recordingShares) {
		wg := addWorkgroup(t, st, "")
		acc := addAccount(t, st, wg, "user", "pw")
		sh := addShare(t, st, "s1")
		if err := st.AddConnection(wg, sh, nil); err != nil {
			t.Fatalf("AddConnection: %v", err)
		}
		if err := st.SetAccessRights(AccessRights{ShareName: sh.Name, AccountID: acc.ID, ReadAccess: true}); err != nil {
			t.Fatalf("SetAccessRights: %v", err)
		}

		// A second workgroup connected to the same share must survive.
		other := addWorkgroup(t, st, "other")
		if err := st.AddConnection(other, sh, nil); err != nil {
			t.Fatalf("AddConnection other: %v", err)
		}

		rs.disconnected = nil
		rs.accessGone = nil
		if err := st.RemoveWorkgroup(wg); err != nil {
			t.Fatalf("RemoveWorkgroup: %v", err)
		}
		if accs, err := st.FindAccounts(wg.UUID.String()); err != nil || len(accs) != 0 {
			t.Fatalf("accounts not cascaded: %v %+v", err, accs)
		}
		if connected, _, err := st.IsConnected(wg, sh); err != nil || connected {
			t.Fatalf("connection not cascaded: %v %v", err, connected)
		}
		if ars, err := st.GetAccounts(sh); err != nil || len(ars) != 0 {
			t.Fatalf("policies not cascaded: %v %+v", err, ars)
		}

		// The share itself and the other workgroup's connection stay.
		if shares, err := st.GetAllShares(); err != nil || len(shares) != 1 {
			t.Fatalf("share must not be removed: %v %v", err, shareNames(shares))
		}
		if connected, _, err := st.IsConnected(other, sh); err != nil || !connected {
			t.Fatalf("unrelated connection removed: %v %v", err, connected)
		}

		// The share manager is told to drop the connection and the accounts.
		if len(rs.disconnected) != 1 || rs.disconnected[0] != wg.UUID.String()+"/"+sh.Name {
			t.Fatalf("RemoveConnection not called: %+v", rs.disconnected)
		}
		if len(rs.accessGone) != 1 || rs.accessGone[0] != wg.UUID.String()+"/user" {
			t.Fatalf("RemoveAccess not called: %+v", rs.accessGone)
		}
	})
}

func TestStoreBans(t *testing.T) {
	forEachStore(t, func(t *testing.T, st Store, _ *recordingShares) {
		if banned, _, err := st.IsBanned("1.2.3.4"); err != nil || banned {
			t.Fatalf("IsBanned: %v %v", err, banned)
		}

		if err := st.BanHost("1.2.3.4", "abuse"); err != nil {
			t.Fatalf("BanHost: %v", err)
		}
		// A repeated ban keeps the original reason.
		if err := st.BanHost("1.2.3.4", "other"); err != nil {
			t.Fatalf("BanHost repeated: %v", err)
		}
		banned, reason, err := st.IsBanned("1.2.3.4")
		if err != nil || !banned || reason != "abuse" {
			t.Fatalf("IsBanned: %v %v %q", err, banned, reason)
		}

		// A ban without a reason is allowed.
		if err := st.BanHost("5.6.7.8", ""); err != nil {
			t.Fatalf("BanHost without reason: %v", err)
		}
		if banned, reason, err := st.IsBanned("5.6.7.8"); err != nil || !banned || reason != "" {
			t.Fatalf("IsBanned without reason: %v %v %q", err, banned, reason)
		}

		// UnbanHost removes a single host.
		if err := st.UnbanHost("1.2.3.4"); err != nil {
			t.Fatalf("UnbanHost: %v", err)
		}
		if banned, _, err := st.IsBanned("1.2.3.4"); err != nil || banned {
			t.Fatalf("IsBanned after unban: %v %v", err, banned)
		}
		if banned, _, err := st.IsBanned("5.6.7.8"); err != nil || !banned {
			t.Fatalf("UnbanHost removed an unrelated host: %v %v", err, banned)
		}
		// Unbanning a host that isn't banned is not an error.
		if err := st.UnbanHost("1.2.3.4"); err != nil {
			t.Fatalf("UnbanHost missing: %v", err)
		}

		// ClearBans empties the list.
		if err := st.BanHost("1.1.1.1", "spam"); err != nil {
			t.Fatalf("BanHost: %v", err)
		}
		if err := st.ClearBans(); err != nil {
			t.Fatalf("ClearBans: %v", err)
		}
		for _, host := range []string{"1.1.1.1", "5.6.7.8"} {
			if banned, _, err := st.IsBanned(host); err != nil || banned {
				t.Fatalf("IsBanned(%s) after ClearBans: %v %v", host, err, banned)
			}
		}
	})
}
