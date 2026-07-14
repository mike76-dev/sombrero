package stores

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"go.sia.tech/core/types"
	"lukechampine.com/frand"
)

func makeKey(t *testing.T) types.PrivateKey {
	t.Helper()
	key := make(types.PrivateKey, 64)
	frand.Read(key)
	return key
}

func TestWorkgroups(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	u := uuid.New()

	// Add without name.
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}

	// FindWorkgroup by UUID.
	wg, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if wg.UUID != u {
		t.Fatalf("FindWorkgroup: want UUID %v, got %v", u, wg.UUID)
	}
	if wg.ID == 0 {
		t.Fatal("FindWorkgroup: expected non-zero ID")
	}
	if wg.Name != "" {
		t.Fatalf("FindWorkgroup: expected empty name, got %q", wg.Name)
	}

	// GetWorkgroupByID.
	byID, err := db.GetWorkgroupByID(wg.ID)
	if err != nil {
		t.Fatalf("GetWorkgroupByID: %v", err)
	}
	if byID.UUID != u {
		t.Fatalf("GetWorkgroupByID: want UUID %v, got %v", u, byID.UUID)
	}

	// FindWorkgroup for unknown UUID returns zero value.
	missing, err := db.FindWorkgroup(uuid.New())
	if err != nil {
		t.Fatalf("FindWorkgroup missing: %v", err)
	}
	if missing.ID != 0 {
		t.Fatal("FindWorkgroup missing: expected zero value")
	}

	// RemoveWorkgroup.
	if err := db.RemoveWorkgroup(wg); err != nil {
		t.Fatalf("RemoveWorkgroup: %v", err)
	}
	gone, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup after remove: %v", err)
	}
	if gone.ID != 0 {
		t.Fatal("FindWorkgroup after remove: expected zero value")
	}

	// Add with name.
	u2 := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u2, Name: "acme"}); err != nil {
		t.Fatalf("AddWorkgroup with name: %v", err)
	}

	// FindWorkgroupByName.
	wg2, err := db.FindWorkgroupByName("acme")
	if err != nil {
		t.Fatalf("FindWorkgroupByName: %v", err)
	}
	if wg2.UUID != u2 {
		t.Fatalf("FindWorkgroupByName: want UUID %v, got %v", u2, wg2.UUID)
	}
	if wg2.Name != "acme" {
		t.Fatalf("FindWorkgroupByName: want name %q, got %q", "acme", wg2.Name)
	}

	// FindWorkgroup by UUID also returns the name.
	wg2ByUUID, err := db.FindWorkgroup(u2)
	if err != nil {
		t.Fatalf("FindWorkgroup (named): %v", err)
	}
	if wg2ByUUID.Name != "acme" {
		t.Fatalf("FindWorkgroup (named): want name %q, got %q", "acme", wg2ByUUID.Name)
	}

	// FindWorkgroupByName for unknown name returns zero value.
	noWG, err := db.FindWorkgroupByName("unknown")
	if err != nil {
		t.Fatalf("FindWorkgroupByName missing: %v", err)
	}
	if noWG.ID != 0 {
		t.Fatal("FindWorkgroupByName missing: expected zero value")
	}
}

