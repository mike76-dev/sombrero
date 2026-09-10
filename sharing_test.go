package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/smb2"
)

// createStatus is what the server answered a create with, the request having reached it at all.
func (cl *testClient) createStatus(buf []byte, err error) uint32 {
	cl.h.t.Helper()

	if err != nil {
		cl.h.t.Fatalf("the create failed: %v", err)
	}

	return smb2.Header(buf).Status()
}

// TestSharingConflictWeighsBothDirections is the rule itself, one right at a time. Each of the six
// ways two opens can clash is here on its own, against the pair that differs only in the bit that
// settles it: an open is refused for doing what the other will not allow, and for not allowing what
// the other does. The side that is not being weighed reads the file, since an open that does
// nothing with it is not weighed at all.
func TestSharingConflictWeighsBothDirections(t *testing.T) {
	const (
		read       = smb2.FILE_READ_DATA
		write      = smb2.FILE_WRITE_DATA
		remove     = smb2.DELETE
		attributes = smb2.FILE_READ_ATTRIBUTES | smb2.SYNCHRONIZE
	)

	tests := []struct {
		name                        string
		access, mode, held, allowed uint32
		want                        bool
	}{
		{"nothing asked for and nothing held", 0, 0, 0, 0, false},
		{"reading what the other allows to be read", read, everySharing, read, smb2.FILE_SHARE_READ, false},
		{"reading what the other will not have read", read, everySharing, read, smb2.FILE_SHARE_WRITE, true},
		{"writing what the other allows to be written", write, everySharing, read, smb2.FILE_SHARE_WRITE, false},
		{"writing what the other will not have written", write, everySharing, read, smb2.FILE_SHARE_READ, true},
		{"deleting what the other allows to be deleted", remove, everySharing, read, smb2.FILE_SHARE_DELETE, false},
		{"deleting what the other will not have deleted", remove, everySharing, read, smb2.FILE_SHARE_READ, true},
		{"allowing the reading the other is doing", read, smb2.FILE_SHARE_READ, read, everySharing, false},
		{"not allowing the reading the other is doing", read, smb2.FILE_SHARE_WRITE, read, everySharing, true},
		{"allowing the writing the other is doing", read, smb2.FILE_SHARE_WRITE, write, everySharing, false},
		{"not allowing the writing the other is doing", read, smb2.FILE_SHARE_READ, write, everySharing, true},
		{"allowing the delete the other may make", read, smb2.FILE_SHARE_DELETE, remove, everySharing, false},
		{"not allowing the delete the other may make", read, smb2.FILE_SHARE_READ, remove, everySharing, true},

		// The generic rights name the same three things, and are weighed as what they stand for.
		{"a generic reader the other will not have read", smb2.GENERIC_READ, everySharing, read, smb2.FILE_SHARE_WRITE, true},
		{"not allowing the generic writing the other is doing", read, smb2.FILE_SHARE_READ, smb2.GENERIC_WRITE, everySharing, true},
		{"everything at once against an open that allows it all", smb2.GENERIC_ALL, everySharing, read, everySharing, false},

		// An open for the attributes alone takes no part, whatever mode either side names: it is not
		// refused, and it keeps nobody out.
		{"reading the attributes of a file taken to itself", attributes, 0, write, 0, false},
		{"reading beside an attribute open that shares nothing", read, everySharing, attributes, 0, false},
		{"taking a file to itself beside an attribute open", write, 0, attributes, everySharing, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sharingConflict(tt.access, tt.mode, tt.held, tt.allowed); got != tt.want {
				t.Errorf("the opens were judged to clash: %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAnOpenThatSharesNothingKeepsTheFileToItself is what a sharing mode is for. A client that
// means to have the file to itself says so in the mode it opens under, and until it lets go
// nobody else may have the file at all: this is the promise the applications rely on to keep two
// people from writing one document, and without it the second writer's upload quietly replaces the
// first one's.
func TestAnOpenThatSharesNothingKeepsTheFileToItself(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, writeAccess, 0)); status != smb2.STATUS_OK {
		t.Fatalf("the first open was answered %#x, want it granted", status)
	}

	if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, readAccess, everySharing)); status != smb2.STATUS_SHARING_VIOLATION {
		t.Errorf("an open beside one that shares nothing was answered %#x, want STATUS_SHARING_VIOLATION", status)
	}
}

// TestAnOpenIsRefusedForWhatItWillNotAllow is the rule read the other way about. A client that
// opens a file allowing nothing is refused for what the opens already on it are doing, and not
// only for what it means to do itself.
func TestAnOpenIsRefusedForWhatItWillNotAllow(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, writeAccess, everySharing)); status != smb2.STATUS_OK {
		t.Fatalf("the first open was answered %#x, want it granted", status)
	}

	// It only means to read, which the open standing there allows; what it will not have is that
	// open going on writing.
	if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, readAccess, 0)); status != smb2.STATUS_SHARING_VIOLATION {
		t.Errorf("an open allowing nothing was answered %#x, want STATUS_SHARING_VIOLATION", status)
	}
}

