package main

import (
	"math"

	"github.com/mike76-dev/sombrero/smb2"
)

// byteRange is a stretch of a file as a lock names it. Neither end is bounded by the size of the
// file: a range may sit past the end of it, or cover no byte at all ([FSBO] 3.2).
type byteRange struct {
	offset uint64
	length uint64
}

// end is the first offset past the range, saturated at the largest there is. A range whose end
// runs off what a 64-bit number holds is one Windows itself refuses to reason about, and letting
// it wrap around would make it cover the start of the file.
func (r byteRange) end() uint64 {
	if r.offset+r.length < r.offset {
		return math.MaxUint64
	}

	return r.offset + r.length
}

// overlaps reports whether the two ranges cover a byte in common, or, where one of them covers
// none, whether the other holds the offset it sits at without starting there ([FSBO] 3.2.1).
func (r byteRange) overlaps(other byteRange) bool {
	switch {
	case r.length == 0 && other.length == 0:
		return false
	case r.length == 0:
		return other.offset < r.offset && other.end() > r.offset
	case other.length == 0:
		return r.offset < other.offset && r.end() > other.offset
	default:
		return r.offset < other.end() && other.offset < r.end()
	}
}

// lockSequenceEntries is how many lock sequences an open remembers ([MS-SMB2] 3.3.1.5). The
// entries are named by the request, counting from one.
const lockSequenceEntries = 64

// lockSequenceEntry is what an open remembers of one lock request: the sequence number it carried,
// once it has carried one at all.
type lockSequenceEntry struct {
	number uint8
	valid  bool
}

// replayedLock reports whether the request is one the open has already been answered for, which is
// what a client sends again when it has reclaimed a handle and cannot know how far the server got
// with it. An index naming no entry is one there is nothing remembered under.
func (op *open) replayedLock(index uint32, number uint8) bool {
	if index == 0 || index > lockSequenceEntries {
		return false
	}

	op.mu.Lock()
	defer op.mu.Unlock()

	entry := &op.lockSequence[index-1]
	if !entry.valid {
		return false
	}

	if entry.number == number {
		return true
	}

	// A different sequence number under the same entry is the client having moved on, so what is
	// remembered there answers for nothing any more.
	entry.valid = false

	return false
}

// rememberLock records that the request has been carried out, so that the same one arriving again
// is answered from what is remembered rather than done a second time.
func (op *open) rememberLock(index uint32, number uint8) {
	if index == 0 || index > lockSequenceEntries {
		return
	}

	op.mu.Lock()
	defer op.mu.Unlock()

	op.lockSequence[index-1] = lockSequenceEntry{number: number, valid: true}
}

// byteRangeLock is a range of a file an open has claimed. The claim belongs to the open rather
// than to the session or the client behind it: a second handle on the same file, however it was
// come by, is as much a stranger to the range as anybody else ([FSBO] 3.2).
type byteRangeLock struct {
	byteRange
	open      *open
	exclusive bool
}

// validLockFlags reports whether the flags of an element are one of the combinations a lock may
// carry ([MS-SMB2] 2.2.26.1). An unlock among them is a request asking for both at once, and is
// refused here along with the undefined bits.
func validLockFlags(flags uint32) bool {
	switch flags &^ smb2.LOCKFLAG_FAIL_IMMEDIATELY {
	case smb2.LOCKFLAG_SHARED_LOCK, smb2.LOCKFLAG_EXCLUSIVE_LOCK:
		return true
	default:
		return false
	}
}

// lockConflict reports whether the range is one the open may not have. fs.mu must be held.
func (fs *fileState) lockConflict(op *open, r byteRange, exclusive bool) bool {
	for _, held := range fs.locks {
		if !held.overlaps(r) {
			continue
		}

		if held.exclusive {
			// An open is never in its own way over a range it holds exclusively: it may take
			// the range again, and it may take a shared lock over it ([FSBO] 3.4.2).
			if held.open != op {
				return true
			}

			continue
		}

		// A shared range keeps everyone out of writing it, the open that took it included, so
		// an exclusive claim over one is refused whoever holds it.
		if exclusive {
			return true
		}
	}

	return false
}

// lockWaiter is the channel closed the next time a range of the file is given back, which is what
// a request waiting for one listens on. fs.mu must be held.
func (fs *fileState) lockWaiter() chan struct{} {
	if fs.lockWait == nil {
		fs.lockWait = make(chan struct{})
	}

	return fs.lockWait
}

// lockChanged tells whatever is waiting for a range that the ones held have changed, so that it
// looks again. fs.mu must be held.
func (fs *fileState) lockChanged() {
	if fs.lockWait != nil {
		close(fs.lockWait)
		fs.lockWait = nil
	}
}

