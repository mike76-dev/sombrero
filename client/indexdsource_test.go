package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
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
	pins    int
	err     error
}

// pin adds an object of the given slab lengths to the account, and an event for
// it, the way pinning one does: the key of an object is a hash of its slabs, so
// it is what the object turns out to be rather than something chosen for it.
func (fa *fakeAccount) pin(at time.Time, meta json.RawMessage, lengths ...uint32) types.Hash256 {
	if fa.objects == nil {
		fa.objects = make(map[types.Hash256]sdk.Object)
	}
	fa.pins++

	var size uint64
	ss := make([]slabs.SlabSlice, 0, len(lengths))
	for i, length := range lengths {
		ss = append(ss, slabs.SlabSlice{
			Version:       1,
			EncryptionKey: slabs.EncryptionKey{byte(fa.pins), byte(i + 1)},
			MinShards:     2,
			Sectors:       []slabs.PinnedSector{{Root: types.Hash256{byte(fa.pins), byte(i + 1)}, HostKey: types.PublicKey{byte(i + 2)}}},
			Length:        length,
		})
		size += uint64(length)
	}

	obj := sdk.NewUnsafeObject([32]byte{byte(fa.pins)}, ss)
	if len(meta) > 0 {
		obj.UpdateMetadata(meta)
	}

	key := obj.ID()
	fa.objects[key] = obj
	fa.events = append(fa.events, PinnedObject{Key: key, Size: size, UpdatedAt: at})

	return key
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

		// The real log hands over the object along with the event, which is
		// what saves asking after each of them.
		if obj, ok := fa.objects[ev.Key]; ok && !ev.Deleted {
			ev.Object = &obj
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

// TestDescribeAccount verifies what describing an untagged indexd account comes
// to: the objects it holds now, each with what it takes to take them over.
func TestDescribeAccount(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	fa := &fakeAccount{}
	kept := fa.pin(at, nil, 1<<20, 2<<20)
	dropped := fa.pin(at.Add(time.Second), nil, 4096)

	// An object of no slabs is the same object whatever it was made of, its key
	// being a hash of them, so there is only ever one of it.
	empty := fa.pin(at.Add(2*time.Second), nil)

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
	if part.Pin.DataKey != [32]byte{1} {
		t.Errorf("the data key of the part: got %x", part.Pin.DataKey)
	}
	if len(part.Pin.Slabs) != 2 || part.Pin.Slabs[0].Length != 1<<20 || part.Pin.Slabs[1].Length != 2<<20 {
		t.Errorf("the slabs of the part: got %+v", part.Pin.Slabs)
	}
	if part.Pin.Slabs[0].Sectors[0].Root != (types.Hash256{1, 1}) {
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

	fa := &fakeAccount{}
	key := fa.pin(at, nil, 1<<10)

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

// tagOf is the tag an object written by a tagging server carries.
func tagOf(t *testing.T, pieces ...objectPiece) json.RawMessage {
	t.Helper()

	meta, err := json.Marshal(objectTag{Version: objectTagVersion, Pieces: pieces})
	if err != nil {
		t.Fatalf("the tag would not encode: %v", err)
	}

	return meta
}

// TestDescribeAccountReadsTheTags verifies what an account whose objects say what
// they hold comes to: the files themselves, under the names they went by, rather
// than the objects they were stored as.
func TestDescribeAccountReadsTheTags(t *testing.T) {
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	fa := &fakeAccount{}

	// A large file in two objects of its own, and two small ones packed into a
	// third together with that file's tail.
	fa.pin(at, tagOf(t, objectPiece{Share: "s", Path: "/big.mp4", Offset: 0, At: 0, Length: 1 << 20, Size: 2<<20 + 30}), 1<<20)
	fa.pin(at.Add(time.Second), tagOf(t, objectPiece{Share: "s", Path: "/big.mp4", Offset: 1 << 20, At: 0, Length: 1 << 20, Size: 2<<20 + 30}), 1<<20)
	fa.pin(at.Add(2*time.Second), tagOf(t,
		objectPiece{Share: "s", Path: "/big.mp4", Offset: 2 << 20, At: 0, Length: 30, Size: 2<<20 + 30},
		objectPiece{Share: "s", Path: "/notes/one.txt", Offset: 0, At: 30, Length: 11, Size: 11},
		objectPiece{Share: "s", Path: "/notes/two.txt", Offset: 0, At: 41, Length: 7, Size: 7},
	), 48)

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{Source: "indexd"})
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
	if stats.Files != 3 || stats.Directories != 0 {
		t.Fatalf("stats: want the 3 files the objects named and no lost+found, got %+v", stats)
	}

	_, files := described(t, buf.Bytes())
	byPath := make(map[string]transfer.File, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}

	// The large file is itself again: its runs in order, covering it whole.
	big, ok := byPath["/big.mp4"]
	if !ok {
		t.Fatalf("the file the objects named was not described: %+v", files)
	}
	if big.Size != 2<<20+30 || len(big.Parts) != 3 {
		t.Fatalf("the described file: got size %d in %d part(s)", big.Size, len(big.Parts))
	}
	if !big.Complete() {
		t.Errorf("the described file has holes in it: %+v", big.Gaps())
	}
	for i, part := range big.Parts {
		if part.Pin == nil || part.Source == nil {
			t.Errorf("part %d says nothing about where its bytes are", i)
		}
		if part.Object != (types.Hash256{}) {
			t.Errorf("part %d names an object this account has not pinned", i)
		}
	}

	// The tail of the large file sits in the object that holds the small ones,
	// at the offset the tag gave it.
	if tail := big.Parts[2]; tail.Offset != 2<<20 || tail.DataOffset != 0 || tail.Length != 30 {
		t.Errorf("the tail of the file: got %+v", tail)
	}

	// The small files are described as themselves, each the run of the packed
	// object that is theirs.
	one, ok := byPath["/notes/one.txt"]
	if !ok {
		t.Fatalf("a packed file was not described: %+v", files)
	}
	if one.Size != 11 || len(one.Parts) != 1 || one.Parts[0].DataOffset != 30 || one.Parts[0].Length != 11 {
		t.Errorf("the packed file: got size %d, parts %+v", one.Size, one.Parts)
	}
	if two := byPath["/notes/two.txt"]; two.Parts[0].DataOffset != 41 {
		t.Errorf("the other packed file starts at %d in the object, want 41", two.Parts[0].DataOffset)
	}
}

// TestDescribeAccountMixesTaggedAndNot verifies that an account written to both
// before and after tagging is described for what each of its objects can say.
func TestDescribeAccountMixesTaggedAndNot(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	fa := &fakeAccount{}

	fa.pin(at, tagOf(t, objectPiece{Share: "s", Path: "/named.bin", Length: 64, Size: 64}), 64)
	older := fa.pin(at.Add(time.Second), nil, 128)

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{Source: "indexd"})
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
		t.Fatalf("stats: want both files and the one lost+found folder, got %+v", stats)
	}

	dirs, files := described(t, buf.Bytes())
	if len(dirs) != 1 || dirs[0].Path != LostAndFound {
		t.Errorf("folders: want %q for what could not be named, got %+v", LostAndFound, dirs)
	}

	var paths []string
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	if !slices.Contains(paths, "/named.bin") {
		t.Errorf("the tagged object was not described as its file: %v", paths)
	}
	if !slices.Contains(paths, path.Join(LostAndFound, older.String())) {
		t.Errorf("the untagged object was not described as itself: %v", paths)
	}
}

// TestCountAccount verifies that what an import has to get through is counted
// for what it is: a file for every one the tags name, however many objects hold
// it, and a slab for every object that names nothing.
func TestCountAccount(t *testing.T) {
	at := time.Now().UTC()
	fa := &fakeAccount{}

	// One tagged object of two files, a second object of one of the same files,
	// and two that say nothing.
	fa.pin(at, tagOf(t,
		objectPiece{Share: "s", Path: "/a.bin", Length: 32, Size: 32},
		objectPiece{Share: "s", Path: "/b.bin", Length: 32, Size: 96},
	), 64)
	fa.pin(at.Add(time.Second), tagOf(t, objectPiece{Share: "s", Path: "/b.bin", Offset: 32, Length: 64, Size: 96}), 64)
	fa.pin(at.Add(2*time.Second), nil, 128)
	fa.pin(at.Add(3*time.Second), nil, 128)

	count, err := CountAccount(context.Background(), fa)
	if err != nil {
		t.Fatalf("CountAccount: %v", err)
	}
	if count != (Count{Files: 2, Slabs: 2}) {
		t.Errorf("want 2 files and 2 slabs, got %+v", count)
	}
	if count.Total() != 4 {
		t.Errorf("total: want 4, got %d", count.Total())
	}
}

// TestProbeAccount verifies what a look at a source reports, which is what
// decides whether an import of it is worth running at all.
func TestProbeAccount(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)

	t.Run("an account whose objects say nothing", func(t *testing.T) {
		fa := &fakeAccount{}
		for i := range 3 {
			fa.pin(at.Add(time.Duration(i)*time.Second), nil, uint32(1024*(i+1)))
		}

		probe, err := ProbeAccount(context.Background(), fa)
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if probe.Objects != 3 || probe.Looked != 3 || probe.Tagged != 0 {
			t.Errorf("the probe: got %+v, want 3 objects of which none are tagged", probe)
		}
	})

	t.Run("an account whose objects name their files", func(t *testing.T) {
		fa := &fakeAccount{}
		fa.pin(at, tagOf(t, objectPiece{Share: "s", Path: "/x.bin", Length: 512, Size: 512}), 512)
		fa.pin(at.Add(time.Second), nil, 1024)

		probe, err := ProbeAccount(context.Background(), fa)
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if probe.Objects != 2 || probe.Looked != 2 || probe.Tagged != 1 {
			t.Errorf("the probe: got %+v, want one of the two tagged", probe)
		}
	})

	t.Run("an account of more objects than are looked at", func(t *testing.T) {
		fa := &fakeAccount{}
		for i := range probeSample + 7 {
			fa.pin(at.Add(time.Duration(i)*time.Second), nil, uint32(512*(i+1)))
		}

		probe, err := ProbeAccount(context.Background(), fa)
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if probe.Objects != probeSample+7 || probe.Looked != probeSample {
			t.Errorf("the probe: got %+v, want all counted and %d looked at", probe, probeSample)
		}
	})

	t.Run("an account of a longer log than is read", func(t *testing.T) {
		fa := &fakeAccount{}
		for i := range probePages*objectPageSize + 50 {
			fa.pin(at.Add(time.Duration(i)*time.Second), nil, uint32(512+i))
		}

		probe, err := ProbeAccount(context.Background(), fa)
		if err != nil {
			t.Fatalf("ProbeAccount: %v", err)
		}
		if !probe.More {
			t.Error("a log that went on past the look was not reported as such")
		}
		if probe.Objects != probePages*objectPageSize {
			t.Errorf("the probe counted %d objects, want the %d it read", probe.Objects, probePages*objectPageSize)
		}
		if probe.Looked != probeSample {
			t.Errorf("the probe looked at %d objects, want %d", probe.Looked, probeSample)
		}
	})

	t.Run("an account that cannot be reached", func(t *testing.T) {
		fa := &fakeAccount{err: errors.New("the indexer is not answering")}
		if _, err := ProbeAccount(context.Background(), fa); err == nil {
			t.Error("an account that could not be listed was reported on anyway")
		}
	})
}

// TestDescribeAccountFailures verifies that a walk which cannot be finished is
// reported rather than written as a description of less than there is.
func TestDescribeAccountFailures(t *testing.T) {
	at := time.Now().UTC()
	fa := &fakeAccount{}
	fa.pin(at, nil, 1<<10)

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
