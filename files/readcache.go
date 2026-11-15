package files

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	// firstFetch is how much of a file is fetched at first; the fetches double
	// while the reads stay sequential, up to the chunk, so that a seek-and-peek
	// costs little and a stream soon reads in whole chunks.
	firstFetch = 1 << 20
	// readCacheBudget is how much fetched data the cache holds for all files.
	readCacheBudget = 128 << 20
	// fetchTimeout is how long a fetch may take.
	fetchTimeout = 5 * time.Minute
)

// fileKey names a file's contents in the cache: a file written anew is another.
type fileKey struct {
	share    string
	key      string
	version  uint64
	size     uint64
	modified time.Time
}

// fetchFunc reads a range of a file into the writer, from wherever it is.
type fetchFunc func(ctx context.Context, offset, length uint64, w io.Writer) error

// readCache holds the pieces of files fetched lately, shared between requests,
// with the next piece fetched ahead of a sequential reader. A client that reads
// a file in many requests, as a mounted WebDAV drive does when a player seeks,
// so finds what it comes back to in hand, and what it goes on to under way.
type readCache struct {
	mu     sync.Mutex
	files  map[fileKey][]*segment
	recent *list.List
	used   uint64
	budget uint64
}

// segment is a piece of a file, fetched or being fetched: data or err is set
// once done is closed.
type segment struct {
	file fileKey
	at   uint64
	n    uint64
	data []byte
	err  error
	done chan struct{}
	elem *list.Element
}

func (s *segment) end() uint64 { return s.at + s.n }

func newReadCache(budget uint64) *readCache {
	return &readCache{files: make(map[fileKey][]*segment), recent: list.New(), budget: budget}
}

// segmentAt is the segment holding the byte at pos, fetched if none does. The
// fetch follows on from a segment ending at pos with twice its size, up to the
// chunk, and otherwise starts small.
func (c *readCache) segmentAt(file fileKey, pos, chunk uint64, fetch fetchFunc) *segment {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, limit := min(firstFetch, chunk), file.size-pos
	for _, s := range c.files[file] {
		if s.at <= pos && pos < s.end() {
			c.recent.MoveToFront(s.elem)
			return s
		}
		if s.end() == pos {
			n = min(2*s.n, chunk)
		}
		if s.at > pos {
			limit = min(limit, s.at-pos)
		}
	}
	return c.start(file, pos, min(n, limit), fetch)
}

// ahead fetches the segment at pos unless one holds it already.
func (c *readCache) ahead(file fileKey, pos, chunk uint64, fetch fetchFunc) {
	c.segmentAt(file, pos, chunk, fetch)
}

// start fetches n bytes of the file from at, dropping the segments longest
// unused to stay within the budget. Called with the lock held.
func (c *readCache) start(file fileKey, at, n uint64, fetch fetchFunc) *segment {
	for e := c.recent.Back(); e != nil && c.used+n > c.budget; {
		prev := e.Prev()
		old := e.Value.(*segment)
		select {
		case <-old.done:
			c.remove(old)
		default:
		}
		e = prev
	}
	s := &segment{file: file, at: at, n: n, done: make(chan struct{})}
	s.elem = c.recent.PushFront(s)
	c.files[file] = append(c.files[file], s)
	c.used += n
	go func() {
		defer close(s.done)
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		var b bytes.Buffer
		b.Grow(int(n))
		if err := fetch(ctx, at, n, &b); err != nil {
			s.err = err
		} else if uint64(b.Len()) != n {
			s.err = fmt.Errorf("read %d bytes of %s at %d, want %d", b.Len(), file.key, at, n)
		} else {
			s.data = b.Bytes()
		}
		if s.err != nil {
			c.mu.Lock()
			c.remove(s)
			c.mu.Unlock()
		}
	}()
	return s
}

// remove forgets a segment. Called with the lock held.
func (c *readCache) remove(s *segment) {
	if s.elem == nil {
		return
	}
	c.recent.Remove(s.elem)
	s.elem = nil
	c.used -= s.n
	segments := c.files[s.file]
	for i, o := range segments {
		if o == s {
			segments = append(segments[:i], segments[i+1:]...)
			break
		}
	}
	if len(segments) == 0 {
		delete(c.files, s.file)
	} else {
		c.files[s.file] = segments
	}
}
