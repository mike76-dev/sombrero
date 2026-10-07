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

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/renterd/v2/api"
)

// CatalogFolder is where a share keeps the catalogs of itself.
const CatalogFolder = transfer.CatalogFolder

// FolderOf is where a share keeps the catalogs of one workgroup's connection to
// it. A share has one tree for all its workgroups, so each has a folder of its
// own under CatalogFolder, belonging to an account of its own.
func FolderOf(workgroup uuid.UUID) string {
	return path.Join(CatalogFolder, workgroup.String())
}

// defaultTick is how often the tier looks at the connections. Each catalog is
// written on the interval of its own, but a connection that has just come up, a
// share that has just been ticked or unticked, should not wait the rest of an
// hour to be noticed.
const defaultTick = time.Minute

// errNotRunning is what a round says of a connection that is not up yet. It is
// the usual state of things at startup, so it is kept for the status and not
// written to the log.
var errNotRunning = errors.New("the connection is not running")

// Report is what both tiers have done; a tier that is off is nil.
type Report struct {
	Local   *Status
	Network *Status
}

// ShareFiles is as much of a connection's client as putting a file into the
// share, listing a folder of it and taking a file out again take.
type ShareFiles interface {
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
	tick     time.Duration
	keep     int
	inline   uint64

	mu      sync.Mutex
	status  Status
	written map[string]Catalog // the newest catalog of each connection, by share and workgroup
}

// NewNetwork starts the network tier as the config says: it looks at the
// connections every tick and writes each one's catalog once per interval, the
// first as soon as the connection is up. It is nil where that tier is off.
func NewNetwork(ctx context.Context, db *stores.Database, clients Clients, cfg stores.BackupConfig) *Network {
	interval := cfg.Network()
	if interval == 0 {
		return nil
	}

	n := &Network{
		db:       db,
		clients:  clients,
		interval: interval,
		tick:     defaultTick,
		keep:     cfg.KeepCount(),
		inline:   cfg.Inline(),
		status:   Status{Path: CatalogFolder, Interval: interval, Keep: cfg.KeepCount()},
	}
	go n.run(ctx)

	return n
}

// run looks at the connections every tick until the context ends.
func (n *Network) run(ctx context.Context) {
	for {
		if err := n.WriteAll(ctx); err != nil && ctx.Err() == nil && !onlyNotRunning(err) {
			log.Printf("backup: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(min(n.tick, n.interval)):
		}
	}
}

// Status reports what the tier has done so far.
func (n *Network) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()

	status := n.status
	status.Catalogs = append([]Catalog(nil), n.status.Catalogs...)
	status.Waiting = append([]Pending(nil), n.status.Waiting...)

	return status
}

// due reports whether a connection's catalog is to be written in this round:
// where it has none yet, or the last one is an interval old.
func (n *Network) due(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	last, ok := n.written[key]
	return !ok || time.Since(last.WrittenAt) >= n.interval
}

// WriteAll writes a catalog into the share of every connection that holds an app
// key, which is what the catalog is sealed with. A connection that is not
// running, or has nobody to own the file, is reported and passed over.
func (n *Network) WriteAll(ctx context.Context) error {
	conns, err := n.db.KeyedConnections()
	if err != nil {
		n.finish(nil, nil, err)
		return err
	}

	// What failed and what is only waiting for its connection are kept apart:
	// the second is the usual state of things for a while after a start.
	written := make(map[string]Catalog)
	var waiting []Pending
	var errs, failed []error
	running := make(map[string]map[string]ShareFiles)
	for _, c := range conns {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := c.Share + "/" + c.Workgroup.UUID.String()
		clients, ok := running[c.Share]
		if !ok {
			if clients, err = n.clients(c.Share); err != nil {
				err = fmt.Errorf("the connections of %s: %w", c.Share, err)
				errs, failed = append(errs, err), append(failed, err)
				continue
			}
			running[c.Share] = clients
		}
		files, ok := clients[c.Workgroup.UUID.String()]

		// A share taken out of the backups keeps no catalogs: left there they
		// would only grow old, and a recovery would restore the share from them.
		if c.SkipBackup {
			if ok {
				if err := n.clear(ctx, c, files); err != nil {
					err = fmt.Errorf("the old catalogs of %s for workgroup %s: %w", c.Share, c.Workgroup.UUID, err)
					errs, failed = append(errs, err), append(failed, err)
				}
			}
			continue
		}
		if !ok {
			errs = append(errs, fmt.Errorf("the catalog of %s for workgroup %s: %w", c.Share, c.Workgroup.UUID, errNotRunning))
			waiting = append(waiting, Pending{Share: c.Share, Workgroup: c.Workgroup.UUID})
			continue
		}

		// A catalog that is not due yet stands as it is.
		if !n.due(key) {
			n.mu.Lock()
			written[key] = n.written[key]
			n.mu.Unlock()
			continue
		}
		cat, err := n.write(ctx, c, files)
		if err != nil {
			err = fmt.Errorf("the catalog of %s for workgroup %s: %w", c.Share, c.Workgroup.UUID, err)
			errs, failed = append(errs, err), append(failed, err)
			continue
		}
		written[key] = cat
	}

	n.finish(written, waiting, joinLine(failed))

	// The round is not done while anything is waiting either.
	return joinLine(errs)
}

