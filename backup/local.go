// Package backup writes catalogs of what the shares hold, so that a server can be
// put back together without its database. A catalog describes one connection:
// the share, the workgroup with its accounts, and every folder and file, as runs
// of the objects on the network.
package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/stores"
)

// A Catalog is one that was written: of which connection, where, and what it
// holds.
type Catalog struct {
	Share     string
	Workgroup uuid.UUID
	Path      string
	Size      int64
	WrittenAt time.Time
	Stats     stores.SnapshotStats
}

// Status is what the local tier has done so far: the newest catalog of each
// connection, and what went wrong the last time, if anything did.
type Status struct {
	Path     string
	Interval time.Duration
	Keep     int
	LastRun  time.Time
	Catalogs []Catalog
	Error    string
}

// Local writes a catalog of every connection to a folder on this machine, on an
// interval, and keeps the newest few of each. The folder holds the app keys, so
// it is made and written for the owner alone.
type Local struct {
	db       *stores.Database
	dir      string
	interval time.Duration
	keep     int
	inline   uint64

	mu     sync.Mutex
	status Status
}

// NewLocal starts the local tier as the config says, writing once right away and
// then every interval. It is nil where that tier is off.
func NewLocal(ctx context.Context, db *stores.Database, cfg stores.BackupConfig) *Local {
	interval := cfg.Local()
	if interval == 0 {
		return nil
	}

	l := &Local{
		db:       db,
		dir:      cfg.Path,
		interval: interval,
		keep:     cfg.KeepCount(),
		inline:   cfg.Inline(),
		status:   Status{Path: cfg.Path, Interval: interval, Keep: cfg.KeepCount()},
	}
	go l.run(ctx)

	return l
}

// run writes the catalogs on the interval until the context ends.
func (l *Local) run(ctx context.Context) {
	for {
		if err := l.WriteAll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("backup: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(l.interval):
		}
	}
}

// Status reports what the tier has done so far.
func (l *Local) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()

	status := l.status
	status.Catalogs = append([]Catalog(nil), l.status.Catalogs...)

	return status
}

// WriteAll writes a catalog of every connection and prunes the old ones. One
// connection that cannot be written does not keep the others from being, and
// what went wrong is kept for whoever asks.
func (l *Local) WriteAll(ctx context.Context) error {
	conns, err := l.db.AllConnections()
	if err != nil {
		l.finish(nil, err)
		return err
	}

	var written []Catalog
	var errs []error
	for _, c := range conns {
		if err := ctx.Err(); err != nil {
			return err
		}
		cat, err := l.write(c)
		if err != nil {
			errs = append(errs, fmt.Errorf("the catalog of %s for workgroup %s: %w", c.Share, c.Workgroup.UUID, err))
			continue
		}
		written = append(written, cat)
	}

	err = errors.Join(errs...)
	l.finish(written, err)

	return err
}

// finish keeps what a round came to.
func (l *Local) finish(written []Catalog, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.status.LastRun = time.Now()
	l.status.Catalogs = written
	l.status.Error = ""
	if err != nil {
		l.status.Error = err.Error()
	}
}

// write writes one catalog, whole or not at all: into a file of its own that
// takes the catalog's name only once it is complete, so a reader never finds
// half of one.
func (l *Local) write(c stores.KeyedConnection) (Catalog, error) {
	dir := filepath.Join(l.dir, folderName(c))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Catalog{}, err
	}

	tmp, err := os.CreateTemp(dir, ".catalog-*")
	if err != nil {
		return Catalog{}, err
	}
	bw := bufio.NewWriter(tmp)
	stats, err := l.db.Snapshot(bw, c.Share, c.Workgroup.ID, l.inline)
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return Catalog{}, err
	}

	now := time.Now().UTC()
	final := filepath.Join(dir, now.Format("20060102T150405.000000000Z")+".catalog")
	if err := os.Rename(tmp.Name(), final); err != nil {
		_ = os.Remove(tmp.Name())
		return Catalog{}, err
	}
	info, err := os.Stat(final)
	if err != nil {
		return Catalog{}, err
	}

	l.prune(dir)

	return Catalog{Share: c.Share, Workgroup: c.Workgroup.UUID, Path: final, Size: info.Size(), WrittenAt: now, Stats: stats}, nil
}

// prune leaves the newest keep catalogs in the folder. Their names are their
// times, so the order of the names is the order of the times.
func (l *Local) prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("backup: failed to list %s: %v", dir, err)
		return
	}

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".catalog") {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, name := range names[min(l.keep, len(names)):] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("backup: failed to remove the old catalog %s: %v", name, err)
		}
	}
}

// folderName names a connection's folder after its share and workgroup. A share
// name may hold anything a client can type, so what a path cannot take goes.
func folderName(c stores.KeyedConnection) string {
	share := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, c.Share)

	return share + "_" + c.Workgroup.UUID.String()
}