func TestUpdateWorkgroup(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	wg, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}

	// New workgroup has no public dirs and case_sensitive defaults to false.
	if len(wg.PublicDirs) != 0 {
		t.Fatalf("PublicDirs: want empty, got %v", wg.PublicDirs)
	}
	if wg.CaseSensitive {
		t.Fatal("CaseSensitive: want false initially")
	}

	// UpdateWorkgroup persists new dirs and case sensitivity.
	wg.PublicDirs = []string{"shared", "public"}
	wg.CaseSensitive = true
	if err := db.UpdateWorkgroup(wg); err != nil {
		t.Fatalf("UpdateWorkgroup: %v", err)
	}

	// FindWorkgroup reflects the update.
	updated, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup after update: %v", err)
	}
	if len(updated.PublicDirs) != 2 || updated.PublicDirs[0] != "shared" || updated.PublicDirs[1] != "public" {
		t.Fatalf("PublicDirs: want [shared public], got %v", updated.PublicDirs)
	}
	if !updated.CaseSensitive {
		t.Fatal("CaseSensitive: want true after update")
	}

	// GetWorkgroupByID also reflects the update.
	byID, err := db.GetWorkgroupByID(wg.ID)
	if err != nil {
		t.Fatalf("GetWorkgroupByID after update: %v", err)
	}
	if len(byID.PublicDirs) != 2 || byID.PublicDirs[0] != "shared" {
		t.Fatalf("GetWorkgroupByID PublicDirs: want [shared public], got %v", byID.PublicDirs)
	}
	if !byID.CaseSensitive {
		t.Fatal("GetWorkgroupByID CaseSensitive: want true")
	}

	// Clearing dirs sets them back to empty.
	wg.PublicDirs = nil
	wg.CaseSensitive = false
	if err := db.UpdateWorkgroup(wg); err != nil {
		t.Fatalf("UpdateWorkgroup clear: %v", err)
	}
	cleared, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup after clear: %v", err)
	}
	if len(cleared.PublicDirs) != 0 {
		t.Fatalf("PublicDirs after clear: want empty, got %v", cleared.PublicDirs)
	}
	if cleared.CaseSensitive {
		t.Fatal("CaseSensitive after clear: want false")
	}

	// UpdateWorkgroup on a missing ID returns an error.
	ghost := Workgroup{ID: 999999, PublicDirs: []string{"x"}}
	if err := db.UpdateWorkgroup(ghost); err == nil {
		t.Fatal("UpdateWorkgroup missing ID: expected error, got nil")
	}

	// AddWorkgroup with dirs pre-set round-trips through FindWorkgroup.
	u2 := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u2, PublicDirs: []string{"reports"}, CaseSensitive: true}); err != nil {
		t.Fatalf("AddWorkgroup with dirs: %v", err)
	}
	wg2, err := db.FindWorkgroup(u2)
	if err != nil {
		t.Fatalf("FindWorkgroup with dirs: %v", err)
	}
	if len(wg2.PublicDirs) != 1 || wg2.PublicDirs[0] != "reports" {
		t.Fatalf("FindWorkgroup PublicDirs: want [reports], got %v", wg2.PublicDirs)
	}
	if !wg2.CaseSensitive {
		t.Fatal("FindWorkgroup CaseSensitive: want true")
	}

	// FindWorkgroupByName returns public dirs as well.
	u3 := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u3, Name: "labeled", PublicDirs: []string{"inbox"}}); err != nil {
		t.Fatalf("AddWorkgroup with name and dirs: %v", err)
	}
	wg3, err := db.FindWorkgroupByName("labeled")
	if err != nil {
		t.Fatalf("FindWorkgroupByName: %v", err)
	}
	if len(wg3.PublicDirs) != 1 || wg3.PublicDirs[0] != "inbox" {
		t.Fatalf("FindWorkgroupByName PublicDirs: want [inbox], got %v", wg3.PublicDirs)
	}
}

func TestAccounts(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}

	acc := Account{Username: "alice", Password: "s3cr3t", Workgroup: u.String()}
	if err := db.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// HasAccount.
	has, err := db.HasAccount(acc.Username, acc.Workgroup)
	if err != nil {
		t.Fatalf("HasAccount: %v", err)
	}
	if !has {
		t.Fatal("HasAccount: expected true")
	}

	// HasAccount for unknown user.
	has, err = db.HasAccount("nobody", acc.Workgroup)
	if err != nil {
		t.Fatalf("HasAccount missing: %v", err)
	}
	if has {
		t.Fatal("HasAccount missing: expected false")
	}

	// FindAccount.
	found, err := db.FindAccount(acc.Username, acc.Workgroup)
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}
	if found.Username != acc.Username {
		t.Fatalf("FindAccount: want username %q, got %q", acc.Username, found.Username)
	}
	if found.Workgroup != acc.Workgroup {
		t.Fatalf("FindAccount: want workgroup %q, got %q", acc.Workgroup, found.Workgroup)
	}
	if len(found.NTHash) == 0 {
		t.Fatal("FindAccount: expected non-empty NTHash")
	}

	// FindAccount for unknown user returns zero value.
	missing, err := db.FindAccount("nobody", acc.Workgroup)
	if err != nil {
		t.Fatalf("FindAccount missing: %v", err)
	}
	if missing.ID != 0 {
		t.Fatal("FindAccount missing: expected zero value")
	}

	// GetAccountByID.
	byID, err := db.GetAccountByID(found.ID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if byID.Username != acc.Username {
		t.Fatalf("GetAccountByID: want %q, got %q", acc.Username, byID.Username)
	}

	// FindAccounts lists all accounts for the workgroup.
	if err := db.AddAccount(Account{Username: "bob", Password: "pw", Workgroup: u.String()}); err != nil {
		t.Fatalf("AddAccount bob: %v", err)
	}
	accs, err := db.FindAccounts(u.String())
	if err != nil {
		t.Fatalf("FindAccounts: %v", err)
	}
	if len(accs) != 2 {
		t.Fatalf("FindAccounts: want 2, got %d", len(accs))
	}

	// RemoveAccount.
	if err := db.RemoveAccount(acc.Username, acc.Workgroup); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	has, err = db.HasAccount(acc.Username, acc.Workgroup)
	if err != nil {
		t.Fatalf("HasAccount after remove: %v", err)
	}
	if has {
		t.Fatal("HasAccount after remove: expected false")
	}

	// RemoveAccounts removes all remaining accounts.
	if err := db.RemoveAccounts(u.String()); err != nil {
		t.Fatalf("RemoveAccounts: %v", err)
	}
	accs, err = db.FindAccounts(u.String())
	if err != nil {
		t.Fatalf("FindAccounts after RemoveAccounts: %v", err)
	}
	if len(accs) != 0 {
		t.Fatalf("FindAccounts after RemoveAccounts: want 0, got %d", len(accs))
	}
}

