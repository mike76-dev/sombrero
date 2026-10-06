package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
)

// catalogBody is the smallest catalog there is: a header and a connection.
func catalogBody(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{Source: "sombrero", Share: "myshare"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Connection(transfer.Connection{Share: transfer.Share{Name: "myshare", Type: "indexd"}, Workgroup: transfer.Workgroup{UUID: testUUID}}); err != nil {
		t.Fatalf("Connection: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return buf.Bytes()
}

// postBytes sends a request with the bytes as they are, which is how a catalog
// travels.
func postBytes(api *API, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, req)
	return w
}

// TestRestore tests POST /restore.
func TestRestore(t *testing.T) {
	t.Run("POST restores the catalog and says what came back", func(t *testing.T) {
		var got stores.RestoreOptions
		ms := &mockStore{
			restore: func(_ context.Context, r *transfer.Reader, opts stores.RestoreOptions) (stores.RestoreStats, error) {
				got = opts
				if r.Connection() == nil {
					t.Error("the store was handed a description with no connection")
				}
				return stores.RestoreStats{
					Share: "myshare", Workgroup: testUUID, Accounts: 2, Policies: 1,
					ApplyStats: stores.ApplyStats{Directories: 3, Files: 4, AlreadyThere: 1, Incomplete: 1},
				}, nil
			},
		}
		w := postBytes(newTestAPI(ms), "/restore?force=true", catalogBody(t))
		checkStatus(t, w, http.StatusOK)
		res := decodeJSON[RestoreResponse](t, w)
		if res.Kind != "connection" || res.Share != "myshare" || res.Workgroup != testUUID.String() || res.Accounts != 2 || res.Policies != 1 || res.Files != 4 || res.Incomplete != 1 {
			t.Errorf("the response: got %+v", res)
		}
		if !got.Force {
			t.Error("force was asked for and not passed on")
		}
	})

	t.Run("POST restores a catalog of the server", func(t *testing.T) {
		var buf bytes.Buffer
		w, _ := transfer.NewWriter(&buf, transfer.Header{Source: "sombrero"})
		_ = w.Server(transfer.Server{Bans: []transfer.Ban{{Host: "192.168.1.100"}}})
		_ = w.Close()

		ms := &mockStore{
			restoreServer: func(_ context.Context, r *transfer.Reader) (stores.ServerRestoreStats, error) {
				if r.Server() == nil || len(r.Server().Bans) != 1 {
					t.Error("the store was handed something other than the catalog of the server")
				}
				return stores.ServerRestoreStats{Shares: 2, Workgroups: 1, Accounts: 3, Bans: 1}, nil
			},
			restore: func(context.Context, *transfer.Reader, stores.RestoreOptions) (stores.RestoreStats, error) {
				t.Error("a catalog of the server was restored as a connection")
				return stores.RestoreStats{}, nil
			},
		}
		res := decodeJSON[RestoreResponse](t, postBytes(newTestAPI(ms), "/restore", buf.Bytes()))
		if res.Kind != "server" || res.Shares != 2 || res.Workgroups != 1 || res.Accounts != 3 || res.Bans != 1 {
			t.Errorf("the response: got %+v", res)
		}
	})

	t.Run("POST restores a catalog from the folder on this machine", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "myshare_"+testUUID.String())
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		stored := filepath.Join(sub, "20261006T000000.000000000Z.catalog")
		if err := os.WriteFile(stored, catalogBody(t), 0o600); err != nil {
			t.Fatal(err)
		}

		var restored bool
		ms := &mockStore{
			restore: func(_ context.Context, r *transfer.Reader, opts stores.RestoreOptions) (stores.RestoreStats, error) {
				restored = r.Connection() != nil && opts.Force
				return stores.RestoreStats{Share: "myshare", Workgroup: testUUID}, nil
			},
		}
		api := newTestAPI(ms)
		api.cfg.Backup.Path = dir

		// The folder lists it, and it restores by its path.
		w := doRequest(api, http.MethodGet, "/backup/catalogs", nil)
		checkStatus(t, w, http.StatusOK)
		list := decodeJSON[[]StoredCatalogResponse](t, w)
		if len(list) != 1 || list[0].Path != stored || list[0].Kind != "connection" || list[0].Share != "myshare" || list[0].Workgroup != testUUID.String() {
			t.Errorf("the folder: got %+v", list)
		}

		w = doRequest(api, http.MethodPost, "/restore", StoredRestoreRequest{Path: stored, Force: true})
		checkStatus(t, w, http.StatusOK)
		if res := decodeJSON[RestoreResponse](t, w); res.Kind != "connection" || res.Share != "myshare" || !restored {
			t.Errorf("the response: got %+v, restored %v", res, restored)
		}

		// A catalog that was pruned since the list was made is said to be gone.
		w = doRequest(api, http.MethodPost, "/restore", StoredRestoreRequest{Path: filepath.Join(sub, "20261001T000000.000000000Z.catalog")})
		checkStatus(t, w, http.StatusNotFound)

		// Nothing outside the folder is read, however it is spelled.
		outside := filepath.Join(t.TempDir(), "elsewhere.catalog")
		if err := os.WriteFile(outside, catalogBody(t), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{outside, filepath.Join(dir, "..", filepath.Base(filepath.Dir(outside)), "elsewhere.catalog"), "/etc/passwd"} {
			w := doRequest(api, http.MethodPost, "/restore", StoredRestoreRequest{Path: path})
			checkStatus(t, w, http.StatusBadRequest)
		}
	})

	t.Run("POST refuses what is not a catalog", func(t *testing.T) {
		w := postBytes(newTestAPI(&mockStore{}), "/restore", []byte("just some bytes"))
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST reports a connection that is there already as a conflict", func(t *testing.T) {
		ms := &mockStore{
			restore: func(context.Context, *transfer.Reader, stores.RestoreOptions) (stores.RestoreStats, error) {
				return stores.RestoreStats{}, stores.ErrConnectionExists
			},
		}
		w := postBytes(newTestAPI(ms), "/restore", catalogBody(t))
		checkStatus(t, w, http.StatusConflict)
	})

	t.Run("POST reports a share that serves something else", func(t *testing.T) {
		ms := &mockStore{
			restore: func(context.Context, *transfer.Reader, stores.RestoreOptions) (stores.RestoreStats, error) {
				return stores.RestoreStats{Workgroup: uuid.Nil}, stores.ErrShareMismatch
			},
		}
		w := postBytes(newTestAPI(ms), "/restore", catalogBody(t))
		checkStatus(t, w, http.StatusBadRequest)
	})
}
