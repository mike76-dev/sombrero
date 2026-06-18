package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/core/types"
	sdk "go.sia.tech/siastorage"
)

// Store implements the database store.
type Store interface {
	IsBanned(host string) (bool, string, error)
	BanHost(host, reason string) error
	UnbanHost(host string) error
	ClearBans() error

	GetAccountByID(id int) (acc stores.Account, err error)
	FindAccount(username, workgroup string) (acc stores.Account, err error)
	AddAccount(acc stores.Account) error
	HasAccount(username, workgroup string) (bool, error)
	RemoveAccount(username, workgroup string) error
	FindAccounts(workgroup string) (accs []stores.Account, err error)
	RemoveAccounts(workgroup string) error

	GetAccessRights(share stores.Share, acc stores.Account) (ar stores.AccessRights, err error)
	SetAccessRights(ar stores.AccessRights) error
	RemoveAccessRights(share stores.Share, acc stores.Account) error
	ClearAccessRights(acc stores.Account) error

	RegisterShare(s stores.Share) error
	UnregisterShare(name string) error
	GetShare(name string) (s stores.Share, err error)
	GetShares(acc stores.Account) (shares []stores.Share, err error)
	GetAccounts(sh stores.Share) (ars []stores.AccessRights, err error)
}

// IsBannedResponse is the response type for GET /banned request.
type IsBannedResponse struct {
	Banned bool   `json:"banned"`
	Reason string `json:"reason"`
}

// API represents the API call handler.
type API struct {
	router httprouter.Router
	store  Store
	cfg    stores.IndexdConfig
	ctx    context.Context
	rl     *ratelimiter
}

// NewAPI returns an initialized API object.
func NewAPI(ctx context.Context, s Store, cfg stores.IndexdConfig) *API {
	api := &API{
		store: s,
		cfg:   cfg,
		ctx:   ctx,
		rl:    newRatelimiter(ctx),
	}
	api.buildHTTPRoutes()
	return api
}

// BasicAuth wraps an http.Handler to force a basic auth with a password.
func BasicAuth(password string) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if _, p, ok := req.BasicAuth(); !ok || p != password {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, req)
		})
	}
}

// ServeHTTP implements http.HandlerFunc.
func (api *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.router.ServeHTTP(w, r)
}

// buildHTTPRoutes maps the routes to the respective handlers.
func (api *API) buildHTTPRoutes() {
	router := httprouter.New()

	router.GET("/banned/:host", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.bannedHandlerGET(w, req, ps)
	})

	router.PUT("/ban/:host", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.banHandlerPUT(w, req, ps)
	})

	router.DELETE("/ban/:host", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.banHandlerDELETE(w, req, ps)
	})

	router.DELETE("/bans", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.bansHandlerDELETE(w, req, ps)
	})

	router.GET("/account", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.accountHandlerGET(w, req, ps)
	})

	router.POST("/account", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.accountHandlerPOST(w, req, ps)
	})

	router.DELETE("/account", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.accountHandlerDELETE(w, req, ps)
	})

	router.GET("/accounts", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.accountsHandlerGET(w, req, ps)
	})

	router.DELETE("/accounts", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.accountsHandlerDELETE(w, req, ps)
	})

	router.POST("/share", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.shareHandlerPOST(w, req, ps)
	})

	router.GET("/share/:name", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.shareHandlerGET(w, req, ps)
	})

	router.DELETE("/share/:name", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.shareHandlerDELETE(w, req, ps)
	})

	router.GET("/share/:name/accounts", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.shareAccountsHandlerGET(w, req, ps)
	})

	router.GET("/share/:name/policy", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.policyHandlerGET(w, req, ps)
	})

	router.PUT("/share/:name/policy", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.policyHandlerPUT(w, req, ps)
	})

	router.DELETE("/share/:name/policy", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.policyHandlerDELETE(w, req, ps)
	})

	router.GET("/account/shares", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.accountSharesHandlerGET(w, req, ps)
	})

	router.DELETE("/account/policy", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.accountPolicyHandlerDELETE(w, req, ps)
	})

	api.router = *router
}

// writeJSON writes a JSON object to the response body.
func writeJSON(w http.ResponseWriter, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err := json.NewEncoder(w).Encode(obj)
	if _, isJsonErr := err.(*json.SyntaxError); isJsonErr {
		log.Printf("failed to encode API response: %v", err)
	}
}

// writeError writes an error response to the response body.
func writeError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	err := json.NewEncoder(w).Encode(message)
	if _, isJsonErr := err.(*json.SyntaxError); isJsonErr {
		log.Printf("failed to encode API error response: %v", err)
	}
}

