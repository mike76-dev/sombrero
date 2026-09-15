package client

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	defaultStorageTTL     = 30 * time.Second
	storageRefreshTimeout = 10 * time.Second
)

// storageCache keeps the last storage answer. Only the first call waits for the backend; a stale
// answer is returned at once and refreshed in the background. The zero value is ready to use.
type storageCache struct {
	mu         sync.Mutex
	ttl        time.Duration // defaultStorageTTL if zero
	info       StorageInfo
	known      bool
	checked    time.Time
	refreshing bool
}

// get returns the cached answer, fetching it the first time and refreshing it once stale.
func (sc *storageCache) get(ctx context.Context, fetch func(context.Context) (StorageInfo, error)) (StorageInfo, error) {
	sc.mu.Lock()
	if !sc.known {
		sc.mu.Unlock()

		info, err := fetch(ctx)
		if err != nil {
			return StorageInfo{}, err
		}

		sc.mu.Lock()
		sc.info, sc.known, sc.checked = info, true, time.Now()
		sc.mu.Unlock()

		return info, nil
	}

	info := sc.info
	ttl := sc.ttl
	if ttl == 0 {
		ttl = defaultStorageTTL
	}
	if !sc.refreshing && time.Since(sc.checked) >= ttl {
		sc.refreshing = true
		go sc.refresh(fetch)
	}
	sc.mu.Unlock()

	return info, nil
}

// refresh fetches a new answer. A failure keeps the old one, and is not retried before the TTL.
func (sc *storageCache) refresh(fetch func(context.Context) (StorageInfo, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), storageRefreshTimeout)
	defer cancel()

	info, err := fetch(ctx)

	sc.mu.Lock()
	defer sc.mu.Unlock()

	sc.refreshing = false
	sc.checked = time.Now()
	if err != nil {
		log.Printf("failed to refresh the storage info, keeping the last answer: %v", err)
		return
	}
	sc.info = info
}
