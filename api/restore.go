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

// Restorer is the part of a store that recreates a connection from its catalog.
// Only the database-backed store has it.
type Restorer interface {
	Restore(ctx context.Context, r *transfer.Reader, opts stores.RestoreOptions) (stores.RestoreStats, error)
}

// RestoreResponse is the response type of POST /restore: what came back.
type RestoreResponse struct {
	Share        string `json:"share"`
	Workgroup    string `json:"workgroup"`
	Accounts     int    `json:"accounts"`
	Policies     int    `json:"policies"`
	Directories  int    `json:"directories"`
	Files        int    `json:"files"`
	AlreadyThere int    `json:"alreadyThere"`
	Incomplete   int    `json:"incomplete"`
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
