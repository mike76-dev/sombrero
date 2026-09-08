package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
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
	UpdateShare(s stores.Share) error
	UnregisterShare(name string) error
	GetShare(name string) (s stores.Share, err error)
	GetShares(acc stores.Account) (shares []stores.Share, err error)
	GetAllShares() (shares []stores.Share, err error)
	GetAccounts(sh stores.Share) (ars []stores.AccessRights, err error)

	AddConnection(wg stores.Workgroup, share stores.Share, appKey types.PrivateKey) error
	RemoveConnection(wg stores.Workgroup, share stores.Share) error
	IsConnected(wg stores.Workgroup, share stores.Share) (bool, types.PrivateKey, error)
}

// Server is as much of the running SMB server as the API needs: the statistics
// it keeps, and the share connections it holds, which is where the storage
// backend lives.
type Server interface {
	// Stats returns a snapshot of the current server statistics.
	Stats() ServerStats

	// ShareConnections returns the clients of the named share's workgroup
	// connections, keyed by workgroup UUID, starting the ones that are not
	// running. failed names the workgroups whose connection could not be
	// started, keyed the same way.
	ShareConnections(name string) (conns map[string]client.Client, failed map[string]string, err error)

	// OfferSDK hands over an SDK instance that is already authorized for the
	// workgroup, for the connection that is about to be made to reuse instead of
	// building a second one. DiscardSDK closes an offer that was not taken up.
	OfferSDK(wg stores.Workgroup, share stores.Share, sdkClient *sdk.SDK)
	DiscardSDK(wg stores.Workgroup, share stores.Share)
}

// OrphanedSlab is one entry of an orphan scan: a slab that the share's
// connection has pinned and pays for, while no file references it.
type OrphanedSlab struct {
	Workgroup string        `json:"workgroup"`
	Key       types.Hash256 `json:"key"`
	Size      uint64        `json:"size"`
	PinnedAt  time.Time     `json:"pinnedAt"`
}

// OrphansResponse is the response type for GET /share/:name/orphans.
type OrphansResponse struct {
	// Slabs is what the scan found, oldest pin first, and Count and Size are
	// its totals — what the button acts on and what unpinning would reclaim.
	Slabs []OrphanedSlab `json:"slabs"`
	Count int            `json:"count"`
	Size  uint64         `json:"size"`

	// MinAge is the age a slab had to reach to be reported, in seconds.
	MinAge float64 `json:"minAge"`

	// Errors names the workgroup connections that could not be scanned, if
	// any. The scan of the others still counts.
	Errors map[string]string `json:"errors,omitempty"`
}

// FragmentedSlab is one entry of a fragmentation report: a slab holding dead
// space that editing or deleting files left behind.
type FragmentedSlab struct {
	Workgroup     string        `json:"workgroup"`
	Key           types.Hash256 `json:"key"`
	Size          uint64        `json:"size"`
	Filled        uint64        `json:"filled"`
	Used          uint64        `json:"used"`
	Wasted        uint64        `json:"wasted"`
	Pieces        int           `json:"pieces"`
	Fragmentation float64       `json:"fragmentation"`
}

// FragmentationResponse is the response type for GET /share/:name/fragmentation.
type FragmentationResponse struct {
	// Slabs is what the check found, most fragmented first.
	Slabs []FragmentedSlab `json:"slabs"`

	// Total counts every slab of the share and Wasted is the dead space in all
	// of them, which is what Fragmented and FragmentedWasted are a part of.
	Total            int    `json:"total"`
	Wasted           uint64 `json:"wasted"`
	Fragmented       int    `json:"fragmented"`
	FragmentedWasted uint64 `json:"fragmentedWasted"`

	// Threshold is the dead space a slab had to hold to be listed, as a
	// fraction of its size.
	Threshold float64 `json:"threshold"`

	// Errors names the workgroup connections that could not be checked, if
	// any. The check of the others still counts.
	Errors map[string]string `json:"errors,omitempty"`
}

// DefragmentResponse is the response type for POST /share/:name/fragmentation.
// It reports one round: the slabs whose contents were moved back into the
// upload queue, how much that was, and the dead space those slabs held.
type DefragmentResponse struct {
	Slabs     int               `json:"slabs"`
	Moved     uint64            `json:"moved"`
	Reclaimed uint64            `json:"reclaimed"`
	Errors    map[string]string `json:"errors,omitempty"`
}

