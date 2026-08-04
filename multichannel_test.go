package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// A session may be reached over more than one connection. A break notification is the one thing
// the server sends off its own back, so it is also the only thing that has to choose a channel
// to travel over - and to try another when the one it chose is gone.

func TestIntegrationBreakTravelsOverAnotherChannel(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	held, _ := alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if level := createdOplockLevel(held); level != smb2.OPLOCK_LEVEL_BATCH {
		t.Fatalf("alice was granted %#x rather than a batch oplock", level)
	}

	// A second channel of the same session. The break would go over the first one, which is the
	// connection the open belongs to, so killing that one is what forces the choice.
	alt := alice.addChannel()
	alice.goesAway()

	// Alice answers over the channel she still has.
	answered := make(chan []byte, 1)
	go func() {
		select {
		case note := <-alt.sent:
			alt.ackBreak(brokenFileID(note), smb2.OPLOCK_LEVEL_NONE)
			answered <- note
		case <-time.After(20 * time.Second):
			answered <- nil
		}
	}()

	bob := h.dial("bob")
	buf, async := bob.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OVERWRITE)

	note := <-answered
	if note == nil {
		t.Fatal("the break never reached alice over the channel she had left")
	}
	if fid := brokenFileID(note); !bytes.Equal(fid, createdFileID(held)) {
		t.Errorf("the break names % x, want alice's open % x", fid, createdFileID(held))
	}

	// Nothing was queued for the connection that had gone.
	alice.quiet(0, "a break was queued for a connection that had gone away")

	if !async {
		t.Error("bob's create was answered without waiting for the break")
	}
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Errorf("bob's create failed with %#x", status)
	}
}

// With every channel gone there is nowhere to send the break. The client cannot be caching on
// the strength of a promise the server has no way of withdrawing, so the oplock goes at once
// rather than after the acknowledgment timer.
func TestIntegrationOplockGoesWhenNoChannelIsLeft(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OPEN)
	if !h.srv.hasHoldersOn(h.share, "dir/file", nil, nil, [16]byte{}) {
		t.Fatal("alice was not granted an oplock to begin with")
	}

	alt := alice.addChannel()
	alice.goesAway()
	alt.goesAway()

	bob := h.dial("bob")

	start := time.Now()
	buf, _ := bob.create("dir/file", smb2.OPLOCK_LEVEL_BATCH, smb2.FILE_OVERWRITE)
	waited := time.Since(start)

	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}

	// The acknowledgment timer is 35 seconds. Anything near it means the break was left to
	// expire rather than given up as undeliverable.
	if waited > oplockBreakTimeout/4 {
		t.Errorf("bob waited %v for a client that could not be reached", waited)
	}
}

// A lease belongs to the client rather than to one of its sessions, so a break may travel over
// any connection that client has, not only the channels of one session.
//
// Which connection is tried first is not fixed: they are walked in map order. So the scenario is
// run a number of times, and the dead one comes first in all but a vanishing fraction of runs -
// otherwise a server that gave up after one failed send would pass half the time.
func TestIntegrationLeaseBreakSurvivesADeadConnection(t *testing.T) {
	for range 10 {
		h := newSMBTest(t)
		h.files.put("dir/file", 1024)

		var guid [16]byte
		guid[0] = 0xaa

		alice := h.dialAs("alice", guid)
		alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)

		// A second connection of the same client, with a session of its own.
		second := h.dialAs("alice", guid)
		alice.goesAway()

		bob := h.dial("bob")

		// The notification has to be seen arriving, not merely inferred from bob getting through:
		// a server that gave up on the dead connection would revoke the lease outright and let him
		// through just the same.
		arrived := make(chan []byte, 1)
		go func() {
			select {
			case note := <-second.sent:
				if isLeaseBreak(note) {
					second.ackLeaseBreak(brokenLeaseKey(note), smb2.SMB2_LEASE_NONE)
				}
				arrived <- note
			case <-time.After(20 * time.Second):
				arrived <- nil
			}
		}()

		buf, _ := bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE)
		if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
			t.Fatalf("bob's create failed with %#x", status)
		}

		note := <-arrived
		if note == nil {
			t.Fatal("the lease break never reached the connection that was still up")
		}
		if !isLeaseBreak(note) {
			t.Errorf("what arrived was not a lease break")
		}

		alice.quiet(0, "a lease break was queued for a connection that had gone away")
	}
}

func TestIntegrationLeaseGoesWhenTheClientCannotBeReached(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/file", 1024)

	alice := h.dial("alice")
	alice.createLeased("dir/file", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if !h.srv.hasHoldersOn(h.share, "dir/file", nil, nil, [16]byte{}) {
		t.Fatal("alice was not granted a lease to begin with")
	}

	alice.goesAway()

	bob := h.dial("bob")

	start := time.Now()
	buf, _ := bob.create("dir/file", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OVERWRITE)
	waited := time.Since(start)

	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("bob's create failed with %#x", status)
	}
	if waited > leaseBreakTimeout/4 {
		t.Errorf("bob waited %v for a client that could not be reached", waited)
	}
}
