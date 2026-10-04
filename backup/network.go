package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/renterd/v2/api"
)

// CatalogFolder is where a share keeps the catalogs of itself.
const CatalogFolder = "/.sombrero/catalog"

// Report is what both tiers have done; a tier that is off is nil.
type Report struct {
	Local   *Status
	Network *Status
}

// ShareFiles is as much of a connection's client as putting a file into the
// share, listing a folder of it and taking a file out again take.
type ShareFiles interface {
	MakeDirectory(ctx context.Context, acc stores.Account, path string) error
	StartUpload(ctx context.Context, acc stores.Account, path string) (string, error)
	Write(ctx context.Context, r io.Reader, path, uploadID string, partNumber int, offset, length uint64) (string, error)
	FinishUpload(ctx context.Context, path, uploadID string, parts []api.MultipartCompletedPart) error
	List(ctx context.Context, acc stores.Account, path string) ([]client.ObjectInfo, error)
	Delete(ctx context.Context, acc stores.Account, path string, batch bool) error
}

// Clients hands out the connections of a share that are running, by workgroup
// UUID.
type Clients func(share string) (map[string]ShareFiles, error)

// Network writes a catalog of every connection into its own share, as a file
// under CatalogFolder, so that the account on the network carries the names of
// what it holds. The catalog is sealed with a key derived from the app key, and
// the folder belongs to one account of the workgroup, so the members of the
// workgroup cannot read the keys and hashes in it over SMB.
type Network struct {
	db       *stores.Database
	clients  Clients
	interval time.Duration
	keep     int
	inline   uint64

	mu     sync.Mutex
	status Status
}

// NewNetwork starts the network tier as the config says, writing once right away
// and then every interval. It is nil where that tier is off.
func NewNetwork(ctx context.Context, db *stores.Database, clients Clients, cfg stores.BackupConfig) *Network {
	interval := cfg.Network()
	if interval == 0 {
		return nil
	}

	n := &Network{
		db:       db,
		clients:  clients,
		interval: interval,
		keep:     cfg.KeepCount(),
		inline:   cfg.Inline(),
		status:   Status{Path: CatalogFolder, Interval: interval, Keep: cfg.KeepCount()},
	}
	go n.run(ctx)

	return n
}

// run writes the catalogs on the interval until the context ends.
func (n *Network) run(ctx context.Context) {
	for {
		if err := n.WriteAll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("backup: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(n.interval):
		}
	}
}

// Status reports what the tier has done so far.
func (n *Network) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()

	status := n.status
	status.Catalogs = append([]Catalog(nil), n.status.Catalogs...)

	return status
}

// WriteAll writes a catalog into the share of every connection that holds an app
// key, which is what the catalog is sealed with. A connection that is not
// running, or has nobody to own the file, is reported and passed over.
func (n *Network) WriteAll(ctx context.Context) error {
	conns, err := n.db.KeyedConnections()
	if err != nil {
		n.finish(nil, err)
		return err
	}

	var written []Catalog
	var errs []error
	running := make(map[string]map[string]ShareFiles)
	for _, c := range conns {
		if err := ctx.Err(); err != nil {
			return err
		}

		clients, ok := running[c.Share]
		if !ok {
			if clients, err = n.clients(c.Share); err != nil {
				errs = append(errs, fmt.Errorf("the connections of %s: %w", c.Share, err))
				continue
			}
			running[c.Share] = clients
		}
		files, ok := clients[c.Workgroup.UUID.String()]
		if !ok {
			errs = append(errs, fmt.Errorf("the catalog of %s for workgroup %s: the connection is not running", c.Share, c.Workgroup.UUID))
			continue
		}

		cat, err := n.write(ctx, c, files)
		if err != nil {
			errs = append(errs, fmt.Errorf("the catalog of %s for workgroup %s: %w", c.Share, c.Workgroup.UUID, err))
			continue
		}
		written = append(written, cat)
	}

	err = errors.Join(errs...)
	n.finish(written, err)

	return err
}

// finish keeps what a round came to.
func (n *Network) finish(written []Catalog, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.status.LastRun = time.Now()
	n.status.Catalogs = written
	n.status.Error = ""
	if err != nil {
		n.status.Error = err.Error()
	}
}

// write writes one connection's catalog into its share and prunes the folder.
func (n *Network) write(ctx context.Context, c stores.KeyedConnection, files ShareFiles) (Catalog, error) {
	wg, err := n.db.GetWorkgroupByID(c.Workgroup.ID)
	if err != nil {
		return Catalog{}, err
	}
	share, err := n.db.GetShare(c.Share)
	if err != nil {
		return Catalog{}, err
	}
	_, key, err := n.db.IsConnected(wg, share)
	if err != nil {
		return Catalog{}, err
	}
	if len(key) == 0 {
		return Catalog{}, errors.New("the connection has no app key to seal the catalog with")
	}

	// The catalog belongs to one account of the workgroup, in a folder of that
	// account's, which is what keeps the others from reading it.
	accounts, err := n.db.FindAccounts(wg.UUID.String())
	if err != nil {
		return Catalog{}, err
	}
	if len(accounts) == 0 {
		return Catalog{}, errors.New("the workgroup has no account to own the catalog")
	}
	owner := accounts[0]

	var plain bytes.Buffer
	stats, err := n.db.Snapshot(&plain, share.Name, wg.ID, n.inline)
	if err != nil {
		return Catalog{}, err
	}
	sealed, err := Seal(key, plain.Bytes())
	if err != nil {
		return Catalog{}, err
	}

	for _, dir := range []string{path.Dir(CatalogFolder), CatalogFolder} {
		if err := files.MakeDirectory(ctx, owner, dir); err != nil && !errors.Is(err, stores.ErrDirectoryExists) {
			return Catalog{}, fmt.Errorf("failed to make %s: %w", dir, err)
		}
	}

	now := time.Now().UTC()
	name := path.Join(CatalogFolder, now.Format("20060102T150405.000000000Z")+".catalog")
	uploadID, err := files.StartUpload(ctx, owner, name)
	if err != nil {
		return Catalog{}, err
	}
	if _, err := files.Write(ctx, bytes.NewReader(sealed), name, uploadID, 1, 0, uint64(len(sealed))); err != nil {
		return Catalog{}, err
	}
	if err := files.FinishUpload(ctx, name, uploadID, nil); err != nil {
		return Catalog{}, err
	}

	n.prune(ctx, files, owner)

	return Catalog{Share: share.Name, Workgroup: wg.UUID, Path: name, Size: int64(len(sealed)), WrittenAt: now, Stats: stats}, nil
}

// prune leaves the newest keep catalogs in the share's folder. Their names are
// their times, so the order of the names is the order of the times.
func (n *Network) prune(ctx context.Context, files ShareFiles, owner stores.Account) {
	entries, err := files.List(ctx, owner, CatalogFolder)
	if err != nil {
		log.Printf("backup: failed to list %s: %v", CatalogFolder, err)
		return
	}

	var names []string
	for _, entry := range entries {
		if name := path.Base(entry.Key); strings.HasSuffix(name, ".catalog") {
			names = append(names, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, name := range names[min(n.keep, len(names)):] {
		if err := files.Delete(ctx, owner, path.Join(CatalogFolder, name), false); err != nil {
			log.Printf("backup: failed to remove the old catalog %s: %v", name, err)
		}
	}
}
