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

// lockRanges takes the ranges of a lock request for the open. A range that cannot be had puts back
// the ones taken before it in the same request, so that a refused request leaves nothing behind;
// one that is malformed does not, the ranges already taken being taken ([MS-SMB2] 3.3.5.14.2).
func (fs *fileState) lockRanges(op *open, locks []smb2.Lock) uint32 {
	// A request naming several ranges has one answer to give, so it cannot both wait for one of
	// them and report on another: every element of it has to be one that fails immediately.
	if len(locks) > 1 {
		for _, l := range locks {
			if l.Flags&smb2.LOCKFLAG_FAIL_IMMEDIATELY == 0 {
				return smb2.STATUS_INVALID_PARAMETER
			}
		}
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	taken := 0
	for _, l := range locks {
		if !validLockFlags(l.Flags) {
			return smb2.STATUS_INVALID_PARAMETER
		}

		r := byteRange{offset: l.Offset, length: l.Length}
		exclusive := l.Flags&smb2.LOCKFLAG_EXCLUSIVE_LOCK != 0
		if fs.lockConflict(op, r, exclusive) {
			fs.locks = fs.locks[:len(fs.locks)-taken]

			return smb2.STATUS_LOCK_NOT_GRANTED
		}

		fs.locks = append(fs.locks, byteRangeLock{byteRange: r, open: op, exclusive: exclusive})
		taken++
	}

	return smb2.STATUS_OK
}

// unlockRanges gives back the ranges of an unlock request. Each is named exactly as it was taken,
// and what has been given back stays given back however the rest of the request ends
// ([MS-SMB2] 3.3.5.14.1).
func (fs *fileState) unlockRanges(op *open, locks []smb2.Lock) uint32 {
	fs.mu.Lock()
	defer fs.mu.Unlock()

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

	fs.locks = kept
}
