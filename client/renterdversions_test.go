package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/renterd/v2/api"
)

// renterdBucket is a renterd that holds objects: enough of the bus and worker
// API for the versions to be kept, moved, copied, read and deleted.
type renterdBucket struct {
	mu      sync.Mutex
	objects map[string]api.ObjectMetadata
	uploads int
}

// serve answers the request out of the bucket.
func (b *renterdBucket) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()

	body, _ := io.ReadAll(r.Body)

	switch {
	case r.URL.Path == "/api/bus/objects/rename":
		var req api.ObjectsRenameRequest
		json.Unmarshal(body, &req)
		if req.Mode == api.ObjectsRenameModeMulti {
			for key, o := range b.objects {
				if strings.HasPrefix(key, req.From) {
					delete(b.objects, key)
					o.Key = req.To + strings.TrimPrefix(key, req.From)
					b.objects[o.Key] = o
				}
			}
			return
		}
		o, ok := b.objects[req.From]
		if !ok {
			http.Error(w, "object not found", http.StatusNotFound)
			return
		}
		delete(b.objects, req.From)
		o.Key = req.To
		b.objects[req.To] = o
	case r.URL.Path == "/api/bus/objects/copy":
		var req api.CopyObjectsRequest
		json.Unmarshal(body, &req)
		o, ok := b.objects[req.SourceKey]
		if !ok {
			http.Error(w, "object not found", http.StatusNotFound)
			return
		}
		o.Key, o.ModTime = req.DestinationKey, api.TimeRFC3339(time.Now())
		b.objects[req.DestinationKey] = o
	case r.URL.Path == "/api/bus/multipart/create":
		b.uploads++
		json.NewEncoder(w).Encode(api.MultipartCreateResponse{UploadID: fmt.Sprintf("upload-%d", b.uploads)})
	case r.URL.Path == "/api/bus/multipart/complete":
		var req api.MultipartCompleteRequest
		json.Unmarshal(body, &req)
		b.objects[req.Key] = api.ObjectMetadata{Key: req.Key, Size: 1, ModTime: api.TimeRFC3339(time.Now())}
		json.NewEncoder(w).Encode(api.MultipartCompleteResponse{})
	case r.URL.Path == "/api/worker/objects/remove":
		var req api.ObjectsRemoveRequest
		json.Unmarshal(body, &req)
		for key := range b.objects {
			if strings.HasPrefix(key, req.Prefix) {
				delete(b.objects, key)
			}
		}
	case strings.HasPrefix(r.URL.Path, "/api/worker/object/"):
		key := strings.TrimPrefix(r.URL.Path, "/api/worker/object")
		if _, ok := b.objects[key]; !ok {
			http.Error(w, "object not found", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			delete(b.objects, key)
			return
		}
		w.Write([]byte("contents of " + key))
	case strings.HasPrefix(r.URL.Path, "/api/bus/objects"):
		prefix := strings.TrimPrefix(r.URL.Path, "/api/bus/objects")
		if !strings.HasPrefix(prefix, "/") {
			prefix = "/" + prefix
		}
		prefix = strings.TrimSuffix(prefix, "/") + "/"
		folded := r.URL.Query().Get("delimiter") != ""
		var res api.ObjectsResponse
		dirs := make(map[string]bool)
		for key, o := range b.objects {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if i := strings.Index(key[len(prefix):], "/"); i >= 0 && folded {
				dir := key[:len(prefix)+i+1]
				if !dirs[dir] {
					dirs[dir] = true
					res.Objects = append(res.Objects, api.ObjectMetadata{Key: dir})
				}
				continue
			}
			res.Objects = append(res.Objects, o)
		}
		sort.Slice(res.Objects, func(i, j int) bool { return res.Objects[i].Key < res.Objects[j].Key })
		json.NewEncoder(w).Encode(res)
	default:
		w.Write([]byte("{}"))
	}
}

// put places an object in the bucket.
func (b *renterdBucket) put(key string, size int64, modified time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = api.ObjectMetadata{Key: key, Size: size, ModTime: api.TimeRFC3339(modified)}
}

// keys lists the objects of the bucket, sorted.
func (b *renterdBucket) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.objects))
	for key := range b.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// versionedRenterd is a renterd client keeping versions on a bucket, under
