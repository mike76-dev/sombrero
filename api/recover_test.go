package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/backup"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
	sdk "go.sia.tech/siastorage"
)

// fakeCatalogSource stands in for an account that holds one object: a slab of
// the given bytes, tagged as the file it is.
type fakeCatalogSource struct {
	obj  sdk.Object
	data []byte
}

func newFakeCatalogSource(path string, data []byte) *fakeCatalogSource {
	ss := []slabs.SlabSlice{{
		Version:       1,
		EncryptionKey: slabs.EncryptionKey{1},
		MinShards:     2,
		Sectors:       []slabs.PinnedSector{{Root: types.Hash256{1}, HostKey: types.PublicKey{2}}},
		Length:        uint32(len(data)),
	}}
	obj := sdk.NewUnsafeObject([32]byte{3}, ss)
	obj.UpdateMetadata([]byte(fmt.Sprintf(
		`{"sombrero":1,"pieces":[{"share":"myshare","path":%q,"offset":0,"at":0,"length":%d,"size":%d}]}`,
		path, len(data), len(data),
	)))

	return &fakeCatalogSource{obj: obj, data: data}
}

func (f *fakeCatalogSource) ListObjects(_ context.Context, cursor slabs.Cursor, _ int) ([]client.PinnedObject, error) {
	if !cursor.After.IsZero() {
		return nil, nil
	}
	return []client.PinnedObject{{Key: f.obj.ID(), Size: uint64(len(f.data)), UpdatedAt: time.Now(), Object: &f.obj}}, nil
}

func (f *fakeCatalogSource) Object(_ context.Context, key types.Hash256) (sdk.Object, error) {
	if key != f.obj.ID() {
		return sdk.Object{}, errors.New("no such object")
	}
	return f.obj, nil
}

func (f *fakeCatalogSource) Download(_ context.Context, key types.Hash256, offset, length uint64, w io.Writer) error {
	if key != f.obj.ID() || offset+length > uint64(len(f.data)) {
		return errors.New("no such bytes")
	}
	_, err := w.Write(f.data[offset : offset+length])
	return err
}

// TestRecover tests POST /recover.
func TestRecover(t *testing.T) {
	key := make([]byte, 64)
	for i := range key {
		key[i] = byte(i)
	}
	sealed, err := backup.Seal(key, catalogBody(t))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	catalog := backup.CatalogFolder + "/20261005T000000.000000000Z.catalog"

	// An API whose account is the fake, whatever address it is asked for.
	recovering := func(ms *mockStore, source client.CatalogSource, sourceErr error) *API {
		api := newTestAPI(ms)
		api.catalogSource = func(string, types.PrivateKey) (client.CatalogSource, error) { return source, sourceErr }
		return api
	}
	body := RecoverRequest{Address: "https://indexer", AppKey: hex.EncodeToString(key)}

	t.Run("POST finds the catalog and restores it", func(t *testing.T) {
		var restored bool
		ms := &mockStore{
			restore: func(_ context.Context, r *transfer.Reader, opts stores.RestoreOptions) (stores.RestoreStats, error) {
				restored = r.Connection() != nil && r.Connection().Share.Name == "myshare" && !opts.Force
				return stores.RestoreStats{Share: "myshare", Workgroup: testUUID, Accounts: 1, ApplyStats: stores.ApplyStats{Files: 3}}, nil
			},
		}
		w := doRequest(recovering(ms, newFakeCatalogSource(catalog, sealed), nil), http.MethodPost, "/recover", body)
		checkStatus(t, w, http.StatusOK)
		res := decodeJSON[RecoverResponse](t, w)
		if res.Catalog != catalog || res.Kind != "connection" || res.Share != "myshare" || res.Files != 3 {
			t.Errorf("the response: got %+v", res)
		}
		if !restored {
			t.Error("the store was not handed the catalog that was found")
		}
	})

	t.Run("POST reports an account without a catalog", func(t *testing.T) {
		w := doRequest(recovering(&mockStore{}, newFakeCatalogSource("/photo.jpg", []byte("not a catalog")), nil), http.MethodPost, "/recover", body)
		checkStatus(t, w, http.StatusNotFound)
	})

	t.Run("POST reports a catalog another key sealed", func(t *testing.T) {
		other := bytes.Repeat([]byte{9}, 64)
		sealedByOther, _ := backup.Seal(other, catalogBody(t))
		w := doRequest(recovering(&mockStore{}, newFakeCatalogSource(catalog, sealedByOther), nil), http.MethodPost, "/recover", body)
		checkStatus(t, w, http.StatusBadRequest)
	})

	t.Run("POST refuses what it cannot read the account with", func(t *testing.T) {
		for _, bad := range []RecoverRequest{
			{AppKey: hex.EncodeToString(key)},               // no address
			{Address: "https://indexer", AppKey: "not-hex"}, // no key
		} {
			w := doRequest(recovering(&mockStore{}, nil, nil), http.MethodPost, "/recover", bad)
			checkStatus(t, w, http.StatusBadRequest)
		}
		w := doRequest(recovering(&mockStore{}, nil, errors.New("unauthorized")), http.MethodPost, "/recover", body)
		checkStatus(t, w, http.StatusBadRequest)
	})
}
