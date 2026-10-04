package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/v2/api"
)

// listing is what a fake renterd answers one listing with.
type listing []api.ObjectMetadata

// entry is a file of a listing.
func entry(key string, size int64, at time.Time) api.ObjectMetadata {
	return api.ObjectMetadata{Key: key, Size: size, ModTime: api.TimeRFC3339(at)}
}

// serveTree makes the fake renterd answer each listing with the entries of the
// folder that was asked for, which is what walking it goes by.
func serveTree(f *fakeRenterd, tree map[string]listing) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.raw = func(w http.ResponseWriter, r *http.Request) {
		key, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key = key[len("/api/bus/objects"):]
		if key == "" {
			key = "/"
		}

		entries, ok := tree[key]
		if !ok {
			http.Error(w, "no such folder: "+key, http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ObjectsResponse{Objects: entries})
	}
}

// described reads back what a walk wrote.
func described(t *testing.T, b []byte) ([]transfer.Directory, []transfer.File) {
	t.Helper()

	r, err := transfer.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	var dirs []transfer.Directory
	var files []transfer.File
	for {
		dir, file, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if dir != nil {
			dirs = append(dirs, *dir)
		}
		if file != nil {
			files = append(files, *file)
		}
	}

	return dirs, files
}

// TestRenterdDescribe verifies what walking a renterd bucket comes to: every
// folder and file it holds, with the bytes left where they are.
func TestRenterdDescribe(t *testing.T) {
	f := newFakeRenterd(t)
	defer f.Close()

	at := time.Now().UTC().Truncate(time.Second)
	serveTree(f, map[string]listing{
		"/": {entry("/holiday/", 0, at), entry("/readme.txt", 40, at)},
		"/holiday/": {
			entry("/holiday/empty/", 0, at),
			entry("/holiday/beach.raw", 1<<20, at),
			entry("/holiday/nothing.bin", 0, at),
		},
		"/holiday/empty/": {},
	})

	rc := NewRenterdClient(f.URL, "hunter2", "default").(*RenterdClient)
	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{CreatedAt: at, Source: "renterd", Origin: f.URL})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	stats, err := rc.Describe(context.Background(), w)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if stats.Directories != 2 || stats.Files != 3 {
		t.Errorf("stats: want 2 folders and 3 files, got %+v", stats)
	}

	dirs, files := described(t, buf.Bytes())

	// The folders are named without the slash they are listed with, and the one
	// that holds nothing is named too.
	wantDirs := []string{"/holiday", "/holiday/empty"}
	for i, want := range wantDirs {
		if i >= len(dirs) {
			t.Fatalf("folders: want %v, got %+v", wantDirs, dirs)
		}
		if dirs[i].Path != want {
			t.Errorf("folder %d: want %q, got %q", i, want, dirs[i].Path)
		}
	}

	byPath := make(map[string]transfer.File, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}

	beach, ok := byPath["/holiday/beach.raw"]
	if !ok {
		t.Fatalf("files: want the one in the folder, got %+v", files)
	}
	if beach.Size != 1<<20 || len(beach.Parts) != 1 {
		t.Fatalf("the described file: got size %d in %d part(s)", beach.Size, len(beach.Parts))
	}
	if !beach.Complete() {
		t.Error("the described file does not cover itself")
	}

	// The bytes stay where they are: the part says where to fetch them, and
	// nothing about pinning what holds them.
	part := beach.Parts[0]
	if part.Pin != nil || part.Object != (types.Hash256{}) {
		t.Errorf("the part claims to be this server's own: %+v", part)
	}
	if part.Source == nil {
		t.Fatal("the part says nothing about where its bytes are")
	}
	if part.Source.Kind != "renterd" || part.Source.Origin != f.URL || part.Source.Bucket != "default" {
		t.Errorf("the source of the part: got %+v", *part.Source)
	}
	if part.Source.Key != "/holiday/beach.raw" {
		t.Errorf("the key of the part: want the path of the file, got %q", part.Source.Key)
	}

	// A file of no bytes is made of no parts, which is not the same as one whose
	// parts are missing.
	nothing := byPath["/holiday/nothing.bin"]
	if len(nothing.Parts) != 0 || !nothing.Complete() {
		t.Errorf("the empty file: got %d part(s), complete %v", len(nothing.Parts), nothing.Complete())
	}
}

// TestRenterdDescribeStopsOnCancellation verifies that a walk of a bucket that
// nobody is waiting for any more stops.
func TestRenterdDescribeStopsOnCancellation(t *testing.T) {
	f := newFakeRenterd(t)
	defer f.Close()

	serveTree(f, map[string]listing{"/": {entry("/a.txt", 1, time.Now())}})
	rc := NewRenterdClient(f.URL, "hunter2", "default").(*RenterdClient)

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{Source: "renterd"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rc.Describe(ctx, w); err == nil {
		t.Error("a walk nobody is waiting for went on")
	}
}
