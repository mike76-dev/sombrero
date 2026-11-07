package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
)

// TestShareClient verifies that the client handed out is the workgroup's own
// on an indexd share, the share's one on a renterd share, and none for a
// workgroup that is not connected.
func TestShareClient(t *testing.T) {
	store, err := stores.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatalf("could not create the store: %v", err)
	}
	t.Cleanup(store.Close)

	wg := uuid.New()
	if err := store.AddWorkgroup(stores.Workgroup{UUID: wg, Name: "testwg"}); err != nil {
		t.Fatalf("could not add the workgroup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := newServerState(ctx, store, stores.Config{})

	own := newFakeClient()
	indexd := &share{name: "docs", backend: "indexd", indexdConns: map[string]*indexdConn{wg.String(): {client: own, maxUploadSize: 40 << 20}}}
	srv.shareList[indexd.name] = indexd

	shared := newFakeClient()
	renterd := &share{name: "pub", backend: "renterd", client: shared, maxUploadSize: 4 << 20}
	srv.shareList[renterd.name] = renterd

	if c, part, err := srv.ShareClient(stores.Share{Name: "docs"}, wg.String()); err != nil || c != own || part != 40<<20 {
		t.Errorf("indexd share: want the workgroup's client with its part size, got %v, %d (%v)", c, part, err)
	}
	if c, part, err := srv.ShareClient(stores.Share{Name: "pub"}, wg.String()); err != nil || c != shared || part != 4<<20 {
		t.Errorf("renterd share: want the share's client with its part size, got %v, %d (%v)", c, part, err)
	}

	// A workgroup with no connection to the share, stored or running, gets none.
	if _, _, err := srv.ShareClient(stores.Share{Name: "docs"}, uuid.NewString()); !errors.Is(err, errShareUnavailable) {
		t.Errorf("unconnected workgroup: want %v, got %v", errShareUnavailable, err)
	}
	if _, _, err := srv.ShareClient(stores.Share{Name: "docs"}, "not-a-uuid"); !errors.Is(err, errShareUnavailable) {
		t.Errorf("malformed workgroup: want %v, got %v", errShareUnavailable, err)
	}
}
