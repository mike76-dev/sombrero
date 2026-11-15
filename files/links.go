package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
)

// maxLinkBody bounds the request to make a link, which holds a time at most.
const maxLinkBody = 4 << 10

// LinkEntry is a sharing link as the API shows it: the address whoever holds
// it reads the file at, who made it and when, and when it expires, if ever.
type LinkEntry struct {
	Token     string     `json:"token"`
	URL       string     `json:"url"`
	Path      string     `json:"path"`
	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// LinkRequest is the body of POST /share/:name/link: when the link expires,
// or nothing for one that lasts.
type LinkRequest struct {
	ExpiresAt *time.Time `json:"expiresAt"`
}

// linkEntry is the link as the API shows it, at the address the request came to.
func (h *Handler) linkEntry(req *http.Request, l stores.Link) LinkEntry {
	e := LinkEntry{
		Token:     l.Token,
		URL:       h.publicBase(req).JoinPath("files", "link", l.Token).String(),
		Path:      l.Path,
		CreatedBy: l.Account.Username,
		CreatedAt: l.CreatedAt.UTC(),
	}
	if !l.ExpiresAt.IsZero() {
		at := l.ExpiresAt.UTC()
		e.ExpiresAt = &at
	}
	return e
}

// createLinkHandler handles the POST /share/:name/link calls: a link to the
// file the path query names, read through the account's own access.
func (h *Handler) createLinkHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	f, ok := h.fileOf(w, req, ps, s, Rights{})
	if !ok {
		return
	}
	if f.gone {
		writeError(w, "file not found", http.StatusNotFound)
		return
	}
	var lr LinkRequest
	if req.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxLinkBody)).Decode(&lr); err != nil {
			writeError(w, "invalid request", http.StatusBadRequest)
			return
		}
	}
	var expires time.Time
	if lr.ExpiresAt != nil {
		if !lr.ExpiresAt.After(h.now()) {
			writeError(w, "the expiry is in the past", http.StatusBadRequest)
			return
		}
		expires = *lr.ExpiresAt
	}

	l, err := h.store.CreateLink(f.share.Name, f.path, s.account, expires)
	if err != nil {
		log.Printf("file API: failed to create a link to %s on %s: %v", f.path, f.share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusCreated, h.linkEntry(req, l))
}

// linksHandler handles the GET /share/:name/links calls: the links to the file
// the path query names that the accounts of the workgroup made, newest first.
func (h *Handler) linksHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	f, ok := h.fileOf(w, req, ps, s, Rights{})
	if !ok {
		return
	}
	links, err := h.store.Links(f.share.Name, f.path, s.account)
	if err != nil {
		log.Printf("file API: failed to list the links to %s on %s: %v", f.path, f.share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	entries := make([]LinkEntry, 0, len(links))
	for _, l := range links {
		entries = append(entries, h.linkEntry(req, l))
	}
	writeJSON(w, entries)
}

// deleteLinkHandler handles the DELETE /share/:name/link calls: the link with
// the token query goes, where an account of the workgroup made it.
func (h *Handler) deleteLinkHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	token := req.URL.Query().Get("token")
	l, err := h.store.Link(token)
	if err == nil && l.Share == share.Name {
		err = h.store.DeleteLink(token, s.account)
	} else if err == nil {
		err = stores.ErrNotFound
	}
	if errors.Is(err, stores.ErrNotFound) {
		writeError(w, "link not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("file API: failed to delete a link on %s: %v", share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkHandler handles the GET and HEAD /link/:token calls, which need no
// session: the file is read as the account that made the link, while the
// link lasts and the account may read the file. The file is shown unless the
// download query asks for it to be saved.
func (h *Handler) linkHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	l, err := h.store.Link(ps.ByName("token"))
	if err == nil && !l.ExpiresAt.IsZero() && !h.now().Before(l.ExpiresAt) {
		err = stores.ErrNotFound
	}
	if errors.Is(err, stores.ErrNotFound) {
		writeError(w, "link not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("file API: failed to find a link: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	share, err := h.store.GetShare(l.Share)
	if err != nil {
		log.Printf("file API: failed to find share %s: %v", l.Share, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	r := Rights{}
	if share.Name != "" {
		if r, err = h.rightsOn(l.Account, share); err != nil {
			log.Printf("file API: failed to read the policy: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if !r.Read {
		writeError(w, "link not found", http.StatusNotFound)
		return
	}
	c, partSize, err := h.server.ShareClient(share, l.Account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, l.Account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return
	}

	ctx := req.Context()
	key := strings.TrimPrefix(l.Path, "/")
	oi, err := c.Object(ctx, l.Account, key)
	if errors.Is(err, stores.ErrNotFound) || err == nil && strings.HasSuffix(oi.Key, "/") {
		writeError(w, "file not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("file API: failed to look up %s on %s: %v", l.Path, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	contentHeaders(w, path.Base(l.Path), req.URL.Query().Get("download") == "")
	rd := h.newReader(fileKey{share: share.Name, key: key, size: oi.Size, modified: oi.ModifiedAt}, partSize,
		func(ctx context.Context, offset, length uint64, w io.Writer) error {
			return c.Read(ctx, l.Account, key, offset, length, w)
		})
	http.ServeContent(w, req, "", oi.ModifiedAt, rd)
}
