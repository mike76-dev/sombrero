package client

import (
	"encoding/json"
	"fmt"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
)

// fakeObject is an object of a fake backend, with what SlabObject says of one.
type fakeObject struct {
	id       types.Hash256
	size     uint64
	created  time.Time
	updated  time.Time
	metadata json.RawMessage
	slabs    []slabs.SlabSlice
}

// newFakeObject makes an object of the given key and slabs, its ID derived from
// both the way a real object is content-addressed.
func newFakeObject(dataKey [32]byte, ss []slabs.SlabSlice) *fakeObject {
	var size uint64
	for _, s := range ss {
		size += uint64(s.Length)
	}

	return &fakeObject{
		id:    types.HashBytes(fmt.Appendf(nil, "%v%v", dataKey, ss)),
		size:  size,
		slabs: ss,
	}
}

func (o *fakeObject) ID() types.Hash256         { return o.id }
func (o *fakeObject) Size() uint64              { return o.size }
func (o *fakeObject) CreatedAt() time.Time      { return o.created }
func (o *fakeObject) UpdatedAt() time.Time      { return o.updated }
func (o *fakeObject) Metadata() json.RawMessage { return o.metadata }

// sealFake is the sealed form the test fakes exchange: the identity in the
// clear where a real seal holds ciphertext.
func sealFake(o *fakeObject) slabs.SealedObject {
	return slabs.SealedObject{
		EncryptedDataKey:  o.id[:],
		Slabs:             o.slabs,
		EncryptedMetadata: o.metadata,
		CreatedAt:         o.created,
		UpdatedAt:         o.updated,
	}
}

// testUnsealer opens what sealFake sealed, as the source account would.
type testUnsealer struct{}

// ObjectFromSealed implements ObjectUnsealer.
func (testUnsealer) ObjectFromSealed(sealed slabs.SealedObject) (SlabObject, error) {
	var id types.Hash256
	copy(id[:], sealed.EncryptedDataKey)
	var size uint64
	for _, s := range sealed.Slabs {
		size += uint64(s.Length)
	}

	return &fakeObject{
		id:       id,
		size:     size,
		created:  sealed.CreatedAt,
		updated:  sealed.UpdatedAt,
		metadata: sealed.EncryptedMetadata,
		slabs:    sealed.Slabs,
	}, nil
}
