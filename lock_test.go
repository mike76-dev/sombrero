package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// lockStatus is what the server answered a lock request with, the request having reached it at all.
func (cl *testClient) lockStatus(buf []byte, err error) uint32 {
	cl.h.t.Helper()

	if err != nil {
		cl.h.t.Fatalf("the lock request failed: %v", err)
	}

	return smb2.Header(buf).Status()
}

// TestLockOnAHandleThatIsGoneIsRefused is the lock that names no open. Nothing is locked here, and
// nothing has to be, but [MS-SMB2] 3.3.5.14 still has the handle looked up and answers the request
// with STATUS_FILE_CLOSED when it names none. A success in its place tells the client that a range
// of a file it no longer holds is now its own, which is the one answer that cannot be true.
func TestLockOnAHandleThatIsGoneIsRefused(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	// A handle that is open is locked over, so that what follows is refused for the handle rather
	// than for the lock.
	buf, err := cl.lockRange(fid, 0, 512)
	if err != nil {
		t.Fatalf("the lock failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("a lock over an open handle was answered %#x, want it granted", status)
	}

	// The volatile half of a handle that exists, with a persistent half that belongs to nothing.
	forged := make([]byte, 16)
	copy(forged, fid)
	binary.LittleEndian.PutUint64(forged[8:], binary.LittleEndian.Uint64(fid[8:])^0xdeadbeef)

	buf, err = cl.lockRange(forged, 0, 512)
	if err != nil {
		t.Fatalf("the lock failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("a lock on a mismatched handle was answered %#x, want STATUS_FILE_CLOSED", status)
	}

	if _, err := cl.closeHandle(fid); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	buf, err = cl.lockRange(fid, 0, 512)
	if err != nil {
		t.Fatalf("the lock failed: %v", err)
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("a lock on a closed handle was answered %#x, want STATUS_FILE_CLOSED", status)
	}
}

// TestAnExclusiveRangeKeepsAnotherOpenOut is what a lock is for: a range one handle holds
// exclusively is a range no other handle may have, whichever way it asks for it.
func TestAnExclusiveRangeKeepsAnotherOpenOut(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the first exclusive lock was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRange(other, 256, 512)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("an exclusive lock overlapping another open's was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}

	if status := cl.lockStatus(cl.lockRangeShared(other, 256, 512)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("a shared lock over another open's exclusive range was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}

	// The range next to it is nobody's, and stays available.
	if status := cl.lockStatus(cl.lockRange(other, 512, 512)); status != smb2.STATUS_OK {
		t.Errorf("an exclusive lock clear of the held range was answered %#x, want it granted", status)
	}
}

// TestSharedRangesAreSharedButNotWithAnExclusiveOne is the other half of the rule: shared locks sit
// on one range together, and shut out only a claim that wants the range to itself.
func TestSharedRangesAreSharedButNotWithAnExclusiveOne(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRangeShared(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the first shared lock was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRangeShared(other, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("a second shared lock over the same range was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRange(other, 0, 512)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("an exclusive lock over a shared range was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}
}

// TestAnOpenIsNotInItsOwnWayOverARangeItHolds covers ownership. A range belongs to the handle that
// took it, which may take it again and may share it with itself; a shared range is nobody's to
// claim exclusively, its own holder included ([FSBO] 3.4.2).
func TestAnOpenIsNotInItsOwnWayOverARangeItHolds(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	if status := cl.lockStatus(cl.lockRange(fid, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the exclusive lock was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRange(fid, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("taking the same range exclusively again was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRangeShared(fid, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("a shared lock over the handle's own exclusive range was answered %#x, want it granted", status)
	}

	// The shared lock just taken is now in the way of an exclusive one, on this handle as much as
	// on any other.
	if status := cl.lockStatus(cl.lockRange(fid, 0, 512)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("an exclusive lock over the handle's own shared range was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}
}

// TestAnUnlockNamesTheRangeExactly is the unlock that has to match. Part of a range, a span across
// two of them, or a range another handle took are all answered STATUS_RANGE_NOT_LOCKED ([FSBO] 3.2).
func TestAnUnlockNamesTheRangeExactly(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the exclusive lock was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.unlockRange(held, 0, 256)); status != smb2.STATUS_RANGE_NOT_LOCKED {
		t.Errorf("unlocking part of a locked range was answered %#x, want STATUS_RANGE_NOT_LOCKED", status)
	}

	if status := cl.lockStatus(cl.unlockRange(other, 0, 512)); status != smb2.STATUS_RANGE_NOT_LOCKED {
		t.Errorf("unlocking another open's range was answered %#x, want STATUS_RANGE_NOT_LOCKED", status)
	}

	if status := cl.lockStatus(cl.unlockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("unlocking the range as it was taken was answered %#x, want it given back", status)
	}

	if status := cl.lockStatus(cl.unlockRange(held, 0, 512)); status != smb2.STATUS_RANGE_NOT_LOCKED {
		t.Errorf("unlocking the range a second time was answered %#x, want STATUS_RANGE_NOT_LOCKED", status)
	}

	// The range is free, which is what the unlock was for.
	if status := cl.lockStatus(cl.lockRange(other, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("an exclusive lock over the freed range was answered %#x, want it granted", status)
	}
}

// TestAnUnlockGivesBackTheExclusiveLockFirst is the handle holding one range both ways. Each lock
// is given back on its own, and the exclusive one goes first ([FSBO] 3.4.2).
func TestAnUnlockGivesBackTheExclusiveLockFirst(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the exclusive lock was answered %#x, want it granted", status)
	}
	if status := cl.lockStatus(cl.lockRangeShared(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the shared lock over it was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.unlockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the first unlock was answered %#x, want it given back", status)
	}

	// The exclusive lock is the one that went: another handle may share the range, but not take
	// it for itself while the shared lock stands.
	if status := cl.lockStatus(cl.lockRangeShared(other, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("a shared lock after the exclusive one was given back was answered %#x, want it granted", status)
	}
	if status := cl.lockStatus(cl.lockRange(other, 0, 512)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("an exclusive lock while the shared one stands was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}
}

// TestARefusedLockPutsBackWhatTheRequestTook is the request of several ranges that cannot have all
// of them: the ones it did take are given back, so that a client told no is holding nothing
// ([MS-SMB2] 3.3.5.14.2).
func TestARefusedLockPutsBackWhatTheRequestTook(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 512, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the lock standing in the way was answered %#x, want it granted", status)
	}

	// The first range is free and the second one is not.
	both := []smb2.Lock{
		{Offset: 0, Length: 512, Flags: smb2.LOCKFLAG_EXCLUSIVE_LOCK | smb2.LOCKFLAG_FAIL_IMMEDIATELY},
		{Offset: 512, Length: 512, Flags: smb2.LOCKFLAG_EXCLUSIVE_LOCK | smb2.LOCKFLAG_FAIL_IMMEDIATELY},
	}
	if status := cl.lockStatus(cl.lockElements(other, both)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Fatalf("a request of a range that was taken was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}

	// Had the first range been kept, this would be refused.
	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("the range of the refused request was answered %#x, want it free again", status)
	}
}

// TestSeveralRangesMustAllFailImmediately is the request the server cannot answer: it has one
// status to give, so it may not be asked to wait for one range while reporting on another.
func TestSeveralRangesMustAllFailImmediately(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	both := []smb2.Lock{
		{Offset: 0, Length: 512, Flags: smb2.LOCKFLAG_EXCLUSIVE_LOCK | smb2.LOCKFLAG_FAIL_IMMEDIATELY},
		{Offset: 512, Length: 512, Flags: smb2.LOCKFLAG_EXCLUSIVE_LOCK},
	}
	if status := cl.lockStatus(cl.lockElements(fid, both)); status != smb2.STATUS_INVALID_PARAMETER {
		t.Fatalf("a request with a range that may wait was answered %#x, want STATUS_INVALID_PARAMETER", status)
	}

	// Nothing of it was taken, the request having been turned away before any of it was weighed.
	if status := cl.lockStatus(cl.lockRange(fid, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("the range of the refused request was answered %#x, want it free", status)
	}
}

// TestLockFlagsOutsideTheValidCombinations is the flags field held to the five combinations
// [MS-SMB2] 2.2.26.1 allows. Anything else is a request the server cannot make sense of.
func TestLockFlagsOutsideTheValidCombinations(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	tests := []struct {
		name  string
		flags uint32
		want  uint32
	}{
		{"nothing at all", 0, smb2.STATUS_INVALID_PARAMETER},
		{"shared and exclusive at once", smb2.LOCKFLAG_SHARED_LOCK | smb2.LOCKFLAG_EXCLUSIVE_LOCK, smb2.STATUS_INVALID_PARAMETER},
		{"locking and unlocking at once", smb2.LOCKFLAG_UNLOCK | smb2.LOCKFLAG_SHARED_LOCK, smb2.STATUS_INVALID_PARAMETER},
		{"a bit that is not defined", smb2.LOCKFLAG_EXCLUSIVE_LOCK | 0x00000008, smb2.STATUS_INVALID_PARAMETER},
		{"an unlock told to fail immediately", smb2.LOCKFLAG_UNLOCK | smb2.LOCKFLAG_FAIL_IMMEDIATELY, smb2.STATUS_RANGE_NOT_LOCKED},
		{"a lock that may wait", smb2.LOCKFLAG_EXCLUSIVE_LOCK, smb2.STATUS_OK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elements := []smb2.Lock{{Offset: 4096, Length: 16, Flags: tt.flags}}
			if status := cl.lockStatus(cl.lockElements(fid, elements)); status != tt.want {
				t.Errorf("flags %#x were answered %#x, want %#x", tt.flags, status, tt.want)
			}
		})
	}
}

// TestAZeroLengthRangeConflictsWithWhatContainsIt is the range covering no byte, which conflicts
// with a range that holds the offset it sits at but not with one that starts there ([FSBO] 3.2.1).
func TestAZeroLengthRangeConflictsWithWhatContainsIt(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 6, 0)); status != smb2.STATUS_OK {
		t.Fatalf("a lock of no length was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRange(other, 5, 10)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("a range containing the empty one was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}

	if status := cl.lockStatus(cl.lockRange(other, 6, 10)); status != smb2.STATUS_OK {
		t.Errorf("a range starting at the empty one was answered %#x, want it granted", status)
	}
}

// TestTheLocksOfAHandleGoWhenItCloses is what a lock is worth after the handle behind it: nothing.
// A client that never unlocks would otherwise leave the range shut for as long as the file is open
// to anybody ([FSBO] 3.2).
func TestTheLocksOfAHandleGoWhenItCloses(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the exclusive lock was answered %#x, want it granted", status)
	}

	if _, err := cl.closeHandle(held); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	if status := cl.lockStatus(cl.lockRange(other, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("the range of the closed handle was answered %#x, want it free again", status)
	}
}

// TestIntegrationALockThatMayWaitIsAnsweredWhenTheRangeComesFree is the request that was not told
// to fail immediately: it is neither granted nor refused, it waits, and the client hears about it
// when the range is given back ([MS-SMB2] 3.3.5.14.2).
func TestIntegrationALockThatMayWaitIsAnsweredWhenTheRangeComesFree(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the lock standing in the way was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRangeWaiting(other, 0, 512)); status != smb2.STATUS_PENDING {
		t.Fatalf("a lock willing to wait was answered %#x, want it taken on", status)
	}

	cl.quiet(200*time.Millisecond, "the waiting lock was answered while the range was still taken")

	if status := cl.lockStatus(cl.unlockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the unlock was answered %#x, want the range given back", status)
	}

	answer := cl.recv(2 * time.Second)
	if got := smb2.Header(answer).Command(); got != smb2.SMB2_LOCK {
		t.Fatalf("what came back answers command %d, want the lock", got)
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the waiting lock was answered %#x, want it granted", status)
	}

	// The range is the waiter's now, and not the handle's that gave it up.
	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("the range the waiter was given was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}
}

// TestIntegrationAWaitingLockIsAnsweredWhenItIsCancelled is the client that stops waiting. The
// request is answered as cancelled and the range it was waiting for is left where it was.
func TestIntegrationAWaitingLockIsAnsweredWhenItIsCancelled(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the lock standing in the way was answered %#x, want it granted", status)
	}

	interim, err := cl.lockRangeWaiting(other, 0, 512)
	if err != nil {
		t.Fatalf("the waiting lock failed: %v", err)
	}
	if status := smb2.Header(interim).Status(); status != smb2.STATUS_PENDING {
		t.Fatalf("a lock willing to wait was answered %#x, want it taken on", status)
	}

	asyncID := smb2.Header(interim).AsyncID()
	if err := cl.cancel(asyncCancelRequest(asyncID, cl.ss.sessionID, cl.tc.treeID)); err != nil {
		t.Fatalf("the cancel failed: %v", err)
	}

	answer := cl.recv(2 * time.Second)
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_CANCELLED {
		t.Fatalf("the cancelled lock was answered %#x, want STATUS_CANCELLED", status)
	}

	// Nothing was taken for it. Were the waiter holding the range after all, this would be refused.
	if status := cl.lockStatus(cl.unlockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the unlock was answered %#x, want the range given back", status)
	}
	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("the range after the cancelled wait was answered %#x, want it free", status)
	}
}

// TestIntegrationAWaitingLockIsAnsweredWhenTheHandleGoes is the handle closed out from under a
// request that is still waiting. The client is waiting on that request, and a handle that is gone
// is the answer to it: left unanswered, it is one the client counts as outstanding for as long as
// it holds the connection.
func TestIntegrationAWaitingLockIsAnsweredWhenTheHandleGoes(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	if status := cl.lockStatus(cl.lockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the lock standing in the way was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockRangeWaiting(other, 0, 512)); status != smb2.STATUS_PENDING {
		t.Fatalf("a lock willing to wait was answered %#x, want it taken on", status)
	}

	if _, err := cl.closeHandle(other); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	answer := cl.recv(2 * time.Second)
	if got := smb2.Header(answer).Command(); got != smb2.SMB2_LOCK {
		t.Fatalf("what came back answers command %d, want the lock", got)
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_FILE_CLOSED {
		t.Errorf("the lock whose handle went was answered %#x, want STATUS_FILE_CLOSED", status)
	}
}

// parked waits until a request is waiting for a range of the file behind the handle, so that what
// follows reaches a waiter that has gone to sleep rather than one still on its way there.
func (h *smbTest) parked(fid []byte) {
	h.t.Helper()

	fs := h.srv.globalOpenTable[openIDOf(fid)].file
	for i := 0; i < 200; i++ {
		fs.mu.Lock()
		waiting := fs.lockWait != nil
		fs.mu.Unlock()

		if waiting {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	h.t.Fatal("nothing ever waited for the range")
}

// TestIntegrationAWokenLockLooksAtTheRangeAgain is the waiter told that the ranges have changed
// when the one it wants has not. Every waiter is woken by any range being given back, so one that
// took what it was waiting for on being woken would take a range that is still somebody else's.
func TestIntegrationAWokenLockLooksAtTheRangeAgain(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	// Two ranges, of which the waiter below wants only the first.
	for _, offset := range []uint64{0, 512} {
		if status := cl.lockStatus(cl.lockRange(held, offset, 512)); status != smb2.STATUS_OK {
			t.Fatalf("the lock at %d was answered %#x, want it granted", offset, status)
		}
	}

	if status := cl.lockStatus(cl.lockRangeWaiting(other, 0, 512)); status != smb2.STATUS_PENDING {
		t.Fatalf("a lock willing to wait was answered %#x, want it taken on", status)
	}
	h.parked(other)

	// The range given back is not the one being waited for, so the waiter is woken for nothing
	// and has to go back to waiting.
	if status := cl.lockStatus(cl.unlockRange(held, 512, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the unlock was answered %#x, want the range given back", status)
	}

	cl.quiet(200*time.Millisecond, "the waiting lock was granted a range that was still taken")

	// The range it is waiting for, and the answer it has been waiting for.
	if status := cl.lockStatus(cl.unlockRange(held, 0, 512)); status != smb2.STATUS_OK {
		t.Fatalf("the unlock was answered %#x, want the range given back", status)
	}

	answer := cl.recv(2 * time.Second)
	if got := smb2.Header(answer).Command(); got != smb2.SMB2_LOCK {
		t.Fatalf("what came back answers command %d, want the lock", got)
	}
	if status := smb2.Header(answer).Status(); status != smb2.STATUS_OK {
		t.Errorf("the waiting lock was answered %#x, want it granted", status)
	}
}

// TestDropLockGivesBackOnlyTheRangeTakenLast is the range granted to a client that had already
// given up on it. One lock goes back, not every lock the open holds over the range.
func TestDropLockGivesBackOnlyTheRangeTakenLast(t *testing.T) {
	fs := new(fileState)
	holder, other := new(open), new(open)

	taken := smb2.Lock{Offset: 0, Length: 512, Flags: smb2.LOCKFLAG_EXCLUSIVE_LOCK | smb2.LOCKFLAG_FAIL_IMMEDIATELY}
	for i := 0; i < 2; i++ {
		if status, wait := fs.lockRanges(holder, []smb2.Lock{taken}); status != smb2.STATUS_OK || wait {
			t.Fatalf("lock %d was answered %#x (waiting %v), want it granted", i, status, wait)
		}
	}

	fs.dropLock(holder, taken)

	// One of the two is left, so the range is still the holder's.
	if status, _ := fs.lockRanges(other, []smb2.Lock{taken}); status != smb2.STATUS_LOCK_NOT_GRANTED {
		t.Errorf("the range after one lock went back was answered %#x, want STATUS_LOCK_NOT_GRANTED", status)
	}

	fs.dropLock(holder, taken)

	if status, _ := fs.lockRanges(other, []smb2.Lock{taken}); status != smb2.STATUS_OK {
		t.Errorf("the range after both locks went back was answered %#x, want it free", status)
	}
}

// exclusiveElement is the one lock element the sequence tests send, over and over.
func exclusiveElement(offset, length uint64) []smb2.Lock {
	return []smb2.Lock{{
		Offset: offset,
		Length: length,
		Flags:  smb2.LOCKFLAG_EXCLUSIVE_LOCK | smb2.LOCKFLAG_FAIL_IMMEDIATELY,
	}}
}

// unlockElement is exclusiveElement giving the range back.
func unlockElement(offset, length uint64) []smb2.Lock {
	return []smb2.Lock{{Offset: offset, Length: length, Flags: smb2.LOCKFLAG_UNLOCK}}
}

// TestAnUnlockSentAgainUnderItsSequenceIsAnsweredAsItWas is the request a client sends twice. It has
// reclaimed a handle, or holds it over more than one channel, and cannot know how far the server
// got; the second copy carries the sequence of the first and is answered from what the open
// remembers, not weighed anew ([MS-SMB2] 3.3.5.14). Weighed again, it finds the range already given
// back and reports a range that is not locked, for a request that succeeded.
func TestAnUnlockSentAgainUnderItsSequenceIsAnsweredAsItWas(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	cl.conn.serverCapabilities |= smb2.GLOBAL_CAP_MULTI_CHANNEL

	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	if status := cl.lockStatus(cl.lockSequenced(fid, exclusiveElement(0, 512), 1, 1)); status != smb2.STATUS_OK {
		t.Fatalf("the lock was answered %#x, want it granted", status)
	}

	if status := cl.lockStatus(cl.lockSequenced(fid, unlockElement(0, 512), 2, 1)); status != smb2.STATUS_OK {
		t.Fatalf("the unlock was answered %#x, want the range given back", status)
	}

	if status := cl.lockStatus(cl.lockSequenced(fid, unlockElement(0, 512), 2, 1)); status != smb2.STATUS_OK {
		t.Errorf("the unlock sent again was answered %#x, want the answer it got the first time", status)
	}
}

// TestALockSentAgainUnderItsSequenceIsNotTakenTwice is the same request on the way in. Taking the
// range a second time leaves the open holding two locks over it where the client believes it holds
// one, and the range stays shut after the unlock the client does send.
func TestALockSentAgainUnderItsSequenceIsNotTakenTwice(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	cl.conn.serverCapabilities |= smb2.GLOBAL_CAP_MULTI_CHANNEL

	first, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	second, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	held, other := createdFileID(first), createdFileID(second)

	for i := 0; i < 2; i++ {
		if status := cl.lockStatus(cl.lockSequenced(held, exclusiveElement(0, 512), 1, 1)); status != smb2.STATUS_OK {
			t.Fatalf("lock %d was answered %#x, want it granted", i, status)
		}
	}

	if status := cl.lockStatus(cl.lockSequenced(held, unlockElement(0, 512), 2, 1)); status != smb2.STATUS_OK {
		t.Fatalf("the unlock was answered %#x, want the range given back", status)
	}

	// One lock was taken, so one unlock gives the range back for good.
	if status := cl.lockStatus(cl.lockRange(other, 0, 512)); status != smb2.STATUS_OK {
		t.Errorf("the range after the one unlock was answered %#x, want it free", status)
	}
}

// TestALockUnderANewSequenceNumberIsWeighedAgain is the entry reused. A client works through the
// entries of its array and comes back around to one, and the number tells the new request from the
// one that entry was remembering: only the number it was answered under is a request sent again.
func TestALockUnderANewSequenceNumberIsWeighedAgain(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	cl.conn.serverCapabilities |= smb2.GLOBAL_CAP_MULTI_CHANNEL

	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	if status := cl.lockStatus(cl.lockSequenced(fid, exclusiveElement(0, 512), 1, 1)); status != smb2.STATUS_OK {
		t.Fatalf("the lock was answered %#x, want it granted", status)
	}
	if status := cl.lockStatus(cl.lockSequenced(fid, unlockElement(0, 512), 2, 1)); status != smb2.STATUS_OK {
		t.Fatalf("the unlock was answered %#x, want the range given back", status)
	}

	// The same entry under a number it was never answered under is a request of its own.
	if status := cl.lockStatus(cl.lockSequenced(fid, unlockElement(0, 512), 2, 2)); status != smb2.STATUS_RANGE_NOT_LOCKED {
		t.Errorf("an unlock under a new sequence number was answered %#x, want it weighed again", status)
	}

	// And what the entry remembered is gone with it, so the older number is weighed too.
	if status := cl.lockStatus(cl.lockSequenced(fid, unlockElement(0, 512), 2, 1)); status != smb2.STATUS_RANGE_NOT_LOCKED {
		t.Errorf("an unlock under the entry that was reset was answered %#x, want it weighed again", status)
	}
}

// TestLockSequencesOutsideTheArrayAreNotRemembered is the request that names no entry of it. Zero is
// reserved and the array holds 64, so neither end has anywhere to be remembered.
func TestLockSequencesOutsideTheArrayAreNotRemembered(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	cl.conn.serverCapabilities |= smb2.GLOBAL_CAP_MULTI_CHANNEL

	created, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)
	fid := createdFileID(created)

	for _, index := range []uint32{0, lockSequenceEntries + 1} {
		if status := cl.lockStatus(cl.lockSequenced(fid, exclusiveElement(0, 512), index, 1)); status != smb2.STATUS_OK {
			t.Fatalf("the lock under index %d was answered %#x, want it granted", index, status)
		}

		if status := cl.lockStatus(cl.lockSequenced(fid, unlockElement(0, 512), index, 1)); status != smb2.STATUS_OK {
			t.Fatalf("the unlock under index %d was answered %#x, want the range given back", index, status)
		}

		if status := cl.lockStatus(cl.lockSequenced(fid, unlockElement(0, 512), index, 1)); status != smb2.STATUS_RANGE_NOT_LOCKED {
			t.Errorf("the unlock sent again under index %d was answered %#x, want it weighed again", index, status)
		}
	}
}

// TestADurableHandleRemembersItsLockSequences is the handle the numbering is really for: a client
// that reclaims one has to send again what it cannot know the fate of. Such an open is sequenced
// whatever the connection can do, where an ordinary one on a connection that neither binds nor
// reconnects has no reason to remember anything.
func TestADurableHandleRemembersItsLockSequences(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")

	durable, _ := cl.createDurable("file", [16]byte{1}, false)
	plain, _ := cl.create("file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	tests := []struct {
		name string
		fid  []byte
		want uint32
	}{
		{"a durable handle", createdFileID(durable), smb2.STATUS_OK},
		{"a handle that is nothing of the kind", createdFileID(plain), smb2.STATUS_RANGE_NOT_LOCKED},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status := cl.lockStatus(cl.lockSequenced(tt.fid, exclusiveElement(0, 512), 1, 1)); status != smb2.STATUS_OK {
				t.Fatalf("the lock was answered %#x, want it granted", status)
			}
			if status := cl.lockStatus(cl.lockSequenced(tt.fid, unlockElement(0, 512), 2, 1)); status != smb2.STATUS_OK {
				t.Fatalf("the unlock was answered %#x, want the range given back", status)
			}

			if status := cl.lockStatus(cl.lockSequenced(tt.fid, unlockElement(0, 512), 2, 1)); status != tt.want {
				t.Errorf("the unlock sent again was answered %#x, want %#x", status, tt.want)
			}
		})
	}
}