// the policy, with an account to act as.
func versionedRenterd(t *testing.T, policy stores.VersionPolicy) (*RenterdVersions, *renterdBucket, stores.Account) {
	t.Helper()

	ctx := context.Background()
	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)
	db.SetVersioning(policy)
	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")

	bucket := &renterdBucket{objects: make(map[string]api.ObjectMetadata)}
	f := newFakeRenterd(t)
	f.answerWith(bucket.serve)
	rv := WithRenterdVersions(f.client("default"), db, share.Name).(*RenterdVersions)
	return rv, bucket, acc
}

// save writes the file anew, as a client saving it does.
func save(t *testing.T, rv *RenterdVersions, acc stores.Account, path string) {
	t.Helper()

	ctx := context.Background()
	id, err := rv.StartUpload(ctx, acc, path)
	if err != nil {
		t.Fatalf("StartUpload(%s): %v", path, err)
	}
	if err := rv.FinishUpload(ctx, path, id, nil); err != nil {
		t.Fatalf("FinishUpload(%s): %v", path, err)
	}
}

// sizesOf lists the sizes of the versions of the file, newest first.
func sizesOf(t *testing.T, rv *RenterdVersions, acc stores.Account, path string) []uint64 {
	t.Helper()

	versions, err := rv.Versions(context.Background(), acc, path)
	if err != nil {
		t.Fatalf("Versions(%s): %v", path, err)
	}
	sizes := make([]uint64, 0, len(versions))
	for _, v := range versions {
		sizes = append(sizes, v.Size)
	}
	return sizes
}

// TestRenterdVersionsKeepTheReplacedFile verifies that a save, a deletion and
// a restore leave the versions the policy says, held in the bucket under their
// IDs, and readable and restorable from there.
func TestRenterdVersionsKeepTheReplacedFile(t *testing.T) {
	rv, bucket, acc := versionedRenterd(t, stores.VersionPolicy{Keep: true, Gap: 10 * time.Minute, MaxSession: time.Hour, Count: 10})
	ctx := context.Background()
	modified := time.Now().Add(-time.Hour)
	bucket.put("/doc.txt", 400, modified)

	save(t, rv, acc, "doc.txt")
	versions, err := rv.Versions(ctx, acc, "doc.txt")
	if err != nil || len(versions) != 1 || versions[0].Size != 400 || versions[0].ReplacedBy != "alice" || !versions[0].ModifiedAt.Equal(modified.Truncate(time.Microsecond)) {
		t.Fatalf("want the replaced file as the one version, got %+v, %v", versions, err)
	}
	first := versions[0].ID
	if keys := bucket.keys(); len(keys) != 2 || keys[0] != "/"+versionKey(first) || keys[1] != "/doc.txt" {
		t.Errorf("want the version under its ID beside the file, got %v", keys)
	}
	var buf bytes.Buffer
	if err := rv.ReadVersion(ctx, acc, "doc.txt", first, 0, 10, &buf); err != nil || buf.String() != "contents of /"+versionKey(first) {
		t.Errorf("want the version read from its object, got %q, %v", buf.String(), err)
	}
	if err := rv.ReadVersion(ctx, acc, "other.txt", first, 0, 10, &buf); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("a version under another path: want ErrNotFound, got %v", err)
	}

	// A save within the session leaves nothing; a deletion leaves the file as a version.
	save(t, rv, acc, "doc.txt")
	if sizes := sizesOf(t, rv, acc, "doc.txt"); len(sizes) != 1 {
		t.Errorf("want the save within the session not kept, got %v", sizes)
	}
	if err := rv.Delete(ctx, acc, "doc.txt", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "doc.txt"); len(sizes) != 2 || sizes[0] != 1 {
		t.Errorf("want the deleted file as the newest version, got %v", sizes)
	}
	if keys := bucket.keys(); len(keys) != 2 || strings.HasSuffix(keys[1], "doc.txt") {
		t.Errorf("want the file gone and two versions held, got %v", keys)
	}
	if err := rv.Delete(ctx, acc, "doc.txt", false); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("deleting what is gone: want ErrNotFound, got %v", err)
	}

	// A restore copies the version onto the file, and notes it holds that version.
	if err := rv.RestoreVersion(ctx, acc, "doc.txt", first); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}
	if oi, err := rv.Object(ctx, acc, "doc.txt"); err != nil || oi.Size != 400 {
		t.Errorf("want the file back with the version's contents, got %+v, %v", oi, err)
	}
	if err := rv.RestoreVersion(ctx, acc, "other.txt", first); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("restoring under another path: want ErrNotFound, got %v", err)
	}
	if err := rv.Delete(ctx, acc, "doc.txt", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "doc.txt"); len(sizes) != 2 {
		t.Errorf("want the restored version not kept again, got %v", sizes)
	}
	if keys := bucket.keys(); len(keys) != 2 {
		t.Errorf("want the file deleted outright, got %v", keys)
	}

	// The versions go on request, objects and all.
	if err := rv.KeepVersion(ctx, acc, "doc.txt", first, true); err != nil {
		t.Fatalf("KeepVersion: %v", err)
	}
	if versions, _ := rv.Versions(ctx, acc, "doc.txt"); !versions[1].Kept {
		t.Errorf("want the version kept, got %+v", versions)
	}
	if err := rv.DeleteVersions(ctx, acc, "doc.txt", 0); err != nil {
		t.Fatalf("DeleteVersions: %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "doc.txt"); len(sizes) != 0 {
		t.Errorf("want no versions left, got %v", sizes)
	}
	if keys := bucket.keys(); len(keys) != 0 {
		t.Errorf("want the bucket empty, got %v", keys)
	}
}

