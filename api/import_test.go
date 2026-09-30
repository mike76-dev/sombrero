package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/v2/api"
)

// emptyRenterd stands in for a renterd server whose bucket holds nothing, which
// is the shortest import there is.
func emptyRenterd(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ObjectsResponse{})
	}))
	t.Cleanup(srv.Close)

	return srv
}

// importingStore is the store an import needs: one that takes the rows.
func importingStore(share string) *mockStore {
	return &mockStore{
		findWorkgroup: foundWorkgroup(),
		getShare:      foundShare(share, "indexd"),
		findAccount:   foundAccount("alice", testUUID.String()),
	}
}

// connectedServer serves the share, with the workgroup connected to it.
func connectedServer() *mockServer {
	return &mockServer{
		shareConnections: func(string) (map[string]client.Client, map[string]string, error) {
			return map[string]client.Client{testUUID.String(): &mockClient{}}, nil, nil
		},
	}
}

// awaitImport polls until the import has ended.
func awaitImport(t *testing.T, api *API, path string) ImportStatusResponse {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		w := doRequest(api, http.MethodGet, path, nil)
		checkStatus(t, w, http.StatusOK)
		res := decodeJSON[ImportStatusResponse](t, w)
		if res.State != ImportRunning {
			return res
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("the import did not finish")

	return ImportStatusResponse{}
}

func TestImport(t *testing.T) {
	path := "/import/" + testUUID.String() + "/myshare"

	t.Run("GET reports no import as idle", func(t *testing.T) {
		ms := importingStore("myshare")
		res := decodeJSON[ImportStatusResponse](t, doRequest(newTestAPI(ms), http.MethodGet, path, nil))
		if res.State != ImportIdle {
			t.Errorf("state: want %q, got %q", ImportIdle, res.State)
		}
	})

	t.Run("GET unknown workgroup returns 404", func(t *testing.T) {
		w := doRequest(newTestAPI(&mockStore{}), http.MethodGet, "/import/unknown-name/myshare", nil)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("POST runs an import of an empty source", func(t *testing.T) {
		srv := emptyRenterd(t)
		api := newTestAPIWithServer(importingStore("myshare"), connectedServer())

		w := doRequest(api, http.MethodPost, path, ImportRequest{
			Source: "renterd", Address: srv.URL, Bucket: "default", Username: "alice",
		})
		checkStatus(t, w, http.StatusAccepted)
		if res := decodeJSON[ImportStatusResponse](t, w); res.State != ImportRunning {
			t.Fatalf("state: want %q, got %q", ImportRunning, res.State)
		}

		res := awaitImport(t, api, path)
		if res.State != ImportDone {
			t.Fatalf("state: want %q, got %q (%s)", ImportDone, res.State, res.Error)
		}
		if res.Pinned != 0 || res.Copied != 0 || res.Failed != 0 {
			t.Errorf("an import of nothing: got %+v", res)
		}
		if res.Source != "renterd" {
			t.Errorf("source: want %q, got %q", "renterd", res.Source)
		}
	})

	t.Run("POST refuses a renterd share", func(t *testing.T) {
		ms := importingStore("myshare")
		ms.getShare = foundShare("myshare", "renterd")
		w := doRequest(newTestAPIWithServer(ms, connectedServer()), http.MethodPost, path, ImportRequest{
			Source: "renterd", Address: "http://127.0.0.1:9980", Username: "alice",
		})
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST refuses a source it cannot read", func(t *testing.T) {
		for _, body := range []ImportRequest{
			{Source: "renterd", Username: "alice"},                                        // no address
			{Source: "somewhere-else", Address: "http://x", Username: "alice"},            // no such kind
			{Source: "indexd", Address: "http://x", Username: "alice", AppKey: "not-hex"}, // no key to read it with
			{Source: "indexd", Address: "http://x", Username: "alice", AppKey: hex.EncodeToString(make([]byte, 8))},
		} {
			w := doRequest(newTestAPIWithServer(importingStore("myshare"), connectedServer()), http.MethodPost, path, body)
			checkStatus(t, w, http.StatusBadRequest)
		}
	})

	t.Run("POST refuses an account that is not there", func(t *testing.T) {
		ms := importingStore("myshare")
		ms.findAccount = func(string, string) (stores.Account, error) { return stores.Account{}, nil }
		w := doRequest(newTestAPIWithServer(ms, connectedServer()), http.MethodPost, path, ImportRequest{
			Source: "renterd", Address: "http://127.0.0.1:9980", Username: "nobody",
		})
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST refuses a share the workgroup is not connected to", func(t *testing.T) {
		w := doRequest(newTestAPIWithServer(importingStore("myshare"), &mockServer{}), http.MethodPost, path, ImportRequest{
			Source: "renterd", Address: "http://127.0.0.1:9980", Username: "alice",
		})
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST refuses a second import of the same share", func(t *testing.T) {
		api := newTestAPIWithServer(importingStore("myshare"), connectedServer())
		key := connectKey(stores.Workgroup{UUID: testUUID}, stores.Share{Name: "myshare"})
		if _, started := api.imports.begin(key, "renterd", func() {}); !started {
			t.Fatal("the import did not start")
		}

		w := doRequest(api, http.MethodPost, path, ImportRequest{
			Source: "renterd", Address: "http://127.0.0.1:9980", Username: "alice",
		})
		checkStatus(t, w, http.StatusConflict)
	})

	t.Run("DELETE calls off a running import", func(t *testing.T) {
		api := newTestAPI(importingStore("myshare"))
		key := connectKey(stores.Workgroup{UUID: testUUID}, stores.Share{Name: "myshare"})

		called := make(chan struct{})
		run, started := api.imports.begin(key, "renterd", func() { close(called) })
		if !started {
			t.Fatal("the import did not start")
		}

		w := doRequest(api, http.MethodDelete, path, nil)
		checkStatus(t, w, http.StatusOK)
		select {
		case <-called:
		default:
			t.Error("the import was not called off")
		}

		// What it came to is reported until it is forgotten.
		run.finish(ImportCancelled, client.ImportStats{Copied: 2}, nil)
		res := decodeJSON[ImportStatusResponse](t, doRequest(api, http.MethodGet, path, nil))
		if res.State != ImportCancelled || res.Copied != 2 {
			t.Errorf("the called-off import: got %+v", res)
		}
	})

	t.Run("DELETE without an import returns 404", func(t *testing.T) {
		w := doRequest(newTestAPI(importingStore("myshare")), http.MethodDelete, path, nil)
		checkStatus(t, w, http.StatusNotFound)
	})
}

// TestImportStampsAndReports verifies what the import is given to work with: the
// times of the source, and the failures it is to report.
func TestImportStampsAndReports(t *testing.T) {
	var stamped struct {
		share, path string
	}
	ms := importingStore("myshare")
	ms.setFileTimes = func(share, path string, _, _ time.Time) error {
		stamped.share, stamped.path = share, path
		return nil
	}

	store, ok := Store(ms).(Transfers)
	if !ok {
		t.Fatal("the store an import needs is not the store the API has")
	}
	if err := store.SetFileTimes("myshare", "/x.bin", time.Now(), time.Now()); err != nil {
		t.Fatalf("SetFileTimes: %v", err)
	}
	if stamped.share != "myshare" || stamped.path != "/x.bin" {
		t.Errorf("the times were put back on %q of %q", stamped.path, stamped.share)
	}

	// The failures of an import are kept for the status to report, up to the
	// first few of them.
	run := &importRun{state: ImportRunning}
	for i := 0; i < maxImportFailures+5; i++ {
		run.failure("/x.bin", context.DeadlineExceeded)
	}
	if got := len(run.status().Failures); got != maxImportFailures {
		t.Errorf("failures kept: want %d, got %d", maxImportFailures, got)
	}
}

// TestImportAppMetadata verifies that an import names this server to the source
// indexer the way the rest of the server does, which is what its keys hang off.
func TestImportAppMetadata(t *testing.T) {
	api := NewAPI(context.Background(), &mockStore{}, nil, stores.Config{
		Mode: stores.ModeNormal,
		Indexd: stores.IndexdConfig{
			Name:        "Sombrero",
			Description: "Sombrero SMB server",
		},
	}, testVersion)

	meta := api.appMetadata()
	want := types.HashBytes([]byte("SombreroSombrero SMB server"))
	if meta.ID != want {
		t.Errorf("app ID: want %s, got %s", want, meta.ID)
	}
}
