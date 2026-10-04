package transfer

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
)

// testHeader is the header the tests write, with a time that survives the
// round trip the encoder makes of it.
func testHeader() Header {
	return Header{
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		Source:    "renterd",
		Origin:    "http://127.0.0.1:9980",
		Share:     "pictures",
		Workgroup: "12345678-1234-1234-1234-123456789abc",
	}
}

// testSlab is a slab slice as another server would describe one: a key, the
// shards it takes to recover it, and the hosts that hold them.
func testSlab() slabs.SlabSlice {
	var key slabs.EncryptionKey
	key[0] = 7
	return slabs.SlabSlice{
		Version:       1,
		EncryptionKey: key,
		MinShards:     2,
		Sectors: []slabs.PinnedSector{
			{Root: types.Hash256{1}, HostKey: types.PublicKey{2}},
			{Root: types.Hash256{3}, HostKey: types.PublicKey{4}},
		},
		Offset: 0,
		Length: 1 << 20,
	}
}

// read drains a stream into the folders and files it holds.
func read(t *testing.T, b []byte) (*Reader, []Directory, []File) {
	t.Helper()

	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	var dirs []Directory
	var files []File
	for {
		dir, f, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if dir != nil {
			dirs = append(dirs, *dir)
		}
		if f != nil {
			files = append(files, *f)
		}
	}

	return r, dirs, files
}

