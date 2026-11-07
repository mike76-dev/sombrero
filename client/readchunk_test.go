package client

import "testing"

// TestReadChunkSize: a chunk is a whole number of small slabs or an exact fraction of a large one,
// and no larger than a client waits for on a slow evening.
func TestReadChunkSize(t *testing.T) {
	const mib = 1024 * 1024
	for slab, want := range map[uint64]uint64{0: 8 * mib, 1 * mib: 8 * mib, 8 * mib: 8 * mib, 40 * mib: 10 * mib, 64 * mib: 16 * mib} {
		if got := ReadChunkSize(slab); got != want {
			t.Errorf("slab of %d MiB: chunk of %d MiB, want %d", slab/mib, got/mib, want/mib)
		}
	}
}
