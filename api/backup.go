package api

import (
	"log"
	"net/http"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/backup"
	"github.com/mike76-dev/sombrero/stores"
)

// BackupResponse is the response type of GET /backup: what is configured, and
// what the tiers that are on have done. A tier that is off is left out.
type BackupResponse struct {
	Enabled   bool        `json:"enabled"`
	BufferAge string      `json:"bufferAge"`
	Local     *TierStatus `json:"local,omitempty"`
	Network   *TierStatus `json:"network,omitempty"`
}

// TierStatus is one tier: where it writes, how often, the newest catalog of each
// connection, and the catalog of the server where the tier writes one.
type TierStatus struct {
	Path     string                 `json:"path"`
	Interval string                 `json:"interval"`
	Keep     int                    `json:"keep"`
	LastRun  *time.Time             `json:"lastRun,omitempty"`
	Error    string                 `json:"error,omitempty"`
	Catalogs []CatalogResponse      `json:"catalogs"`
	Server   *ServerCatalogResponse `json:"server,omitempty"`
}

// ServerCatalogResponse is the catalog of the server itself and what it holds.
type ServerCatalogResponse struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	WrittenAt  time.Time `json:"writtenAt"`
	Shares     int       `json:"shares"`
	Workgroups int       `json:"workgroups"`
	Accounts   int       `json:"accounts"`
	Bans       int       `json:"bans"`
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

// StoredCatalogResponse is one entry of GET /backup/catalogs: a catalog in the
// backup folder on this machine, by what it says of itself.
type StoredCatalogResponse struct {
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Share     string    `json:"share,omitempty"`
	Workgroup string    `json:"workgroup,omitempty"`
	WrittenAt time.Time `json:"writtenAt"`
	Size      int64     `json:"size"`
}

// storedCatalogsHandlerGET handles GET /backup/catalogs: what the backup folder on
// this machine holds, newest first, for a restore to pick from.
func (api *API) storedCatalogsHandlerGET(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	stored, err := backup.ListFolder(api.cfg.Backup.Path)
	if err != nil {
		log.Printf("failed to list the backup folder: %v", err)
		writeError(w, "the backup folder could not be read: "+err.Error(), http.StatusInternalServerError)
		return
	}

	res := make([]StoredCatalogResponse, 0, len(stored))
	for _, s := range stored {
		entry := StoredCatalogResponse{Path: s.Path, Kind: s.Kind, Share: s.Share, WrittenAt: s.WrittenAt, Size: s.Size}
		if s.Kind == "connection" {
			entry.Workgroup = s.Workgroup.String()
		}
		res = append(res, entry)
	}

	writeJSON(w, res)
}

// backupHandlerGET handles GET /backup.
func (api *API) backupHandlerGET(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	res := BackupResponse{
		Enabled:   api.cfg.Backup.Enabled,
		BufferAge: stores.BufferAge(api.cfg.BufferAge(true)).String(),
	}
	if api.server != nil {
		report := api.server.BackupStatus()
		res.Local = tierStatus(report.Local)
		res.Network = tierStatus(report.Network)
	}

	writeJSON(w, res)
}

// tierStatus is a tier's status as the API reports it, nil for a tier that is off.
func tierStatus(status *backup.Status) *TierStatus {
	if status == nil {
		return nil
	}

	tier := &TierStatus{
		Path:     status.Path,
		Interval: status.Interval.String(),
		Keep:     status.Keep,
		Error:    status.Error,
		Catalogs: make([]CatalogResponse, 0, len(status.Catalogs)),
	}
	if !status.LastRun.IsZero() {
		last := status.LastRun
		tier.LastRun = &last
	}
	for _, c := range status.Catalogs {
		tier.Catalogs = append(tier.Catalogs, CatalogResponse{
			Share: c.Share, Workgroup: c.Workgroup.String(), Path: c.Path, Size: c.Size, WrittenAt: c.WrittenAt,
			Directories: c.Stats.Directories, Files: c.Stats.Files, Incomplete: c.Stats.Incomplete, Inlined: c.Stats.Inlined,
		})
	}
	if s := status.Server; s != nil {
		tier.Server = &ServerCatalogResponse{
			Path: s.Path, Size: s.Size, WrittenAt: s.WrittenAt,
			Shares: s.Stats.Shares, Workgroups: s.Stats.Workgroups, Accounts: s.Stats.Accounts, Bans: s.Stats.Bans,
		}
	}

	return tier
}
