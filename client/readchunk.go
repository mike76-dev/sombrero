package client

const (
	// Bounds for the read chunk size. Within these bounds, chunks are kept aligned to the
	// slab size of the backend, so that a chunk download translates into as few range
	// requests as possible. A read not in the cache waits for its whole chunk, and an SMB
	// client gives up on a read after some twenty seconds, so the chunks are kept small.
	minReadChunkSize = 8 * 1024 * 1024
	maxReadChunkSize = 16 * 1024 * 1024
)

// ReadChunkSize picks a read chunk size for the given slab size: a whole multiple of
// small slabs, or an exact fraction of a large slab.
func ReadChunkSize(slabSize uint64) uint64 {
	if slabSize == 0 {
		return minReadChunkSize
	}

	chunk := slabSize
	for chunk < minReadChunkSize {
		chunk += slabSize
	}
	for chunk > maxReadChunkSize {
		chunk /= 2
	}

	return chunk
}
