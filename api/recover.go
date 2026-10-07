package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/backup"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	sdk "go.sia.tech/siastorage"
)

// RecoverRequest is the body of POST /recover: the account to recover from, by
// the indexer it is at and its app key. Force applies the catalog over a
// connection the server has already.
type RecoverRequest struct {
	Address string `json:"address"`
	AppKey  string `json:"appKey"`
	Force   bool   `json:"force,omitempty"`
}

// RecoverResponse is the response type of POST /recover: the catalogs that were
// found in the account, one per connection it serves, and what restoring each
// came to.
type RecoverResponse struct {
	Catalogs []RecoveredCatalog `json:"catalogs"`
}

// RecoveredCatalog is one catalog found in the account. Error says why it could
// not be restored, where it could not; the others are restored all the same.
type RecoveredCatalog struct {
	Catalog string `json:"catalog"`
	Error   string `json:"error,omitempty"`
	RestoreResponse
}

// recoveryTimeout bounds a recovery: the walk of the account's object log and the
// download of the catalog.
const recoveryTimeout = 10 * time.Minute

// catalogSource builds what a recovery reads an account with. The tests put a
// source of their own here.
type catalogSource func(address string, key types.PrivateKey) (client.CatalogSource, error)

// sdkCatalogSource reads an account through the SDK, as the server does.
func (api *API) sdkCatalogSource(address string, key types.PrivateKey) (client.CatalogSource, error) {
	sdkClient, err := sdk.NewBuilder(address, api.appMetadata()).SDK(key)
	if err != nil {
		return nil, err
	}

	return client.NewIndexdAccount(sdkClient), nil
}

// recoverHandlerPOST handles POST /recover: it finds the newest catalog the account
// carries of itself, opens it with the app key, and restores the connection it
// describes. What was written to the share after that catalog is not in it, and
// an import of the same account into the restored share brings it over.
func (api *API) recoverHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	store, ok := api.store.(Restorer)
	if !ok {
		writeError(w, "recovering needs the database-backed store, which the Lite mode does not use", http.StatusBadRequest)
		return
	}

	var body RecoverRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Address == "" {
		writeError(w, "the address of the indexer cannot be empty", http.StatusBadRequest)
		return
	}
	key, err := hex.DecodeString(body.AppKey)
	if err != nil || len(key) != 64 {
		writeError(w, "recovering an account needs its 64-byte app key, as 128 hex characters", http.StatusBadRequest)
		return
	}

	source, err := api.catalogSource(body.Address, types.PrivateKey(key))
	if err != nil {
		writeError(w, "that indexer did not accept the app key: "+err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), recoveryTimeout)
	defer cancel()

	found, held, err := client.FindCatalogs(ctx, source)
	switch {
	case errors.Is(err, client.ErrNoCatalog):
		writeError(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		log.Printf("failed to look for a catalog in the account: %v", err)
		writeError(w, "the account could not be read: "+err.Error(), http.StatusBadGateway)
		return
	}

	// One connection that cannot be restored does not keep the others from
	// being: each says for itself what came of it.
	res := RecoverResponse{Catalogs: make([]RecoveredCatalog, 0, len(found))}
	for _, catalog := range found {
		entry := RecoveredCatalog{Catalog: catalog.Path}
		stats, err := recoverCatalog(ctx, store, key, catalog, stores.RestoreOptions{Force: body.Force, Held: held})
		if err != nil {
			log.Printf("failed to restore the catalog %s: %v", catalog.Path, err)
			entry.Error = err.Error()
		} else {
			entry.RestoreResponse = connectionResponse(stats)
		}
		res.Catalogs = append(res.Catalogs, entry)
	}

	writeJSON(w, res)
}

// recoverCatalog opens one catalog found in an account and restores it.
func recoverCatalog(ctx context.Context, store Restorer, key []byte, catalog client.FoundCatalog, opts stores.RestoreOptions) (stores.RestoreStats, error) {
	plain, err := backup.Open(key, catalog.Data)
	if err != nil {
		return stores.RestoreStats{}, err
	}
	r, err := transfer.NewReader(bytes.NewReader(plain))
	if err != nil {
		return stores.RestoreStats{}, fmt.Errorf("it does not read as a catalog: %w", err)
	}

	return store.Restore(ctx, r, opts)
}

// heldObjects returns what says whether the account a catalog belongs to still
// holds an object, read with the app key the catalog carries. A catalog without
// a key is taken at its word, and an account that cannot be read is refused,
// since a restore that cannot check would point at whatever is gone.
func (api *API) heldObjects(w http.ResponseWriter, ctx context.Context, conn *transfer.Connection) (func(types.Hash256) bool, bool) {
	if len(conn.AppKey) == 0 {
		return nil, true
	}

	source, err := api.catalogSource(conn.Share.Server, types.PrivateKey(conn.AppKey))
	if err != nil {
		writeError(w, "the indexer did not accept the app key in the catalog, so the catalog cannot be checked against the account: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}
	held, err := client.HeldObjects(ctx, source)
	if err != nil {
		writeError(w, "the account could not be read, so the catalog cannot be checked against it: "+err.Error(), http.StatusBadGateway)
		return nil, false
	}

	return held, true
}