// UnpinOrphansResponse is the response type for DELETE /share/:name/orphans.
type UnpinOrphansResponse struct {
	Unpinned int               `json:"unpinned"`
	Freed    uint64            `json:"freed"`
	Failed   int               `json:"failed"`
	Errors   map[string]string `json:"errors,omitempty"`
}

// SettingsResponse is the response type for GET /settings. It carries what the
// web UI has to know about how the server is configured, rather than anything
// else the config file holds.
type SettingsResponse struct {
	Mode string `json:"mode"`

	// Anonymous is the server-wide switch every share's own setting hangs off:
	// with it off, no share admits an anonymous session however it is set up.
	Anonymous bool `json:"anonymous"`
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

// ConnectStatusResponse is the response type of every /connect/:workgroup/:share
// call. Started and Since — when the attempt began and when it entered the phase
// it is in — are only there for an attempt that is being tracked. URL is the
// approval link while the attempt waits for one. AppKey is the key a first-time
// registration derived, which the caller should persist for future reconnections:
// it is reported once, on the first read after the attempt connected, and never
// again.
type ConnectStatusResponse struct {
	State   ConnectState `json:"state"`
	Started *time.Time   `json:"started,omitempty"`
	Since   *time.Time   `json:"since,omitempty"`
	URL     string       `json:"url,omitempty"`
	AppKey  string       `json:"appKey,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// API represents the API call handler.
type API struct {
	router   httprouter.Router
	store    Store
	server   Server
	cfg      stores.Config
	mode     stores.ServerMode
	ctx      context.Context
	connects connectTracker
}

// NewAPI returns an initialized API object. srv is the running SMB server and
// may be nil, in which case the statistics come back empty and the endpoints
// that need a storage backend report the share as unavailable.
func NewAPI(ctx context.Context, s Store, srv Server, cfg stores.Config) *API {
	api := &API{
		store:  s,
		server: srv,
		cfg:    cfg,
		mode:   cfg.Mode,
		ctx:    ctx,
	}
	api.buildHTTPRoutes()
	return api
}

// BasicAuth wraps an http.Handler to force a basic auth with a password.
// Only the password is checked; the username is ignored.
// The passwords are hashed before they are compared, so that the comparison
// runs over a fixed number of bytes and leaks neither the contents nor the
// length of the configured password.
func BasicAuth(password string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(password))
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			_, p, ok := req.BasicAuth()
			got := sha256.Sum256([]byte(p))
			if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
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

	router.GET("/share/:name/orphans", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.orphansHandlerGET(w, req, ps)
	})

	router.DELETE("/share/:name/orphans", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.orphansHandlerDELETE(w, req, ps)
	})

	router.PUT("/share/:name", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.shareHandlerPUT(w, req, ps)
	})

	router.GET("/share/:name/fragmentation", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.fragmentationHandlerGET(w, req, ps)
	})

	router.POST("/share/:name/fragmentation", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.fragmentationHandlerPOST(w, req, ps)
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

	router.GET("/settings", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.settingsHandlerGET(w, req, ps)
	})

	router.GET("/connect/:workgroup/:share", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		api.connectHandlerGET(w, req, ps)
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
	writeJSONStatus(w, http.StatusOK, obj)
}

// writeJSONStatus writes a JSON object to the response body under the given
// status code.
func writeJSONStatus(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
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
	if err := api.store.ClearBans(); err != nil {
		log.Printf("failed to clear bans: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// accountHandlerGET handles the GET /account calls.
func (api *API) accountHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
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
		if errors.Is(err, stores.ErrAccountNotFound) {
			writeError(w, "account not found", http.StatusNotFound)
			return
		} else if err != nil {
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
		if errors.Is(err, stores.ErrAccountNotFound) {
			writeError(w, "account not found", http.StatusNotFound)
			return
		} else if err != nil {
			log.Printf("failed to find account: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, acc)
}

// accountHandlerPOST handles the POST /account calls.
func (api *API) accountHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
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

	// An account with no password is one anybody can log in as, so it is only
	// worth having where a share takes guests. Without one it could connect
	// nowhere, and the refusal says so rather than leaving an open account
	// behind that never works.
	if acc.Password == "" {
		shares, err := api.store.GetAllShares()
		if err != nil {
			log.Printf("failed to retrieve shares: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		var guests bool
		for _, share := range shares {
			if share.AllowGuest {
				guests = true
				break
			}
		}
		if !guests {
			writeError(w, "no share offers guest access, so a passwordless account has nowhere to connect", http.StatusBadRequest)
			return
		}
	}

	if err := api.store.AddAccount(acc); err != nil {
		log.Printf("failed to add account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// accountHandlerDELETE handles the DELETE /account calls.
func (api *API) accountHandlerDELETE(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
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

// checkShareAccess validates what a share says about guest and anonymous
// access, and reports the status and message to refuse it with. An anonymous
// session is confined to the public folder, so a share that offers it has to
// name one, and the server has to allow anonymous access at all.
func checkShareAccess(share stores.Share, anonymous bool) (int, string) {
	if share.PublicDir != "" && strings.ContainsAny(share.PublicDir, `/\`) {
		return http.StatusBadRequest, "the public folder is a folder name, not a path"
	}

	if !share.AllowAnonymous {
		return 0, ""
	}
	if !anonymous {
		return http.StatusBadRequest, "anonymous access is turned off in the server config"
	}
	if share.PublicDir == "" {
		return http.StatusBadRequest, "anonymous access needs a public folder to confine it to"
	}

	return 0, ""
}

// shareHandlerPOST handles the POST /share calls.
func (api *API) shareHandlerPOST(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
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
	if status, msg := checkShareAccess(share, api.cfg.Anonymous); msg != "" {
		writeError(w, msg, status)
		return
	}

	if err := api.store.RegisterShare(share); err != nil {
		log.Printf("failed to register share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// shareHandlerPUT handles the PUT /share/:name calls. It changes what a share
// offers its clients — guest and anonymous access, the public folder, and the
// remark — and leaves what it is backed by alone: a share that changed its
// server or its redundancy would be a different share holding the same files.
func (api *API) shareHandlerPUT(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	shareName := strings.ToLower(ps.ByName("name"))
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	var settings stores.Share
	if err := json.NewDecoder(req.Body).Decode(&settings); err != nil {
		writeError(w, "invalid share structure", http.StatusBadRequest)
		return
	}

	share, err := api.store.GetShare(shareName)
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if share.Name == "" {
		writeError(w, "share not found", http.StatusNotFound)
		return
	}

	share.Remark = settings.Remark
	share.AllowGuest = settings.AllowGuest
	share.AllowAnonymous = settings.AllowAnonymous
	share.PublicDir = settings.PublicDir

	if status, msg := checkShareAccess(share, api.cfg.Anonymous); msg != "" {
		writeError(w, msg, status)
		return
	}

	if err := api.store.UpdateShare(share); err != nil {
		log.Printf("failed to update share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// sharesHandlerGET handles the GET /shares calls.
func (api *API) sharesHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
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

// scanMinAge reads the minimum age a slab must have reached to count as
// orphaned from the request, given in seconds. It writes the error response
// itself when the value makes no sense.
func scanMinAge(w http.ResponseWriter, req *http.Request) (time.Duration, bool) {
	raw := req.FormValue("minAge")
	if raw == "" {
		return client.DefaultOrphanMinAge, true
	}

	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || secs < 0 {
		writeError(w, "minAge must be a non-negative number of seconds", http.StatusBadRequest)
		return 0, false
	}
	if secs == 0 {
		// Zero would report the slabs of the uploads that are in flight right
		// now, which are unreferenced only because they are not finished.
		writeError(w, "minAge of 0 would report the uploads that are still in flight", http.StatusBadRequest)
		return 0, false
	}

	return time.Duration(secs * float64(time.Second)), true
}

// fragmentationThreshold reads the dead space a slab has to hold to be
// reported from the request, as a fraction of its size. Left out, the
// connections report at the level they are configured with.
func fragmentationThreshold(w http.ResponseWriter, req *http.Request) (float64, bool) {
	raw := req.FormValue("threshold")
	if raw == "" {
		return 0, true
	}

	t, err := strconv.ParseFloat(raw, 64)
	if err != nil || t <= 0 || t > 1 {
		writeError(w, "threshold must be a fraction between 0 and 1", http.StatusBadRequest)
		return 0, false
	}

	return t, true
}

// scannableShare resolves the workgroup connections of the named share, and
// writes the error response itself when the share cannot be scanned. The
// connections it could not reach are returned alongside, to be reported with
// whatever the scan of the others finds.
func (api *API) scannableShare(w http.ResponseWriter, name string) (map[string]client.Client, map[string]string, bool) {
	if name == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return nil, nil, false
	}

	share, err := api.store.GetShare(name)
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return nil, nil, false
	}
	if share.Name == "" {
		writeError(w, "share not found", http.StatusNotFound)
		return nil, nil, false
	}
	if share.Type != "indexd" {
		writeError(w, client.ErrNoSlabScan.Error(), http.StatusBadRequest)
		return nil, nil, false
	}

	if api.server == nil {
		writeError(w, "the share connections are not available on this server", http.StatusServiceUnavailable)
		return nil, nil, false
	}

	conns, failed, err := api.server.ShareConnections(name)
	if failed == nil {
		failed = make(map[string]string)
	}
	if err != nil {
		if errors.Is(err, client.ErrNoSlabScan) {
			writeError(w, err.Error(), http.StatusBadRequest)
			return nil, nil, false
		}
		// The share is in the store but the server cannot serve it, which is
		// not something the caller can tell from the response alone.
		log.Printf("failed to resolve the connections of share %s: %v", name, err)
		writeError(w, "share not connected: "+err.Error(), http.StatusNotFound)
		return nil, nil, false
	}
	if len(conns) == 0 {
		// Nothing is connected, so nothing of this share is pinned right now.
		// Scanning would report every slab of it as an orphan, which is the
		// one answer that must never be acted on.
		writeError(w, "no workgroup is connected to this share", http.StatusConflict)
		return nil, nil, false
	}

	return conns, failed, true
}

// orphansHandlerGET handles the GET /share/:name/orphans calls. It reports the
// slabs the share's connections have pinned that no file references, without
// changing anything.
func (api *API) orphansHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	conns, failed, ok := api.scannableShare(w, ps.ByName("name"))
	if !ok {
		return
	}
	minAge, ok := scanMinAge(w, req)
	if !ok {
		return
	}

	res := OrphansResponse{
		Slabs:  []OrphanedSlab{},
		MinAge: minAge.Seconds(),
	}

	var scanned int
	for wg, c := range conns {
		orphans, err := c.OrphanedSlabs(req.Context(), minAge)
		if err != nil {
			log.Printf("failed to scan share %s of workgroup %s for orphaned slabs: %v", ps.ByName("name"), wg, err)
			failed[wg] = err.Error()
			continue
		}
		scanned++
		for _, orphan := range orphans {
			res.Slabs = append(res.Slabs, OrphanedSlab{
				Workgroup: wg,
				Key:       orphan.Key,
				Size:      orphan.Size,
				PinnedAt:  orphan.PinnedAt,
			})
			res.Count++
			res.Size += orphan.Size
		}
	}

	// A connection that could not be reached is reported alongside what the
	// others found, so that the count is never mistaken for the whole share.
	// Only a scan that saw nothing at all fails.
	if scanned == 0 {
		writeError(w, "failed to scan the share for orphaned slabs", http.StatusInternalServerError)
		return
	}
	if len(failed) > 0 {
		res.Errors = failed
	}

	// Oldest pin first across all the workgroups, so that the listing reads
	// the same way it does per connection.
	sort.Slice(res.Slabs, func(i, j int) bool {
		if !res.Slabs[i].PinnedAt.Equal(res.Slabs[j].PinnedAt) {
			return res.Slabs[i].PinnedAt.Before(res.Slabs[j].PinnedAt)
		}
		return bytes.Compare(res.Slabs[i].Key[:], res.Slabs[j].Key[:]) < 0
	})

	writeJSON(w, res)
}

// orphansHandlerDELETE handles the DELETE /share/:name/orphans calls. Each
// connection re-runs the scan and unpins what it finds, so what is dropped is
// what is orphaned at that moment rather than what a previous scan reported.
func (api *API) orphansHandlerDELETE(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	conns, failed, ok := api.scannableShare(w, ps.ByName("name"))
	if !ok {
		return
	}
	minAge, ok := scanMinAge(w, req)
	if !ok {
		return
	}

	var res UnpinOrphansResponse

	var ran int
	for wg, c := range conns {
		unpinned, err := c.UnpinOrphanedSlabs(req.Context(), minAge)
		if err != nil {
			log.Printf("failed to unpin the orphaned slabs of share %s of workgroup %s: %v", ps.ByName("name"), wg, err)
			failed[wg] = err.Error()
		} else {
			ran++
		}

		// A run that failed part of the way through still dropped what it got
		// to, so its counts are reported either way.
		res.Unpinned += unpinned.Unpinned
		res.Freed += unpinned.Freed
		res.Failed += unpinned.Failed
	}

	// Unpinning is a change, so a run that got part of the way through is
	// reported rather than hidden behind an error: those slabs are gone, and
	// the caller has to know. Only a run that changed nothing at all fails.
	if ran == 0 && res.Unpinned == 0 {
		writeError(w, "failed to unpin the orphaned slabs", http.StatusInternalServerError)
		return
	}
	if len(failed) > 0 {
		res.Errors = failed
	}

	writeJSON(w, res)
}

// fragmentationHandlerGET handles the GET /share/:name/fragmentation calls. It
// reports the dead space that editing and deleting files has left behind in the
// share's slabs, without changing anything.
func (api *API) fragmentationHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	conns, failed, ok := api.scannableShare(w, ps.ByName("name"))
	if !ok {
		return
	}
	threshold, ok := fragmentationThreshold(w, req)
	if !ok {
		return
	}

	res := FragmentationResponse{
		Slabs:     []FragmentedSlab{},
		Threshold: threshold,
	}

	var checked int
	for wg, c := range conns {
		report, err := c.Fragmentation(req.Context(), threshold)
		if err != nil {
			log.Printf("failed to check share %s of workgroup %s for fragmentation: %v", ps.ByName("name"), wg, err)
			failed[wg] = err.Error()
			continue
		}
		checked++

		// Left out of the request, the threshold is whatever the connections
		// are configured with, which is the same for all of them.
		res.Threshold = report.Threshold
		res.Total += report.Stats.Slabs
		res.Wasted += report.Stats.Wasted
		res.Fragmented += report.Stats.Fragmented
		res.FragmentedWasted += report.Stats.FragmentedWasted

		for _, slab := range report.Slabs {
			res.Slabs = append(res.Slabs, FragmentedSlab{
				Workgroup:     wg,
				Key:           slab.Key,
				Size:          slab.Size,
				Filled:        slab.Filled,
				Used:          slab.Used,
				Wasted:        slab.Wasted(),
				Pieces:        slab.Pieces,
				Fragmentation: slab.Fragmentation(),
			})
		}
	}

	// A connection that could not be checked is reported alongside what the
	// others found, so that the counts are never mistaken for the whole share.
	// Only a check that saw nothing at all fails.
	if checked == 0 {
		writeError(w, "failed to check the share for fragmentation", http.StatusInternalServerError)
		return
	}
	if len(failed) > 0 {
		res.Errors = failed
	}

	// Most fragmented first across all the workgroups, so that the listing
	// reads the same way it does per connection.
	sort.Slice(res.Slabs, func(i, j int) bool {
		if res.Slabs[i].Wasted != res.Slabs[j].Wasted {
			return res.Slabs[i].Wasted > res.Slabs[j].Wasted
		}
		return bytes.Compare(res.Slabs[i].Key[:], res.Slabs[j].Key[:]) < 0
	})

	writeJSON(w, res)
}

// fragmentationHandlerPOST handles the POST /share/:name/fragmentation calls.
// Each connection repacks the slabs it finds fragmented at that moment, rather
// than what a previous check reported, and does one round of it: as many slabs
// as it takes to free one, or none where that cannot be reached.
func (api *API) fragmentationHandlerPOST(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	conns, failed, ok := api.scannableShare(w, ps.ByName("name"))
	if !ok {
		return
	}

	var res DefragmentResponse

	var ran int
	for wg, c := range conns {
		report, err := c.Defragment(req.Context())
		if err != nil {
			log.Printf("failed to defragment share %s of workgroup %s: %v", ps.ByName("name"), wg, err)
			failed[wg] = err.Error()
		} else {
			ran++
		}

		// A round that failed part of the way through still moved what it got
		// to, so its counts are reported either way.
		res.Slabs += report.Slabs
		res.Moved += report.Moved
		res.Reclaimed += report.Reclaimed
	}

	// Repacking is a change, so a round that got part of the way through is
	// reported rather than hidden behind an error. Only one that moved nothing
	// at all fails.
	if ran == 0 && res.Slabs == 0 {
		writeError(w, "failed to defragment the share", http.StatusInternalServerError)
		return
	}
	if len(failed) > 0 {
		res.Errors = failed
	}

	writeJSON(w, res)
}

// policyHandlerGET handles the GET /share/:name/policy calls.
func (api *API) policyHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
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

// resolveConnection looks up the workgroup and the share the :workgroup and
// :share parameters name, the pair every /connect call is about.
// On failure it writes the appropriate error response and returns false.
func (api *API) resolveConnection(w http.ResponseWriter, ps httprouter.Params) (stores.Workgroup, stores.Share, bool) {
	wg, ok := api.resolveWorkgroup(w, ps.ByName("workgroup"))
	if !ok {
		return stores.Workgroup{}, stores.Share{}, false
	}

	share, err := api.store.GetShare(strings.ToLower(ps.ByName("share")))
	if err != nil {
		log.Printf("failed to find share: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return stores.Workgroup{}, stores.Share{}, false
	}
	if share.Name == "" {
		writeError(w, "share not found", http.StatusNotFound)
		return stores.Workgroup{}, stores.Share{}, false
	}

	return wg, share, true
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
	name := stores.NormalizeWorkgroupName(body.Name)
	wg := stores.Workgroup{UUID: u, Name: name}
	if err := api.store.AddWorkgroup(wg); err != nil {
		log.Printf("failed to add workgroup: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("created new workgroup: %s", u)
	writeJSON(w, WorkgroupResponse{UUID: u, Name: name})
}

// workgroupsHandlerGET handles the GET /workgroups calls.
func (api *API) workgroupsHandlerGET(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
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
	var stats ServerStats
	if api.server != nil {
		stats = api.server.Stats()
	}

	writeJSON(w, stats)
}

// settingsHandlerGET handles the GET /settings calls.
func (api *API) settingsHandlerGET(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	writeJSON(w, SettingsResponse{
		Mode:      api.cfg.Mode.String(),
		Anonymous: api.cfg.Anonymous,
	})
}

// connectHandlerGET handles the GET /connect/:workgroup/:share calls. It reports
// the phase a connection attempt is in, and, once there is none left to report,
// whether the workgroup is connected to the share.
// :workgroup may be a UUID or a workgroup name.
func (api *API) connectHandlerGET(w http.ResponseWriter, _ *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
		return
	}

	if a := api.connects.get(connectKey(wg, share)); a != nil {
		writeJSON(w, a.status())
		return
	}

	connected, _, err := api.store.IsConnected(wg, share)
	if err != nil {
		log.Printf("failed to check the connection: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	state := ConnectIdle
	if connected {
		state = ConnectConnected
	}
	writeJSON(w, ConnectStatusResponse{State: state})
}

// connectHandlerPOST handles the POST /connect/:workgroup/:share calls.
// It initiates an indexd connection-approval flow by sending a registration request
// to the indexer and returning the URL the admin must visit to approve it. The
// attempt then runs on its own: approving it is what carries it through to a
// connection, and GET /connect/:workgroup/:share is what follows it there.
// :workgroup may be a UUID or a workgroup name.
func (api *API) connectHandlerPOST(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
		return
	}
	if share.Type != "indexd" {
		writeError(w, "connection requests are only supported for indexd shares", http.StatusBadRequest)
		return
	}

	// The attempt is tracked before the indexer is asked for anything, so that a
	// second request does not send the admin a link that the first one's approval
	// would leave behind unapproved.
	attempt, started := api.connects.begin(connectKey(wg, share), ConnectAwaitingApproval)
	if !started {
		writeError(w, "this workgroup is already connecting to this share", http.StatusConflict)
		return
	}

	builder := sdk.NewBuilder(share.ServerName, sdk.AppMetadata{
		ID:          types.HashBytes(append([]byte(api.cfg.Indexd.Name), []byte(api.cfg.Indexd.Description)...)),
		Name:        api.cfg.Indexd.Name,
		Description: api.cfg.Indexd.Description,
		LogoURL:     api.cfg.Indexd.LogoURL,
		ServiceURL:  api.cfg.Indexd.ServiceURL,
	})

	approvalURL, err := builder.RequestConnection(req.Context())
	if err != nil {
		api.connects.drop(connectKey(wg, share))
		log.Printf("failed to request connection: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The phase reported is the one the attempt starts in, taken before it is set
	// running: it is not this call's place to say how far it got in the meantime.
	attempt.requested(approvalURL)
	res := attempt.status()
	go api.runConnect(attempt, builder, wg, share, nil)

	writeJSONStatus(w, http.StatusAccepted, res)
}

// runConnect carries a connection attempt through to its end. Where a builder is
// given, it waits for the admin to approve the request with the indexer and
// registers the app key that comes of it; the connection itself is made either
// way. It runs in the background, and the phases it moves through are what
// GET /connect/:workgroup/:share reports.
func (api *API) runConnect(attempt *connectAttempt, builder *sdk.Builder, wg stores.Workgroup, share stores.Share, appKey types.PrivateKey) {
	key := connectKey(wg, share)
	defer func() { go api.connects.forget(api.ctx, key, attempt) }()

	// Only a key this call derives is the caller's to be told about: one it
	// passed in is one it already has.
	var derived types.PrivateKey

	if builder != nil {
		ctx, cancel := context.WithTimeout(api.ctx, connectApprovalTimeout)
		defer cancel()

		if err := builder.WaitForApproval(ctx); err != nil {
			log.Printf("connection approval failed: %v", err)
			attempt.fail("connection not approved: " + err.Error())
			return
		}

		attempt.advance(ConnectRegistering)
		sdkInst, err := builder.Register(api.ctx, api.cfg.Indexd.SeedPhrase)
		if err != nil {
			log.Printf("failed to register app: %v", err)
			attempt.fail("failed to register the app key with the indexer")
			return
		}
		appKey = sdkInst.AppKey()
		derived = appKey

		// Registering built an authorized SDK with its host connections warmed up
		// already, so the connection below is offered that one rather than left to
		// build a second one and warm the same hosts again.
		if api.server != nil {
			api.server.OfferSDK(wg, share, sdkInst)
			defer api.server.DiscardSDK(wg, share)
		} else if err := sdkInst.Close(); err != nil {
			log.Printf("failed to close the registered SDK: %v", err)
		}
	}

	attempt.advance(ConnectConnecting)
	if err := api.store.AddConnection(wg, share, appKey); err != nil {
		log.Printf("failed to add connection: %v", err)
		attempt.fail("failed to connect the share")
		return
	}

	attempt.finish(derived)
}

// connectHandlerPUT handles the PUT /connect/:workgroup/:share calls. Two paths:
//  1. No body, renterd share — no key required.
//  2. Body with appKey (hex) — indexd, reconnecting with an existing key.
//
// A first-time indexd connection has no PUT of its own: it is the approval of
// the request POST /connect/:workgroup/:share makes that carries it through.
// The connection is made in the background, and the response is the phase it
// starts in; GET /connect/:workgroup/:share reports the rest.
//
// :workgroup may be a UUID or a workgroup name.
func (api *API) connectHandlerPUT(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
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
		writeError(w, "an indexd share is connected for the first time by approving a request from POST /connect/:workgroup/:share, and reconnected with the app key that made it", http.StatusBadRequest)
		return
	}

	attempt, started := api.connects.begin(connectKey(wg, share), ConnectConnecting)
	if !started {
		writeError(w, "this workgroup is already connecting to this share", http.StatusConflict)
		return
	}
	res := attempt.status()
	go api.runConnect(attempt, nil, wg, share, appKey)

	writeJSONStatus(w, http.StatusAccepted, res)
}

// connectHandlerDELETE handles the DELETE /connect/:workgroup/:share calls.
// :workgroup may be a UUID or a workgroup name.
func (api *API) connectHandlerDELETE(w http.ResponseWriter, _ *http.Request, ps httprouter.Params) {
	wg, share, ok := api.resolveConnection(w, ps)
	if !ok {
		return
	}

	api.connects.drop(connectKey(wg, share))
	if err := api.store.RemoveConnection(wg, share); err != nil {
		log.Printf("failed to remove connection: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}
