package files

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/julienschmidt/httprouter"
)

// activityPage is how many entries of the feed a call returns unless told otherwise.
const activityPage = 50

// ActivityEntry is an entry of a share's activity feed: what an account of
// the workgroup did, where and when. Target is the new path of a rename or a
// move, Size that of an uploaded or restored file.
type ActivityEntry struct {
	ID       uint64    `json:"id"`
	At       time.Time `json:"at"`
	Username string    `json:"username"`
	Action   string    `json:"action"`
	Path     string    `json:"path"`
	Target   string    `json:"target,omitempty"`
	Size     uint64    `json:"size,omitempty"`
}

// activityHandler handles the GET /share/:name/activity calls: what the
// accounts of the session's workgroup did on the share, newest first. The
// before query names the entry to go on from, and limit how many to send.
func (h *Handler) activityHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	q := req.URL.Query()
	var before uint64
	if v := q.Get("before"); v != "" {
		var err error
		if before, err = strconv.ParseUint(v, 10, 64); err != nil {
			writeError(w, "invalid before", http.StatusBadRequest)
			return
		}
	}
	limit := activityPage
	if v := q.Get("limit"); v != "" {
		var err error
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			writeError(w, "invalid limit", http.StatusBadRequest)
			return
		}
	}

	activity, err := h.store.Activity(share.Name, s.account, before, limit)
	if err != nil {
		log.Printf("file API: failed to list the activity on %s: %v", share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	entries := make([]ActivityEntry, 0, len(activity))
	for _, a := range activity {
		entries = append(entries, ActivityEntry{ID: a.ID, At: a.At.UTC(), Username: a.Username, Action: a.Action, Path: a.Path, Target: a.Target, Size: a.Size})
	}
	writeJSON(w, entries)
}
