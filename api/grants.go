package api

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
)

// Grants is the part of a store that shares folders between workgroups. Only
// the database-backed store has it: grants hang on directories, which the Lite
// mode does not model.
type Grants interface {
	SetGrant(shareName, path string, wg stores.Workgroup, level stores.GrantLevel) error
	RemoveGrant(shareName, path string, wg stores.Workgroup) error
	Grants(shareName, path string) ([]stores.Grant, error)
	ShareGrants(shareName string) ([]stores.Grant, error)
	WorkgroupGrants(wg stores.Workgroup) ([]stores.Grant, error)
}

// GrantInfo is a grant as the API reports it, the level by name.
type GrantInfo struct {
	ShareName     string    `json:"shareName"`
	Path          string    `json:"path"`
	Workgroup     uuid.UUID `json:"workgroup"`
	WorkgroupName string    `json:"workgroupName,omitempty"`
	Level         string    `json:"level"`
}

func grantInfos(grants []stores.Grant) []GrantInfo {
	infos := make([]GrantInfo, 0, len(grants))
	for _, g := range grants {
		infos = append(infos, GrantInfo{
			ShareName:     g.ShareName,
			Path:          g.Path,
			Workgroup:     g.Workgroup.UUID,
			WorkgroupName: g.Workgroup.Name,
			Level:         g.Level.String(),
		})
	}
	return infos
}

// grantsHandlerGET handles the GET /share/:name/grants calls.
func (api *API) grantsHandlerGET(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	shareName := strings.ToLower(ps.ByName("name"))
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	gr, ok := api.store.(Grants)
	if !ok {
		writeJSON(w, []GrantInfo{})
		return
	}

	// Without a path, the whole share's grants are listed.
	var grants []stores.Grant
	var err error
	if path := req.FormValue("path"); path != "" {
		grants, err = gr.Grants(shareName, path)
	} else {
		grants, err = gr.ShareGrants(shareName)
	}
	if err != nil {
		log.Printf("failed to list the grants: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, grantInfos(grants))
}

// grantHandlerPUT handles the PUT /share/:name/grant calls.
func (api *API) grantHandlerPUT(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	shareName := strings.ToLower(ps.ByName("name"))
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
	} else if share.Type != "indexd" {
		writeError(w, "only the folders of an indexd share can be granted", http.StatusBadRequest)
		return
	}

	path := req.FormValue("path")
	if path == "" {
		writeError(w, "path cannot be empty", http.StatusBadRequest)
		return
	}

	level, err := stores.ParseGrantLevel(strings.ToLower(req.FormValue("level")))
	if err != nil {
		writeError(w, "the level is one of read, write, delete and admin", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	gr, ok := api.store.(Grants)
	if !ok {
		writeError(w, "the Lite mode cannot grant folders", http.StatusBadRequest)
		return
	}

	if err := gr.SetGrant(shareName, path, wg, level); err != nil {
		switch {
		case errors.Is(err, stores.ErrNotConnected):
			writeError(w, "the workgroup has to be connected to the share before a folder can be granted to it", http.StatusConflict)
		case errors.Is(err, stores.ErrNotFound):
			writeError(w, "no such folder", http.StatusNotFound)
		default:
			log.Printf("failed to set the grant: %v", err)
			writeError(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	writeSuccess(w)
}

// grantHandlerDELETE handles the DELETE /share/:name/grant calls.
func (api *API) grantHandlerDELETE(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	shareName := strings.ToLower(ps.ByName("name"))
	if shareName == "" {
		writeError(w, "share name cannot be empty", http.StatusBadRequest)
		return
	}

	path := req.FormValue("path")
	if path == "" {
		writeError(w, "path cannot be empty", http.StatusBadRequest)
		return
	}

	wg, ok := api.resolveWorkgroup(w, req.FormValue("workgroup"))
	if !ok {
		return
	}

	gr, ok := api.store.(Grants)
	if !ok {
		writeSuccess(w)
		return
	}

	if err := gr.RemoveGrant(shareName, path, wg); err != nil {
		log.Printf("failed to remove the grant: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeSuccess(w)
}

// workgroupGrantsHandlerGET handles the GET /workgroup/:id/grants calls.
func (api *API) workgroupGrantsHandlerGET(w http.ResponseWriter, _ *http.Request, ps httprouter.Params) {
	wg, ok := api.resolveWorkgroup(w, ps.ByName("id"))
	if !ok {
		return
	}

	gr, ok := api.store.(Grants)
	if !ok {
		writeJSON(w, []GrantInfo{})
		return
	}

	grants, err := gr.WorkgroupGrants(wg)
	if err != nil {
		log.Printf("failed to list the grants: %v", err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, grantInfos(grants))
}
