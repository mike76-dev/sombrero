package client

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
)

// fakeUsage is a measurement the test sets by hand.
type fakeUsage struct {
	mu       sync.Mutex
	buffered uint64
}

func (f *fakeUsage) set(n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buffered = n
}

func (f *fakeUsage) usage() (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buffered, f.buffered, nil
}

func TestBacklog_Reserve(t *testing.T) {
	ctx := context.Background()

	t.Run("below the limit a write is admitted and counted", func(t *testing.T) {
		u := &fakeUsage{}
		b := newBacklog(u.usage, 100)
		b.refresh()

		if err := b.reserve(ctx, nil, 60); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if buffered, _ := b.Load(); buffered != 60 {
			t.Fatalf("want 60 bytes counted, got %d", buffered)
		}
		if b.full() {
			t.Fatal("full below the limit")
		}

		// Admitted while below the limit, even though it goes over.
		if err := b.reserve(ctx, nil, 60); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if !b.full() {
			t.Fatal("not full over the limit")
		}
	})

	t.Run("at the limit a write waits for the backlog to drain", func(t *testing.T) {
		u := &fakeUsage{buffered: 100}
		b := newBacklog(u.usage, 100)
		b.refresh()

		done := make(chan error, 1)
		go func() { done <- b.reserve(ctx, nil, 10) }()

		select {
		case err := <-done:
			t.Fatalf("admitted at the limit: %v", err)
		case <-time.After(50 * time.Millisecond):
		}

		// A measurement still at the limit keeps it waiting.
		b.refresh()
		select {
		case err := <-done:
			t.Fatalf("admitted at the limit: %v", err)
		case <-time.After(50 * time.Millisecond):
		}

		u.set(50)
		b.refresh()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("still waiting after the backlog drained")
		}
	})

	t.Run("a write that waits too long fails as a full backlog", func(t *testing.T) {
		u := &fakeUsage{buffered: 100}
		b := newBacklog(u.usage, 100)
		b.wait = 20 * time.Millisecond
		b.refresh()

		if err := b.reserve(ctx, nil, 10); !errors.Is(err, ErrBacklogFull) {
			t.Fatalf("want ErrBacklogFull, got %v", err)
		}
	})

	t.Run("an unset limit measures but holds nothing back", func(t *testing.T) {
		u := &fakeUsage{buffered: 100 << 30}
		b := newBacklog(u.usage, 0)
		b.wait = 20 * time.Millisecond
		b.refresh()

		if b.full() {
			t.Fatal("full with no limit to reach")
		}
		if err := b.reserve(ctx, nil, 1<<30); err != nil {
			t.Fatalf("reserve: %v", err)
		}

		buffered, limit, onDisk := b.Stats()
		if buffered != 100<<30 || limit != 0 || onDisk != 100<<30 {
			t.Fatalf("stats: got %d buffered, limit %d, %d on disk", buffered, limit, onDisk)
		}
	})

	t.Run("a waiting write gives up on cancellation and on close", func(t *testing.T) {
		u := &fakeUsage{buffered: 100}
		b := newBacklog(u.usage, 100)
		b.refresh()

		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := b.reserve(cctx, nil, 10); !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}

		closing := make(chan struct{})
		close(closing)
		if err := b.reserve(ctx, closing, 10); !errors.Is(err, errClosing) {
			t.Fatalf("want errClosing, got %v", err)
		}
	})
}

func TestBacklog_VacuumLagging(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name                    string
		buffered, onDisk, limit uint64
		want                    bool
	}{
		{"an empty table's overhead under a tiny limit", 0, 22 * mib, 1, false},
		{"a table at its peak within the limit", 100 * mib, 1000 * mib, 1000 * mib, false},
		{"a table past twice the limit", 100 * mib, 2100 * mib, 1000 * mib, true},
		{"a tiny limit with dead rows past the slack", 0, 100 * mib, 1, true},
		{"a table past twice the limit but full of live data", 2000 * mib, 2050 * mib, 1000 * mib, false},
	}
	for _, tt := range tests {
		if got := vacuumLagging(tt.buffered, tt.onDisk, tt.limit); got != tt.want {
			t.Errorf("%s: want %v, got %v", tt.name, tt.want, got)
		}
	}
}

// TestIndexdClient_BacklogFull verifies that a full backlog uploads leftovers the
// packer would otherwise keep, and fails writes that wait too long for room.
func TestIndexdClient_BacklogFull(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	// Measured only when the test says so, so that it stays full.
	b := newBacklog(db.BufferUsage, 1)
	b.wait = 50 * time.Millisecond
	b.refresh()

	fb := newFakeBackend()
	c := newIndexdClient(db, fb, share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false, withBacklog(b))
	t.Cleanup(func() { _ = c.Close() })

	// Far short of a slab, this would wait for more data to pack it with.
	data := []byte("left over")
	uploadBuffered(t, ctx, c, acc, "a.txt", data)
	waitForObjects(t, fb, 1)
	mustReadEquals(t, ctx, c, acc, "a.txt", data)

	uploadID, err := c.StartUpload(ctx, acc, "b.txt")
	if err != nil {
		t.Fatalf("StartUpload: %v", err)
	}
	if _, err := c.Write(ctx, bytes.NewReader(data), "b.txt", uploadID, 1, 0, uint64(len(data))); !errors.Is(err, ErrBacklogFull) {
		t.Fatalf("want ErrBacklogFull, got %v", err)
	}

	// Measured again, the uploaded leftover has left room.
	b.refresh()
	if _, err := c.Write(ctx, bytes.NewReader(data), "b.txt", uploadID, 1, 0, uint64(len(data))); err != nil {
		t.Fatalf("Write after the backlog drained: %v", err)
	}
}
