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

// TestFindCatalogs verifies that the newest whole catalog of every connection an
// account carries is found, by the names its slabs' tags give them, and that one
// with a piece missing is passed over for the one before.
func TestFindCatalogs(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	fa := &fakeAccount{data: make(map[types.Hash256][]byte)}
	first := transfer.CatalogFolder + "/11111111-1111-1111-1111-111111111111/"
	second := transfer.CatalogFolder + "/22222222-2222-2222-2222-222222222222/"

	// The first workgroup: an older catalog, whole, packed after a photo.
	older := []byte("the older catalog")
	key := fa.pin(at, tagOf(t,
		objectPiece{Share: "s", Path: "/photo.jpg", At: 0, Length: 100, Size: 100},
		objectPiece{Share: "s", Path: first + "20261001T000000.000000000Z.catalog", At: 100, Length: uint64(len(older)), Size: uint64(len(older))},
	), 200)
	fa.data[key] = append(append(bytes.Repeat([]byte("p"), 100), older...), bytes.Repeat([]byte("x"), 200-100-len(older))...)

	// Its newest catalog is in two pieces, one of which is not on the network
	// any more.
	key = fa.pin(at.Add(time.Second), tagOf(t,
		objectPiece{Share: "s", Path: first + "20261003T000000.000000000Z.catalog", Offset: 0, At: 0, Length: 10, Size: 20},
	), 10)
	fa.data[key] = bytes.Repeat([]byte("n"), 10)

	// The one between is whole, in two objects.
	middle := []byte("the catalog in between, in two pieces")
	key = fa.pin(at.Add(2*time.Second), tagOf(t,
		objectPiece{Share: "s", Path: first + "20261002T000000.000000000Z.catalog", Offset: 0, At: 0, Length: 10, Size: uint64(len(middle))},
	), 10)
	fa.data[key] = middle[:10]
	key = fa.pin(at.Add(3*time.Second), tagOf(t,
		objectPiece{Share: "s", Path: first + "20261002T000000.000000000Z.catalog", Offset: 10, At: 5, Length: uint64(len(middle) - 10), Size: uint64(len(middle))},
	), uint32(len(middle)))
	fa.data[key] = append(bytes.Repeat([]byte("y"), 5), middle[10:]...)

	// The second workgroup shares the account, and has a catalog of its own
	// that is older than any of the first's.
	other := []byte("the other workgroup's catalog")
	key = fa.pin(at.Add(4*time.Second), tagOf(t,
		objectPiece{Share: "s", Path: second + "20260901T000000.000000000Z.catalog", At: 0, Length: uint64(len(other)), Size: uint64(len(other))},
	), uint32(len(other)))
	fa.data[key] = other

	found, held, err := FindCatalogs(context.Background(), fa)
	if err != nil {
		t.Fatalf("FindCatalogs: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d catalog(s), want one per workgroup: %+v", len(found), found)
	}
	if !held(key) || held(types.Hash256{99}) {
		t.Error("what the account holds is not told from what it does not")
	}
	if found[0].Share != "s" || found[0].Path != first+"20261002T000000.000000000Z.catalog" || !bytes.Equal(found[0].Data, middle) {
		t.Errorf("the first workgroup's: %s, %q", found[0].Path, found[0].Data)
	}
	if found[1].Path != second+"20260901T000000.000000000Z.catalog" || !bytes.Equal(found[1].Data, other) {
		t.Errorf("the second workgroup's: %s, %q", found[1].Path, found[1].Data)
	}

	// An account that says nothing of a catalog has none to find.
	bare := &fakeAccount{}
	bare.pin(at, tagOf(t, objectPiece{Share: "s", Path: "/photo.jpg", Length: 100, Size: 100}), 100)
	bare.pin(at.Add(time.Second), nil, 100)
	if _, _, err := FindCatalogs(context.Background(), bare); !errors.Is(err, ErrNoCatalog) {
		t.Errorf("an account without a catalog: got %v", err)
	}
}
