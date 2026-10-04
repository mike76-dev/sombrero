package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/renterd/v2/api"
)

// fakeShare stands in for a connection's client: the folders and files put into
// the share, kept in memory.
type fakeShare struct {
	mu      sync.Mutex
	dirs    map[string]bool
	files   map[string][]byte
	uploads map[string]string
	owners  map[string]string
}

func newFakeShare() *fakeShare {
	return &fakeShare{dirs: map[string]bool{}, files: map[string][]byte{}, uploads: map[string]string{}, owners: map[string]string{}}
}

func (f *fakeShare) MakeDirectory(_ context.Context, acc stores.Account, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[p] {
		return stores.ErrDirectoryExists
	}
	f.dirs[p] = true
	f.owners[p] = acc.Username
	return nil
}

func (f *fakeShare) StartUpload(_ context.Context, acc stores.Account, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("upload-%d", len(f.uploads)+1)
	f.uploads[id] = p
	f.owners[p] = acc.Username
	return id, nil
}

func (f *fakeShare) Write(_ context.Context, r io.Reader, p, uploadID string, _ int, _, _ uint64) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads[uploadID] != p {
		return "", errors.New("no such upload")
	}
	f.files[p] = data
	return "etag", nil
}

func (f *fakeShare) FinishUpload(_ context.Context, p, uploadID string, _ []api.MultipartCompletedPart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads[uploadID] != p {
		return errors.New("no such upload")
	}
	delete(f.uploads, uploadID)
	return nil
}

func (f *fakeShare) List(_ context.Context, _ stores.Account, dir string) ([]client.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ois []client.ObjectInfo
	for p := range f.files {
		if path.Dir(p) == dir {
			ois = append(ois, client.ObjectInfo{Key: p, Size: uint64(len(f.files[p]))})
		}
	}
	sort.Slice(ois, func(i, j int) bool { return ois[i].Key < ois[j].Key })
	return ois, nil
}

func (f *fakeShare) Delete(_ context.Context, _ stores.Account, p string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; !ok {
		return errors.New("no such file")
	}
	delete(f.files, p)
	return nil
}

// catalogs returns the catalog files in the share, newest last.
func (f *fakeShare) catalogs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for p := range f.files {
		if strings.HasPrefix(p, CatalogFolder+"/") {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	return names
}

// TestNetworkWritesSealedCatalogs verifies that each round puts a catalog into
// the share that only the app key opens, owned by an account of the workgroup,
// and that only the newest few stay.
func TestNetworkWritesSealedCatalogs(t *testing.T) {
	ctx := context.Background()
	db, wg, key := connectedStore(t, ctx)

	share := newFakeShare()
	n := &Network{
		db: db,
		clients: func(string) (map[string]ShareFiles, error) {
			return map[string]ShareFiles{wg.UUID.String(): share}, nil
		},
		keep:   2,
		inline: 1024,
		status: Status{Path: CatalogFolder, Keep: 2},
	}
	for range 3 {
		if err := n.WriteAll(ctx); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}
	}

	names := share.catalogs()
	if len(names) != 2 {
		t.Fatalf("the share holds %d catalog(s), want the newest 2: %v", len(names), names)
	}
	if !share.dirs[path.Dir(CatalogFolder)] || !share.dirs[CatalogFolder] || share.owners[CatalogFolder] != "alice" {
		t.Errorf("the folders: %v, owned by %q", share.dirs, share.owners[CatalogFolder])
	}

	status := n.Status()
	if len(status.Catalogs) != 1 || status.Catalogs[0].Path != names[1] || status.Error != "" {
		t.Errorf("status: got %+v", status)
	}

	// The file is sealed, and opens into a catalog of the connection.
	sealed := share.files[names[1]]
	if _, err := transfer.NewReader(bytes.NewReader(sealed)); err == nil {
		t.Error("the catalog in the share is in the clear")
	}
	plain, err := Open(key, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r, err := transfer.NewReader(bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if conn := r.Connection(); conn == nil || conn.Share.Name != "idx" || !bytes.Equal(conn.AppKey, key) {
		t.Errorf("the catalog belongs to %+v", conn)
	}

	// A connection that is not running is reported and the round goes on.
	n.clients = func(string) (map[string]ShareFiles, error) { return nil, nil }
	if err := n.WriteAll(ctx); err == nil || !strings.Contains(n.Status().Error, "not running") {
		t.Errorf("a connection that is not running: got %v, status %q", err, n.Status().Error)
	}
}
