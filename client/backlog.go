package client

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/stores"
)

// ErrBacklogFull fails a write that waited too long for the upload backlog to drain.
var ErrBacklogFull = errors.New("too much data is waiting to be uploaded")

// errClosing fails a write that was waiting for room when the client closed.
var errClosing = errors.New("the client is closing")

// BacklogReporter is a client that buffers what it is given before uploading it.
type BacklogReporter interface {
	// Backlog returns how much is buffered across all shares, and the limit; zero means none.
	Backlog() (buffered, limit uint64)
}

const (
	// How often the backlog is measured.
	backlogRefresh = 2 * time.Second

	// How long a write waits for the backlog to drain below the limit.
	backlogWait = 5 * time.Minute

	// How often a vacuum falling behind is reported, at most.
	vacuumWarnInterval = time.Hour
)

// Backlog tracks the data all indexd shares keep buffered in the database, and
// holds writes back while it is at the limit.
type Backlog struct {
	limit uint64
	usage func() (buffered, onDisk uint64, err error)
	wait  time.Duration // a field so tests can shorten it

	mu       sync.Mutex
	buffered uint64
	changed  chan struct{} // closed and replaced on every measurement
	lastWarn time.Time
}

// NewBacklog measures the backlog and keeps measuring it until ctx is done.
func NewBacklog(ctx context.Context, db *stores.Database, limit uint64) *Backlog {
	b := newBacklog(db.BufferUsage, limit)
	b.refresh()
	go b.run(ctx)
	return b
}

func newBacklog(usage func() (uint64, uint64, error), limit uint64) *Backlog {
	return &Backlog{
		limit:   limit,
		usage:   usage,
		wait:    backlogWait,
		changed: make(chan struct{}),
	}
}

func (b *Backlog) run(ctx context.Context) {
	ticker := time.NewTicker(backlogRefresh)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.refresh()
		}
	}
}

// refresh takes a new measurement and wakes the writes waiting for room.
func (b *Backlog) refresh() {
	buffered, onDisk, err := b.usage()
	if err != nil {
		log.Printf("failed to measure the upload backlog: %v", err)
		return
	}

	b.mu.Lock()
	b.buffered = buffered
	close(b.changed)
	b.changed = make(chan struct{})

	// The table stays at its largest backlog, so twice the limit means dead rows piling up.
	warn := onDisk > 2*b.limit && time.Since(b.lastWarn) >= vacuumWarnInterval
	if warn {
		b.lastWarn = time.Now()
	}
	b.mu.Unlock()

	if warn {
		log.Printf("the upload buffers take %d bytes on disk for %d bytes of data, with a limit of %d: autovacuum may be falling behind", onDisk, buffered, b.limit)
	}
}

// Load returns the buffered data as last measured, plus what was reserved since.
func (b *Backlog) Load() (buffered, limit uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffered, b.limit
}

// full reports whether the backlog is at the limit.
func (b *Backlog) full() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffered >= b.limit
}

// reserve waits until the backlog is below the limit, then counts n bytes into it
// until the next measurement does.
func (b *Backlog) reserve(ctx context.Context, closing <-chan struct{}, n uint64) error {
	timer := time.NewTimer(b.wait)
	defer timer.Stop()

	for {
		b.mu.Lock()
		if b.buffered < b.limit {
			b.buffered += n
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-changed:
		case <-timer.C:
			return ErrBacklogFull
		case <-ctx.Done():
			return ctx.Err()
		case <-closing:
			return errClosing
		}
	}
}
