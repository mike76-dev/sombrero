package api

import (
	"context"
	"errors"
	"log"
	"net/http"

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

// restoreHandlerPOST handles POST /restore. The body is a catalog, and ?force=true
// has it applied over a connection the server has already, adding what is
// missing.
func (api *API) restoreHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	store, ok := api.store.(Restorer)
	if !ok {
		writeError(w, "restoring needs the database-backed store, which the Lite mode does not use", http.StatusBadRequest)
		return
	}

	r, err := transfer.NewReader(http.MaxBytesReader(w, req.Body, maxCatalogSize))
	if err != nil {
		writeError(w, "the body is not a catalog: "+err.Error(), http.StatusBadRequest)
		return
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

	force := req.URL.Query().Get("force") == "true"
	stats, err := store.Restore(req.Context(), r, stores.RestoreOptions{Force: force})
	switch {
	case errors.Is(err, stores.ErrConnectionExists):
		writeError(w, err.Error()+"; add force=true to apply the catalog over it", http.StatusConflict)
		return
	case errors.Is(err, stores.ErrNotACatalog), errors.Is(err, stores.ErrShareMismatch),
		errors.Is(err, transfer.ErrTruncated), errors.Is(err, transfer.ErrCorrupted):
		writeError(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		log.Printf("failed to restore a catalog: %v", err)
		writeError(w, "the catalog could not be restored: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, RestoreResponse{
		Kind:         "connection",
		Share:        stats.Share,
		Workgroup:    stats.Workgroup.String(),
		Accounts:     stats.Accounts,
		Policies:     stats.Policies,
		Directories:  stats.Directories,
		Files:        stats.Files,
		AlreadyThere: stats.AlreadyThere,
		Incomplete:   stats.Incomplete,
	})
}
