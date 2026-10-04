package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/backup"
	"github.com/mike76-dev/sombrero/stores"
)

// TestBackupStatus tests GET /backup.
func TestBackupStatus(t *testing.T) {
	t.Run("GET says the tiers are off", func(t *testing.T) {
		w := doRequest(newTestAPIWithServer(&mockStore{}, &mockServer{}), http.MethodGet, "/backup", nil)
		checkStatus(t, w, http.StatusOK)
		res := decodeJSON[BackupResponse](t, w)
		if res.Enabled || res.Local != nil || res.Network != nil || res.BufferAge != "never" {
			t.Errorf("want nothing on, got %+v", res)
		}
	})

	t.Run("GET reports what the local tier has written", func(t *testing.T) {
		written := time.Now().UTC().Truncate(time.Second)
		srv := &mockServer{backups: &backup.Status{
			Path: "/var/backups/sombrero", Interval: 15 * time.Minute, Keep: 7, LastRun: written,
			Catalogs: []backup.Catalog{{
				Share: "myshare", Workgroup: testUUID, Path: "/var/backups/sombrero/myshare/x.catalog", Size: 1234, WrittenAt: written,
				Stats: stores.SnapshotStats{Directories: 2, Files: 5, Inlined: 100, Incomplete: 1},
			}},
		}}
		w := doRequest(newTestAPIWithServer(&mockStore{}, srv), http.MethodGet, "/backup", nil)
		checkStatus(t, w, http.StatusOK)
		res := decodeJSON[BackupResponse](t, w)
		if res.Local == nil || res.Local.Path != "/var/backups/sombrero" || res.Local.Keep != 7 || res.Local.LastRun == nil {
			t.Fatalf("the local tier: got %+v", res.Local)
		}
		if len(res.Local.Catalogs) != 1 {
			t.Fatalf("want the one catalog, got %+v", res.Local.Catalogs)
		}
		if c := res.Local.Catalogs[0]; c.Share != "myshare" || c.Workgroup != testUUID.String() || c.Files != 5 || c.Incomplete != 1 || c.Size != 1234 {
			t.Errorf("the catalog: got %+v", c)
		}
	})
}
