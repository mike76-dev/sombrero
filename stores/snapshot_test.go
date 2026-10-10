package stores

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// readCatalog drains a catalog into what it holds.
func readCatalog(t *testing.T, b []byte) (*transfer.Reader, []transfer.Directory, []transfer.File) {
	t.Helper()

	r, err := transfer.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var dirs []transfer.Directory
	var files []transfer.File
	for {
		dir, f, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if dir != nil {
			dirs = append(dirs, *dir)
		}
		if f != nil {
			files = append(files, *f)
		}
	}

	return r, dirs, files
}

// catalogFixture is a connection with everything a catalog has to carry: two
// accounts, a policy, a public folder, an app key, and rows of every shape.
type catalogFixture struct {
	target     TransferTarget
	alice, bob Account
	wg         Workgroup
	share      Share
	key        types.PrivateKey
	object     types.Hash256
	small      []byte
}

// plantCatalogFixture sets the fixture up in the store.
func plantCatalogFixture(t *testing.T, db *Database) catalogFixture {
	t.Helper()

	target, alice := applyTarget(t, db)
	wg, err := db.GetWorkgroupByID(target.Workgroup)
	if err != nil {
		t.Fatalf("GetWorkgroupByID: %v", err)
	}
	share, err := db.GetShare(target.Share)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}

	bob := addAccount(t, db, wg, "bob", "pw")
	wg.PublicDirs = []PublicDir{{Path: "Public", ReadOnly: true}}
	if err := db.UpdateWorkgroup(wg); err != nil {
		t.Fatalf("UpdateWorkgroup: %v", err)
	}
	key := make(types.PrivateKey, 64)
	for i := range key {
		key[i] = byte(i)
	}
	if err := db.AddConnection(wg, share, key); err != nil {
		t.Fatalf("AddConnection: %v", err)
	}
	if err := db.SetAccessRights(AccessRights{ShareName: share.Name, AccountID: bob.ID, ReadAccess: true, ExecuteAccess: true}); err != nil {
		t.Fatalf("SetAccessRights: %v", err)
	}

	// A private folder of alice's, a file on the network, a small file still
	// buffered, which a catalog carries, a bigger one it leaves out, an empty
	// one, and a file of bob's.
	now := time.Now().UTC().Truncate(time.Second)
	object := types.Hash256{9}
	if _, err := db.ApplyDirectory(target, transfer.Directory{Path: "/holiday", Private: true, CreatedAt: now, ModifiedAt: now}); err != nil {
		t.Fatalf("ApplyDirectory: %v", err)
	}
	small := bytes.Repeat([]byte("s"), 100)
	for _, f := range []transfer.File{
		{Path: "/holiday/beach.raw", Size: 300, CreatedAt: now, ModifiedAt: now, Parts: []transfer.Part{
			{Offset: 0, DataOffset: 1000, Length: 100, Object: object},
			{Offset: 100, DataOffset: 0, Length: 200, Object: types.Hash256{8}},
		}},
		{Path: "/holiday/small.txt", Size: 100, CreatedAt: now, ModifiedAt: now, Parts: []transfer.Part{{Length: 100, Inline: small}}},
		{Path: "/holiday/big.bin", Size: 3000, CreatedAt: now, ModifiedAt: now, Parts: []transfer.Part{{Length: 3000, Inline: bytes.Repeat([]byte("b"), 3000)}}},
		{Path: "/holiday/empty", CreatedAt: now, ModifiedAt: now},
	} {
		if _, err := db.ApplyFile(target, f); err != nil {
			t.Fatalf("ApplyFile(%s): %v", f.Path, err)
		}
	}
	bobs := target
	bobs.Owner = bob
	if _, err := db.ApplyFile(bobs, transfer.File{Path: "/bob.bin", Size: 10, CreatedAt: now, ModifiedAt: now, Parts: []transfer.Part{{Length: 10, Object: object}}}); err != nil {
		t.Fatalf("ApplyFile(bob): %v", err)
	}

	return catalogFixture{target: target, alice: alice, bob: bob, wg: wg, share: share, key: key, object: object, small: small}
}

