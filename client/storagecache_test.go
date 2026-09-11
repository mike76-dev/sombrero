package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// countingFetch is a fake backend that counts calls and can fail or hang until released.
type countingFetch struct {
	mu      sync.Mutex
	calls   int
	space   uint64
	err     error
	release chan struct{}
}

func (f *countingFetch) fetch(ctx context.Context) (StorageInfo, error) {
	f.mu.Lock()
	f.calls++
	release, space, err := f.release, f.space, f.err
	f.mu.Unlock()

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return StorageInfo{}, ctx.Err()
		}
	}
	if err != nil {
		return StorageInfo{}, err
	}
	return StorageInfo{RemainingStorage: space}, nil
}

func (f *countingFetch) set(space uint64, err error, release chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.space, f.err, f.release = space, err, release
}

func (f *countingFetch) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// eventually waits for cond to hold, and fails the test if it never does.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never happened", what)
}

func TestStorageCache(t *testing.T) {
	ctx := context.Background()

	t.Run("the first question waits for the backend and the next ones do not ask it", func(t *testing.T) {
		f := &countingFetch{space: 100}
		sc := &storageCache{ttl: time.Hour}

		for i := 0; i < 3; i++ {
			info, err := sc.get(ctx, f.fetch)
			if err != nil || info.RemainingStorage != 100 {
				t.Fatalf("answer %d: %+v, %v", i, info, err)
			}
		}
		if n := f.count(); n != 1 {
			t.Errorf("the backend was asked %d times, want once", n)
		}
	})

	t.Run("a stale answer comes back at once and is refreshed behind it", func(t *testing.T) {
		f := &countingFetch{space: 100}
		sc := &storageCache{ttl: 10 * time.Millisecond}
		if _, err := sc.get(ctx, f.fetch); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)

		// The backend hangs now, as it does with the link full: the answer must not wait on it.
		release := make(chan struct{})
		f.set(200, nil, release)

		start := time.Now()
		info, err := sc.get(ctx, f.fetch)
		if err != nil || info.RemainingStorage != 100 {
			t.Fatalf("stale answer: %+v, %v; want the last one", info, err)
		}
		if waited := time.Since(start); waited > 100*time.Millisecond {
			t.Errorf("the stale answer took %v; want it at once", waited)
		}

		// However often it is asked in the meantime, one refresh is running at a time.
		eventually(t, "the refresh asking the backend", func() bool { return f.count() == 2 })
		for i := 0; i < 5; i++ {
			sc.get(ctx, f.fetch)
		}
		if n := f.count(); n != 2 {
			t.Errorf("the backend was asked %d times during one refresh, want 2", n)
		}

		close(release)
		eventually(t, "the refreshed answer", func() bool {
			info, _ := sc.get(ctx, f.fetch)
			return info.RemainingStorage == 200
		})
	})

	t.Run("a refresh that fails keeps the last answer and waits out the interval", func(t *testing.T) {
		f := &countingFetch{space: 100}
		sc := &storageCache{ttl: 50 * time.Millisecond}
		if _, err := sc.get(ctx, f.fetch); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)

		f.set(0, errors.New("indexer unreachable"), nil)
		sc.get(ctx, f.fetch)
		eventually(t, "the failed refresh", func() bool { return f.count() == 2 })

		for i := 0; i < 5; i++ {
			info, err := sc.get(ctx, f.fetch)
			if err != nil || info.RemainingStorage != 100 {
				t.Fatalf("after a failed refresh: %+v, %v; want the last answer", info, err)
			}
		}
		if n := f.count(); n != 2 {
			t.Errorf("a backend that just failed was asked %d times, want it left alone until the interval passes", n)
		}
	})

	t.Run("a first question that fails is an error, and the next one asks again", func(t *testing.T) {
		f := &countingFetch{err: errors.New("indexer unreachable")}
		sc := &storageCache{ttl: time.Hour}

		if _, err := sc.get(ctx, f.fetch); err == nil {
			t.Fatal("want the error when there is no answer to fall back on")
		}

		f.set(100, nil, nil)
		info, err := sc.get(ctx, f.fetch)
		if err != nil || info.RemainingStorage != 100 {
			t.Fatalf("second question: %+v, %v", info, err)
		}
		if n := f.count(); n != 2 {
			t.Errorf("the backend was asked %d times, want twice", n)
		}
	})
}
