package main

import (
	"sync"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
	"github.com/mike76-dev/sombrero/stores"
)

// TestPersistedOpensAreListedWithoutRacingTheWriter is the file being uploaded into the directory
// being listed. A file that has been created but not yet uploaded is not in the store, so a
// listing of its directory folds it in from the persisted opens of the tree connect - and it used
// to read the size and the modification time straight out of the open, holding nothing but the
// lock of the table it found the open in. The writer of that same file moves both fields under
// the lock of the open itself, once per contiguous chunk it buffers, so a client copying a file
// into a window it also has open raced on every chunk.
//
// The two paths are driven directly rather than through the dispatcher, so that what is under
// test is the locking of the fields and not the ordering of the requests that reach them.
func TestPersistedOpensAreListedWithoutRacingTheWriter(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	acc := stores.Account{Username: "alice", Workgroup: h.workgroup}

	h.files.putDir("docs")
	dir := h.srv.globalOpenTable[openIDOf(createdFileID(cl.openDir("docs")))]

	// A create of a file that is not in the store leaves a persisted open behind: the client is
	// told the file exists from that moment, and nothing can be uploaded until it holds data.
	created, _ := cl.create("docs/notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the file was answered with %#x", status)
	}
	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]

	if _, ok := cl.tc.persistedFile("docs/notes.txt"); !ok {
		t.Fatal("the created file left no persisted state, so a listing has nothing to race with")
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		data := make([]byte, 512)
		for i := range 64 {
			if err := file.write(uint64(i*len(data)), data); err != nil {
				t.Errorf("the write of chunk %d failed: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for range 64 {
			if err := dir.queryDirectory(acc, "*"); err != nil {
				t.Errorf("the listing of the directory failed: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// TestDirectorySnapshotDoesNotRaceTheWriter is the same fields read by the other reader of the
// persisted opens: the watch a client puts on a directory takes a fingerprint of it every fifteen
// seconds for as long as the watch stands, which is exactly as long as a copy into that directory
// takes.
func TestDirectorySnapshotDoesNotRaceTheWriter(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")
	acc := stores.Account{Username: "alice", Workgroup: h.workgroup}

	h.files.putDir("docs")
	dir := h.srv.globalOpenTable[openIDOf(createdFileID(cl.openDir("docs")))]

	created, _ := cl.create("docs/notes.txt", smb2.OPLOCK_LEVEL_NONE, smb2.FILE_CREATE)
	if status := smb2.Header(created).Status(); status != smb2.STATUS_OK {
		t.Fatalf("the create of the file was answered with %#x", status)
	}
	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		data := make([]byte, 512)
		for i := range 64 {
			if err := file.write(uint64(i*len(data)), data); err != nil {
				t.Errorf("the write of chunk %d failed: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for range 64 {
			if _, err := dir.directorySnapshot(acc); err != nil {
				t.Errorf("the snapshot of the directory failed: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