// TestSnapshot verifies what a catalog of a connection carries: everything the
// database alone knows, read back the way a restore would read it.
func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	fx := plantCatalogFixture(t, db)
	target, alice, wg, share, key, object, small := fx.target, fx.alice, fx.wg, fx.share, fx.key, fx.object, fx.small

	// A catalog the share keeps of itself, small enough to be carried; its
	// parent folder is one like any other.
	now := time.Now().UTC().Truncate(time.Second)
	older := path.Join(transfer.CatalogFolder, wg.UUID.String())
	if _, err := db.ApplyDirectory(target, transfer.Directory{Path: older, CreatedAt: now, ModifiedAt: now}); err != nil {
		t.Fatalf("ApplyDirectory(%s): %v", older, err)
	}
	if _, err := db.ApplyFile(target, transfer.File{Path: older + "/older.catalog", Size: 50, CreatedAt: now, ModifiedAt: now, Parts: []transfer.Part{{Length: 50, Inline: bytes.Repeat([]byte("c"), 50)}}}); err != nil {
		t.Fatalf("ApplyFile(catalog): %v", err)
	}

	var buf bytes.Buffer
	stats, err := db.Snapshot(&buf, share.Name, target.Workgroup, 1000)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if stats.Directories != 2 || stats.Files != 5 || stats.Inlined != 100 || stats.Incomplete != 1 {
		t.Errorf("stats: got %+v", stats)
	}

	r, dirs, files := readCatalog(t, buf.Bytes())
	if h := r.Header(); h.Source != "sombrero" || h.Share != share.Name || h.Workgroup != wg.UUID.String() {
		t.Errorf("header: got %+v", h)
	}

	conn := r.Connection()
	if conn == nil {
		t.Fatal("the catalog says nothing about what it belongs to")
	}
	if conn.Share.Name != share.Name || conn.Share.Type != "indexd" || conn.Share.Server != share.ServerName {
		t.Errorf("the share: got %+v", conn.Share)
	}
	if conn.Workgroup.UUID != wg.UUID || conn.Workgroup.Name != wg.Name || len(conn.Workgroup.PublicDirs) != 1 || !conn.Workgroup.PublicDirs[0].ReadOnly {
		t.Errorf("the workgroup: got %+v", conn.Workgroup)
	}
	if len(conn.Accounts) != 2 || conn.Accounts[0].Name != alice.Username || conn.Accounts[1].Name != "bob" || !bytes.Equal(conn.Accounts[1].PasswordHash, ntHash("pw")) {
		t.Errorf("the accounts: got %+v", conn.Accounts)
	}
	if len(conn.Policies) != 1 || conn.Policies[0] != (transfer.Policy{Account: "bob", Read: true, Execute: true}) {
		t.Errorf("the policies: got %+v", conn.Policies)
	}
	if !bytes.Equal(conn.AppKey, key) {
		t.Errorf("the app key: got %x", conn.AppKey)
	}

	byDir := make(map[string]transfer.Directory, len(dirs))
	for _, d := range dirs {
		byDir[d.Path] = d
	}
	if _, ok := byDir["/.sombrero"]; len(dirs) != 2 || !ok || byDir["/holiday"].Owner != alice.Username || !byDir["/holiday"].Private {
		t.Errorf("the folders: got %+v", dirs)
	}

	byPath := make(map[string]transfer.File, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	beach := byPath["/holiday/beach.raw"]
	if beach.Owner != alice.Username || len(beach.Parts) != 2 || beach.Parts[0].Object != object || beach.Parts[0].DataOffset != 1000 || beach.Parts[1].Offset != 100 {
		t.Errorf("the file on the network: got %+v", beach)
	}
	if got := byPath["/holiday/small.txt"]; len(got.Parts) != 1 || !bytes.Equal(got.Parts[0].Inline, small) || !got.Complete() {
		t.Errorf("the small buffered file: got %+v", got)
	}
	if got := byPath["/holiday/big.bin"]; len(got.Parts) != 0 || got.Complete() || got.Size != 3000 {
		t.Errorf("the big buffered file: got %+v", got)
	}
	if got := byPath["/holiday/empty"]; len(got.Parts) != 0 || !got.Complete() {
		t.Errorf("the empty file: got %+v", got)
	}
	if got := byPath["/bob.bin"]; got.Owner != "bob" {
		t.Errorf("bob's file: got %+v", got)
	}
	for p := range byPath {
		if strings.HasPrefix(p, transfer.CatalogFolder) {
			t.Errorf("the catalog carries the older catalog %s", p)
		}
	}

	// A connection that is not there is said so.
	if _, err := db.Snapshot(io.Discard, "nowhere", target.Workgroup, 1000); !errors.Is(err, ErrNotFound) {
		t.Errorf("a snapshot of nothing: got %v", err)
	}
}
