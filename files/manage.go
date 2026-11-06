package files

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// target is a path on a share a call is about to act on: the client that
// reaches it, what the account may do there, and what is there now, if
// anything.
type target struct {
	client client.Client
	key    string
	access stores.PathAccess
	object client.ObjectInfo
	exists bool
}

// isDir reports whether what is at the path is a folder.
func (t target) isDir() bool {
	return t.exists && strings.HasSuffix(t.object.Key, "/")
}

// resolve finds what the path names on the share and what the account may do
// there, answering the call itself where the path cannot be acted on. A
// backend without folder rights grants everything the share's policy does.
func (h *Handler) resolve(w http.ResponseWriter, req *http.Request, s session, share stores.Share, c client.Client, p string) (target, bool) {
	t := target{client: c, key: strings.TrimPrefix(p, "/"), access: stores.PathAccess{Write: true, Delete: true}}
	ctx := req.Context()
	if ac, ok := c.(client.AccessChecker); ok {
		pa, err := ac.PathAccess(ctx, s.account, t.key)
		if errors.Is(err, stores.ErrNotFound) {
			writeError(w, "not found", http.StatusNotFound)
			return t, false
		} else if err != nil {
			log.Printf("file API: failed to check %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return t, false
		}
		t.access = pa
	}

	oi, err := c.Object(ctx, s.account, t.key)
	if err == nil {
		t.object, t.exists = oi, true
	} else if !errors.Is(err, stores.ErrNotFound) {
		log.Printf("file API: failed to look up %s on %s: %v", p, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return t, false
	}
	return t, true
}

// folderHandler handles the POST /share/:name/folder calls, making the
// folder the path query names.
func (h *Handler) folderHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Write {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	p, ok := cleanPath(req.FormValue("path"))
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
	t, ok := h.resolve(w, req, s, share, c, p)
	if !ok {
		return
	}
	if t.exists {
		writeError(w, "already exists", http.StatusConflict)
		return
	}
	if !t.access.Write {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}

	ctx := req.Context()
	if err := c.MakeDirectory(ctx, s.account, t.key); err != nil {
		switch {
		case errors.Is(err, stores.ErrDirectoryExists):
			writeError(w, "already exists", http.StatusConflict)
		case errors.Is(err, stores.ErrNotFound):
			writeError(w, "not found", http.StatusNotFound)
		default:
			log.Printf("file API: failed to make folder %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	writeJSONStatus(w, http.StatusCreated, h.entryAt(ctx, c, s, p))
}

// deleteFileHandler handles the DELETE /share/:name/file calls, and
// deleteFolderHandler the DELETE /share/:name/folder calls. A folder goes
// with everything in it only when the call says recursive.
func (h *Handler) deleteFileHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	h.deleteHandler(w, req, ps, s, false)
}

func (h *Handler) deleteFolderHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	h.deleteHandler(w, req, ps, s, true)
}

func (h *Handler) deleteHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session, dir bool) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Delete {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	p, ok := cleanPath(req.FormValue("path"))
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
	t, ok := h.resolve(w, req, s, share, c, p)
	if !ok {
		return
	}
	if !t.exists {
		writeError(w, "not found", http.StatusNotFound)
		return
	}
	if t.isDir() != dir {
		if dir {
			writeError(w, "not a folder", http.StatusBadRequest)
		} else {
			writeError(w, "not a file", http.StatusBadRequest)
		}
		return
	}
	if !t.access.Delete {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}

	ctx := req.Context()
	if dir && req.FormValue("recursive") == "" {
		empty, err := c.IsEmpty(ctx, s.account, t.key+"/")
		if err != nil {
			log.Printf("file API: failed to look into %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !empty {
			writeError(w, "folder not empty", http.StatusConflict)
			return
		}
	}
	if err := c.Delete(ctx, s.account, t.key, dir); err != nil {
		if errors.Is(err, stores.ErrNotFound) {
			writeError(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("file API: failed to delete %s on %s: %v", p, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renameHandler handles the POST /share/:name/rename calls, moving what the
// path query names to the to query, over what is there if the call says
// replace. Moving takes the right to delete the source and to write at the
// destination, as it does over SMB.
func (h *Handler) renameHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Write || !r.Delete {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	from, ok := cleanPath(req.FormValue("path"))
	if !ok || from == "/" {
		writeError(w, "invalid path", http.StatusBadRequest)
		return
	}
	to, ok := cleanPath(req.FormValue("to"))
	if !ok || to == "/" || to == from || strings.HasPrefix(to, from+"/") {
		writeError(w, "invalid destination", http.StatusBadRequest)
		return
	}
	c, _, err := h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, s.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return
	}

	src, ok := h.resolve(w, req, s, share, c, from)
	if !ok {
		return
	}
	if !src.exists {
		writeError(w, "not found", http.StatusNotFound)
		return
	}
	if !src.access.Delete {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	dst, ok := h.resolve(w, req, s, share, c, to)
	if !ok {
		return
	}
	if !dst.access.Write {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	replace := req.FormValue("replace") != ""
	if dst.exists {
		switch {
		case !replace:
			writeError(w, "destination already exists", http.StatusConflict)
			return
		case dst.isDir() != src.isDir():
			writeError(w, "destination is of the other kind", http.StatusBadRequest)
			return
		}
	}

	ctx := req.Context()
	if err := c.Rename(ctx, s.account, src.key, dst.key, src.isDir(), replace); err != nil {
		switch {
		case errors.Is(err, stores.ErrAccessDenied):
			writeError(w, "access denied", http.StatusForbidden)
		case errors.Is(err, stores.ErrNotFound):
			writeError(w, "not found", http.StatusNotFound)
		case errors.Is(err, stores.ErrDirectoryExists):
			writeError(w, "destination already exists", http.StatusConflict)
		default:
			log.Printf("file API: failed to move %s to %s on %s: %v", from, to, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, h.entryAt(ctx, c, s, to))
}

// entryAt describes what is now at the path, from the share where it answers
// and from the path alone where it does not.
func (h *Handler) entryAt(ctx context.Context, c client.Client, s session, p string) Entry {
	e := Entry{Name: path.Base(p), Path: p}
	if oi, err := c.Object(ctx, s.account, strings.TrimPrefix(p, "/")); err == nil {
		e.Dir = strings.HasSuffix(oi.Key, "/")
		e.Size = oi.Size
		e.CreatedAt, e.ModifiedAt = oi.CreatedAt, oi.ModifiedAt
	}
	return e
}
