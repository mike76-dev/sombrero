package files

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// The editor speaks WOPI, the Web Application Open Platform Interface: it
// fetches a file from here, lets the browsers edit it, and sends it back on
// every save, naming the file by an access token this server issued.
const (
	// discoveryTTL is how long the editor's discovery document is trusted
	// before it is fetched again.
	discoveryTTL = 10 * time.Minute

	// wopiLockTTL is how long the editor's lock on a file lasts unless it is
	// refreshed, as the protocol sets it.
	wopiLockTTL = 30 * time.Minute

	// wopiBody bounds a save; a document larger than this is not one the
	// editor should be fed in the first place.
	wopiBody = 1 << 30
)

// editor is the Collabora Online server documents are edited in: where the
// browsers reach it, where it reaches this server, and what it has said it
// opens.
type editor struct {
	collabora *url.URL
	public    *url.URL

	mu      sync.Mutex
	actions map[string]string
	fetched time.Time
}

// SetEditor names the Collabora Online server, as the browsers reach it, and
// this server as Collabora reaches it, which may be left empty for the address
// the browser used.
func (h *Handler) SetEditor(collabora, public string) error {
	c, err := url.Parse(collabora)
	if err != nil || c.Scheme == "" || c.Host == "" {
		return fmt.Errorf("the editor address %q is not a URL", collabora)
	}
	e := &editor{collabora: c}
	if public != "" {
		p, err := url.Parse(public)
		if err != nil || p.Scheme == "" || p.Host == "" {
			return fmt.Errorf("the server address %q is not a URL", public)
		}
		e.public = p
	}
	h.editor = e
	return nil
}

// discovery is the editor's discovery document, as far as it is read: the
// applications it offers, each with the actions it takes on file extensions.
type discovery struct {
	Zones []struct {
		Apps []struct {
			Actions []struct {
				Name   string `xml:"name,attr"`
				Ext    string `xml:"ext,attr"`
				URLSrc string `xml:"urlsrc,attr"`
			} `xml:"action"`
		} `xml:"app"`
	} `xml:"net-zone"`
}

// action returns the editor's URL for the action on files of the extension,
// from the discovery document, fetched anew when it has aged.
func (e *editor) action(name, ext string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.actions == nil || time.Since(e.fetched) > discoveryTTL {
		actions, err := e.fetch()
		if err != nil {
			return "", err
		}
		e.actions, e.fetched = actions, time.Now()
	}
	return e.actions[name+" "+strings.ToLower(ext)], nil
}

// fetch reads the discovery document.
func (e *editor) fetch() (map[string]string, error) {
	c := &http.Client{Timeout: 15 * time.Second}
	res, err := c.Get(e.collabora.JoinPath("hosting", "discovery").String())
	if err != nil {
		return nil, fmt.Errorf("failed to reach the editor: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the editor answered the discovery with %s", res.Status)
	}

	var d discovery
	if err := xml.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&d); err != nil {
		return nil, fmt.Errorf("failed to read the discovery document: %w", err)
	}
	actions := make(map[string]string)
	for _, z := range d.Zones {
		for _, a := range z.Apps {
			for _, act := range a.Actions {
				if act.Ext != "" && act.URLSrc != "" {
					actions[act.Name+" "+strings.ToLower(act.Ext)] = act.URLSrc
				}
			}
		}
	}
	return actions, nil
}

// wopiToken is what an access token stands for: an account's hold on one
// file, with or without the right to write it, until the session it was
// issued from runs out.
type wopiToken struct {
	account   stores.Account
	workgroup stores.Workgroup
	share     stores.Share
	path      string
	write     bool
	expires   time.Time
}

// wopiLock is the editor's lock on a file.
type wopiLock struct {
	lock    string
	expires time.Time
}

// wopiFileID names a file to the editor: the share and the path, encoded so
// that they survive a URL.
func wopiFileID(share, p string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(share + "\x00" + p))
}

// EditSession is the response of GET /share/:name/edit: where the browser
// loads the editor from, the access token it posts there, and when the token
// expires, in milliseconds since the epoch, as the editor expects it.
type EditSession struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	TTL   int64  `json:"ttl"`
}

