package api

import (
	"net/http"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
)

// BackupResponse is the response type of GET /backup: what is configured, and
// what the tiers that are on have done.
type BackupResponse struct {
	Enabled   bool                 `json:"enabled"`
	BufferAge string               `json:"bufferAge"`
	Local     *LocalBackupStatus   `json:"local,omitempty"`
	Network   *NetworkBackupStatus `json:"network,omitempty"`
}

// LocalBackupStatus is the local tier: where it writes, how often, and the newest
// catalog of each connection.
type LocalBackupStatus struct {
	Path     string            `json:"path"`
	Interval string            `json:"interval"`
	Keep     int               `json:"keep"`
	LastRun  *time.Time        `json:"lastRun,omitempty"`
	Error    string            `json:"error,omitempty"`
	Catalogs []CatalogResponse `json:"catalogs"`
}

// NetworkBackupStatus is the network tier as configured.
type NetworkBackupStatus struct {
	Interval string `json:"interval"`
	Keep     int    `json:"keep"`
}

// CatalogResponse is one catalog that was written and what it holds.
type CatalogResponse struct {
	Share       string    `json:"share"`
	Workgroup   string    `json:"workgroup"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	WrittenAt   time.Time `json:"writtenAt"`
	Directories int       `json:"directories"`
	Files       int       `json:"files"`
	Incomplete  int       `json:"incomplete"`
	Inlined     uint64    `json:"inlined"`
}

// backupHandlerGET handles GET /backup.
func (api *API) backupHandlerGET(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	res := BackupResponse{
		Enabled:   api.cfg.Backup.Enabled,
		BufferAge: stores.BufferAge(api.cfg.BufferAge()).String(),
	}
	if network := api.cfg.Backup.Network(); network > 0 {
		res.Network = &NetworkBackupStatus{Interval: network.String(), Keep: api.cfg.Backup.KeepCount()}
	}

	if api.server != nil {
		if status := api.server.BackupStatus(); status != nil {
			local := &LocalBackupStatus{
				Path:     status.Path,
				Interval: status.Interval.String(),
				Keep:     status.Keep,
				Error:    status.Error,
				Catalogs: make([]CatalogResponse, 0, len(status.Catalogs)),
			}
			if !status.LastRun.IsZero() {
				last := status.LastRun
				local.LastRun = &last
			}
			for _, c := range status.Catalogs {
				local.Catalogs = append(local.Catalogs, CatalogResponse{
					Share: c.Share, Workgroup: c.Workgroup.String(), Path: c.Path, Size: c.Size, WrittenAt: c.WrittenAt,
					Directories: c.Stats.Directories, Files: c.Stats.Files, Incomplete: c.Stats.Incomplete, Inlined: c.Stats.Inlined,
				})
			}
			res.Local = local
		}
	}

	writeJSON(w, res)
}
