package files

import (
	"log"
	"net/http"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
)

// searchHandler handles the GET /share/:name/search calls: the files and
// folders whose names contain the q query, whatever the case, among those the
// account may list, by path and at most stores.SearchLimit of them.
func (h *Handler) searchHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	q := strings.TrimSpace(req.URL.Query().Get("q"))
	if q == "" {
		writeError(w, "nothing to search for", http.StatusBadRequest)
		return
	}
	c, _, err := h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, s.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return
	}
	entries := []Entry{}
	if searcher, ok := c.(client.Searcher); ok {
		ois, err := searcher.Search(req.Context(), s.account, q)
		if err != nil {
			log.Printf("file API: failed to search %s for %q: %v", share.Name, q, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		entries = listEntries(ois)
	}
	writeJSON(w, entries)
}