// writeSuccess sets the 204 status code.
func writeSuccess(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// bannedHandlerGET handles the GET /banned/:host calls.
func (api *API) bannedHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	host := ps.ByName("host")
	isBanned, reason, err := api.store.IsBanned(host)
	if err != nil {
		log.Printf("failed to check ban status: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, IsBannedResponse{
		Banned: isBanned,
		Reason: reason,
	})
}

// banHandlerPUT handles the PUT /ban/:host calls.
func (api *API) banHandlerPUT(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	host := ps.ByName("host")
	reason := req.FormValue("reason")
	if err := api.store.BanHost(host, reason); err != nil {
		log.Printf("failed to ban host: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// banHandlerDELETE handles the DELETE /ban/:host calls.
func (api *API) banHandlerDELETE(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	host := ps.ByName("host")
	if err := api.store.UnbanHost(host); err != nil {
		log.Printf("failed to unban host: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// bansHandlerDELETE handles the DELETE /bans calls.
func (api *API) bansHandlerDELETE(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	if err := api.store.ClearBans(); err != nil {
		log.Printf("failed to clear bans: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// accountHandlerGET handles the GET /account calls.
func (api *API) accountHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	var acc stores.Account
	var err error
	idValue := req.FormValue("id")
	if idValue == "" {
		username := strings.ToLower(req.FormValue("username"))
		workgroup := strings.ToLower(req.FormValue("workgroup"))
		if username == "" {
			writeError(w, "username cannot be empty", http.StatusBadRequest)
			return
		}
		acc, err = api.store.FindAccount(username, workgroup)
		if err != nil {
			log.Printf("failed to find account: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
	} else {
		id, _ := strconv.ParseInt(idValue, 10, 64)
		if id <= 0 {
			writeError(w, "invalid account ID", http.StatusBadRequest)
			return
		}
		acc, err = api.store.GetAccountByID(int(id))
		if err != nil {
			log.Printf("failed to find account: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, acc)
}

// accountHandlerPOST handles the POST /account calls.
func (api *API) accountHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	var acc stores.Account
	if err := json.NewDecoder(req.Body).Decode(&acc); err != nil {
		writeError(w, "invalid account structure", http.StatusBadRequest)
		return
	}
	acc.Username = strings.ToLower(acc.Username)
	acc.Workgroup = strings.ToLower(acc.Workgroup)

	if err := api.store.AddAccount(acc); err != nil {
		log.Printf("failed to add account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// accountHandlerDELETE handles the DELETE /account calls.
func (api *API) accountHandlerDELETE(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	username := strings.ToLower(req.FormValue("username"))
	workgroup := strings.ToLower(req.FormValue("workgroup"))
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	if err := api.store.RemoveAccount(username, workgroup); err != nil {
		log.Printf("failed to remove account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// accountsHandlerGET handles the GET /accounts calls.
func (api *API) accountsHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	workgroup := strings.ToLower(req.FormValue("workgroup"))
	accs, err := api.store.FindAccounts(workgroup)
	if err != nil {
		log.Printf("failed to find accounts: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, accs)
}

// accountsHandlerDELETE handles the DELETE /accounts calls.
func (api *API) accountsHandlerDELETE(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	workgroup := strings.ToLower(req.FormValue("workgroup"))
	if err := api.store.RemoveAccounts(workgroup); err != nil {
		log.Printf("failed to remove accounts: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// shareHandlerPOST handles the POST /share calls.
func (api *API) shareHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	var share stores.Share
	if err := json.NewDecoder(req.Body).Decode(&share); err != nil {
		writeError(w, "invalid share structure", http.StatusBadRequest)
		return
	}
	share.Name = strings.ToLower(share.Name)
	share.Type = strings.ToLower(share.Type)
	if share.Type != "renterd" && share.Type != "indexd" {
		writeError(w, "wrong share type", http.StatusBadRequest)
		return
	}

	if share.Type == "indexd" {
		builder := sdk.NewBuilder(share.ServerName, sdk.AppMetadata{
			ID:          types.HashBytes(append([]byte(api.cfg.Name), []byte(api.cfg.Description)...)),
			Name:        api.cfg.Name,
			Description: api.cfg.Description,
			LogoURL:     api.cfg.LogoURL,
			ServiceURL:  api.cfg.ServiceURL,
		})
		respURL, err := builder.RequestConnection(api.ctx)
		if err != nil {
			log.Printf("failed to request app connection: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		fmt.Println("Please approve the app connection by visiting the following URL:", respURL)
		approved, err := builder.WaitForApproval(api.ctx)
		if err != nil {
			log.Printf("failed to wait for app approval: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		} else if !approved {
			log.Print("app connection was declined")
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		client, err := builder.Register(api.ctx, api.cfg.SeedPhrase)
		if err != nil {
			log.Printf("failed to register app: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		share.AppKey = make(types.PrivateKey, 64)
		copy(share.AppKey, client.AppKey()[:])
		log.Print("app registered successfully")
	}

	if err := api.store.RegisterShare(share); err != nil {
		log.Printf("failed to register share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// shareHandlerGET handles the GET /share/:idorname calls.
func (api *API) shareHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	shareName := ps.ByName("name")
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	share, err := api.store.GetShare(shareName)
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	share.Password = "" // Do not expose the API password.
	writeJSON(w, share)
}

// shareHandlerDELETE handles the DELETE /share/:idorname calls.
func (api *API) shareHandlerDELETE(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	shareName := ps.ByName("name")
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	if err := api.store.UnregisterShare(shareName); err != nil {
		log.Printf("failed to remove share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// shareAccountsHandlerGET handles the GET /share/:idorname/accounts calls.
func (api *API) shareAccountsHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	shareName := ps.ByName("name")
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	share, err := api.store.GetShare(shareName)
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	ars, err := api.store.GetAccounts(share)
	if err != nil {
		log.Printf("failed to find accounts: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, ars)
}

// policyHandlerGET handles the GET /share/:idorname/policy calls.
func (api *API) policyHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	shareName := ps.ByName("name")
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	username := strings.ToLower(req.FormValue("username"))
	workgroup := strings.ToLower(req.FormValue("workgroup"))
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	acc, err := api.store.FindAccount(username, workgroup)
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	share, err := api.store.GetShare(shareName)
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	ar, err := api.store.GetAccessRights(share, acc)
	if err != nil {
		log.Printf("failed to retrieve policy: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, ar)
}

// policyHandlerPUT handles the PUT /share/:idorname/policy calls.
func (api *API) policyHandlerPUT(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	shareName := ps.ByName("name")
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	share, err := api.store.GetShare(shareName)
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	} else if share.Name == "" {
		writeError(w, "no such share", http.StatusBadRequest)
		return
	}

	username := strings.ToLower(req.FormValue("username"))
	workgroup := strings.ToLower(req.FormValue("workgroup"))
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	ra := strings.ToLower(req.FormValue("read"))
	readAccess := ra == "true"
	wa := strings.ToLower(req.FormValue("write"))
	writeAccess := wa == "true"
	da := strings.ToLower(req.FormValue("delete"))
	deleteAccess := da == "true"
	ea := strings.ToLower(req.FormValue("execute"))
	executeAccess := ea == "true"

	acc, err := api.store.FindAccount(username, workgroup)
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := api.store.SetAccessRights(stores.AccessRights{
		ShareName:     shareName,
		AccountID:     acc.ID,
		ReadAccess:    readAccess,
		WriteAccess:   writeAccess,
		DeleteAccess:  deleteAccess,
		ExecuteAccess: executeAccess,
	}); err != nil {
		log.Printf("failed to set policy: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// policyHandlerDELETE handles the DELETE /share/:idorname/policy calls.
func (api *API) policyHandlerDELETE(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	shareName := ps.ByName("name")
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	username := strings.ToLower(req.FormValue("username"))
	workgroup := strings.ToLower(req.FormValue("workgroup"))
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	acc, err := api.store.FindAccount(username, workgroup)
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	share, err := api.store.GetShare(shareName)
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := api.store.RemoveAccessRights(share, acc); err != nil {
		log.Printf("failed to remove policy: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// accountSharesHandlerGET handles the GET /account/shares calls.
func (api *API) accountSharesHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	username := strings.ToLower(req.FormValue("username"))
	workgroup := strings.ToLower(req.FormValue("workgroup"))
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	acc, err := api.store.FindAccount(username, workgroup)
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	shares, err := api.store.GetShares(acc)
	if err != nil {
		log.Printf("failed to find shares: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	for i := range shares {
		shares[i].Password = "" // Do not expose the API password.
	}

	writeJSON(w, shares)
}

// accountPolicyHandlerDELETE handles the DELETE /account/policy calls.
func (api *API) accountPolicyHandlerDELETE(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	username := strings.ToLower(req.FormValue("username"))
	workgroup := strings.ToLower(req.FormValue("workgroup"))
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	acc, err := api.store.FindAccount(username, workgroup)
	if err != nil {
		log.Printf("failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := api.store.ClearAccessRights(acc); err != nil {
		log.Printf("failed to clear policies: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}
