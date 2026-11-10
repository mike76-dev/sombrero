package files

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// VersionEntry is one previous version of a file: the response of GET
// /share/:name/versions lists them newest first.
type VersionEntry struct {
	ID         uint64    `json:"id"`
	Size       uint64    `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
	ReplacedAt time.Time `json:"replacedAt"`
	ReplacedBy string    `json:"replacedBy,omitempty"`
}

// fileRef is the file a call about versions names, resolved on its share.
type fileRef struct {
	share    stores.Share
	client   client.Client
	partSize uint64
	path     string
	key      string
	object   client.ObjectInfo
}

// fileOf resolves the file a call names, answering the call itself where the
// policy does not grant the rights the call needs, or there is no such file.
func (h *Handler) fileOf(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session, need Rights) (fileRef, bool) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return fileRef{}, false
	}
	if !r.Read || need.Write && !r.Write || need.Delete && !r.Delete {
		writeError(w, "access denied", http.StatusForbidden)
		return fileRef{}, false
	}

	p, ok := cleanPath(req.URL.Query().Get("path"))
	if !ok || p == "/" {
		writeError(w, "invalid path", http.StatusBadRequest)
		return fileRef{}, false
	}

	c, partSize, err := h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, s.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return fileRef{}, false
	}

	key := strings.TrimPrefix(p, "/")
	oi, err := c.Object(req.Context(), s.account, key)
	if errors.Is(err, stores.ErrNotFound) {
		// A deleted file lives on in its versions, where the share keeps them.
		if v, kept := c.(client.Versioner); kept {
			versions, verr := v.Versions(req.Context(), s.account, key)
			switch {
			case verr != nil && !errors.Is(verr, stores.ErrNotFound):
				err = verr
			case verr == nil && len(versions) > 0:
				oi, err = client.ObjectInfo{Key: p}, nil
			}
		}
	}
	if errors.Is(err, stores.ErrNotFound) {
		writeError(w, "file not found", http.StatusNotFound)
		return fileRef{}, false
	} else if err != nil {
		log.Printf("file API: failed to look up %s on %s: %v", p, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return fileRef{}, false
	}
	if strings.HasSuffix(oi.Key, "/") {
		writeError(w, "not a file", http.StatusBadRequest)
		return fileRef{}, false
	}
	return fileRef{share: share, client: c, partSize: partSize, path: p, key: key, object: oi}, true
}

// versionID reads the id query of a call, zero where there is none.
func versionID(w http.ResponseWriter, req *http.Request) (uint64, bool) {
	q := req.URL.Query().Get("id")
	if q == "" {
		return 0, true
	}
	id, err := strconv.ParseUint(q, 10, 64)
	if err != nil || id == 0 {
		writeError(w, "invalid version", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// snapshotTime reads the at query of a call, an RFC 3339 time, zero where
// there is none.
func snapshotTime(w http.ResponseWriter, req *http.Request) (time.Time, bool) {
	q := req.URL.Query().Get("at")
	if q == "" {
		return time.Time{}, true
	}
	at, err := time.Parse(time.RFC3339, q)
	if err != nil {
		writeError(w, "invalid time", http.StatusBadRequest)
		return time.Time{}, false
	}
	return at, true
}

// versionNamed finds the version a call names, by the id query or as the copy
// of the file that stood at the time the at query names, answering the call
// itself where there is none. A zero ID is the file as it is now.
func (h *Handler) versionNamed(w http.ResponseWriter, req *http.Request, s session, f fileRef) (client.VersionInfo, bool) {
	id, ok := versionID(w, req)
	if !ok {
		return client.VersionInfo{}, false
	}
	at, ok := snapshotTime(w, req)
	if !ok {
		return client.VersionInfo{}, false
	}
	if id == 0 && at.IsZero() {
		writeError(w, "invalid version", http.StatusBadRequest)
		return client.VersionInfo{}, false
	}

	versioner, kept := f.client.(client.Versioner)
	if !kept {
		writeError(w, "version not found", http.StatusNotFound)
		return client.VersionInfo{}, false
	}
	ctx := req.Context()
	if !at.IsZero() {
		v, err := versioner.ObjectAt(ctx, s.account, f.key, at)
		if errors.Is(err, stores.ErrNotFound) {
			writeError(w, "version not found", http.StatusNotFound)
			return client.VersionInfo{}, false
		} else if err != nil {
			log.Printf("file API: failed to find %s on %s at %s: %v", f.path, f.share.Name, at, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return client.VersionInfo{}, false
		}
		return v, true
	}
	versions, err := versioner.Versions(ctx, s.account, f.key)
	if err != nil {
		log.Printf("file API: failed to list the versions of %s on %s: %v", f.path, f.share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return client.VersionInfo{}, false
	}
	for _, v := range versions {
		if v.ID == id {
			return v, true
		}
	}
	writeError(w, "version not found", http.StatusNotFound)
	return client.VersionInfo{}, false
}

// versionsHandler handles the GET /share/:name/versions calls: the previous
// versions of the file the path names, newest first. A share that keeps none
// answers with an empty list.
func (h *Handler) versionsHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	f, ok := h.fileOf(w, req, ps, s, Rights{})
	if !ok {
		return
	}

	entries := []VersionEntry{}
	if v, kept := f.client.(client.Versioner); kept {
		versions, err := v.Versions(req.Context(), s.account, f.key)
		if err != nil {
			log.Printf("file API: failed to list the versions of %s on %s: %v", f.path, f.share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, v := range versions {
			entries = append(entries, VersionEntry{ID: v.ID, Size: v.Size, ModifiedAt: v.ModifiedAt, ReplacedAt: v.ReplacedAt, ReplacedBy: v.ReplacedBy})
		}
	}
	writeJSON(w, entries)
}

// versionHandler handles the GET /share/:name/version calls, sending the
// version the id query names of the file the path names, or the copy of the
// file that stood at the time the at query names, as a download does.
func (h *Handler) versionHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	f, ok := h.fileOf(w, req, ps, s, Rights{})
	if !ok {
		return
	}
	version, ok := h.versionNamed(w, req, s, f)
	if !ok {
		return
	}

	ctx := req.Context()
	fetch := func(offset, length uint64, w io.Writer) error {
		return f.client.Read(ctx, s.account, f.key, offset, length, w)
	}
	if version.ID != 0 {
		versioner := f.client.(client.Versioner)
		fetch = func(offset, length uint64, w io.Writer) error {
			return versioner.ReadVersion(ctx, s.account, f.key, version.ID, offset, length, w)
		}
	}

	name := path.Base(f.path)
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	disposition := "attachment"
	if req.URL.Query().Get("inline") != "" {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))

	rd := &objectReader{
		fetch: fetch,
		path:  f.key,
		size:  version.Size,
		chunk: h.chunkSize(f.partSize),
	}
	http.ServeContent(w, req, "", version.ModifiedAt, rd)
}

// restoreHandler handles the POST /share/:name/restore calls, making the
// version the id query names, or the copy of the file that stood at the time
// the at query names, the contents of the file again. The contents it replaces
// are kept as a version like any other.
func (h *Handler) restoreHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	f, ok := h.fileOf(w, req, ps, s, Rights{Write: true})
	if !ok {
		return
	}
	version, ok := h.versionNamed(w, req, s, f)
	if !ok {
		return
	}
	ctx := req.Context()
	if version.ID == 0 {
		// The file holds the contents of the time already.
		writeJSON(w, h.entryAt(ctx, f.client, s, f.path))
		return
	}

	versioner := f.client.(client.Versioner)
	id := version.ID
	err := versioner.RestoreVersion(ctx, s.account, f.key, id)
	switch {
	case errors.Is(err, stores.ErrNotFound):
		writeError(w, "version not found", http.StatusNotFound)
	case errors.Is(err, stores.ErrAccessDenied):
		writeError(w, "access denied", http.StatusForbidden)
	case errors.Is(err, stores.ErrUploadInFlight):
		writeError(w, "the file is being written by somebody else", http.StatusConflict)
	case err != nil:
		log.Printf("file API: failed to restore version %d of %s on %s: %v", id, f.path, f.share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
	default:
		writeJSON(w, h.entryAt(ctx, f.client, s, f.path))
	}
}

// snapshotsHandler handles the GET /share/:name/snapshots calls: the times the
// folder the path names has previous states at, newest first, which the list
// call takes as its at query. The times are in UTC, so that they survive a
// query string: a zone offset's plus sign would read as a space there.
func (h *Handler) snapshotsHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	p, ok := cleanPath(req.URL.Query().Get("path"))
	if !ok {
		writeError(w, "invalid path", http.StatusBadRequest)
		return
	}
	c, _, err := h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, s.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return
	}
	if p != "/" {
		t, ok := h.resolve(w, req, s, share, c, p)
		if !ok {
			return
		}
		if !t.exists {
			writeError(w, "folder not found", http.StatusNotFound)
			return
		}
		if !t.isDir() {
			writeError(w, "not a folder", http.StatusBadRequest)
			return
		}
	}

	times := []time.Time{}
	if v, kept := c.(client.Versioner); kept {
		times, err = v.Snapshots(req.Context(), s.account, strings.TrimPrefix(p, "/"))
		if err != nil {
			log.Printf("file API: failed to list the snapshots of %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		for i, t := range times {
			times[i] = t.UTC()
		}
		if times == nil {
			times = []time.Time{}
		}
	}
	writeJSON(w, times)
}

// deleteVersionsHandler handles the DELETE /share/:name/versions calls,
// removing the version the id query names, or every version of the file
// without one.
func (h *Handler) deleteVersionsHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	f, ok := h.fileOf(w, req, ps, s, Rights{Delete: true})
	if !ok {
		return
	}
	id, ok := versionID(w, req)
	if !ok {
		return
	}

	versioner, kept := f.client.(client.Versioner)
	if !kept {
		writeError(w, "version not found", http.StatusNotFound)
		return
	}
	err := versioner.DeleteVersions(req.Context(), s.account, f.key, id)
	switch {
	case errors.Is(err, stores.ErrNotFound):
		writeError(w, "version not found", http.StatusNotFound)
	case errors.Is(err, stores.ErrAccessDenied):
		writeError(w, "access denied", http.StatusForbidden)
	case err != nil:
		log.Printf("file API: failed to delete versions of %s on %s: %v", f.path, f.share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