func TestShares(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

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

	// RegisterShare.
	if err := db.RegisterShare(share); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}

	// GetShare.
	got, err := db.GetShare(share.Name)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if got.Name != share.Name {
		t.Fatalf("GetShare: want name %q, got %q", share.Name, got.Name)
	}
	if got.Type != share.Type {
		t.Fatalf("GetShare: want type %q, got %q", share.Type, got.Type)
	}
	if got.DataShards != share.DataShards {
		t.Fatalf("GetShare: want DataShards %d, got %d", share.DataShards, got.DataShards)
	}
	if got.ParityShards != share.ParityShards {
		t.Fatalf("GetShare: want ParityShards %d, got %d", share.ParityShards, got.ParityShards)
	}

	// GetShare for unknown name returns zero value.
	empty, err := db.GetShare("nosuchshare")
	if err != nil {
		t.Fatalf("GetShare missing: %v", err)
	}
	if empty.Name != "" {
		t.Fatal("GetShare missing: expected empty share")
	}

	// UnregisterShare.
	if err := db.UnregisterShare(share.Name); err != nil {
		t.Fatalf("UnregisterShare: %v", err)
	}
	gone, err := db.GetShare(share.Name)
	if err != nil {
		t.Fatalf("GetShare after unregister: %v", err)
	}
	if gone.Name != "" {
		t.Fatal("GetShare after unregister: expected empty share")
	}
}

func TestGetShares(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	wg, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if err := db.AddAccount(Account{Username: "user1", Password: "pw", Workgroup: u.String()}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, err := db.FindAccount("user1", u.String())
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}

	for _, name := range []string{"shareA", "shareB"} {
		if err := db.RegisterShare(Share{Name: name, Type: "renterd", ServerName: "srv"}); err != nil {
			t.Fatalf("RegisterShare %s: %v", name, err)
		}
	}

	shareA, err := db.GetShare("shareA")
	if err != nil {
		t.Fatalf("GetShare shareA: %v", err)
	}
	if err := db.AddConnection(wg, shareA, makeKey(t)); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}

	// Grant read access to shareA only.
	if err := db.SetAccessRights(AccessRights{
		ShareName:  "shareA",
		AccountID:  acc.ID,
		ReadAccess: true,
	}); err != nil {
		t.Fatalf("SetAccessRights: %v", err)
	}

	shares, err := db.GetShares(acc)
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(shares) != 1 || shares[0].Name != "shareA" {
		t.Fatalf("GetShares: want [shareA], got %v", shares)
	}
}

