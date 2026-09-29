package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
)

// fakeSource stands in for whoever holds the bytes now, answering with the
// content it was given for each key.
type fakeSource struct {
	content map[string][]byte
	err     error
	reads   int
}

// ReadPart implements PartReader.
func (fs *fakeSource) ReadPart(ctx context.Context, part transfer.Part, offset, length uint64, w io.Writer) error {
	if fs.err != nil {
		return fs.err
	}
	fs.reads++

	data, ok := fs.content[part.Source.Key]
	if !ok {
		return errors.New("no such key: " + part.Source.Key)
	}

	at := part.DataOffset + offset
	if at+length > uint64(len(data)) {
		return io.ErrUnexpectedEOF
	}
	_, err := w.Write(data[at : at+length])

	return err
}

// describeFiles writes a description of what the source holds, the way a walk
// of it would.
func describeFiles(t *testing.T, files []transfer.File, dirs ...transfer.Directory) *transfer.Reader {
	t.Helper()

	var buf bytes.Buffer
	w, err := transfer.NewWriter(&buf, transfer.Header{CreatedAt: time.Now(), Source: "renterd"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, dir := range dirs {
		if err := w.Directory(dir); err != nil {
			t.Fatalf("Directory(%q): %v", dir.Path, err)
		}
	}
	for _, f := range files {
		if err := w.File(f); err != nil {
			t.Fatalf("File(%q): %v", f.Path, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := transfer.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	return r
}

// TestCopy verifies that what a description names is fetched from the source and
// written to the destination, where it is read back as an ordinary file.
func TestCopy(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	// Three chunks and a bit, so that a file is moved in more than one go.
	content := bytes.Repeat([]byte("sombrero"), 1000)
	src := &fakeSource{content: map[string][]byte{"/holiday/beach.raw": content}}

	files := []transfer.File{
		{
			Path: "/holiday/beach.raw", Size: uint64(len(content)),
			Parts: []transfer.Part{{
				Offset: 0, Length: uint64(len(content)),
				Source: &transfer.Source{Kind: "renterd", Key: "/holiday/beach.raw"},
			}},
		},
		{
			// Bytes the description carries are written without asking the
			// source for them at all.
			Path: "/holiday/notes.txt", Size: 11,
			Parts: []transfer.Part{{Offset: 0, Length: 11, Inline: []byte("hello there")}},
		},
		{Path: "/holiday/empty.bin"},
	}

	var progress int
	stats, err := Copy(ctx, c, src, describeFiles(t, files, transfer.Directory{Path: "/holiday"}), CopyOptions{
		Account:   acc,
		ChunkSize: 3000,
		Progress:  func(string, uint64, uint64) { progress++ },
	})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if stats.Files != 3 || stats.Directories != 1 || stats.Failed != 0 {
		t.Fatalf("stats: want 3 files in 1 folder and nothing failed, got %+v", stats)
	}
	if stats.Bytes != uint64(len(content))+11 {
		t.Errorf("bytes copied: want %d, got %d", len(content)+11, stats.Bytes)
	}
	if progress == 0 {
		t.Error("the copy reported no progress")
	}

	// The file is there to be read, and it reads back as what the source held.
	waitForRead(t, ctx, c, acc, "/holiday/beach.raw", content)
	waitForRead(t, ctx, c, acc, "/holiday/notes.txt", []byte("hello there"))

	info, err := c.Object(ctx, acc, "/holiday/empty.bin")
	if err != nil {
		t.Fatalf("the empty file: %v", err)
	}
	if info.Size != 0 {
		t.Errorf("the empty file: want no bytes, got %d", info.Size)
	}
}

// TestCopyLeavesWhatIsThere verifies that a copy taken up again does not write
// over what the one before it finished.
func TestCopyLeavesWhatIsThere(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	content := []byte("hello world")
	src := &fakeSource{content: map[string][]byte{"/x.txt": content}}
	files := []transfer.File{{
		Path: "/x.txt", Size: uint64(len(content)),
		Parts: []transfer.Part{{Offset: 0, Length: uint64(len(content)), Source: &transfer.Source{Key: "/x.txt"}}},
	}}

	if _, err := Copy(ctx, c, src, describeFiles(t, files), CopyOptions{Account: acc}); err != nil {
		t.Fatalf("the first copy: %v", err)
	}
	waitForRead(t, ctx, c, acc, "/x.txt", content)

	reads := src.reads
	stats, err := Copy(ctx, c, src, describeFiles(t, files), CopyOptions{Account: acc})
	if err != nil {
		t.Fatalf("the second copy: %v", err)
	}
	if stats.Skipped != 1 || stats.Files != 0 {
		t.Errorf("the second copy: want the file left alone, got %+v", stats)
	}
	if src.reads != reads {
		t.Errorf("the second copy read the source %d more time(s)", src.reads-reads)
	}
}

// TestCopyGoesOnAfterAFileItCannotHave verifies that one file nobody can fetch
// does not take the rest of the copy with it.
func TestCopyGoesOnAfterAFileItCannotHave(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	content := []byte("hello world")
	src := &fakeSource{content: map[string][]byte{"/good.txt": content}}
	files := []transfer.File{
		{
			Path: "/gone.txt", Size: 5,
			Parts: []transfer.Part{{Offset: 0, Length: 5, Source: &transfer.Source{Key: "/gone.txt"}}},
		},
		{
			Path: "/good.txt", Size: uint64(len(content)),
			Parts: []transfer.Part{{Offset: 0, Length: uint64(len(content)), Source: &transfer.Source{Key: "/good.txt"}}},
		},
	}

	var failures []string
	stats, err := Copy(ctx, c, src, describeFiles(t, files), CopyOptions{
		Account: acc,
		OnError: func(path string, err error) { failures = append(failures, path) },
	})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if stats.Failed != 1 || stats.Files != 1 {
		t.Fatalf("stats: want one file copied and one failed, got %+v", stats)
	}
	if len(failures) != 1 || failures[0] != "/gone.txt" {
		t.Errorf("the failures reported: got %v", failures)
	}

	waitForRead(t, ctx, c, acc, "/good.txt", content)

	// The file that could not be had was called off rather than left in flight,
	// so the path is free for another try.
	if _, err := c.Object(ctx, acc, "/gone.txt"); !errors.Is(err, stores.ErrNotFound) {
		t.Errorf("the file that could not be fetched: want it gone, got %v", err)
	}
	if _, err := c.StartUpload(ctx, acc, "/gone.txt"); err != nil {
		t.Errorf("another try at the file that could not be fetched: %v", err)
	}
}

// fullDestination is a destination with no room for the first tries, the way one
// whose staging area is full answers.
type fullDestination struct {
	full  int
	wrote int
}

func (fd *fullDestination) Write(ctx context.Context, r io.Reader, path, uploadID string, partNumber int, offset, length uint64) (string, error) {
	if fd.full > 0 {
		fd.full--
		return "", fmt.Errorf("couldn't buffer the data: %w", ErrBacklogFull)
	}
	fd.wrote++

	return "", nil
}

// TestCopyWaitsForRoom verifies that a destination with nowhere to put the data
// yet is waited for rather than given up on, which is what a share filling
// faster than it drains needs.
func TestCopyWaitsForRoom(t *testing.T) {
	dst := &fullDestination{full: 2}
	var stats CopyStats

	err := write(context.Background(), dst, "/x.txt", "upload", 0, 5, []byte("hello"),
		CopyOptions{Retry: time.Millisecond}, &stats)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if stats.Waits != 2 || dst.wrote != 1 {
		t.Errorf("want two waits and the chunk written once, got %d wait(s) and %d write(s)", stats.Waits, dst.wrote)
	}

	// A wait nobody is waiting for is given up on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst = &fullDestination{full: 1}
	if err := write(ctx, dst, "/x.txt", "upload", 0, 5, []byte("hello"), CopyOptions{Retry: time.Hour}, &stats); !errors.Is(err, context.Canceled) {
		t.Errorf("a wait that was called off: want it stopped, got %v", err)
	}
}

// TestCopyStopsWhenCancelled verifies that a copy nobody is waiting for stops.
func TestCopyStopsWhenCancelled(t *testing.T) {
	ctx := context.Background()

	db := stores.NewTestStore(t, ctx)
	t.Cleanup(db.Close)

	acc := newTestAccount(t, db, "alice", "secret123")
	share := newTestShare(t, db, "testshare")
	grantFullAccess(t, db, share, acc)

	c := newIndexdClient(db, newFakeBackend(), share.Name, workgroupID(t, db, acc), 1, 0, PackingOptions{}, FragmentationOptions{}, false)
	t.Cleanup(func() { _ = c.Close() })

	files := []transfer.File{{
		Path: "/x.txt", Size: 5,
		Parts: []transfer.Part{{Offset: 0, Length: 5, Source: &transfer.Source{Key: "/x.txt"}}},
	}}
	src := &fakeSource{content: map[string][]byte{"/x.txt": []byte("hello")}}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Copy(cancelled, c, src, describeFiles(t, files), CopyOptions{Account: acc}); !errors.Is(err, context.Canceled) {
		t.Errorf("a copy nobody is waiting for: want it stopped, got %v", err)
	}
}
