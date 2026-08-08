package main

import (
	"bytes"
	"testing"

	"github.com/mike76-dev/sombrero/smb2"
)

// TestWriteInvalidatesTheReadCache is the file read, then written over, then read again.
//
// A read fills a cache of chunks on the handle so that the next read of the same region does not
// have to go back to the network - which, on this network, is the difference between a video that
// plays and one that does not. Nothing dropped those chunks when the file was written, and a write
// does not edit the object in place: it uploads the file anew. So the region the client had just
// overwritten went on reading back as whatever was there before, for as long as the handle stood.
func TestWriteInvalidatesTheReadCache(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	before := bytes.Repeat([]byte("O"), 4096)
	h.files.putData("clip.mp4", before)

	created := cl.createWithOptions("clip.mp4", smb2.FILE_OPEN, 0)
	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]

	// The read that fills the cache.
	got, err := file.read(0, 1024)
	if err != nil {
		t.Fatalf("the first read failed: %v", err)
	}
	if !bytes.Equal(got, before[:1024]) {
		t.Fatalf("the first read gave back something other than what the store holds")
	}

	// The client writes over the whole of it.
	after := bytes.Repeat([]byte("N"), 4096)
	if err := file.write(0, after); err != nil {
		t.Fatalf("the write failed: %v", err)
	}

	// Both ways in: the one that answers off the cache without leaving the handle, and the one
	// that goes and gets what it does not have.
	if data, ok := file.tryReadCached(0, 1024); ok && !bytes.Equal(data, after[:1024]) {
		t.Error("the cached read gave back the file as it stood before the write")
	}

	got, err = file.read(0, 1024)
	if err != nil {
		t.Fatalf("the read after the write failed: %v", err)
	}
	if !bytes.Equal(got, after[:1024]) {
		t.Error("the read after the write gave back the file as it stood before it")
	}
}

// TestOverwritingCreateInvalidatesTheReadCache is the same staleness reached through a create
// rather than a write, and through another handle rather than this one. A create that supersedes or
// overwrites empties the file, and the cache belongs to the handle: the handle that emptied the file
// need not be the one holding the contents of it from before.
func TestOverwritingCreateInvalidatesTheReadCache(t *testing.T) {
	h := newSMBTest(t)
	cl := h.dial("alice")

	// A file that exists only as the state the tree connect keeps under its name, which every open
	// on it shares.
	created := cl.createWithOptions("clip.mp4", smb2.FILE_CREATE, 0)
	file := h.srv.globalOpenTable[openIDOf(createdFileID(created))]

	// A cache is put on the handle by hand: what matters is that a create which empties the file
	// leaves nothing of it to be read, not how the chunks came to be there.
	file.file.mu.Lock()
	file.file.size = 4096
	file.file.mu.Unlock()
	file.mu.Lock()
	file.buffer[0] = &readChunk{data: bytes.Repeat([]byte("O"), 4096), done: closedChan()}
	file.cacheOrder = []uint64{0}
	file.cacheGeneration = file.file.generationNow()
	file.mu.Unlock()

	// The overwriting create is answered with an open of its own, so nothing about the handle above
	// is touched by it. What the file is has moved on all the same.
	cl.createWithOptions("clip.mp4", smb2.FILE_OVERWRITE_IF, 0)

	if size := file.file.sizeNow(); size != 0 {
		t.Errorf("the overwriting create left the file at %d bytes, want it emptied", size)
	}

	// The read is what the cache is for, so the read is what says whether it survived.
	if data, ok := file.tryReadCached(0, 4096); ok {
		t.Errorf("the first handle served %d bytes of the file as it was before it was emptied", len(data))
	}

	file.mu.Lock()
	cached := len(file.buffer)
	file.mu.Unlock()
	if cached != 0 {
		t.Error("the contents of the emptied file were left in the read cache")
	}
}

// closedChan returns a channel that is already closed, which is how a cache entry says its
// download has finished.
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}
