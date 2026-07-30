package main

import (
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// A lease key names one file. The client picks the key, and the server ties it to whatever file
// it was first used on, so that a break notification - which carries the key and no file ID -
// says without ambiguity what the client is to stop caching.
//
// The exception is a file on its way out. A client that deletes a file and opens another has no
// reason to think up a fresh key for it, and refusing would leave it uncached until it did
// (3.3.5.9.8, 3.3.5.9.11).

// A create that takes the file out in order to delete it frees the key straight away: the client
// need not wait for the handle to go before using it on something else.
func TestIntegrationDeleteOnCloseCreateFreesTheLeaseKey(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/one", 1024)
	h.files.put("dir/two", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeasedWithOptions("dir/one", aliceKey, rwh, smb2.FILE_DELETE_ON_CLOSE)
	if state, found := createdLeaseState(held); !found || state == smb2.SMB2_LEASE_NONE {
		t.Fatalf("alice was granted %#x on the first file, want a lease", state)
	}

	buf, _ := alice.createLeased("dir/two", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("using the key on another file returned %#x, want it to be allowed", status)
	}
	if state, found := createdLeaseState(buf); !found || state == smb2.SMB2_LEASE_NONE {
		t.Errorf("alice was granted %#x on the second file, want a lease", state)
	}
}

// Marking the file for deletion through an open handle frees the key the same way.
func TestIntegrationDeletionMarkFreesTheLeaseKey(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/one", 1024)
	h.files.put("dir/two", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeased("dir/one", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, found := createdLeaseState(held); !found || state == smb2.SMB2_LEASE_NONE {
		t.Fatalf("alice was granted %#x on the first file, want a lease", state)
	}

	// Until the file is marked, the key is still tied to it.
	refused, _ := alice.createLeased("dir/two", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(refused).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Fatalf("using the key on another file returned %#x before the mark, want invalid parameter", status)
	}

	resp, err := alice.markForDeletion(createdFileID(held))
	if err != nil {
		t.Fatalf("the deletion mark did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("marking the file for deletion returned %#x", status)
	}

	buf, _ := alice.createLeased("dir/two", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Errorf("using the key on another file returned %#x after the mark, want it to be allowed", status)
	}
}

// The exemption frees the key once, not for good: the lease follows it to the new file, and is
// tied to that one as it was to the first.
func TestIntegrationReusedLeaseKeyIsTiedToTheNewFile(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/one", 1024)
	h.files.put("dir/two", 1024)
	h.files.put("dir/three", 1024)

	alice := h.dial("alice")
	alice.createLeasedWithOptions("dir/one", aliceKey, rwh, smb2.FILE_DELETE_ON_CLOSE)

	buf, _ := alice.createLeased("dir/two", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Fatalf("using the key on the second file returned %#x, want it to be allowed", status)
	}

	third, _ := alice.createLeased("dir/three", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(third).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("using the key on a third file returned %#x, want invalid parameter", status)
	}
}

// Renaming is the other way a lease stops naming the file it was taken out on. The file has not
// gone anywhere, so the key stays tied to it - under the new name.
func TestIntegrationRenameMovesTheLeaseToTheNewName(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/one", 1024)
	h.files.put("dir/three", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeased("dir/one", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if state, found := createdLeaseState(held); !found || state == smb2.SMB2_LEASE_NONE {
		t.Fatalf("alice was granted %#x, want a lease", state)
	}

	resp, err := alice.rename(createdFileID(held), "dir/two")
	if err != nil {
		t.Fatalf("the rename did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the rename returned %#x", status)
	}

	// The lease has moved with the file, so the key works on the new name.
	buf, _ := alice.createLeased("dir/two", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_OK {
		t.Errorf("using the key on the new name returned %#x, want it to be allowed", status)
	}

	// And it is tied to it: a rename is not a file going away.
	third, _ := alice.createLeased("dir/three", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(third).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("using the key on an unrelated file returned %#x, want invalid parameter", status)
	}
}

// A rename undoes a pending deletion as far as the lease key is concerned: the file is staying,
// so the key is its again.
func TestIntegrationRenameTiesTheKeyBackToTheFile(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/one", 1024)
	h.files.put("dir/three", 1024)

	alice := h.dial("alice")
	held, _ := alice.createLeasedWithOptions("dir/one", aliceKey, rwh, smb2.FILE_DELETE_ON_CLOSE)
	if state, found := createdLeaseState(held); !found || state == smb2.SMB2_LEASE_NONE {
		t.Fatalf("alice was granted %#x, want a lease", state)
	}

	resp, err := alice.rename(createdFileID(held), "dir/two")
	if err != nil {
		t.Fatalf("the rename did not come back: %v", err)
	}
	if status := smb2.Header(resp).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the rename returned %#x", status)
	}

	buf, _ := alice.createLeased("dir/three", aliceKey, rwh, 2, smb2.FILE_OPEN)
	if status := smb2.Header(buf).Status(); status != smb2.STATUS_INVALID_PARAMETER {
		t.Errorf("the key was still free after the rename: got %#x, want invalid parameter", status)
	}
}

// The exemption is about the key, not about what may be cached: a file on its way out is still
// somebody else's business if they have it open.
func TestIntegrationDeleteOnCloseDoesNotWidenTheLease(t *testing.T) {
	h := newSMBTest(t)
	h.files.put("dir/one", 1024)

	bob := h.dial("bob")
	bob.create("dir/one", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_OPEN)

	alice := h.dial("alice")
	held, _ := alice.createLeasedWithOptions("dir/one", aliceKey, rwh, smb2.FILE_DELETE_ON_CLOSE)

	state, found := createdLeaseState(held)
	if !found {
		t.Fatal("alice was answered without a lease context")
	}
	if state&smb2.SMB2_LEASE_WRITE_CACHING != 0 {
		t.Errorf("alice was granted %#x while bob had the file open, want no write caching", state)
	}
}
