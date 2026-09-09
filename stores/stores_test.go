package stores

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The behavior the PostgreSQL store shares with the JSON store is tested by
// the suite in store_suite_test.go. Only what is specific to the Normal mode
// is tested here.

// TestDatabaseShareTypes verifies that the Normal mode accepts share types
// other than renterd, which the Lite mode rejects with ErrLiteMode.
func TestDatabaseShareTypes(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	if err := db.RegisterShare(Share{Name: "idx", Type: "indexd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}
	sh, err := db.GetShare("idx")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if sh.Type != "indexd" {
		t.Fatalf("GetShare: want type %q, got %q", "indexd", sh.Type)
	}
}

// TestDatabaseHasConnections verifies what the API asks before it lets the
// address of an indexd share be changed: whether anything is connected to it.
func TestDatabaseHasConnections(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	db.WithShares(&recordingShares{})
	sh := addShare(t, db, "idx")
	other := addShare(t, db, "untouched")
	wg := addWorkgroup(t, db, "acme")

	for _, name := range []string{sh.Name, other.Name} {
		if connected, err := db.HasConnections(name); err != nil || connected {
			t.Fatalf("HasConnections %s before connecting: %v %v", name, connected, err)
		}
	}

	if err := db.AddConnection(wg, sh, nil); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}
	if connected, err := db.HasConnections(sh.Name); err != nil || !connected {
		t.Fatalf("HasConnections after connecting: %v %v", connected, err)
	}

	// One share's connection says nothing about another's.
	if connected, err := db.HasConnections(other.Name); err != nil || connected {
		t.Fatalf("HasConnections of another share: %v %v", connected, err)
	}

	if err := db.RemoveConnection(wg, sh); err != nil {
		t.Fatalf("RemoveConnection: %v", err)
	}
	if connected, err := db.HasConnections(sh.Name); err != nil || connected {
		t.Fatalf("HasConnections after disconnecting: %v %v", connected, err)
	}
}

// TestDatabaseWorkgroupIDs verifies that the workgroup and account IDs are
// assigned by the database and are never reused.
func TestDatabaseWorkgroupIDs(t *testing.T) {
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
	if err := db.RemoveWorkgroup(wg); err != nil {
		t.Fatalf("RemoveWorkgroup: %v", err)
	}

	u2 := uuid.New()
	if err := db.AddWorkgroup(Workgroup{UUID: u2}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	wg2, err := db.FindWorkgroup(u2)
	if err != nil {
		t.Fatalf("FindWorkgroup: %v", err)
	}
	if wg2.ID <= wg.ID {
		t.Fatalf("workgroup ID reused: %d after %d", wg2.ID, wg.ID)
	}
}

// TestDatabaseOutlivesSetupContext verifies that the store keeps working once
// the context it was opened with is cancelled. That context is the process'
// signal context, and a shutdown is exactly when the graceful stop still has
// work to record: an upload cut short has to be requeued, a slab of a deleted
// file has to be unpinned. Only Close ends the store.
func TestDatabaseOutlivesSetupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := NewTestStore(t, ctx)
	defer db.Close()

	cancel()

	if err := db.RegisterShare(Share{Name: "idx", Type: "indexd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare after cancelling the setup context: %v", err)
	}

	db.Close()

	if err := db.RegisterShare(Share{Name: "idx2", Type: "indexd", ServerName: "srv"}); err == nil {
		t.Fatal("RegisterShare succeeded after Close")
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
