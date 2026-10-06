package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
)

// Restorer is the part of a store that recreates a connection, or what belongs
// to the server, from a catalog. Only the database-backed store has it.
type Restorer interface {
	Restore(ctx context.Context, r *transfer.Reader, opts stores.RestoreOptions) (stores.RestoreStats, error)
	RestoreServer(ctx context.Context, r *transfer.Reader) (stores.ServerRestoreStats, error)
}

// RestoreResponse is the response type of POST /restore: what came back. Kind
// says which catalog it was, "connection" or "server", and the counts that go
// with the other kind are left out.
type RestoreResponse struct {
	Kind         string `json:"kind"`
	Share        string `json:"share,omitempty"`
	Workgroup    string `json:"workgroup,omitempty"`
	Accounts     int    `json:"accounts"`
	Policies     int    `json:"policies,omitempty"`
	Directories  int    `json:"directories,omitempty"`
	Files        int    `json:"files,omitempty"`
	AlreadyThere int    `json:"alreadyThere,omitempty"`
	Incomplete   int    `json:"incomplete,omitempty"`
	Shares       int    `json:"shares,omitempty"`
	Workgroups   int    `json:"workgroups,omitempty"`
	Bans         int    `json:"bans,omitempty"`
}

// maxCatalogSize bounds what a restore reads off a request.
const maxCatalogSize = 1 << 30

// StoredRestoreRequest is the JSON body of POST /restore for a catalog in the
// server's own folder, which the browser cannot read but the server can.
type StoredRestoreRequest struct {
	Path  string `json:"path"`
	Force bool   `json:"force,omitempty"`
}

// restoreHandlerPOST handles POST /restore. The body is the catalog itself, with
// ?force=true to apply it over a connection the server has already; or, sent as
// JSON, the path of a catalog in the backup folder on this machine.
func (api *API) restoreHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	store, ok := api.store.(Restorer)
	if !ok {
		writeError(w, "restoring needs the database-backed store, which the Lite mode does not use", http.StatusBadRequest)
		return
	}

	var r *transfer.Reader
	var force bool
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		var body StoredRestoreRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, "invalid body", http.StatusBadRequest)
			return
		}
		f, ok := api.openStoredCatalog(w, body.Path)
		if !ok {
			return
		}
		defer f.Close()
		var err error
		if r, err = transfer.NewReader(f); err != nil {
			writeError(w, body.Path+" is not a catalog: "+err.Error(), http.StatusBadRequest)
			return
		}
		force = body.Force
	} else {
		var err error
		if r, err = transfer.NewReader(http.MaxBytesReader(w, req.Body, maxCatalogSize)); err != nil {
			writeError(w, "the body is not a catalog: "+err.Error(), http.StatusBadRequest)
			return
		}
		force = req.URL.Query().Get("force") == "true"
	}

	if r.Server() != nil {
		stats, err := store.RestoreServer(req.Context(), r)
		if err != nil {
			log.Printf("failed to restore a catalog of the server: %v", err)
			writeError(w, "the catalog could not be restored: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, RestoreResponse{Kind: "server", Shares: stats.Shares, Workgroups: stats.Workgroups, Accounts: stats.Accounts, Bans: stats.Bans})
		return
	}

	res, ok := restoreConnection(w, req.Context(), store, r, force)
	if !ok {
		return
	}

	writeJSON(w, res)
}

// openStoredCatalog opens a catalog in the backup folder on this machine, and
// nothing outside it: the API is the admin's, but it is not a way to read files.
func (api *API) openStoredCatalog(w http.ResponseWriter, path string) (*os.File, bool) {
	dir := api.cfg.Backup.Path
	if dir == "" {
		writeError(w, "no backup folder is configured on this machine", http.StatusBadRequest)
		return nil, false
	}
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
		writeError(w, "the catalog has to be in the backup folder, "+dir, http.StatusBadRequest)
		return nil, false
	}

	f, err := os.Open(path)
	if err != nil {
		writeError(w, "the catalog could not be opened: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}

	return f, true
}

// restoreConnection applies a catalog of a connection and says what came of it,
// or writes why it could not be applied and reports false.
func restoreConnection(w http.ResponseWriter, ctx context.Context, store Restorer, r *transfer.Reader, force bool) (RestoreResponse, bool) {
	stats, err := store.Restore(ctx, r, stores.RestoreOptions{Force: force})
	switch {
	case errors.Is(err, stores.ErrConnectionExists):
		writeError(w, err.Error()+"; ask for the catalog to be applied over it", http.StatusConflict)
		return RestoreResponse{}, false
	case errors.Is(err, stores.ErrNotACatalog), errors.Is(err, stores.ErrShareMismatch),
		errors.Is(err, transfer.ErrTruncated), errors.Is(err, transfer.ErrCorrupted):
		writeError(w, err.Error(), http.StatusBadRequest)
		return RestoreResponse{}, false
	case err != nil:
		log.Printf("failed to restore a catalog: %v", err)
		writeError(w, "the catalog could not be restored: "+err.Error(), http.StatusInternalServerError)
		return RestoreResponse{}, false
	}

	return RestoreResponse{
		Kind:         "connection",
		Share:        stats.Share,
		Workgroup:    stats.Workgroup.String(),
		Accounts:     stats.Accounts,
		Policies:     stats.Policies,
		Directories:  stats.Directories,
		Files:        stats.Files,
		AlreadyThere: stats.AlreadyThere,
		Incomplete:   stats.Incomplete,
	}, true
}
