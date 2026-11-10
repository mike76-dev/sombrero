// Package files serves the file API: the shares as the accounts see them over
// SMB, reachable over HTTP with the same accounts and the same rights.
package files

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// Store is the part of the store the file API reads the accounts and the
// shares from.
type Store interface {
	FindAccount(username, workgroup string) (stores.Account, error)
	FindWorkgroup(u uuid.UUID) (stores.Workgroup, error)
	FindWorkgroupByName(name string) (stores.Workgroup, error)
	GetShare(name string) (stores.Share, error)
	GetShares(acc stores.Account) ([]stores.Share, error)
	GetAccessRights(share stores.Share, acc stores.Account) (stores.AccessRights, error)
}

// Server is as much of the SMB server as the file API needs: the way to the
// storage behind each share.
type Server interface {
	// ShareClient returns the client the workgroup reaches the share through,
	// starting its connection where it is not running, and the size of the
	// parts the share takes an upload in.
	ShareClient(share stores.Share, workgroup string) (client.Client, uint64, error)
}

const (
	// sessionCookie carries the session token between the calls.
	sessionCookie = "sombrero_session"

	// sessionLifetime is how long a login is good for.
	sessionLifetime = 24 * time.Hour

	// maxLoginBody bounds the login request, which holds three short strings.
	maxLoginBody = 4 << 10
)

// LoginRequest is the request body of POST /login. The workgroup goes by its
// name or its UUID, like the domain of an SMB login.
type LoginRequest struct {
	Workgroup string `json:"workgroup"`
	Username  string `json:"username"`
	Password  string `json:"password"`
}

// Identity is who a session belongs to: the response of POST /login and GET /me.
type Identity struct {
	Username  string            `json:"username"`
	Workgroup WorkgroupIdentity `json:"workgroup"`
}

// WorkgroupIdentity names the workgroup of an identity.
type WorkgroupIdentity struct {
	UUID uuid.UUID `json:"uuid"`
	Name string    `json:"name,omitempty"`
}

// session is a login that has not expired yet.
type session struct {
	account   stores.Account
	workgroup stores.Workgroup
	expires   time.Time
}

// Handler serves the file API. It is mounted below a prefix of the server's
// choosing and sees the paths with the prefix taken off.
type Handler struct {
	router *httprouter.Router
	store  Store
	server Server

	mu       sync.Mutex
	sessions map[string]session

	// now is the clock, and chunkSize how much of a file a download fetches
	// at a time for the share's part size; both replaceable by the tests.
	now       func() time.Time
	chunkSize func(partSize uint64) uint64
}

// New returns a file API handler reading the accounts and the shares from the
// store and reaching the shares through the server.
func New(store Store, srv Server) *Handler {
	h := &Handler{
		router:    httprouter.New(),
		store:     store,
		server:    srv,
		sessions:  make(map[string]session),
		now:       time.Now,
		chunkSize: client.ReadChunkSize,
	}
	h.router.POST("/login", h.loginHandler)
	h.router.POST("/logout", h.logoutHandler)
	h.router.GET("/me", h.meHandler)
	h.router.GET("/shares", h.withSession(h.sharesHandler))
	h.router.GET("/share/:name/list", h.withSession(h.listHandler))
	h.router.GET("/share/:name/file", h.withSession(h.fileHandler))
	h.router.HEAD("/share/:name/file", h.withSession(h.fileHandler))
	h.router.PUT("/share/:name/file", h.withSession(h.uploadHandler))
	h.router.DELETE("/share/:name/file", h.withSession(h.deleteFileHandler))
	h.router.POST("/share/:name/folder", h.withSession(h.folderHandler))
	h.router.DELETE("/share/:name/folder", h.withSession(h.deleteFolderHandler))
	h.router.POST("/share/:name/rename", h.withSession(h.renameHandler))
	h.router.GET("/share/:name/snapshots", h.withSession(h.snapshotsHandler))
	h.router.GET("/share/:name/versions", h.withSession(h.versionsHandler))
	h.router.DELETE("/share/:name/versions", h.withSession(h.deleteVersionsHandler))
	h.router.GET("/share/:name/version", h.withSession(h.versionHandler))
	h.router.POST("/share/:name/restore", h.withSession(h.restoreHandler))
	h.router.POST("/share/:name/keep", h.withSession(h.keepHandler))
	h.router.DELETE("/share/:name/keep", h.withSession(h.keepHandler))
	return h
}