// TestSharingIsWeighedRightByRight is the mode taken apart. Reading, writing and deleting are
// allowed one at a time, and an open clashes only over the right it actually wants.
func TestSharingIsWeighedRightByRight(t *testing.T) {
	tests := []struct {
		name          string
		access, mode  uint32
		second, allow uint32
		want          uint32
	}{
		{
			"a reader beside a writer that allows reading",
			writeAccess, smb2.FILE_SHARE_READ,
			readAccess, everySharing,
			smb2.STATUS_OK,
		},
		{
			"a writer beside a writer that allows reading alone",
			writeAccess, smb2.FILE_SHARE_READ,
			writeAccess, everySharing,
			smb2.STATUS_SHARING_VIOLATION,
		},
		{
			"a second reader where only reading is allowed",
			readAccess, smb2.FILE_SHARE_READ,
			readAccess, smb2.FILE_SHARE_READ,
			smb2.STATUS_OK,
		},
		{
			"a delete beside an open that allows everything but",
			writeAccess, smb2.FILE_SHARE_READ | smb2.FILE_SHARE_WRITE,
			smb2.DELETE | smb2.FILE_READ_ATTRIBUTES, everySharing,
			smb2.STATUS_SHARING_VIOLATION,
		},
		{
			"a reader that will not have the file deleted, beside one that may delete it",
			writeAccess, everySharing,
			readAccess, smb2.FILE_SHARE_READ | smb2.FILE_SHARE_WRITE,
			smb2.STATUS_SHARING_VIOLATION,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSMBTest(t)
			h.files.put("file", 1024)

			cl := h.dial("alice")
			if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, tt.access, tt.mode)); status != smb2.STATUS_OK {
				t.Fatalf("the first open was answered %#x, want it granted", status)
			}

			if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, tt.second, tt.allow)); status != tt.want {
				t.Errorf("the second open was answered %#x, want %#x", status, tt.want)
			}
		})
	}
}

// TestAnAttributeOpenTakesNoPartInSharing is what Explorer and the Linux clients rely on. Both open
// files and folders for their attributes alone, under whatever mode, and the file system weighs
// none of it: such an open is never refused for sharing, and its mode keeps nobody out. Weighing it
// is what refused the share root to Explorer and file contents to Nautilus.
func TestAnAttributeOpenTakesNoPartInSharing(t *testing.T) {
	const attributes = smb2.FILE_READ_ATTRIBUTES | smb2.SYNCHRONIZE

	t.Run("a reader beside an attribute open that shares nothing", func(t *testing.T) {
		h := newSMBTest(t)
		h.files.put("file", 1024)

		cl := h.dial("alice")
		if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, attributes, 0)); status != smb2.STATUS_OK {
			t.Fatalf("the attribute open was answered %#x, want it granted", status)
		}

		if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, readAccess, everySharing)); status != smb2.STATUS_OK {
			t.Errorf("a reader beside it was answered %#x, want it granted", status)
		}
	})

	t.Run("an attribute open beside a file taken to itself", func(t *testing.T) {
		h := newSMBTest(t)
		h.files.put("file", 1024)

		cl := h.dial("alice")
		if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, writeAccess, 0)); status != smb2.STATUS_OK {
			t.Fatalf("the exclusive open was answered %#x, want it granted", status)
		}

		// How a client shows the size of a file that somebody is still writing.
		if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, attributes, 0)); status != smb2.STATUS_OK {
			t.Errorf("an attribute open beside it was answered %#x, want it granted", status)
		}
	})
}

// TestSharingViolationNamesWhatStoodInTheWay is the refusal as the log reports it. An open kept for
// a connection that has gone blocks a file as surely as a live one, and the two call for different
// answers, so the report has to tell them apart.
func TestSharingViolationNamesWhatStoodInTheWay(t *testing.T) {
	live := &open{
		grantedAccess: smb2.FILE_WRITE_DATA,
		shareMode:     smb2.FILE_SHARE_READ,
		connection:    &connection{clientName: "desktop-1"},
	}
	msg := describeSharingViolation("docs/a.txt", smb2.FILE_READ_DATA, 0, live)
	for _, want := range []string{`"docs/a.txt"`, "access 0x1", "access 0x2", "sharing 0x1", "desktop-1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report of a live open lacks %q: %s", want, msg)
		}
	}

	orphaned := &open{
		grantedAccess:  smb2.FILE_WRITE_DATA,
		isDurable:      true,
		disconnectTime: time.Now().Add(-90 * time.Second),
	}
	msg = describeSharingViolation("docs/a.txt", smb2.FILE_READ_DATA, 0, orphaned)
	if !strings.Contains(msg, "durable open whose connection went 1m30s ago") {
		t.Errorf("the report of an orphaned durable open does not say so: %s", msg)
	}
}

// TestSharingIsWeighedAgainstTheSameFileOnly is the mode kept to the file it was named for: a
// client that takes one file to itself has said nothing about any other.
func TestSharingIsWeighedAgainstTheSameFileOnly(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)
	h.files.put("other", 1024)

	cl := h.dial("alice")
	if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, writeAccess, 0)); status != smb2.STATUS_OK {
		t.Fatalf("the first open was answered %#x, want it granted", status)
	}

	if status := cl.createStatus(cl.createSharing("other", smb2.FILE_OPEN, writeAccess, 0)); status != smb2.STATUS_OK {
		t.Errorf("an open of another file was answered %#x, want it granted", status)
	}
}

// TestTheFileIsSharedAgainWhenTheHandleCloses is what a sharing mode is worth after the handle
// behind it: nothing. A mode left standing would keep the file shut for as long as the server runs.
func TestTheFileIsSharedAgainWhenTheHandleCloses(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("file", 1024)

	cl := h.dial("alice")
	held, _ := cl.createSharing("file", smb2.FILE_OPEN, writeAccess, 0)
	if status := smb2.Header(held).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the first open was answered %#x, want it granted", status)
	}

	if _, err := cl.closeHandle(createdFileID(held)); err != nil {
		t.Fatalf("the close failed: %v", err)
	}

	if status := cl.createStatus(cl.createSharing("file", smb2.FILE_OPEN, writeAccess, everySharing)); status != smb2.STATUS_OK {
		t.Errorf("an open after the file was let go of was answered %#x, want it granted", status)
	}
}
