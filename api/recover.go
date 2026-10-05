package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/backup"
	"github.com/mike76-dev/sombrero/client"
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

// RecoverResponse is the response type of POST /recover: which catalog was found
// in the account, and what restoring it came to.
type RecoverResponse struct {
	Catalog string `json:"catalog"`
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

	found, err := client.FindCatalog(ctx, source)
	switch {
	case errors.Is(err, client.ErrNoCatalog):
		writeError(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		log.Printf("failed to look for a catalog in the account: %v", err)
		writeError(w, "the account could not be read: "+err.Error(), http.StatusBadGateway)
		return
	}

	plain, err := backup.Open(key, found.Data)
	if err != nil {
		writeError(w, "the catalog "+found.Path+" does not open with this key: "+err.Error(), http.StatusBadRequest)
		return
	}
	r, err := transfer.NewReader(bytes.NewReader(plain))
	if err != nil {
		writeError(w, "the catalog "+found.Path+" does not read as one: "+err.Error(), http.StatusBadRequest)
		return
	}

	res, ok := restoreConnection(w, ctx, store, r, body.Force)
	if !ok {
		return
	}

	writeJSON(w, RecoverResponse{Catalog: found.Path, RestoreResponse: res})
}
