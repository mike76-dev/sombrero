package api

import (
	"net/http"
	"testing"

	"github.com/mike76-dev/sombrero/stores"
)

func TestGrants(t *testing.T) {
	t.Run("PUT sets a grant", func(t *testing.T) {
		var gotShare, gotPath string
		var gotWG stores.Workgroup
		var gotLevel stores.GrantLevel
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
			setGrant: func(share, path string, wg stores.Workgroup, level stores.GrantLevel) error {
				gotShare, gotPath, gotWG, gotLevel = share, path, wg, level
				return nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/grant?path=/proj&workgroup="+testUUID.String()+"&level=write", nil)
		checkStatus(t, w, http.StatusNoContent)
		if gotShare != "myshare" || gotPath != "/proj" || gotWG.UUID != testUUID || gotLevel != stores.LevelWrite {
			t.Errorf("wrong grant: %s %s %v %v", gotShare, gotPath, gotWG.UUID, gotLevel)
		}
	})

	t.Run("PUT on a renterd share returns 400", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "renterd"),
		}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/grant?path=/proj&workgroup="+testUUID.String()+"&level=read", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT with a level off the ladder returns 400", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
		}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/grant?path=/proj&workgroup="+testUUID.String()+"&level=owner", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT without a path returns 400", func(t *testing.T) {
		ms := &mockStore{getShare: foundShare("myshare", "indexd")}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/grant?workgroup="+testUUID.String()+"&level=read", nil)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("PUT for an unconnected workgroup returns 409", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
			setGrant: func(string, string, stores.Workgroup, stores.GrantLevel) error {
				return stores.ErrNotConnected
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/grant?path=/proj&workgroup="+testUUID.String()+"&level=read", nil)
		checkStatus(t, w, http.StatusConflict)
	})

	t.Run("PUT on a missing folder returns 404", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			getShare:      foundShare("myshare", "indexd"),
			setGrant: func(string, string, stores.Workgroup, stores.GrantLevel) error {
				return stores.ErrNotFound
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodPut,
			"/share/myshare/grant?path=/gone&workgroup="+testUUID.String()+"&level=read", nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("GET lists the grants of a folder", func(t *testing.T) {
		ms := &mockStore{
			grants: func(share, path string) ([]stores.Grant, error) {
				return []stores.Grant{{
					ShareName: share,
					Path:      path,
					Workgroup: stores.Workgroup{UUID: testUUID, Name: "others"},
					Level:     stores.LevelDelete,
				}}, nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare/grants?path=/proj", nil)
		checkStatus(t, w, http.StatusOK)
		got := decodeJSON[[]GrantInfo](t, w)
		if len(got) != 1 || got[0].Workgroup != testUUID || got[0].Level != "delete" || got[0].Path != "/proj" {
			t.Errorf("wrong grants: %+v", got)
		}
	})

	t.Run("GET without a path lists the whole share", func(t *testing.T) {
		ms := &mockStore{
			shareGrants: func(share string) ([]stores.Grant, error) {
				return []stores.Grant{
					{ShareName: share, Path: "/proj", Workgroup: stores.Workgroup{UUID: testUUID}, Level: stores.LevelRead},
					{ShareName: share, Path: "/pub", Workgroup: stores.Workgroup{UUID: testUUID}, Level: stores.LevelAdmin},
				}, nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/share/myshare/grants", nil)
		checkStatus(t, w, http.StatusOK)
		got := decodeJSON[[]GrantInfo](t, w)
		if len(got) != 2 || got[1].Level != "admin" {
			t.Errorf("wrong grants: %+v", got)
		}
	})

	t.Run("DELETE removes a grant", func(t *testing.T) {
		var gotPath string
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			removeGrant: func(_, path string, _ stores.Workgroup) error {
				gotPath = path
				return nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodDelete,
			"/share/myshare/grant?path=/proj&workgroup="+testUUID.String(), nil)
		checkStatus(t, w, http.StatusNoContent)
		if gotPath != "/proj" {
			t.Errorf("wrong path: %s", gotPath)
		}
	})

	t.Run("GET lists what a workgroup was granted", func(t *testing.T) {
		ms := &mockStore{
			findWorkgroup: foundWorkgroup(),
			workgroupGrants: func(wg stores.Workgroup) ([]stores.Grant, error) {
				return []stores.Grant{{
					ShareName: "myshare",
					Path:      "/proj",
					Workgroup: wg,
					Level:     stores.LevelRead,
				}}, nil
			},
		}
		w := doRequest(newTestAPI(ms), http.MethodGet, "/workgroup/"+testUUID.String()+"/grants", nil)
		checkStatus(t, w, http.StatusOK)
		got := decodeJSON[[]GrantInfo](t, w)
		if len(got) != 1 || got[0].ShareName != "myshare" || got[0].Level != "read" {
			t.Errorf("wrong grants: %+v", got)
		}
	})
}
