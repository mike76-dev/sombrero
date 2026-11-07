package files

import (
	"errors"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
)

// Rights is what an account's policy on a share grants it.
type Rights struct {
	Read    bool `json:"read"`
	Write   bool `json:"write"`
	Delete  bool `json:"delete"`
	Execute bool `json:"execute"`
}

// ShareInfo is one entry of GET /shares: a share the account can reach, with
// what it holds there.
type ShareInfo struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Remark string `json:"remark,omitempty"`
	Rights Rights `json:"rights"`
}

// Entry is one file or folder of a listing.
type Entry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Dir        bool      `json:"dir"`
	Size       uint64    `json:"size"`
	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// Listing is the response of GET /share/:name/list: what the account sees in
// the folder.
type Listing struct {
	Share   string  `json:"share"`
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
}

// sharesHandler handles the GET /shares calls: the shares the account can
// reach, which are those its policies admit it to and, for a guest account,
// those that take guests.
func (h *Handler) sharesHandler(w http.ResponseWriter, req *http.Request, _ httprouter.Params, s session) {
	shares, err := h.store.GetShares(s.account)
	if err != nil {
		log.Printf("file API: failed to list shares: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	infos := make([]ShareInfo, 0, len(shares))
	for _, share := range shares {
		r, err := h.rightsOn(s.account, share)
		if err != nil {
			log.Printf("file API: failed to read the policy: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if r == (Rights{}) {
			continue
		}
		infos = append(infos, ShareInfo{Name: share.Name, Type: share.Type, Remark: share.Remark, Rights: r})
	}
	writeJSON(w, infos)
}

// listHandler handles the GET /share/:name/list calls, listing the folder the
// path query names, or the root of the share without one.
func (h *Handler) listHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
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

	// The clients speak the SMB layer's spelling: no leading slash, and a
	// folder to list ends in one. A listing of what is not there is empty
	// rather than an error, so the folder is looked up first.
	ctx := req.Context()
	listPath := ""
	if p != "/" {
		oi, err := c.Object(ctx, s.account, strings.TrimPrefix(p, "/"))
		if errors.Is(err, stores.ErrNotFound) {
			writeError(w, "folder not found", http.StatusNotFound)
			return
		} else if err != nil {
			log.Printf("file API: failed to look up %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !strings.HasSuffix(oi.Key, "/") {
			writeError(w, "not a folder", http.StatusBadRequest)
			return
		}
		listPath = strings.TrimPrefix(p, "/") + "/"
	}

	ois, err := c.List(ctx, s.account, listPath)
	if err != nil {
		log.Printf("file API: failed to list %s on %s: %v", p, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	entries := make([]Entry, 0, len(ois))
	for _, oi := range ois {
		key := strings.TrimSuffix(oi.Key, "/")
		if !strings.HasPrefix(key, "/") {
			key = "/" + key
		}
		if key == "/" {
			continue
		}
		entries = append(entries, Entry{
			Name:       path.Base(key),
			Path:       key,
			Dir:        strings.HasSuffix(oi.Key, "/"),
			Size:       oi.Size,
			CreatedAt:  oi.CreatedAt,
			ModifiedAt: oi.ModifiedAt,
		})
	}
	writeJSON(w, Listing{Share: share.Name, Path: p, Entries: entries})
}

// shareFor finds the share a call names and what the account holds on it,
// answering the call itself where the share is not there or not the account's
// to reach.
func (h *Handler) shareFor(w http.ResponseWriter, s session, name string) (stores.Share, Rights, bool) {
	share, err := h.store.GetShare(name)
	if err != nil {
		log.Printf("file API: failed to find share %s: %v", name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return stores.Share{}, Rights{}, false
	}
	if share.Name == "" {
		writeError(w, "share not found", http.StatusNotFound)
		return stores.Share{}, Rights{}, false
	}
	r, err := h.rightsOn(s.account, share)
	if err != nil {
		log.Printf("file API: failed to read the policy: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return stores.Share{}, Rights{}, false
	}
	if r == (Rights{}) {
		writeError(w, "access denied", http.StatusForbidden)
		return stores.Share{}, Rights{}, false
	}
	return share, r, true
}

// rightsOn is what the account holds on the share: its policy, unless it is a
// guest account on a share that takes none.
func (h *Handler) rightsOn(acc stores.Account, share stores.Share) (Rights, error) {
	if acc.Passwordless() && !share.AllowGuest {
		return Rights{}, nil
	}
	ar, err := h.store.GetAccessRights(share, acc)
	if err != nil {
		return Rights{}, err
	}
	return Rights{Read: ar.ReadAccess, Write: ar.WriteAccess, Delete: ar.DeleteAccess, Execute: ar.ExecuteAccess}, nil
}

// cleanPath turns the path a call names into the share's spelling: absolute,
// forward slashes, no trailing slash. A path that climbs is refused.
func cleanPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	return path.Clean(p), true
}