// lockRanges takes the ranges of a lock request for the open. A range that cannot be had puts back
// the ones taken before it in the same request, so that a refused request leaves nothing behind;
// one that is malformed does not, the ranges already taken being taken ([MS-SMB2] 3.3.5.14.2).
//
// It reports separately that the request is one to wait on rather than one to answer: a range that
// conflicts and was not to be reported at once is queued instead of refused. Such a request names
// the one range, every element of a longer one having had to ask to fail immediately, so nothing
// has been taken by the time it is met.
func (fs *fileState) lockRanges(op *open, locks []smb2.Lock) (uint32, bool) {
	// A request naming several ranges has one answer to give, so it cannot both wait for one of
	// them and report on another: every element of it has to be one that fails immediately.
	if len(locks) > 1 {
		for _, l := range locks {
			if l.Flags&smb2.LOCKFLAG_FAIL_IMMEDIATELY == 0 {
				return smb2.STATUS_INVALID_PARAMETER, false
			}
		}
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	taken := 0
	for _, l := range locks {
		if !validLockFlags(l.Flags) {
			return smb2.STATUS_INVALID_PARAMETER, false
		}

		r := byteRange{offset: l.Offset, length: l.Length}
		exclusive := l.Flags&smb2.LOCKFLAG_EXCLUSIVE_LOCK != 0
		if fs.lockConflict(op, r, exclusive) {
			if l.Flags&smb2.LOCKFLAG_FAIL_IMMEDIATELY == 0 {
				return smb2.STATUS_OK, true
			}

			if taken > 0 {
				fs.locks = fs.locks[:len(fs.locks)-taken]
				fs.lockChanged()
			}

			return smb2.STATUS_LOCK_NOT_GRANTED, false
		}

		fs.locks = append(fs.locks, byteRangeLock{byteRange: r, open: op, exclusive: exclusive})
		taken++
	}

	return smb2.STATUS_OK, false
}

// awaitLock takes the range once nothing is in the way of it, which is what a lock that was not to
// be refused does instead of being refused. The client is left waiting on the request until the
// range comes free, until the wait is called off, or until the handle behind it goes.
func (fs *fileState) awaitLock(op *open, l smb2.Lock, stop, gone <-chan struct{}) uint32 {
	r := byteRange{offset: l.Offset, length: l.Length}
	exclusive := l.Flags&smb2.LOCKFLAG_EXCLUSIVE_LOCK != 0

	for {
		fs.mu.Lock()
		if !fs.lockConflict(op, r, exclusive) {
			fs.locks = append(fs.locks, byteRangeLock{byteRange: r, open: op, exclusive: exclusive})
			fs.mu.Unlock()

			return smb2.STATUS_OK
		}

		// Taken before the lock of the file is let go of, or a range given back in between would
		// be a change this never hears about and goes on waiting through.
		freed := fs.lockWaiter()
		fs.mu.Unlock()

		select {
		case <-freed:
		case <-stop:
			return smb2.STATUS_CANCELLED
		case <-gone:
			return smb2.STATUS_FILE_CLOSED
		}
	}
}

// dropLock gives back the range the open took last, which is what a lock granted to a client that
// is no longer waiting for it comes to: what that client was told is that its request was over.
func (fs *fileState) dropLock(op *open, l smb2.Lock) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	want := byteRangeLock{
		byteRange: byteRange{offset: l.Offset, length: l.Length},
		open:      op,
		exclusive: l.Flags&smb2.LOCKFLAG_EXCLUSIVE_LOCK != 0,
	}

	for i := len(fs.locks) - 1; i >= 0; i-- {
		if fs.locks[i] == want {
			fs.locks = append(fs.locks[:i], fs.locks[i+1:]...)
			fs.lockChanged()

			return
		}
	}
}

// unlockRanges gives back the ranges of an unlock request. Each is named exactly as it was taken,
// and what has been given back stays given back however the rest of the request ends
// ([MS-SMB2] 3.3.5.14.1).
func (fs *fileState) unlockRanges(op *open, locks []smb2.Lock) uint32 {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Whatever the request comes to, a range it did give back is one something may be waiting
	// for. This runs before the lock of the file is let go of, the defers unwinding in reverse.
	var given bool
	defer func() {
		if given {
			fs.lockChanged()
		}
	}()

	for _, l := range locks {
		// An element that asks to lock has no place in a request that unlocks. Whether it is to
		// fail immediately says nothing here, an unlock never having anything to wait for.
		if l.Flags&(smb2.LOCKFLAG_SHARED_LOCK|smb2.LOCKFLAG_EXCLUSIVE_LOCK) != 0 {
			return smb2.STATUS_INVALID_PARAMETER
		}

		// Where the open holds the range both ways, the exclusive lock is the one given back:
		// the shared one outlives it and goes with a second unlock ([FSBO] 3.4.2).
		found := -1
		for i, held := range fs.locks {
			if held.open != op || held.byteRange != (byteRange{offset: l.Offset, length: l.Length}) {
				continue
			}

			if held.exclusive {
				found = i
				break
			}

			if found < 0 {
				found = i
			}
		}

		if found < 0 {
			return smb2.STATUS_RANGE_NOT_LOCKED
		}

		fs.locks = append(fs.locks[:found], fs.locks[found+1:]...)
		given = true
	}

	return smb2.STATUS_OK
}

// releaseLocks drops every range the open holds, which is what the end of a handle does to them:
// a byte-range lock lives no longer than the handle that took it ([FSBO] 3.2).
func (fs *fileState) releaseLocks(op *open) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	kept := fs.locks[:0]
	for _, held := range fs.locks {
		if held.open != op {
			kept = append(kept, held)
		}
	}

	if len(kept) < len(fs.locks) {
		fs.lockChanged()
	}

	fs.locks = kept
}
