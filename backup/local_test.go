package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// noShares is a share manager that does nothing, for a store that is not
// serving anything.
type noShares struct{}

func (noShares) RegisterShare(stores.Share) error                           { return nil }
func (noShares) UpdateShare(stores.Share) error                             { return nil }
func (noShares) RemoveShare(stores.Share) error                             { return nil }
func (noShares) UpdateAccessRights(stores.Share, stores.AccessRights) error { return nil }
func (noShares) RemoveAccess(stores.Account)                                {}
func (noShares) AddConnection(stores.Workgroup, stores.Share, types.PrivateKey) error {
	return nil
}
func (noShares) RemoveConnection(stores.Workgroup, stores.Share) error { return nil }
func (noShares) UnpinSlabs(stores.Workgroup, stores.Share, []types.Hash256) {}

// connectedStore is a store with one connection to write catalogs of.
func connectedStore(t *testing.T, ctx context.Context) (*stores.Database, stores.Workgroup, types.PrivateKey) {
	t.Helper()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)
	db.WithShares(noShares{})

	if err := db.RegisterShare(stores.Share{Name: "idx", Type: "indexd", ServerName: "srv"}); err != nil {
		t.Fatalf("RegisterShare: %v", err)
	}
	if err := db.AddWorkgroup(stores.Workgroup{UUID: uuid.New(), Name: "acme"}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	wg, err := db.FindWorkgroupByName("acme")
	if err != nil {
		t.Fatalf("FindWorkgroupByName: %v", err)
	}
	// A guest comes first, and alice after: it is alice who owns what the
	// workgroup keeps of itself.
	for _, acc := range []stores.Account{
		{Username: "guest", Workgroup: wg.UUID.String()},
		{Username: "alice", Password: "pw", Workgroup: wg.UUID.String()},
	} {
		if err := db.AddAccount(acc); err != nil {
			t.Fatalf("AddAccount(%s): %v", acc.Username, err)
		}
	}
	share, err := db.GetShare("idx")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	key := make(types.PrivateKey, 64)
	for i := range key {
		key[i] = byte(i)
	}
	if err := db.AddConnection(wg, share, key); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}

	return db, wg, key
}

// TestLocalWritesAndKeeps verifies that each round leaves a whole catalog of
// the connection in its folder, for the owner alone, and that only the newest
// few stay.
func TestLocalWritesAndKeeps(t *testing.T) {
	ctx := context.Background()
	db, wg, key := connectedStore(t, ctx)

	dir := t.TempDir()
	l := &Local{db: db, dir: dir, keep: 2, inline: 1024, status: Status{Path: dir, Keep: 2}}
	for range 3 {
		if err := l.WriteAll(ctx); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}
	}

	folder := filepath.Join(dir, "idx_"+wg.UUID.String())
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the folder holds %d file(s), want the newest 2", len(entries))
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(entry.Name(), ".catalog") || info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", entry.Name(), info.Mode())
		}
	}

	status := l.Status()
	if len(status.Catalogs) != 1 || status.Catalogs[0].Share != "idx" || status.Catalogs[0].Workgroup != wg.UUID || status.Error != "" || status.LastRun.IsZero() {
		t.Errorf("status: got %+v", status)
	}

	// The newest catalog reads as one, and says what it belongs to.
	data, err := os.ReadFile(status.Catalogs[0].Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	r, err := transfer.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	conn := r.Connection()
	if conn == nil || conn.Share.Name != "idx" || !bytes.Equal(conn.AppKey, key) || len(conn.Accounts) != 2 || conn.Accounts[1].Name != "alice" {
		t.Errorf("the catalog belongs to %+v", conn)
	}

	// The server itself has a catalog too, kept the same way, and it says what
	// the connections' catalogs cannot: everything that is nobody's.
	if status.Server == nil || status.Server.Stats != (stores.ServerStats{Shares: 1, Workgroups: 1, Accounts: 2}) {
		t.Fatalf("the catalog of the server: got %+v", status.Server)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "server")); err != nil || len(entries) != 2 {
		t.Errorf("the server folder holds %d file(s), want the newest 2: %v", len(entries), err)
	}
	data, err = os.ReadFile(status.Server.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if r, err := transfer.NewReader(bytes.NewReader(data)); err != nil || r.Server() == nil || len(r.Server().Shares) != 1 {
		t.Errorf("the catalog of the server does not read as one: %v", err)
	}

	// The folder lists what it holds, newest first, each catalog by what it is.
	stored, err := ListFolder(dir)
	if err != nil {
		t.Fatalf("ListFolder: %v", err)
	}
	if len(stored) != 4 {
		t.Fatalf("the folder lists %d catalog(s), want 2 of the connection and 2 of the server: %+v", len(stored), stored)
	}
	var connections, servers int
	for i, s := range stored {
		if i > 0 && s.WrittenAt.After(stored[i-1].WrittenAt) {
			t.Errorf("the catalogs are not newest first: %+v", stored)
		}
		switch s.Kind {
		case "connection":
			connections++
			if s.Share != "idx" || s.Workgroup != wg.UUID || s.Size == 0 {
				t.Errorf("a catalog of the connection: got %+v", s)
			}
		case "server":
			servers++
		}
	}
	if connections != 2 || servers != 2 {
		t.Errorf("want 2 catalogs of the connection and 2 of the server, got %d and %d", connections, servers)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.catalog"), []byte("not one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := ListFolder(dir); err != nil || len(again) != 4 {
		t.Errorf("a file that is not a catalog was listed: %d, %v", len(again), err)
	}

	// A folder that cannot be written is reported, not passed over in silence.
	bad := &Local{db: db, dir: filepath.Join(status.Catalogs[0].Path, "under-a-file"), keep: 2, inline: 1024}
	if err := bad.WriteAll(ctx); err == nil {
		t.Error("writing under a file was reported as done")
	}
	if bad.Status().Error == "" {
		t.Error("the failure was not kept in the status")
	}
}