func TestConnections(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	wg, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}

	if err := db.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}
	share, err := db.GetShare("s1")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}

	// Not connected initially.
	connected, _, err := db.IsConnected(wg, share)
	if err != nil {
		t.Fatalf("IsConnected initial: %v", err)
	}
	if connected {
		t.Fatal("IsConnected initial: expected false")
	}

	// AddConnection.
	key := makeKey(t)
	if err := db.AddConnection(wg, share, key); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}

	// IsConnected returns true and the correct key.
	connected, gotKey, err := db.IsConnected(wg, share)
	if err != nil {
		t.Fatalf("IsConnected: %v", err)
	}
	if !connected {
		t.Fatal("IsConnected: expected true")
	}
	if !bytes.Equal([]byte(gotKey), []byte(key)) {
		t.Fatal("IsConnected: app key mismatch")
	}

	// AddConnection is idempotent (ON CONFLICT DO NOTHING); key must not change.
	if err := db.AddConnection(wg, share, makeKey(t)); err != nil {
		t.Fatalf("AddConnection idempotent: %v", err)
	}
	_, gotKey2, err := db.IsConnected(wg, share)
	if err != nil {
		t.Fatalf("IsConnected after idempotent add: %v", err)
	}
	if !bytes.Equal([]byte(gotKey2), []byte(key)) {
		t.Fatal("AddConnection idempotent: key should not have changed")
	}

	// SetAppKey replaces the key.
	newKey := makeKey(t)
	if err := db.SetAppKey(wg, share, newKey); err != nil {
		t.Fatalf("SetAppKey: %v", err)
	}
	_, gotKey, err = db.IsConnected(wg, share)
	if err != nil {
		t.Fatalf("IsConnected after SetAppKey: %v", err)
	}
	if !bytes.Equal([]byte(gotKey), []byte(newKey)) {
		t.Fatal("IsConnected after SetAppKey: key mismatch")
	}

	// A different workgroup is not connected to the same share.
	u2 := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u2}); err != nil {
		t.Fatalf("AddWorkgroup u2: %v", err)
	}
	wg2, err := db.FindWorkgroup(u2)
	if err != nil {
		t.Fatalf("FindWorkgroup u2: %v", err)
	}
	connected, _, err = db.IsConnected(wg2, share)
	if err != nil {
		t.Fatalf("IsConnected wg2: %v", err)
	}
	if connected {
		t.Fatal("IsConnected wg2: expected false")
	}

	// RemoveConnection.
	if err := db.RemoveConnection(wg, share); err != nil {
		t.Fatalf("RemoveConnection: %v", err)
	}
	connected, _, err = db.IsConnected(wg, share)
	if err != nil {
		t.Fatalf("IsConnected after remove: %v", err)
	}
	if connected {
		t.Fatal("IsConnected after remove: expected false")
	}
}

