package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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

	AddWorkgroup(wg stores.Workgroup) error
	UpdateWorkgroup(wg stores.Workgroup) error
	FindWorkgroup(u uuid.UUID) (stores.Workgroup, error)
	FindWorkgroupByName(name string) (stores.Workgroup, error)
	GetWorkgroups() ([]stores.Workgroup, error)
	RemoveWorkgroup(wg stores.Workgroup) error

	GetAccessRights(share stores.Share, acc stores.Account) (ar stores.AccessRights, err error)
	SetAccessRights(ar stores.AccessRights) error
	RemoveAccessRights(share stores.Share, acc stores.Account) error
	ClearAccessRights(acc stores.Account) error

	RegisterShare(s stores.Share) error
	UnregisterShare(name string) error
	GetShare(name string) (s stores.Share, err error)
	GetShares(acc stores.Account) (shares []stores.Share, err error)
	GetAllShares() (shares []stores.Share, err error)
	GetAccounts(sh stores.Share) (ars []stores.AccessRights, err error)

	AddConnection(wg stores.Workgroup, share stores.Share, appKey types.PrivateKey) error
	RemoveConnection(wg stores.Workgroup, share stores.Share) error
}

// ServerStats keeps track of the server statistics.
type ServerStats struct {
	Start      time.Time `json:"start"`      // The time the server started
	FOpens     uint32    `json:"fOpens"`     // The number of total opens
	SOpens     uint32    `json:"sOpens"`     // The number of sessions currently established
	PwErrors   uint32    `json:"pwErrors"`   // The number of password violations
	PermErrors uint32    `json:"permErrors"` // The number of access permission errors
	BytesSent  uint64    `json:"bytesSent"`  // The total number of bytes sent
	BytesRcvd  uint64    `json:"bytesRcvd"`  // The total number of bytes received
}

// IsBannedResponse is the response type for GET /banned request.
type IsBannedResponse struct {
	Banned bool   `json:"banned"`
	Reason string `json:"reason"`
}

// WorkgroupResponse is the response type for POST /workgroup request.
type WorkgroupResponse struct {
	UUID uuid.UUID `json:"uuid"`
	Name string    `json:"name,omitempty"`
}

// ConnectRequestResponse is the response type for POST /connect/request/:workgroup/:share.
type ConnectRequestResponse struct {
	URL string `json:"url"`
}

// ConnectResponse is the response type for PUT /connect/:workgroup/:share when
// completing a first-time registration. AppKey is the derived key the caller
// should persist for future reconnections.
type ConnectResponse struct {
	AppKey string `json:"appKey"`
}

// API represents the API call handler.
type API struct {
	router          httprouter.Router
	store           Store
	cfg             stores.IndexdConfig
	mode            stores.ServerMode
	ctx             context.Context
	rl              *ratelimiter
	stats           func() ServerStats
	pendingBuilders sync.Map // key: "workgroupUUID/shareName" → *sdk.Builder
}