// TestRoundTrip writes a description of the shapes a part comes in and reads it
// back, since a part that does not survive the stream describes nothing.
func TestRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	header := testHeader()

	dir := Directory{Path: "/holiday", Owner: "alice", Private: true, CreatedAt: now, ModifiedAt: now}
	files := []File{
		{
			// A file of this server's own: the object key is all it takes.
			Path:       "/holiday/beach.raw",
			Owner:      "alice",
			Size:       3 << 20,
			CreatedAt:  now,
			ModifiedAt: now,
			Parts: []Part{
				{Offset: 0, Length: 1 << 20, Object: types.Hash256{9}},
				{Offset: 1 << 20, DataOffset: 1 << 20, Length: 2 << 20, Object: types.Hash256{9}},
			},
		},
		{
			// A file of another server's: what it takes to pin it, where to
			// fetch it from instead, and the tail that is carried outright.
			Path:       "/holiday/notes.txt",
			Size:       1<<20 + 11,
			CreatedAt:  now,
			ModifiedAt: now,
			Parts: []Part{
				{
					Offset: 0,
					Length: 1 << 20,
					Object: types.Hash256{5},
					Pin:    &Pin{DataKey: [32]byte{6}, Slabs: []slabs.SlabSlice{testSlab()}},
					Source: &Source{Kind: "renterd", Origin: "http://127.0.0.1:9980", Bucket: "default", Key: "/holiday/notes.txt"},
				},
				{Offset: 1 << 20, Length: 11, Inline: []byte("hello there")},
			},
		},
		{
			// A file the source had not finished writing to the network, which
			// is a gap rather than a claim about bytes nobody has.
			Path:       "/holiday/draft.bin",
			Size:       2 << 20,
			CreatedAt:  now,
			ModifiedAt: now,
			Parts:      []Part{{Offset: 0, Length: 1 << 20, Object: types.Hash256{8}}},
		},
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, header)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Directory(dir); err != nil {
		t.Fatalf("Directory: %v", err)
	}
	for _, f := range files {
		if err := w.File(f); err != nil {
			t.Fatalf("File %q: %v", f.Path, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, gotDirs, gotFiles := read(t, buf.Bytes())

	if !reflect.DeepEqual(r.Header(), header) {
		t.Errorf("header: want %+v, got %+v", header, r.Header())
	}
	if len(gotDirs) != 1 || !reflect.DeepEqual(gotDirs[0], dir) {
		t.Errorf("folders: want %+v, got %+v", dir, gotDirs)
	}
	if !reflect.DeepEqual(gotFiles, files) {
		t.Errorf("files: want %+v, got %+v", files, gotFiles)
	}

	dirs, count := r.Counts()
	if dirs != 1 || count != uint64(len(files)) {
		t.Errorf("counts: want 1 folder and %d files, got %d and %d", len(files), dirs, count)
	}
	if r.Connection() != nil {
		t.Error("a plain description claims to be a catalog")
	}
}

// testConnection is what a catalog says its folders and files belong to.
func testConnection(now time.Time) Connection {
	return Connection{
		Share: Share{
			Name: "pictures", Type: "indexd", Server: "https://indexer", Remark: "holiday snaps",
			CreatedAt: now, DataShards: 10, ParityShards: 20, AllowGuest: true, PublicDir: "Drop",
		},
		Workgroup: Workgroup{
			UUID:       [16]byte{1, 2, 3},
			Name:       "home",
			PublicDirs: []PublicDir{{Path: "Public", ReadOnly: true}, {Path: "Shared", CaseSensitive: true}},
		},
		Accounts: []Account{
			{Name: "alice", PasswordHash: bytes.Repeat([]byte{1}, 16), CreatedAt: now},
			{Name: "bob", PasswordHash: bytes.Repeat([]byte{2}, 16), CreatedAt: now},
		},
		Policies: []Policy{{Account: "alice", Read: true, Write: true, Delete: true, Execute: true}, {Account: "bob", Read: true}},
		AppKey:   bytes.Repeat([]byte{7}, 64),
	}
}

// TestServerCatalogRoundTrip verifies that a catalog of the server comes back as
// written, holds nothing else, and is told apart from a catalog of a connection.
func TestServerCatalogRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	conn := testConnection(now)
	server := Server{
		Shares:     []Share{conn.Share, {Name: "docs", Type: "renterd", Server: "http://127.0.0.1:9980", Bucket: "default", CreatedAt: now}},
		Workgroups: []WorkgroupAccounts{{Workgroup: conn.Workgroup, Accounts: conn.Accounts}, {Workgroup: Workgroup{UUID: [16]byte{9}}}},
		Bans:       []Ban{{Host: "192.168.1.100", Reason: "too many bad passwords"}},
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Server(server); err != nil {
		t.Fatalf("Server: %v", err)
	}
	if err := w.Connection(conn); err == nil {
		t.Error("a connection was taken into a catalog of the server")
	}
	if err := w.Directory(Directory{Path: "/x", CreatedAt: now, ModifiedAt: now}); err == nil {
		t.Error("a folder was taken into a catalog of the server")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, dirs, files := read(t, buf.Bytes())
	if got := r.Server(); got == nil || !reflect.DeepEqual(*got, server) {
		t.Errorf("server: want %+v, got %+v", server, got)
	}
	if r.Connection() != nil || len(dirs) != 0 || len(files) != 0 {
		t.Errorf("a catalog of the server came back with more: %+v %+v %+v", r.Connection(), dirs, files)
	}

	for _, bad := range []Server{
		{Shares: []Share{{}}},
		{Workgroups: []WorkgroupAccounts{{Accounts: []Account{{Name: "x", PasswordHash: []byte{1}}}}}},
		{Bans: []Ban{{}}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v passed as a server record", bad)
		}
	}
}

// TestCatalogRoundTrip verifies that a catalog comes back with what the folders
// and files belong to, and that the connection is taken only where it belongs.
func TestCatalogRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	conn := testConnection(now)
	dir := Directory{Path: "/holiday", Owner: "bob", CreatedAt: now, ModifiedAt: now}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Connection(conn); err != nil {
		t.Fatalf("Connection: %v", err)
	}
	if err := w.Connection(conn); err == nil {
		t.Error("a second connection was taken")
	}
	if err := w.Directory(dir); err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, dirs, files := read(t, buf.Bytes())
	if got := r.Connection(); got == nil || !reflect.DeepEqual(*got, conn) {
		t.Errorf("connection: want %+v, got %+v", conn, got)
	}
	if len(dirs) != 1 || dirs[0].Owner != "bob" || len(files) != 0 {
		t.Errorf("want the one folder of bob's, got %+v and %+v", dirs, files)
	}

	// A connection after the folders is refused by the writer, and by the
	// reader where a writer put it there anyway.
	buf.Reset()
	w, _ = NewWriter(&buf, testHeader())
	_ = w.Directory(dir)
	if err := w.Connection(conn); err == nil {
		t.Error("a connection after a folder was taken")
	}
	_ = w.record(kindConnection, conn)
	_ = w.Close()
	r, err = NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, _, err := r.Next(); err != nil {
		t.Fatalf("the folder: %v", err)
	}
	if _, _, err := r.Next(); err == nil {
		t.Error("a connection among the folders was read past")
	}

	// A connection has to be one a reader can act on.
	for _, bad := range []Connection{
		{},
		{Share: Share{Name: "x"}, AppKey: []byte{1, 2, 3}},
		{Share: Share{Name: "x"}, Accounts: []Account{{Name: "alice", PasswordHash: []byte{1}}}},
		{Share: Share{Name: "x"}, Accounts: []Account{{PasswordHash: bytes.Repeat([]byte{1}, 16)}}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v passed as a connection", bad)
		}
	}
}

// TestGaps verifies what a reader is told about the bytes no part describes,
// which is what the source still held in a buffer.
func TestGaps(t *testing.T) {
	whole := File{Size: 100, Parts: []Part{{Offset: 0, Length: 60}, {Offset: 60, Length: 40}}}
	if !whole.Complete() || whole.Gaps() != nil {
		t.Errorf("a file its parts cover: complete %v, gaps %+v", whole.Complete(), whole.Gaps())
	}

	tail := File{Size: 100, Parts: []Part{{Offset: 0, Length: 60}}}
	if want := []Gap{{Offset: 60, Length: 40}}; !reflect.DeepEqual(tail.Gaps(), want) {
		t.Errorf("a file with a tail left behind: want %+v, got %+v", want, tail.Gaps())
	}

	middle := File{Size: 100, Parts: []Part{{Offset: 0, Length: 20}, {Offset: 70, Length: 30}}}
	if want := []Gap{{Offset: 20, Length: 50}}; !reflect.DeepEqual(middle.Gaps(), want) {
		t.Errorf("a file with a hole in it: want %+v, got %+v", want, middle.Gaps())
	}

	empty := File{Size: 100}
	if want := []Gap{{Offset: 0, Length: 100}}; !reflect.DeepEqual(empty.Gaps(), want) {
		t.Errorf("a file with no parts at all: want %+v, got %+v", want, empty.Gaps())
	}
}

// TestValidate verifies that a description a reader could not act on is refused
// where it is written, rather than carried to whoever reads it.
func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		file File
	}{
		{"no path", File{Path: "", Size: 1, Parts: []Part{{Length: 1, Object: types.Hash256{1}}}}},
		{"a path that is not from the root", File{Path: "holiday/x", Size: 1, Parts: []Part{{Length: 1, Object: types.Hash256{1}}}}},
		{"a path that ends with a slash", File{Path: "/holiday/", Size: 1, Parts: []Part{{Length: 1, Object: types.Hash256{1}}}}},
		{"an empty part", File{Path: "/x", Size: 1, Parts: []Part{{Offset: 0, Length: 0, Object: types.Hash256{1}}}}},
		{"parts that overlap", File{Path: "/x", Size: 100, Parts: []Part{
			{Offset: 0, Length: 60, Object: types.Hash256{1}},
			{Offset: 50, Length: 50, Object: types.Hash256{1}},
		}}},
		{"a part past the end", File{Path: "/x", Size: 10, Parts: []Part{{Offset: 0, Length: 20, Object: types.Hash256{1}}}}},
		{"carried bytes of another length", File{Path: "/x", Size: 10, Parts: []Part{{Offset: 0, Length: 10, Inline: []byte("short")}}}},
		{"a part that says nothing", File{Path: "/x", Size: 10, Parts: []Part{{Offset: 0, Length: 10}}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.file.Validate(); err == nil {
				t.Error("the file was accepted")
			}

			var buf bytes.Buffer
			w, err := NewWriter(&buf, testHeader())
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			if err := w.File(test.file); err == nil {
				t.Error("the file was written")
			}
		})
	}

	if err := (Directory{Path: "holiday"}).Validate(); err == nil {
		t.Error("a folder that is not from the root was accepted")
	}
}

