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
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/renterd/v2/api"
)

// fakeShare stands in for a connection's client: the folders and files put into
// the share, kept in memory.
type fakeShare struct {
	mu      sync.Mutex
	files   map[string][]byte
	uploads map[string]string
	owners  map[string]string
}

func newFakeShare() *fakeShare {
	return &fakeShare{files: map[string][]byte{}, uploads: map[string]string{}, owners: map[string]string{}}
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

// TestNetworkComesBackForWhatWasNotRunning verifies that a round which found a
// connection not running is followed by another one soon, rather than after the
// whole interval: at startup the catalogs should follow the connections up.
func TestNetworkComesBackForWhatWasNotRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, wg, _ := connectedStore(t, ctx)

	share := newFakeShare()
	var mu sync.Mutex
	up := false
	n := &Network{
		db: db,
		clients: func(string) (map[string]ShareFiles, error) {
			mu.Lock()
			defer mu.Unlock()
			if !up {
				return nil, nil
			}
			return map[string]ShareFiles{wg.UUID.String(): share}, nil
		},
		interval: time.Hour,
		keep:     2,
		inline:   1024,
		status:   Status{Path: CatalogFolder, Keep: 2},
	}
	retryInterval = 5 * time.Millisecond
	t.Cleanup(func() { retryInterval = time.Minute })
	go n.run(ctx)

	await := func(what string, cond func(Status) bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if cond(n.Status()) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s: status %+v", what, n.Status())
	}

	// Waiting for a connection is said as that, and is not a failure.
	await("the first round should find the connection not running", func(s Status) bool {
		return len(s.Waiting) == 1 && s.Waiting[0].Share == "idx" && s.Waiting[0].Workgroup == wg.UUID && s.Error == ""
	})
	mu.Lock()
	up = true
	mu.Unlock()
	await("the catalog should be written soon after the connection comes up", func(s Status) bool {
		return s.Error == "" && len(s.Waiting) == 0 && len(s.Catalogs) == 1
	})
}

// TestNetworkKeepsWorkgroupsApart verifies that two workgroups on one share each
// get their catalogs in a folder of their own, owned by an account of their own,
// since the share has one tree for both and neither is to take the other's.
func TestNetworkKeepsWorkgroupsApart(t *testing.T) {
	ctx := context.Background()
	db, first, _ := connectedStore(t, ctx)

	if err := db.AddWorkgroup(stores.Workgroup{UUID: uuid.New(), Name: "beta"}); err != nil {
		t.Fatalf("AddWorkgroup: %v", err)
	}
	second, err := db.FindWorkgroupByName("beta")
	if err != nil {
		t.Fatalf("FindWorkgroupByName: %v", err)
	}
	if err := db.AddAccount(stores.Account{Username: "carol", Password: "pw", Workgroup: second.UUID.String()}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	idx, err := db.GetShare("idx")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if err := db.AddConnection(second, idx, bytes.Repeat([]byte{9}, 64)); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}

	shares := map[string]*fakeShare{first.UUID.String(): newFakeShare(), second.UUID.String(): newFakeShare()}
	n := &Network{
		db: db,
		clients: func(string) (map[string]ShareFiles, error) {
			return map[string]ShareFiles{first.UUID.String(): shares[first.UUID.String()], second.UUID.String(): shares[second.UUID.String()]}, nil
		},
		keep:   2,
		inline: 1024,
	}
	for range 2 {
		if err := n.WriteAll(ctx); err != nil {
			t.Fatalf("WriteAll: %v", err)
		}
	}

	for _, each := range []struct {
		wg       stores.Workgroup
		username string
	}{{first, "alice"}, {second, "carol"}} {
		wg, username := each.wg, each.username
		names := shares[wg.UUID.String()].catalogs()
		if len(names) != 2 || path.Dir(names[0]) != FolderOf(wg.UUID) {
			t.Errorf("the catalogs of %s: got %v", wg.Name, names)
		}
		owner, err := db.FolderOwner("idx", FolderOf(wg.UUID))
		if err != nil || owner.Username != username {
			t.Errorf("the folder of %s belongs to %q, want %s: %v", wg.Name, owner.Username, username, err)
		}
	}
	if got := len(n.Status().Catalogs); got != 2 {
		t.Errorf("the status lists %d catalog(s), want one per workgroup", got)
	}
}

// TestNetworkWritesSealedCatalogs verifies that each round puts a catalog into
// the share that only the app key opens, owned by an account of the workgroup,
// and that only the newest few stay.
func TestNetworkWritesSealedCatalogs(t *testing.T) {
	ctx := context.Background()
	db, wg, key := connectedStore(t, ctx)
	alice, err := db.FindAccount("alice", wg.UUID.String())
	if err != nil {
		t.Fatalf("FindAccount(alice): %v", err)
	}
	guest, err := db.FindAccount("guest", wg.UUID.String())
	if err != nil {
		t.Fatalf("FindAccount(guest): %v", err)
	}

	// An earlier server left the folder to the guest, private to it; the owner
	// of today has to be able to write into it all the same.
	if _, err := db.ApplyDirectory(stores.TransferTarget{Share: "idx", Workgroup: wg.ID, Owner: guest}, transfer.Directory{Path: FolderOf(wg.UUID), Private: true}); err != nil {
		t.Fatalf("ApplyDirectory: %v", err)
	}

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
	if share.owners[names[1]] != "alice" {
		t.Errorf("the catalog was written as %q, want alice", share.owners[names[1]])
	}

	// The workgroup's folder is alice's now, and the catalogs are in it.
	owner, err := db.FolderOwner("idx", FolderOf(wg.UUID))
	if err != nil || owner.ID != alice.ID {
		t.Errorf("the folder belongs to %q, want alice: %v", owner.Username, err)
	}
	if path.Dir(names[1]) != FolderOf(wg.UUID) {
		t.Errorf("the catalog is at %s, want it in the workgroup's own folder", names[1])
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

	// A share left out of the backups gets no catalog, and that is no error.
	idx, err := db.GetShare("idx")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	idx.SkipBackup = true
	if err := db.UpdateShare(idx); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	if err := n.WriteAll(ctx); err != nil || len(n.Status().Catalogs) != 0 || len(share.catalogs()) != 2 {
		t.Errorf("a share left out: %v, %d catalog(s) reported, %d in the share", err, len(n.Status().Catalogs), len(share.catalogs()))
	}
	idx.SkipBackup = false
	if err := db.UpdateShare(idx); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}

	// A connection that is not running is reported and the round goes on.
	n.clients = func(string) (map[string]ShareFiles, error) { return nil, nil }
	if err := n.WriteAll(ctx); err == nil || !onlyNotRunning(err) || len(n.Status().Waiting) != 1 || n.Status().Error != "" {
		t.Errorf("a connection that is not running: got %v, status %+v", err, n.Status())
	}
}
