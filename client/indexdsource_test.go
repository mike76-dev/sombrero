package client

import (
	"bytes"
	"context"
	"errors"
	"path"
	"sort"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
	sdk "go.sia.tech/siastorage"
)

// fakeAccount stands in for an indexd account: the log of what it pinned, and
// the objects that log names.
type fakeAccount struct {
	events  []PinnedObject
	objects map[types.Hash256]sdk.Object
	err     error
}

// pin adds an object of the given slab lengths to the account, and an event for
// it, the way pinning one does.
func (fa *fakeAccount) pin(key types.Hash256, at time.Time, lengths ...uint32) {
	if fa.objects == nil {
		fa.objects = make(map[types.Hash256]sdk.Object)
	}

	var size uint64
	ss := make([]slabs.SlabSlice, 0, len(lengths))
	for i, length := range lengths {
		ss = append(ss, slabs.SlabSlice{
			Version:       1,
			EncryptionKey: slabs.EncryptionKey{byte(i + 1)},
			MinShards:     2,
			Sectors:       []slabs.PinnedSector{{Root: types.Hash256{byte(i + 1)}, HostKey: types.PublicKey{byte(i + 2)}}},
			Length:        length,
		})
		size += uint64(length)
	}

	fa.objects[key] = sdk.NewUnsafeObject([32]byte{key[0]}, ss)
	fa.events = append(fa.events, PinnedObject{Key: key, Size: size, UpdatedAt: at})
}

// unpin records that the object is gone, which is what the log keeps instead of
// forgetting it.
func (fa *fakeAccount) unpin(key types.Hash256, at time.Time) {
	fa.events = append(fa.events, PinnedObject{Key: key, UpdatedAt: at, Deleted: true})
}

func (fa *fakeAccount) ListObjects(ctx context.Context, cursor slabs.Cursor, limit int) ([]PinnedObject, error) {
	if fa.err != nil {
		return nil, fa.err
	}

	log := append([]PinnedObject(nil), fa.events...)
	sort.Slice(log, func(i, j int) bool {
		if !log[i].UpdatedAt.Equal(log[j].UpdatedAt) {
			return log[i].UpdatedAt.Before(log[j].UpdatedAt)
		}
		return bytes.Compare(log[i].Key[:], log[j].Key[:]) < 0
	})

	page := make([]PinnedObject, 0, limit)
	for _, ev := range log {
		after := ev.UpdatedAt.After(cursor.After) ||
			(ev.UpdatedAt.Equal(cursor.After) && bytes.Compare(ev.Key[:], cursor.Key[:]) > 0)
		if !after {
			continue
		}
		page = append(page, ev)
		if len(page) >= limit {
			break
		}
	}

	return page, nil
}

func (fa *fakeAccount) Object(ctx context.Context, key types.Hash256) (sdk.Object, error) {
	obj, ok := fa.objects[key]
	if !ok {
		return sdk.Object{}, errors.New("no such object")
	}

	return obj, nil
}