// withSession wraps a handler that needs a logged-in account.
func (h *Handler) withSession(next func(http.ResponseWriter, *http.Request, httprouter.Params, session)) httprouter.Handle {
	return func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		s, ok := h.authenticate(req)
		if !ok {
			writeError(w, "not logged in", http.StatusUnauthorized)
			return
		}
		next(w, req, ps, s)
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.router.ServeHTTP(w, req)
}

// loginHandler handles the POST /login calls. Whatever is wrong with the
// credentials, the answer is the same, so that it gives nothing away.
func (h *Handler) loginHandler(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	var lr LoginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxLoginBody)).Decode(&lr); err != nil {
		writeError(w, "malformed request", http.StatusBadRequest)
		return
	}
	if lr.Workgroup == "" || lr.Username == "" {
		writeError(w, "workgroup and username are required", http.StatusBadRequest)
		return
	}

	wg, err := h.resolveWorkgroup(lr.Workgroup)
	if err != nil {
		log.Printf("file API: failed to find workgroup: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if wg.ID == 0 {
		writeError(w, "login failure", http.StatusUnauthorized)
		return
	}

	acc, err := h.store.FindAccount(lr.Username, wg.UUID.String())
	if errors.Is(err, stores.ErrAccountNotFound) {
		writeError(w, "login failure", http.StatusUnauthorized)
		return
	} else if err != nil {
		log.Printf("file API: failed to find account: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !acc.CheckPassword(lr.Password) {
		writeError(w, "login failure", http.StatusUnauthorized)
		return
	}

	token := h.startSession(acc, wg)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, identityOf(acc, wg))
}

// resolveWorkgroup finds the workgroup the login names, by UUID or by name. A
// workgroup that is not there is the zero Workgroup, and so is the anonymous
// one: it has no password to log in with.
func (h *Handler) resolveWorkgroup(name string) (stores.Workgroup, error) {
	if u, err := uuid.Parse(name); err == nil {
		if u == stores.AnonymousWorkgroup {
			return stores.Workgroup{}, nil
		}
		return h.store.FindWorkgroup(u)
	}
	return h.store.FindWorkgroupByName(name)
}

// logoutHandler handles the POST /logout calls.
func (h *Handler) logoutHandler(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	if c, err := req.Cookie(sessionCookie); err == nil {
		h.mu.Lock()
		delete(h.sessions, c.Value)
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true})
	w.WriteHeader(http.StatusNoContent)
}

// meHandler handles the GET /me calls.
func (h *Handler) meHandler(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	s, ok := h.authenticate(req)
	if !ok {
		writeError(w, "not logged in", http.StatusUnauthorized)
		return
	}
	writeJSON(w, identityOf(s.account, s.workgroup))
}

// startSession records a login and returns its token. Sessions that have run
// out are dropped on the way, since nothing else looks at them again.
func (h *Handler) startSession(acc stores.Account, wg stores.Workgroup) string {
	now := h.now()
	token := rand.Text()
	h.mu.Lock()
	defer h.mu.Unlock()
	for t, s := range h.sessions {
		if !now.Before(s.expires) {
			delete(h.sessions, t)
		}
	}
	h.sessions[token] = session{account: acc, workgroup: wg, expires: now.Add(sessionLifetime)}
	return token
}

// authenticate returns the session the request's cookie names, if it is still good.
func (h *Handler) authenticate(req *http.Request) (session, bool) {
	c, err := req.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[c.Value]
	if !ok {
		return session{}, false
	}
	if !h.now().Before(s.expires) {
		delete(h.sessions, c.Value)
		return session{}, false
	}
	return s, true
}

func identityOf(acc stores.Account, wg stores.Workgroup) Identity {
	return Identity{Username: acc.Username, Workgroup: WorkgroupIdentity{UUID: wg.UUID, Name: wg.Name}}
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
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		log.Printf("file API: failed to encode response: %v", err)
	}
}

// writeError writes an error message to the response body, as the API does.
func writeError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(message)
}