func TestPolicies(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	wg, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if err := db.AddAccount(Account{Username: "puser", Password: "pw", Workgroup: u.String()}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	acc, err := db.FindAccount("puser", u.String())
	if err != nil {
		t.Fatalf("FindAccount: %v", err)
	}
	if err := db.RegisterShare(Share{Name: "pshare", Type: "renterd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}
	share, err := db.GetShare("pshare")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}

	// GetAccessRights returns zero value when no policy exists.
	ar, err := db.GetAccessRights(share, acc)
	if err != nil {
		t.Fatalf("GetAccessRights pre-set: %v", err)
	}
	if ar.AccountID != 0 {
		t.Fatal("GetAccessRights pre-set: expected zero value")
	}

	// SetAccessRights without a connection must be rejected.
	if err := db.SetAccessRights(AccessRights{ShareName: share.Name, AccountID: acc.ID, ReadAccess: true}); err == nil {
		t.Fatal("SetAccessRights without connection: expected error, got nil")
	}

	// Establish connection so policy operations are now permitted.
	if err := db.AddConnection(wg, share, makeKey(t)); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}

	// SetAccessRights.
	want := AccessRights{
		ShareName:   share.Name,
		AccountID:   acc.ID,
		ReadAccess:  true,
		WriteAccess: true,
	}
	if err := db.SetAccessRights(want); err != nil {
		t.Fatalf("SetAccessRights: %v", err)
	}
	ar, err = db.GetAccessRights(share, acc)
	if err != nil {
		t.Fatalf("GetAccessRights: %v", err)
	}
	if !ar.ReadAccess || !ar.WriteAccess || ar.DeleteAccess || ar.ExecuteAccess {
		t.Fatalf("GetAccessRights: unexpected flags %+v", ar)
	}

	// GetAccounts returns the account we just gave access.
	ars, err := db.GetAccounts(share)
	if err != nil {
		t.Fatalf("GetAccounts: %v", err)
	}
	if len(ars) != 1 || ars[0].AccountID != acc.ID {
		t.Fatalf("GetAccounts: got %v", ars)
	}

	// SetAccessRights acts as an upsert.
	want.DeleteAccess = true
	if err := db.SetAccessRights(want); err != nil {
		t.Fatalf("SetAccessRights upsert: %v", err)
	}
	ar, err = db.GetAccessRights(share, acc)
	if err != nil {
		t.Fatalf("GetAccessRights after upsert: %v", err)
	}
	if !ar.DeleteAccess {
		t.Fatal("GetAccessRights after upsert: expected DeleteAccess true")
	}

	// RemoveAccessRights.
	if err := db.RemoveAccessRights(share, acc); err != nil {
		t.Fatalf("RemoveAccessRights: %v", err)
	}
	ar, err = db.GetAccessRights(share, acc)
	if err != nil {
		t.Fatalf("GetAccessRights after remove: %v", err)
	}
	if ar.AccountID != 0 {
		t.Fatal("GetAccessRights after remove: expected zero value")
	}

	// ClearAccessRights removes all policies for an account.
	if err := db.SetAccessRights(want); err != nil {
		t.Fatalf("SetAccessRights before clear: %v", err)
	}
	if err := db.ClearAccessRights(acc); err != nil {
		t.Fatalf("ClearAccessRights: %v", err)
	}
	ars, err = db.GetAccounts(share)
	if err != nil {
		t.Fatalf("GetAccounts after clear: %v", err)
	}
	if len(ars) != 0 {
		t.Fatalf("GetAccounts after clear: want 0, got %d", len(ars))
	}

	// RemoveConnection cascades and clears all policies for the workgroup on that share.
	if err := db.SetAccessRights(want); err != nil {
		t.Fatalf("SetAccessRights before disconnect: %v", err)
	}
	if err := db.RemoveConnection(wg, share); err != nil {
		t.Fatalf("RemoveConnection: %v", err)
	}
	ar, err = db.GetAccessRights(share, acc)
	if err != nil {
		t.Fatalf("GetAccessRights after disconnect: %v", err)
	}
	if ar.AccountID != 0 {
		t.Fatal("GetAccessRights after disconnect: expected policy to be cascade-deleted")
	}

	// SetAccessRights without a connection must again be rejected.
	if err := db.SetAccessRights(want); err == nil {
		t.Fatal("SetAccessRights after disconnect: expected error, got nil")
	}
}

func TestFlagsFromAccessRights(t *testing.T) {
	tests := []struct {
		ar    AccessRights
		flags uint32
	}{
		{AccessRights{}, 0},
		{AccessRights{ReadAccess: true}, 0x80120089},
		{AccessRights{WriteAccess: true}, 0x400c0116},
		{AccessRights{DeleteAccess: true}, 0x00010040},
		{AccessRights{ExecuteAccess: true}, 0x20000020},
		{
			AccessRights{ReadAccess: true, WriteAccess: true, DeleteAccess: true, ExecuteAccess: true},
			0x80120089 | 0x400c0116 | 0x00010040 | 0x20000020 | 0x12000000,
		},
	}
	for _, tc := range tests {
		if got := FlagsFromAccessRights(tc.ar); got != tc.flags {
			t.Errorf("FlagsFromAccessRights(%+v) = 0x%08x, want 0x%08x", tc.ar, got, tc.flags)
		}
	}
}

// TestRemoveNotifications verifies that removing accounts and workgroups
// notifies the share manager, so that no stale security entries remain
// on the SMB server.
func TestRemoveNotifications(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	rs := &recordingShares{}
	db.WithShares(rs)

	u := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	wg, err := db.FindWorkgroup(u)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if err := db.RegisterShare(Share{Name: "s1", Type: "renterd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}
	share, err := db.GetShare("s1")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if err := db.AddConnection(wg, share, nil); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}
	for _, name := range []string{"user1", "user2", "user3"} {
		if err := db.AddAccount(Account{Username: name, Password: "pw", Workgroup: u.String()}); err != nil {
			t.Fatalf("AddAccount: %v", err)
		}
	}

	// RemoveAccount notifies RemoveAccess.
	if err := db.RemoveAccount("user1", u.String()); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if len(rs.accessGone) != 1 || rs.accessGone[0] != u.String()+"/user1" {
		t.Fatalf("RemoveAccess not called on RemoveAccount: %+v", rs.accessGone)
	}

	// RemoveAccounts notifies RemoveAccess for each remaining account.
	if err := db.RemoveAccounts(u.String()); err != nil {
		t.Fatalf("RemoveAccounts: %v", err)
	}
	if len(rs.accessGone) != 3 {
		t.Fatalf("RemoveAccess not called on RemoveAccounts: %+v", rs.accessGone)
	}

	// RemoveWorkgroup notifies RemoveConnection and RemoveAccess.
	if err := db.AddAccount(Account{Username: "user4", Password: "pw", Workgroup: u.String()}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := db.RemoveWorkgroup(wg); err != nil {
		t.Fatalf("RemoveWorkgroup: %v", err)
	}
	if len(rs.disconnected) != 1 || rs.disconnected[0] != u.String()+"/s1" {
		t.Fatalf("RemoveConnection not called on RemoveWorkgroup: %+v", rs.disconnected)
	}
	if len(rs.accessGone) != 4 || rs.accessGone[3] != u.String()+"/user4" {
		t.Fatalf("RemoveAccess not called on RemoveWorkgroup: %+v", rs.accessGone)
	}
}