// TestRenterdVersionsFollowRenames verifies that the versions move with the
// file and the folder, that a file renamed over is kept, and that the swap
// save of an editor leaves the old file as a version under the name.
func TestRenterdVersionsFollowRenames(t *testing.T) {
	rv, bucket, acc := versionedRenterd(t, stores.VersionPolicy{Keep: true, Count: 10})
	ctx := context.Background()
	bucket.put("/a/x.txt", 100, time.Now().Add(-time.Hour))
	bucket.put("/a/w.txt", 200, time.Now().Add(-time.Hour))
	save(t, rv, acc, "a/x.txt")

	if err := rv.Rename(ctx, acc, "a/x.txt", "a/z.txt", false, false); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "a/z.txt"); len(sizes) != 1 || sizes[0] != 100 {
		t.Errorf("want the version under the new name, got %v", sizes)
	}
	if err := rv.Rename(ctx, acc, "a", "b", true, false); err != nil {
		t.Fatalf("Rename(dir): %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "b/z.txt"); len(sizes) != 1 {
		t.Errorf("want the version under the renamed folder, got %v", sizes)
	}

	// Renamed over, the file at the name becomes a version of it, joined by the versions moving in.
	if err := rv.Rename(ctx, acc, "b/z.txt", "b/w.txt", false, true); err != nil {
		t.Fatalf("Rename(over): %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "b/w.txt"); len(sizes) != 2 || sizes[0] != 200 || sizes[1] != 100 {
		t.Errorf("want the file renamed over as the newest version, got %v", sizes)
	}

	// The swap save: the new contents take the name, the old file is deleted under another.
	save(t, rv, acc, "b/tmp.txt")
	if err := rv.Rename(ctx, acc, "b/w.txt", "b/old.txt", false, false); err != nil {
		t.Fatalf("Rename(away): %v", err)
	}
	if err := rv.Rename(ctx, acc, "b/tmp.txt", "b/w.txt", false, false); err != nil {
		t.Fatalf("Rename(in): %v", err)
	}
	if err := rv.Delete(ctx, acc, "b/old.txt", false); err != nil {
		t.Fatalf("Delete(old): %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "b/w.txt"); len(sizes) != 3 || sizes[0] != 1 {
		t.Errorf("want the old file as the newest version under the name, got %v", sizes)
	}
	if sizes := sizesOf(t, rv, acc, "b/old.txt"); len(sizes) != 0 {
		t.Errorf("want nothing under the temporary name, got %v", sizes)
	}
	keys := bucket.keys()
	if len(keys) != 4 || keys[3] != "/b/w.txt" {
		t.Errorf("want the file and three versions, got %v", keys)
	}

	// The folder takes the versions with it.
	if err := rv.Delete(ctx, acc, "b", true); err != nil {
		t.Fatalf("Delete(dir): %v", err)
	}
	if sizes := sizesOf(t, rv, acc, "b/w.txt"); len(sizes) != 0 {
		t.Errorf("want the versions gone with the folder, got %v", sizes)
	}
	if keys := bucket.keys(); len(keys) != 0 {
		t.Errorf("want the bucket empty, got %v", keys)
	}
}

// TestRenterdVersionsAtATime verifies the folder as it was: the copy of each
// file current at the time, deleted files and their folders included, and the
// times it has previous states at.
func TestRenterdVersionsAtATime(t *testing.T) {
	rv, bucket, acc := versionedRenterd(t, stores.VersionPolicy{Keep: true, Count: 10})
	ctx := context.Background()
	now := time.Now()
	bucket.put("/docs/doc.txt", 400, now.Add(-2*time.Hour))
	bucket.put("/docs/gone.txt", 300, now.Add(-4*time.Hour))
	bucket.put("/docs/old/thing.txt", 200, now.Add(-6*time.Hour))
	bucket.put("/docs/plain.txt", 100, now.Add(-5*time.Hour))
	bucket.put("/docs/sub/deep.txt", 50, now)
	save(t, rv, acc, "docs/doc.txt")
	for _, p := range []string{"docs/gone.txt", "docs/old/thing.txt"} {
		if err := rv.Delete(ctx, acc, p, false); err != nil {
			t.Fatalf("Delete(%s): %v", p, err)
		}
	}

	then := now.Add(-time.Hour)
	v, err := rv.ObjectAt(ctx, acc, "docs/doc.txt", then)
	if err != nil || v.ID == 0 || v.Size != 400 {
		t.Errorf("the replaced file at the time: want its version, got %+v, %v", v, err)
	}
	if v, err := rv.ObjectAt(ctx, acc, "docs/doc.txt", now.Add(time.Hour)); err != nil || v.ID != 0 || v.Size != 1 {
		t.Errorf("the file at a later time: want the file itself, got %+v, %v", v, err)
	}
	if _, err := rv.ObjectAt(ctx, acc, "docs/doc.txt", now.Add(-3*time.Hour)); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("before the file was made: want ErrNotFound, got %v", err)
	}
	if v, err := rv.ObjectAt(ctx, acc, "docs/gone.txt", then); err != nil || v.ID == 0 || v.Size != 300 {
		t.Errorf("the deleted file at the time: want its version, got %+v, %v", v, err)
	}
	if _, err := rv.ObjectAt(ctx, acc, "docs/gone.txt", now.Add(time.Hour)); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("the deleted file after its deletion: want ErrNotFound, got %v", err)
	}

	ois, err := rv.ListAt(ctx, acc, "docs", then)
	if err != nil {
		t.Fatalf("ListAt: %v", err)
	}
	keys := func(ois []ObjectInfo) string {
		got := make([]string, 0, len(ois))
		for _, oi := range ois {
			got = append(got, oi.Key)
		}
		return strings.Join(got, " ")
	}
	if want := "/docs/doc.txt /docs/gone.txt /docs/old/ /docs/plain.txt"; keys(ois) != want {
		t.Errorf("want the folder as it was %q, got %q", want, keys(ois))
	}
	if ois[0].Size != 400 || ois[1].Size != 300 || ois[3].Size != 100 {
		t.Errorf("want the sizes of the time, got %+v", ois)
	}
	if ois, _ := rv.ListAt(ctx, acc, "docs", now.Add(time.Hour)); keys(ois) != "/docs/doc.txt /docs/plain.txt /docs/sub/" {
		t.Errorf("the folder at a later time: want the files and the folder made since, got %q", keys(ois))
	}
	if _, err := rv.ListAt(ctx, acc, "docs", now.Add(-7*time.Hour)); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("before anything was made: want ErrNotFound, got %v", err)
	}

	times, err := rv.Snapshots(ctx, acc, "docs")
	if err != nil || len(times) != 3 || !times[0].Equal(now.Add(-2*time.Hour).Truncate(time.Second)) {
		t.Errorf("want the three modification times newest first, got %v, %v", times, err)
	}
	if n, err := rv.ExpireVersions(ctx, 0); err != nil || n != 0 {
		t.Errorf("ExpireVersions(0): want nothing dropped, got %d, %v", n, err)
	}
}

