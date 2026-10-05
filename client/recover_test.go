package client

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// TestFindCatalog verifies that the newest whole catalog an account carries is
// the one found, by the names its slabs' tags give it, and that one with a piece
// missing is passed over for the one before.
func TestFindCatalog(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	fa := &fakeAccount{data: make(map[types.Hash256][]byte)}
	folder := transfer.CatalogFolder + "/"

	// An older catalog, whole, packed after a photo.
	older := []byte("the older catalog")
	key := fa.pin(at, tagOf(t,
		objectPiece{Share: "s", Path: "/photo.jpg", At: 0, Length: 100, Size: 100},
		objectPiece{Share: "s", Path: folder + "20261001T000000.000000000Z.catalog", At: 100, Length: uint64(len(older)), Size: uint64(len(older))},
	), 200)
	fa.data[key] = append(append(bytes.Repeat([]byte("p"), 100), older...), bytes.Repeat([]byte("x"), 200-100-len(older))...)

	// The newest catalog is in two pieces, one of which is not on the network
	// any more.
	key = fa.pin(at.Add(time.Second), tagOf(t,
		objectPiece{Share: "s", Path: folder + "20261003T000000.000000000Z.catalog", Offset: 0, At: 0, Length: 10, Size: 20},
	), 10)
	fa.data[key] = bytes.Repeat([]byte("n"), 10)

	// The one between is whole, in two objects.
	middle := []byte("the catalog in between, in two pieces")
	key = fa.pin(at.Add(2*time.Second), tagOf(t,
		objectPiece{Share: "s", Path: folder + "20261002T000000.000000000Z.catalog", Offset: 0, At: 0, Length: 10, Size: uint64(len(middle))},
	), 10)
	fa.data[key] = middle[:10]
	key = fa.pin(at.Add(3*time.Second), tagOf(t,
		objectPiece{Share: "s", Path: folder + "20261002T000000.000000000Z.catalog", Offset: 10, At: 5, Length: uint64(len(middle) - 10), Size: uint64(len(middle))},
	), uint32(len(middle)))
	fa.data[key] = append(bytes.Repeat([]byte("y"), 5), middle[10:]...)

	found, err := FindCatalog(context.Background(), fa)
	if err != nil {
		t.Fatalf("FindCatalog: %v", err)
	}
	if found.Path != folder+"20261002T000000.000000000Z.catalog" || !bytes.Equal(found.Data, middle) {
		t.Errorf("found %s: %q", found.Path, found.Data)
	}

	// An account that says nothing of a catalog has none to find.
	bare := &fakeAccount{}
	bare.pin(at, tagOf(t, objectPiece{Share: "s", Path: "/photo.jpg", Length: 100, Size: 100}), 100)
	bare.pin(at.Add(time.Second), nil, 100)
	if _, err := FindCatalog(context.Background(), bare); !errors.Is(err, ErrNoCatalog) {
		t.Errorf("an account without a catalog: got %v", err)
	}
}
