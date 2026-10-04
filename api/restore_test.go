package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
		if res.Share != "myshare" || res.Workgroup != testUUID.String() || res.Accounts != 2 || res.Policies != 1 || res.Files != 4 || res.Incomplete != 1 {
			t.Errorf("the response: got %+v", res)
		}
		if !got.Force {
			t.Error("force was asked for and not passed on")
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