// TestDescribeAccount verifies what describing an indexd account comes to: the
// objects it holds now, each with what it takes to take them over.
func TestDescribeAccount(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	kept, dropped, empty := types.Hash256{1}, types.Hash256{2}, types.Hash256{3}

	fa := &fakeAccount{}
	fa.pin(kept, at, 1<<20, 2<<20)
	fa.pin(dropped, at.Add(time.Second))
	fa.pin(empty, at.Add(2*time.Second))

	// What the account has dropped is in the log as well, and is not what it
	// holds now.
	fa.unpin(dropped, at.Add(3*time.Second))

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{Source: "indexd", Origin: "http://indexer"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	stats, err := DescribeAccount(context.Background(), fa, w, "http://indexer", "")
	if err != nil {
		t.Fatalf("DescribeAccount: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stats.Files != 2 || stats.Directories != 1 {
		t.Errorf("stats: want 2 files under the one folder, got %+v", stats)
	}

	dirs, files := described(t, buf.Bytes())
	if len(dirs) != 1 || dirs[0].Path != LostAndFound {
		t.Fatalf("folders: want %q, got %+v", LostAndFound, dirs)
	}

	byPath := make(map[string]transfer.File, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	if _, ok := byPath[path.Join(LostAndFound, dropped.String())]; ok {
		t.Error("an object the account dropped was described")
	}

	file, ok := byPath[path.Join(LostAndFound, kept.String())]
	if !ok {
		t.Fatalf("the object the account holds was not described: %+v", files)
	}
	if file.Size != 3<<20 || len(file.Parts) != 1 {
		t.Fatalf("the described object: got size %d in %d part(s)", file.Size, len(file.Parts))
	}
	if !file.Complete() {
		t.Error("the described object does not cover itself")
	}

	// The part carries what it takes to pin the object as it is, and where to
	// fetch its bytes if pinning is not to be had.
	part := file.Parts[0]
	if part.Pin == nil {
		t.Fatal("the part says nothing about pinning the object")
	}
	if part.Pin.DataKey != [32]byte{kept[0]} {
		t.Errorf("the data key of the part: got %x", part.Pin.DataKey)
	}
	if len(part.Pin.Slabs) != 2 || part.Pin.Slabs[0].Length != 1<<20 || part.Pin.Slabs[1].Length != 2<<20 {
		t.Errorf("the slabs of the part: got %+v", part.Pin.Slabs)
	}
	if part.Pin.Slabs[0].Sectors[0].Root != (types.Hash256{1}) {
		t.Errorf("the sectors of the part: got %+v", part.Pin.Slabs[0].Sectors)
	}
	if part.Source == nil || part.Source.Kind != "indexd" || part.Source.Origin != "http://indexer" {
		t.Errorf("the source of the part: got %+v", part.Source)
	}
	if part.Source.Key != kept.String() {
		t.Errorf("the key of the part: want the object, got %q", part.Source.Key)
	}

	// An object of no bytes is described by no part, the same as an empty file.
	if file := byPath[path.Join(LostAndFound, empty.String())]; len(file.Parts) != 0 {
		t.Errorf("the empty object: got %d part(s)", len(file.Parts))
	}
}

// TestDescribeAccountUnderAPrefix verifies that the objects go where the caller
// asks for them, since a description of them is applied beside files that have
// names of their own.
func TestDescribeAccountUnderAPrefix(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	key := types.Hash256{7}

	fa := &fakeAccount{}
	fa.pin(key, at, 1<<10)

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{Source: "indexd"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := DescribeAccount(context.Background(), fa, w, "http://indexer", "/taken-over"); err != nil {
		t.Fatalf("DescribeAccount: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dirs, files := described(t, buf.Bytes())
	if len(dirs) != 1 || dirs[0].Path != "/taken-over" {
		t.Errorf("folders: want the prefix that was asked for, got %+v", dirs)
	}
	if len(files) != 1 || files[0].Path != "/taken-over/"+key.String() {
		t.Errorf("files: want the object under the prefix, got %+v", files)
	}
}

// TestDescribeAccountFailures verifies that a walk which cannot be finished is
// reported rather than written as a description of less than there is.
func TestDescribeAccountFailures(t *testing.T) {
	at := time.Now().UTC()
	fa := &fakeAccount{}
	fa.pin(types.Hash256{1}, at, 1<<10)

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{Source: "indexd"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DescribeAccount(ctx, fa, w, "http://indexer", ""); err == nil {
		t.Error("a walk nobody is waiting for went on")
	}

	// An object in the log that the account will not hand over is a walk that
	// cannot be finished.
	fa.events = append(fa.events, PinnedObject{Key: types.Hash256{9}, UpdatedAt: at.Add(time.Second)})
	if _, err := DescribeAccount(context.Background(), fa, w, "http://indexer", ""); err == nil {
		t.Error("an object that could not be retrieved was described anyway")
	}

	fa.err = errors.New("the indexer is not answering")
	if _, err := DescribeAccount(context.Background(), fa, w, "http://indexer", ""); err == nil {
		t.Error("an account that could not be listed was described anyway")
	}
}