// TestNotATransfer verifies that a stream this reader is not meant to read is
// refused before anything in it is believed.
func TestNotATransfer(t *testing.T) {
	if _, err := NewReader(bytes.NewReader([]byte("not a transfer at all"))); !errors.Is(err, ErrNotATransfer) {
		t.Errorf("a stream of something else: want %v, got %v", ErrNotATransfer, err)
	}
	if _, err := NewReader(bytes.NewReader(nil)); !errors.Is(err, ErrNotATransfer) {
		t.Errorf("an empty stream: want %v, got %v", ErrNotATransfer, err)
	}

	// A stream of a version this reader does not know is refused as a whole,
	// since what it holds is not what this one would make of it.
	var buf bytes.Buffer
	e := types.NewEncoder(&buf)
	e.WriteString(magic)
	e.WriteUint8(Version + 1)
	e.Flush()
	if _, err := NewReader(bytes.NewReader(buf.Bytes())); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("a newer stream: want %v, got %v", ErrUnsupportedVersion, err)
	}
}

// TestTruncated verifies that a stream that ends early is reported as such,
// rather than read as a description of fewer files than were written.
func TestTruncated(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.File(File{Path: "/x", Size: 10, Parts: []Part{{Length: 10, Object: types.Hash256{1}}}}); err != nil {
		t.Fatalf("File: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cut := buf.Bytes()[:buf.Len()-8]
	r, err := NewReader(bytes.NewReader(cut))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	for {
		_, _, err := r.Next()
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrTruncated) {
			t.Errorf("a stream that ends early: want %v, got %v", ErrTruncated, err)
		}
		break
	}
}

// TestCorrupted verifies that a stream that was changed on its way is reported
// at the end, which is what the digest is for.
func TestCorrupted(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.File(File{Path: "/x", Size: 10, Parts: []Part{{Length: 10, Inline: []byte("0123456789")}}}); err != nil {
		t.Fatalf("File: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// One byte of the carried data is changed, which the stream only finds out
	// about once it has been read to the end.
	b := buf.Bytes()
	i := bytes.Index(b, []byte("0123456789"))
	if i < 0 {
		t.Fatal("the written data was not found in the stream")
	}
	b[i] = 'x'

	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var last error
	for {
		_, _, err := r.Next()
		if err != nil {
			last = err
			break
		}
	}
	if !errors.Is(last, ErrCorrupted) {
		t.Errorf("a stream that was changed: want %v, got %v", ErrCorrupted, last)
	}
}

// TestUnknownRecordIsSkipped verifies that a stream from a newer writer is read
// for what this reader does know, which is what the record lengths are for.
func TestUnknownRecordIsSkipped(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// A record of a kind this reader has never heard of, between two it knows.
	if err := w.Directory(Directory{Path: "/holiday"}); err != nil {
		t.Fatalf("Directory: %v", err)
	}
	if err := w.record(kindFile+7, Source{Kind: "from the future"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	f := File{Path: "/holiday/x", Size: 4, Parts: []Part{{Length: 4, Object: types.Hash256{1}}}}
	if err := w.File(f); err != nil {
		t.Fatalf("File: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, dirs, files := read(t, buf.Bytes())
	if len(dirs) != 1 || len(files) != 1 {
		t.Fatalf("want the folder and the file this reader knows, got %d and %d", len(dirs), len(files))
	}
	if !reflect.DeepEqual(files[0], f) {
		t.Errorf("the file after the unknown record: want %+v, got %+v", f, files[0])
	}
}

// TestClosedWriter verifies that nothing is written after the stream has been
// ended, since the digest is of what came before it.
func TestClosedWriter(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, testHeader())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := w.Directory(Directory{Path: "/late"}); err == nil {
		t.Error("a folder was written after the stream was ended")
	}
	if err := w.Close(); err == nil {
		t.Error("the stream was ended twice")
	}
}