// editHandler handles the GET /share/:name/edit calls: it issues an access
// token on the file the path names and says where the editor is loaded from,
// to edit where the account may write and to view otherwise.
func (h *Handler) editHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	if h.editor == nil {
		writeError(w, "no editor is configured", http.StatusNotFound)
		return
	}
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	p, ok := cleanPath(req.URL.Query().Get("path"))
	if !ok || p == "/" {
		writeError(w, "invalid path", http.StatusBadRequest)
		return
	}

	c, _, err := h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, s.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := req.Context()
	key := strings.TrimPrefix(p, "/")
	oi, err := c.Object(ctx, s.account, key)
	if errors.Is(err, stores.ErrNotFound) {
		writeError(w, "file not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("file API: failed to look up %s on %s: %v", p, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if strings.HasSuffix(oi.Key, "/") {
		writeError(w, "not a file", http.StatusBadRequest)
		return
	}

	write := r.Write
	if ac, ok := c.(client.AccessChecker); ok && write {
		pa, err := ac.PathAccess(ctx, s.account, key)
		if err != nil {
			log.Printf("file API: failed to check %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		write = pa.Write
	}

	name := "edit"
	if !write {
		name = "view"
	}
	ext := strings.TrimPrefix(path.Ext(p), ".")
	urlsrc, err := h.editor.action(name, ext)
	if err != nil {
		log.Printf("file API: %v", err)
		writeError(w, "the editor is not available at the moment", http.StatusBadGateway)
		return
	}
	if urlsrc == "" {
		writeError(w, "this kind of file is not edited in the browser", http.StatusBadRequest)
		return
	}

	token := rand.Text()
	h.mu.Lock()
	for t, wt := range h.wopiTokens {
		if !h.now().Before(wt.expires) {
			delete(h.wopiTokens, t)
		}
	}
	h.wopiTokens[token] = wopiToken{account: s.account, workgroup: s.workgroup, share: share, path: p, write: write, expires: s.expires}
	h.mu.Unlock()

	src := h.publicBase(req).JoinPath("files", "wopi", "files", wopiFileID(share.Name, p)).String()
	sep := "?"
	if strings.Contains(urlsrc, "?") {
		sep = "&"
		if strings.HasSuffix(urlsrc, "?") || strings.HasSuffix(urlsrc, "&") {
			sep = ""
		}
	}
	writeJSON(w, EditSession{URL: urlsrc + sep + "WOPISrc=" + url.QueryEscape(src), Token: token, TTL: s.expires.UnixMilli()})
}

// publicBase is the address the editor reaches this server at: the one
// configured, or the one the browser used.
func (h *Handler) publicBase(req *http.Request) *url.URL {
	if h.editor.public != nil {
		return h.editor.public
	}
	scheme := "http"
	if req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return &url.URL{Scheme: scheme, Host: req.Host}
}

// withToken wraps a WOPI call: the access token names the file, and the file
// in the URL has to be the one the token is on.
func (h *Handler) withToken(next func(http.ResponseWriter, *http.Request, httprouter.Params, wopiToken)) httprouter.Handle {
	return func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		h.mu.Lock()
		t, ok := h.wopiTokens[req.URL.Query().Get("access_token")]
		h.mu.Unlock()
		if !ok || !h.now().Before(t.expires) {
			writeError(w, "invalid or expired access token", http.StatusUnauthorized)
			return
		}
		if ps.ByName("id") != wopiFileID(t.share.Name, t.path) {
			writeError(w, "file not found", http.StatusNotFound)
			return
		}
		next(w, req, ps, t)
	}
}

// wopiClient reaches the share of the token and looks the file up.
func (h *Handler) wopiClient(w http.ResponseWriter, req *http.Request, t wopiToken) (client.Client, uint64, client.ObjectInfo, bool) {
	c, partSize, err := h.server.ShareClient(t.share, t.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", t.share.Name, t.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return nil, 0, client.ObjectInfo{}, false
	}
	oi, err := c.Object(req.Context(), t.account, strings.TrimPrefix(t.path, "/"))
	if errors.Is(err, stores.ErrNotFound) {
		writeError(w, "file not found", http.StatusNotFound)
		return nil, 0, client.ObjectInfo{}, false
	} else if err != nil {
		log.Printf("file API: failed to look up %s on %s: %v", t.path, t.share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return nil, 0, client.ObjectInfo{}, false
	}
	return c, partSize, oi, true
}

// FileInfo is the response of the editor's CheckFileInfo call, in the
// protocol's own spelling.
type FileInfo struct {
	BaseFileName            string `json:"BaseFileName"`
	Size                    uint64 `json:"Size"`
	OwnerId                 string `json:"OwnerId"`
	UserId                  string `json:"UserId"`
	UserFriendlyName        string `json:"UserFriendlyName"`
	UserCanWrite            bool   `json:"UserCanWrite"`
	UserCanNotWriteRelative bool   `json:"UserCanNotWriteRelative"`
	SupportsLocks           bool   `json:"SupportsLocks"`
	SupportsUpdate          bool   `json:"SupportsUpdate"`
	LastModifiedTime        string `json:"LastModifiedTime"`
	PostMessageOrigin       string `json:"PostMessageOrigin"`
}

// wopiTime is the time as the editor is told it, and told it back.
func wopiTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// checkFileInfoHandler handles GET /wopi/files/:id: the file and what the
// account may do with it.
func (h *Handler) checkFileInfoHandler(w http.ResponseWriter, req *http.Request, _ httprouter.Params, t wopiToken) {
	_, _, oi, ok := h.wopiClient(w, req, t)
	if !ok {
		return
	}
	user := t.workgroup.UUID.String() + "/" + t.account.Username
	writeJSON(w, FileInfo{
		BaseFileName:            path.Base(t.path),
		Size:                    oi.Size,
		OwnerId:                 user,
		UserId:                  user,
		UserFriendlyName:        t.account.Username,
		UserCanWrite:            t.write,
		UserCanNotWriteRelative: true,
		SupportsLocks:           true,
		SupportsUpdate:          true,
		LastModifiedTime:        wopiTime(oi.ModifiedAt),
		PostMessageOrigin:       h.publicBase(req).Scheme + "://" + h.publicBase(req).Host,
	})
}

// getFileHandler handles GET /wopi/files/:id/contents: the file itself.
func (h *Handler) getFileHandler(w http.ResponseWriter, req *http.Request, _ httprouter.Params, t wopiToken) {
	c, partSize, oi, ok := h.wopiClient(w, req, t)
	if !ok {
		return
	}
	key := strings.TrimPrefix(t.path, "/")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rd := &objectReader{
		fetch: func(offset, length uint64, w io.Writer) error {
			return c.Read(req.Context(), t.account, key, offset, length, w)
		},
		path:  key,
		size:  oi.Size,
		chunk: h.chunkSize(partSize),
	}
	http.ServeContent(w, req, "", oi.ModifiedAt, rd)
}

// putFileHandler handles POST /wopi/files/:id/contents: a save. It is refused
// where the account may not write, where another editor holds the lock, and
// where the file changed since the editor last read it, which the editor is
// told so that it asks the user what to do.
func (h *Handler) putFileHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, t wopiToken) {
	if !t.write {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	if held, lock := h.lockHeldByAnother(ps.ByName("id"), req.Header.Get("X-WOPI-Lock")); held {
		w.Header().Set("X-WOPI-Lock", lock)
		writeError(w, "the file is locked", http.StatusConflict)
		return
	}
	c, partSize, oi, ok := h.wopiClient(w, req, t)
	if !ok {
		return
	}
	if stamp := req.Header.Get("X-COOL-WOPI-Timestamp"); stamp != "" {
		known, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || !known.Equal(oi.ModifiedAt) {
			writeJSONStatus(w, http.StatusConflict, map[string]int{"COOLStatusCode": 1010})
			return
		}
	}

	key := strings.TrimPrefix(t.path, "/")
	if _, failure := h.storeBody(req.Context(), c, t.account, t.share.Name, key, partSize, http.MaxBytesReader(w, req.Body, wopiBody)); failure != nil {
		writeError(w, failure.message, failure.code)
		return
	}
	modified := h.now()
	if oi, err := c.Object(req.Context(), t.account, key); err == nil {
		modified = oi.ModifiedAt
	}
	writeJSON(w, map[string]string{"LastModifiedTime": wopiTime(modified)})
}

// lockHeldByAnother reports whether the file is locked with a lock other than
// the one given, and the lock it is held with.
func (h *Handler) lockHeldByAnother(id, lock string) (bool, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.wopiLocks[id]
	if !ok || !h.now().Before(l.expires) {
		delete(h.wopiLocks, id)
		return false, ""
	}
	return l.lock != lock, l.lock
}

// lockHandler handles POST /wopi/files/:id, which carries the lock operations
// in the X-WOPI-Override header. A lock is held for half an hour unless it is
// refreshed, and a lock in the way is reported in the X-WOPI-Lock header.
func (h *Handler) lockHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, t wopiToken) {
	id := ps.ByName("id")
	lock := req.Header.Get("X-WOPI-Lock")
	op := req.Header.Get("X-WOPI-Override")

	h.mu.Lock()
	defer h.mu.Unlock()
	held, ok := h.wopiLocks[id]
	if ok && !h.now().Before(held.expires) {
		delete(h.wopiLocks, id)
		ok = false
	}

	switch op {
	case "GET_LOCK":
		if ok {
			w.Header().Set("X-WOPI-Lock", held.lock)
		} else {
			w.Header().Set("X-WOPI-Lock", "")
		}
		w.WriteHeader(http.StatusOK)
	case "LOCK", "REFRESH_LOCK":
		if lock == "" {
			writeError(w, "a lock is required", http.StatusBadRequest)
			return
		}
		if !t.write {
			writeError(w, "access denied", http.StatusForbidden)
			return
		}
		if ok && held.lock != lock {
			w.Header().Set("X-WOPI-Lock", held.lock)
			writeError(w, "the file is locked", http.StatusConflict)
			return
		}
		if op == "REFRESH_LOCK" && !ok {
			w.Header().Set("X-WOPI-Lock", "")
			writeError(w, "the file is not locked", http.StatusConflict)
			return
		}
		h.wopiLocks[id] = wopiLock{lock: lock, expires: h.now().Add(wopiLockTTL)}
		w.WriteHeader(http.StatusOK)
	case "UNLOCK":
		if lock == "" {
			writeError(w, "a lock is required", http.StatusBadRequest)
			return
		}
		if !ok || held.lock != lock {
			if ok {
				w.Header().Set("X-WOPI-Lock", held.lock)
			} else {
				w.Header().Set("X-WOPI-Lock", "")
			}
			writeError(w, "the lock does not match", http.StatusConflict)
			return
		}
		delete(h.wopiLocks, id)
		w.WriteHeader(http.StatusOK)
	default:
		writeError(w, "the operation is not supported", http.StatusNotImplemented)
	}
}
