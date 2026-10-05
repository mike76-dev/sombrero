package api

import (
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