// finish keeps what a round came to: the newest catalog of every connection
// that is backed up, what is waiting, and what failed.
func (n *Network) finish(written map[string]Catalog, waiting []Pending, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.written = written
	n.status.LastRun = time.Now()
	n.status.Catalogs = n.status.Catalogs[:0]
	for _, cat := range written {
		n.status.Catalogs = append(n.status.Catalogs, cat)
	}
	sort.Slice(n.status.Catalogs, func(i, j int) bool {
		a, b := n.status.Catalogs[i], n.status.Catalogs[j]
		return a.Share < b.Share || a.Share == b.Share && a.Workgroup.String() < b.Workgroup.String()
	})
	n.status.Waiting = waiting
	n.status.Error = ""
	if err != nil {
		n.status.Error = err.Error()
	}
}

// onlyNotRunning reports whether every failure of a round was a connection that
// is not up yet.
func onlyNotRunning(err error) bool {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return errors.Is(err, errNotRunning)
	}
	for _, e := range joined.Unwrap() {
		if !errors.Is(e, errNotRunning) {
			return false
		}
	}

	return true
}

// joinLine joins the failures of a round into one error that prints on one line,
// which is what a log and a status line can take.
func joinLine(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}

	return &roundError{errs: errs, line: strings.Join(parts, "; ")}
}

// roundError is the failures of one round, as one line.
type roundError struct {
	errs []error
	line string
}

func (e *roundError) Error() string   { return e.line }
func (e *roundError) Unwrap() []error { return e.errs }

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

	// The catalog belongs to the oldest account of the workgroup that has a
	// password, in a folder of that account's: the other members do not see it,
	// and a guest, whom anyone can log in as, never owns it.
	accounts, err := n.db.FindAccounts(wg.UUID.String())
	if err != nil {
		return Catalog{}, err
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	var owner stores.Account
	for _, acc := range accounts {
		if !acc.Passwordless() {
			owner = acc
			break
		}
	}
	if owner.ID == 0 {
		return Catalog{}, errors.New("the workgroup has no account with a password to own the catalog")
	}

	var plain bytes.Buffer
	stats, err := n.db.Snapshot(&plain, share.Name, wg.ID, n.inline)
	if err != nil {
		return Catalog{}, err
	}
	sealed, err := Seal(key, plain.Bytes())
	if err != nil {
		return Catalog{}, err
	}

	// The folder is the server's, written as the owner: made if it is not
	// there, and taken over with what is in it if an earlier owner left it.
	target := stores.TransferTarget{Share: share.Name, Workgroup: wg.ID, Owner: owner}
	folder := FolderOf(wg.UUID)
	if err := n.db.AdoptFolder(target, folder); err != nil {
		return Catalog{}, fmt.Errorf("failed to make %s: %w", folder, err)
	}

	now := time.Now().UTC()
	name := path.Join(folder, now.Format("20060102T150405.000000000Z")+".catalog")
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

	n.prune(ctx, files, owner, folder)

	return Catalog{Share: share.Name, Workgroup: wg.UUID, Path: name, Size: int64(len(sealed)), WrittenAt: now, Stats: stats}, nil
}

// clear removes a workgroup's catalogs from a share that is no longer backed up,
// folder and all, as whoever the folder belongs to. A share that has none is
// left as it is, which is what every round after the first finds.
func (n *Network) clear(ctx context.Context, c stores.KeyedConnection, files ShareFiles) error {
	folder := FolderOf(c.Workgroup.UUID)
	owner, err := n.db.FolderOwner(c.Share, folder)
	if err != nil {
		return err
	}
	if owner.ID == 0 {
		return nil
	}
	if err := files.Delete(ctx, owner, folder, true); err != nil {
		return err
	}
	log.Printf("backup: removed the catalogs from %s for workgroup %s, which is no longer backed up", c.Share, c.Workgroup.UUID)

	return nil
}

// prune leaves the newest keep catalogs in the workgroup's folder. Their names
// are their times, so the order of the names is the order of the times.
func (n *Network) prune(ctx context.Context, files ShareFiles, owner stores.Account, folder string) {
	entries, err := files.List(ctx, owner, folder)
	if err != nil {
		log.Printf("backup: failed to list %s: %v", folder, err)
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
		if err := files.Delete(ctx, owner, path.Join(folder, name), false); err != nil {
			log.Printf("backup: failed to remove the old catalog %s: %v", name, err)
		}
	}
}