// NewAPI returns an initialized API object. stats returns a snapshot
// of the current server statistics; it may be nil.
func NewAPI(ctx context.Context, s Store, cfg stores.IndexdConfig, mode stores.ServerMode, stats func() ServerStats) *API {
	api := &API{
		store: s,
		cfg:   cfg,
		mode:  mode,
		ctx:   ctx,
		rl:    newRatelimiter(ctx),
		stats: stats,
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

	router.GET("/ban/:host", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.banHandlerGET(w, req, ps)
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

	router.GET("/shares", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.sharesHandlerGET(w, req, ps)
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

	router.POST("/workgroup", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.workgroupHandlerPOST(w, req, ps)
	})

	router.GET("/workgroups", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.workgroupsHandlerGET(w, req, ps)
	})

	router.GET("/workgroup/:id", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.workgroupHandlerGET(w, req, ps)
	})

	router.PUT("/workgroup/:id", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.workgroupHandlerPUT(w, req, ps)
	})

	router.DELETE("/workgroup/:id", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.workgroupHandlerDELETE(w, req, ps)
	})

	router.GET("/stats", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.statsHandlerGET(w, req, ps)
	})

	router.POST("/connect/:workgroup/:share", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.connectHandlerPOST(w, req, ps)
	})

	router.PUT("/connect/:workgroup/:share", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.connectHandlerPUT(w, req, ps)
	})

	router.DELETE("/connect/:workgroup/:share", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.connectHandlerDELETE(w, req, ps)
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

// banHandlerGET handles the GET /ban/:host calls.
func (api *API) banHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
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
		if username == "" {
			writeError(w, "username cannot be empty", http.StatusBadRequest)
			return
		}
		wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
		if !ok {
			return
		}
		acc, err = api.store.FindAccount(username, wg.UUID.String())
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

	wg, ok := api.resolveWorkgroup(w, acc.Workgroup)
	if !ok {
		return
	}
	acc.Workgroup = wg.UUID.String()

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
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	if err := api.store.RemoveAccount(username, wg.UUID.String()); err != nil {
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

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	accs, err := api.store.FindAccounts(wg.UUID.String())
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

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	if err := api.store.RemoveAccounts(wg.UUID.String()); err != nil {
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
	if api.mode == stores.ModeLite && share.Type != "renterd" {
		writeError(w, "only renterd shares are supported in Lite mode", http.StatusBadRequest)
		return
	}

	if err := api.store.RegisterShare(share); err != nil {
		log.Printf("failed to register share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// sharesHandlerGET handles the GET /shares calls.
func (api *API) sharesHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	shares, err := api.store.GetAllShares()
	if err != nil {
		log.Printf("failed to retrieve shares: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	for i := range shares {
		shares[i].Password = "" // Do not expose the API password.
	}

	writeJSON(w, shares)
}

// shareHandlerGET handles the GET /share/:name calls.
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

// shareHandlerDELETE handles the DELETE /share/:name calls.
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

// shareAccountsHandlerGET handles the GET /share/:name/accounts calls.
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

// policyHandlerGET handles the GET /share/:name/policy calls.
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
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	acc, err := api.store.FindAccount(username, wg.UUID.String())
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

// policyHandlerPUT handles the PUT /share/:name/policy calls.
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
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
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

	acc, err := api.store.FindAccount(username, wg.UUID.String())
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

// policyHandlerDELETE handles the DELETE /share/:name/policy calls.
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
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	acc, err := api.store.FindAccount(username, wg.UUID.String())
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
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	acc, err := api.store.FindAccount(username, wg.UUID.String())
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
	if username == "" {
		writeError(w, "username cannot be empty", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	acc, err := api.store.FindAccount(username, wg.UUID.String())
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

// resolveWorkgroup looks up a workgroup by UUID or name.
// If param parses as a UUID it uses FindWorkgroup; otherwise FindWorkgroupByName.
// On failure it writes the appropriate error response and returns false.
func (api *API) resolveWorkgroup(w http.ResponseWriter, param string) (stores.Workgroup, bool) {
	if u, err := uuid.Parse(param); err == nil {
		wg, err := api.store.FindWorkgroup(u)
		if err != nil {
			log.Printf("failed to find workgroup: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return stores.Workgroup{}, false
		}
		if wg.ID == 0 {
			writeError(w, "workgroup not found", http.StatusNotFound)
			return stores.Workgroup{}, false
		}
		return wg, true
	}
	wg, err := api.store.FindWorkgroupByName(param)
	if err != nil {
		log.Printf("failed to find workgroup: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return stores.Workgroup{}, false
	}
	if wg.ID == 0 {
		writeError(w, "workgroup not found", http.StatusNotFound)
		return stores.Workgroup{}, false
	}
	return wg, true
}

// workgroupHandlerPOST handles the POST /workgroup calls.
func (api *API) workgroupHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Name string `json:"name,omitempty"`
	}
	if req.ContentLength > 0 {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, "invalid body", http.StatusBadRequest)
			return
		}
	}

	u := uuid.New()
	wg := stores.Workgroup{UUID: u, Name: body.Name}
	if err := api.store.AddWorkgroup(wg); err != nil {
		log.Printf("failed to add workgroup: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("created new workgroup: %s", u)
	writeJSON(w, WorkgroupResponse{UUID: u, Name: body.Name})
}

// workgroupsHandlerGET handles the GET /workgroups calls.
func (api *API) workgroupsHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	wgs, err := api.store.GetWorkgroups()
	if err != nil {
		log.Printf("failed to retrieve workgroups: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, wgs)
}

// workgroupHandlerGET handles the GET /workgroup/:id calls.
// :id may be a UUID or a workgroup name.
func (api *API) workgroupHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	wg, ok := api.resolveWorkgroup(w, ps.ByName("id"))
	if !ok {
		return
	}

	writeJSON(w, wg)
}

// workgroupHandlerPUT handles the PUT /workgroup/:id calls.
// It replaces the list of public folders of an existing workgroup.
// :id may be a UUID or a workgroup name.
func (api *API) workgroupHandlerPUT(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	wg, ok := api.resolveWorkgroup(w, ps.ByName("id"))
	if !ok {
		return
	}

	var body struct {
		PublicDirs []stores.PublicDir `json:"publicDirs"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, "invalid body", http.StatusBadRequest)
		return
	}

	wg.PublicDirs = body.PublicDirs

	if err := api.store.UpdateWorkgroup(wg); err != nil {
		log.Printf("failed to update workgroup: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// workgroupHandlerDELETE handles the DELETE /workgroup/:id calls.
// :id may be a UUID or a workgroup name.
func (api *API) workgroupHandlerDELETE(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	wg, ok := api.resolveWorkgroup(w, ps.ByName("id"))
	if !ok {
		return
	}

	if err := api.store.RemoveWorkgroup(wg); err != nil {
		log.Printf("failed to remove workgroup: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// statsHandlerGET handles the GET /stats calls.
func (api *API) statsHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	var stats ServerStats
	if api.stats != nil {
		stats = api.stats()
	}

	writeJSON(w, stats)
}

// connectHandlerPOST handles the POST /connect/:workgroup/:share calls.
// It initiates an indexd connection-approval flow by sending a registration request
// to the indexer and returning the URL the admin must visit to approve it.
// :workgroup may be a UUID or a workgroup name.
func (api *API) connectHandlerPOST(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	wg, ok := api.resolveWorkgroup(w, ps.ByName("workgroup"))
	if !ok {
		return
	}

	share, err := api.store.GetShare(strings.ToLower(ps.ByName("share")))
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if share.Name == "" {
		writeError(w, "share not found", http.StatusNotFound)
		return
	}
	if share.Type != "indexd" {
		writeError(w, "connection requests are only supported for indexd shares", http.StatusBadRequest)
		return
	}

	builder := sdk.NewBuilder(share.ServerName, sdk.AppMetadata{
		ID:          types.HashBytes(append([]byte(api.cfg.Name), []byte(api.cfg.Description)...)),
		Name:        api.cfg.Name,
		Description: api.cfg.Description,
		LogoURL:     api.cfg.LogoURL,
		ServiceURL:  api.cfg.ServiceURL,
	})

	approvalURL, err := builder.RequestConnection(req.Context())
	if err != nil {
		log.Printf("failed to request connection: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	pendingKey := wg.UUID.String() + "/" + share.Name
	api.pendingBuilders.Store(pendingKey, builder)
	go func() {
		select {
		case <-time.After(10 * time.Minute):
			api.pendingBuilders.Delete(pendingKey)
		case <-api.ctx.Done():
		}
	}()
	writeJSON(w, ConnectRequestResponse{URL: approvalURL})
}

// connectHandlerPUT handles the PUT /connect/:workgroup/:share calls.
// Three paths:
//  1. Body with appKey (hex) — reconnect using an existing key.
//  2. No body, indexd share, pending builder present — complete the approval flow
//     started by POST /connect/request, derive the app key, and return it.
//  3. No body, renterd share — no key required.
//
// :workgroup may be a UUID or a workgroup name.
func (api *API) connectHandlerPUT(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	wg, ok := api.resolveWorkgroup(w, ps.ByName("workgroup"))
	if !ok {
		return
	}

	share, err := api.store.GetShare(strings.ToLower(ps.ByName("share")))
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if share.Name == "" {
		writeError(w, "share not found", http.StatusNotFound)
		return
	}

	var body struct {
		AppKey string `json:"appKey,omitempty"` // hex-encoded
	}
	if req.ContentLength > 0 {
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeError(w, "invalid body", http.StatusBadRequest)
			return
		}
	}

	var appKey types.PrivateKey

	if body.AppKey != "" {
		// Path 1: reconnect with a known app key.
		keyBytes, err := hex.DecodeString(body.AppKey)
		if err != nil {
			writeError(w, "invalid app key encoding", http.StatusBadRequest)
			return
		}
		appKey = types.PrivateKey(keyBytes)
		if len(appKey) != 64 {
			writeError(w, "indexd share requires a valid 64-byte app key", http.StatusBadRequest)
			return
		}
	} else if share.Type == "indexd" {
		// Path 2: complete a pending first-time registration.
		pendingKey := wg.UUID.String() + "/" + share.Name
		v, ok := api.pendingBuilders.Load(pendingKey)
		if !ok {
			writeError(w, "no pending connection request found; call POST /connect/request first", http.StatusBadRequest)
			return
		}
		builder := v.(*sdk.Builder)

		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
		defer cancel()

		if err := builder.WaitForApproval(ctx); err != nil {
			api.pendingBuilders.Delete(pendingKey)
			log.Printf("connection approval failed: %v", err)
			writeError(w, "connection not approved: "+err.Error(), http.StatusBadRequest)
			return
		}

		sdkInst, err := builder.Register(req.Context(), api.cfg.SeedPhrase)
		if err != nil {
			api.pendingBuilders.Delete(pendingKey)
			log.Printf("failed to register app: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		api.pendingBuilders.Delete(pendingKey)
		appKey = sdkInst.AppKey()
	}
	// Path 3: renterd share — appKey stays nil, AddConnection handles it.

	if err := api.store.AddConnection(wg, share, appKey); err != nil {
		log.Printf("failed to add connection: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if len(appKey) > 0 {
		writeJSON(w, ConnectResponse{AppKey: hex.EncodeToString(appKey)})
	} else {
		writeSuccess(w)
	}
}

// connectHandlerDELETE handles the DELETE /connect/:workgroup/:share calls.
// :workgroup may be a UUID or a workgroup name.
func (api *API) connectHandlerDELETE(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	if api.rl.limitExceeded(getRemoteHost(req)) {
		writeError(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	wg, ok := api.resolveWorkgroup(w, ps.ByName("workgroup"))
	if !ok {
		return
	}

	share, err := api.store.GetShare(strings.ToLower(ps.ByName("share")))
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if share.Name == "" {
		writeError(w, "share not found", http.StatusNotFound)
		return
	}

	if err := api.store.RemoveConnection(wg, share); err != nil {
		log.Printf("failed to remove connection: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}