// TestCopyAt verifies the choice of the copy current at a time, the save not
// kept within a session resolving to the earliest kept contents after it.
// TestRenterdVersionsHideTheirFolder verifies that the server's folder, where
// the versions are kept, is neither seen nor reached by a client: not listed,
// not there to look up or read, and not to be made, written, renamed or deleted.
func TestRenterdVersionsHideTheirFolder(t *testing.T) {
	rv, bucket, acc := versionedRenterd(t, stores.VersionPolicy{Keep: true, Count: 10})
	ctx := context.Background()
	bucket.put("/doc.txt", 400, time.Now().Add(-time.Hour))
	save(t, rv, acc, "doc.txt")
	object := strings.TrimPrefix(bucket.keys()[0], "/")
	if !strings.HasPrefix(object, renterdVersionsFolder+"/") {
		t.Fatalf("want the version kept under %s, got %q", renterdVersionsFolder, object)
	}

	ois, err := rv.List(ctx, acc, "")
	if err != nil || len(ois) != 1 || ois[0].Key != "/doc.txt" {
		t.Errorf("want the root listing the file alone, got %+v, %v", ois, err)
	}
	ois, err = rv.ListAt(ctx, acc, "", time.Now().Add(time.Hour))
	if err != nil || len(ois) != 1 || ois[0].Key != "/doc.txt" {
		t.Errorf("want the root as it was listing the file alone, got %+v, %v", ois, err)
	}

	for _, p := range []string{renterdReservedFolder, renterdVersionsFolder, object} {
		if _, err := rv.Object(ctx, acc, p); !errors.Is(err, stores.ErrNotFound) {
			t.Errorf("Object(%s): want ErrNotFound, got %v", p, err)
		}
		if _, err := rv.List(ctx, acc, p); !errors.Is(err, stores.ErrNotFound) {
			t.Errorf("List(%s): want ErrNotFound, got %v", p, err)
		}
		if err := rv.Read(ctx, acc, p, 0, 10, io.Discard); !errors.Is(err, stores.ErrNotFound) {
			t.Errorf("Read(%s): want ErrNotFound, got %v", p, err)
		}
		if err := rv.Delete(ctx, acc, p, false); !errors.Is(err, stores.ErrNotFound) {
			t.Errorf("Delete(%s): want ErrNotFound, got %v", p, err)
		}
		if _, err := rv.Versions(ctx, acc, p); !errors.Is(err, stores.ErrNotFound) {
			t.Errorf("Versions(%s): want ErrNotFound, got %v", p, err)
		}
	}
	if err := rv.Delete(ctx, acc, renterdReservedFolder, true); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("deleting the folder: want ErrNotFound, got %v", err)
	}
	if err := rv.MakeDirectory(ctx, acc, renterdReservedFolder); !errors.Is(err, stores.ErrDirectoryExists) {
		t.Errorf("making the folder: want ErrDirectoryExists, got %v", err)
	}
	if _, err := rv.StartUpload(ctx, acc, renterdReservedFolder+"/new.txt"); !errors.Is(err, stores.ErrAccessDenied) {
		t.Errorf("writing into the folder: want ErrAccessDenied, got %v", err)
	}
	if err := rv.Rename(ctx, acc, "doc.txt", renterdReservedFolder+"/doc.txt", false, false); !errors.Is(err, stores.ErrAccessDenied) {
		t.Errorf("renaming into the folder: want ErrAccessDenied, got %v", err)
	}
	if keys := bucket.keys(); len(keys) != 2 || keys[0] != "/"+object || keys[1] != "/doc.txt" {
		t.Errorf("want the bucket untouched, got %v", keys)
	}
}

func TestCopyAt(t *testing.T) {
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	copies := []VersionInfo{
		{ID: 1, Size: 1, ModifiedAt: at(0), ReplacedAt: at(1)},
		{ID: 2, Size: 2, ModifiedAt: at(2), ReplacedAt: at(3)},
		{Size: 3, ModifiedAt: at(3)},
	}
	tests := []struct {
		name string
		at   time.Time
		want uint64
		ok   bool
	}{
		{"before the file", at(-1), 0, false},
		{"the first contents", at(0).Add(30 * time.Minute), 1, true},
		{"a save not kept", at(1).Add(30 * time.Minute), 2, true},
		{"the second contents", at(2).Add(30 * time.Minute), 2, true},
		{"the file as it is", at(4), 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, ok := copyAt(copies, tt.at)
			if ok != tt.ok || v.Size != tt.want {
				t.Errorf("want size %d, %v; got %+v, %v", tt.want, tt.ok, v, ok)
			}
		})
	}
}
