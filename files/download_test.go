package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

var (
	readmeContent  = bytes.Repeat([]byte("0123456789"), 10)
	readmeModified = time.Date(2026, 11, 2, 8, 30, 0, 0, time.UTC)
)

// readingClient is a client with readme.txt of a hundred bytes and the
// folder Projects.
func readingClient() *mockClient {
	return &mockClient{
		object: func(p string) (client.ObjectInfo, error) {
			switch p {
			case "readme.txt":
				return client.ObjectInfo{Key: "/readme.txt", Size: uint64(len(readmeContent)), ModifiedAt: readmeModified}, nil
			case "Projects":
				return client.ObjectInfo{Key: "/Projects/"}, nil
			}
			return client.ObjectInfo{}, stores.ErrNotFound
		},
		read: func(p string, offset, length uint64, w io.Writer) error {
			if p != "readme.txt" || offset+length > uint64(len(readmeContent)) {
				return stores.ErrNotFound
			}
			_, err := w.Write(readmeContent[offset : offset+length])
			return err
		},
	}
}

// TestShowingAFileSandboxesCode verifies that a file shown inline runs nothing
// as the server's origin, unless it is of a kind a browser renders by itself.
func TestShowingAFileSandboxesCode(t *testing.T) {
	for _, tt := range []struct {
		name    string
		sandbox bool
	}{
		{"page.html", true},
		{"logo.svg", true},
		{"notes.txt", true},
		{"data.json", true},
		{"other.bin", true},
		{"photo.jpg", false},
		{"clip.mp4", false},
		{"song.mp3", false},
		{"plan.pdf", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/share/docs/file?path=/"+tt.name+"&inline=1", nil)
		w := httptest.NewRecorder()
		contentHeaders(w, req, tt.name)
		if got := w.Header().Get("Content-Security-Policy") == "sandbox"; got != tt.sandbox {
			t.Errorf("%s shown inline: sandboxed %v, want %v", tt.name, got, tt.sandbox)
		}
		if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
			t.Errorf("%s: want it shown, got %q", tt.name, got)
		}
	}

	// Saved rather than shown, nothing runs, so nothing is sandboxed.
	req := httptest.NewRequest(http.MethodGet, "/share/docs/file?path=/page.html", nil)
	w := httptest.NewRecorder()
	contentHeaders(w, req, "page.html")
	if got := w.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("an attachment: want no sandbox, got %q", got)
	}
}

// download asks for the file with the given headers.
func download(h http.Handler, from *httptest.ResponseRecorder, query string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/share/docs/file?"+query, nil)
	for _, c := range from.Result().Cookies() {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestDownloadingAFile verifies that a file comes back whole, in chunks of
// the size the handler picks, under the headers a browser needs.
func TestDownloadingAFile(t *testing.T) {
	mc := readingClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")
	h.chunkSize = func(uint64) uint64 { return 32 }

	r := download(h, w, "path=/readme.txt", nil)
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if !bytes.Equal(r.Body.Bytes(), readmeContent) {
		t.Errorf("want the file's content, got %q", r.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Length":      "100",
		"Content-Type":        "text/plain; charset=utf-8",
		"Content-Disposition": `attachment; filename=readme.txt`,
		"Accept-Ranges":       "bytes",
		"Last-Modified":       readmeModified.UTC().Format(http.TimeFormat),
	} {
		if got := r.Header().Get(name); got != want {
			t.Errorf("%s: want %q, got %q", name, want, got)
		}
	}
	want := [][2]uint64{{0, 32}, {32, 32}, {64, 32}, {96, 4}}
	if len(mc.reads) != len(want) {
		t.Fatalf("want the reads %v, got %v", want, mc.reads)
	}
	for i := range want {
		if mc.reads[i] != want[i] {
			t.Errorf("read %d: want %v, got %v", i, want[i], mc.reads[i])
		}
	}

	// Asked for inline, the file is shown rather than saved, and shown
	// sandboxed, as text could be a page.
	r = download(h, w, "path=/readme.txt&inline=1", nil)
	if got := r.Header().Get("Content-Disposition"); got != `inline; filename=readme.txt` {
		t.Errorf("inline: want the inline disposition, got %q", got)
	}
	if got := r.Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("inline: want the sandbox, got %q", got)
	}
	if got := r.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("want the type taken as given, got %q", got)
	}

	// A HEAD request describes the file without fetching any of it.
	mc.reads = nil
	req := httptest.NewRequest(http.MethodHead, "/share/docs/file?path=/readme.txt", nil)
	for _, c := range w.Result().Cookies() {
		req.AddCookie(c)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, req)
	if r.Code != http.StatusOK || r.Body.Len() != 0 {
		t.Errorf("HEAD: want %d and no body, got %d and %d bytes", http.StatusOK, r.Code, r.Body.Len())
	}
	if got := r.Header().Get("Content-Length"); got != "100" {
		t.Errorf("HEAD: want the length, got %q", got)
	}
	if len(mc.reads) != 0 {
		t.Errorf("HEAD: want nothing read, got %v", mc.reads)
	}
}

// TestDownloadingARange verifies that a range is served from where it starts
// with one fetch, and that one past the end is refused.
func TestDownloadingARange(t *testing.T) {
	mc := readingClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")
	h.chunkSize = func(uint64) uint64 { return 64 }

	r := download(h, w, "path=/readme.txt", map[string]string{"Range": "bytes=40-59"})
	if r.Code != http.StatusPartialContent {
		t.Fatalf("want %d, got %d (%s)", http.StatusPartialContent, r.Code, r.Body.String())
	}
	if !bytes.Equal(r.Body.Bytes(), readmeContent[40:60]) {
		t.Errorf("want bytes 40-59, got %q", r.Body.String())
	}
	if got := r.Header().Get("Content-Range"); got != "bytes 40-59/100" {
		t.Errorf("Content-Range: want %q, got %q", "bytes 40-59/100", got)
	}
	if len(mc.reads) != 1 || mc.reads[0] != [2]uint64{40, 60} {
		t.Errorf("want one read from 40, got %v", mc.reads)
	}

	if r := download(h, w, "path=/readme.txt", map[string]string{"Range": "bytes=200-"}); r.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("a range past the end: want %d, got %d", http.StatusRequestedRangeNotSatisfiable, r.Code)
	}
}

// TestDownloadRefusals verifies the answers for what cannot be downloaded.
func TestDownloadRefusals(t *testing.T) {
	mc := readingClient()
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"a folder", "path=/Projects", http.StatusBadRequest},
		{"the root", "path=/", http.StatusBadRequest},
		{"no path", "", http.StatusBadRequest},
		{"climbing path", "path=/../readme.txt", http.StatusBadRequest},
		{"not there", "path=/gone.txt", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := download(h, w, tt.query, nil); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}

	g, gw := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "guest", "")
	if r := download(g, gw, "path=/readme.txt", nil); r.Code != http.StatusForbidden {
		t.Errorf("a guest on docs: want %d, got %d", http.StatusForbidden, r.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/share/docs/file?path=/readme.txt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: want %d, got %d", http.StatusUnauthorized, rec.Code)
	}
}

// fetchLog records the fetches a reader makes, from whichever goroutine.
type fetchLog struct {
	mu      sync.Mutex
	fetched [][2]uint64
}

func (l *fetchLog) fetch(_ context.Context, offset, length uint64, w io.Writer) error {
	l.mu.Lock()
	l.fetched = append(l.fetched, [2]uint64{offset, length})
	l.mu.Unlock()
	_, err := w.Write(make([]byte, length))
	return err
}

func (l *fetchLog) all() [][2]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][2]uint64(nil), l.fetched...)
}

func (l *fetchLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fetched = nil
}

// TestObjectReaderFetchesGrow verifies that a reader starts small and doubles
// its fetches while the reads stay sequential, and starts small again after
// a seek, so that a seek-and-peek costs little and a stream soon reads in
// whole chunks.
func TestObjectReaderFetchesGrow(t *testing.T) {
	const mib = 1 << 20
	l := &fetchLog{}
	r := &objectReader{fetch: l.fetch, path: "big.bin", size: 5 * mib, chunk: 8 * mib}

	if n, err := io.Copy(io.Discard, r); err != nil || n != 5*mib {
		t.Fatalf("reading the whole file: got %d bytes, %v", n, err)
	}
	want := [][2]uint64{{0, mib}, {mib, 2 * mib}, {3 * mib, 2 * mib}}
	fetched := l.all()
	if len(fetched) != len(want) {
		t.Fatalf("want the fetches %v, got %v", want, fetched)
	}
	for i := range want {
		if fetched[i] != want[i] {
			t.Errorf("fetch %d: want %v, got %v", i, want[i], fetched[i])
		}
	}

	// A seek out of the chunk in hand starts small again.
	l.reset()
	if _, err := r.Seek(mib, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := io.ReadFull(r, make([]byte, 64<<10)); err != nil {
		t.Fatalf("reading after the seek: %v", err)
	}
	if fetched := l.all(); len(fetched) != 1 || fetched[0] != [2]uint64{mib, mib} {
		t.Errorf("after a seek: want one fetch of a megabyte, got %v", fetched)
	}
}

// TestObjectReaderReadsAhead verifies that the next chunk is fetched while
// the reads are in the second half of the one in hand, that a sequential
// read then takes it, and that a seek elsewhere cancels it.
func TestObjectReaderReadsAhead(t *testing.T) {
	const mib = 1 << 20
	started := make(chan [2]uint64, 8)
	var aheadCtx context.Context
	var mu sync.Mutex
	r := &objectReader{
		fetch: func(ctx context.Context, offset, length uint64, w io.Writer) error {
			mu.Lock()
			if offset == mib {
				aheadCtx = ctx
			}
			mu.Unlock()
			started <- [2]uint64{offset, length}
			_, err := w.Write(make([]byte, length))
			return err
		},
		path:  "big.bin",
		size:  8 * mib,
		chunk: 8 * mib,
	}

	// The first half of the chunk is served with no fetch ahead of it.
	if _, err := io.ReadFull(r, make([]byte, mib/2)); err != nil {
		t.Fatalf("reading the first half: %v", err)
	}
	if got := <-started; got != [2]uint64{0, mib} {
		t.Fatalf("want the first megabyte fetched, got %v", got)
	}
	select {
	case got := <-started:
		t.Fatalf("want nothing fetched ahead in the first half, got %v", got)
	default:
	}

	// Past the middle, the next chunk is fetched before it is asked for.
	if _, err := io.ReadFull(r, make([]byte, 1)); err != nil {
		t.Fatalf("reading past the middle: %v", err)
	}
	select {
	case got := <-started:
		if got != [2]uint64{mib, 2 * mib} {
			t.Fatalf("want the next two megabytes fetched ahead, got %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("want a fetch ahead, got none")
	}

	// A seek elsewhere cancels it and fetches afresh from there.
	if _, err := r.Seek(5*mib, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := io.ReadFull(r, make([]byte, 1)); err != nil {
		t.Fatalf("reading after the seek: %v", err)
	}
	if got := <-started; got != [2]uint64{5 * mib, mib} {
		t.Errorf("after a seek: want a megabyte fetched from there, got %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if aheadCtx == nil || aheadCtx.Err() == nil {
		t.Error("want the fetch ahead cancelled by the seek")
	}
}

// TestObjectReaderReportsShortReads verifies that a backend that hands back
// less than asked for fails the read rather than passing a truncated file off
// as whole.
func TestObjectReaderReportsShortReads(t *testing.T) {
	short := func(_ context.Context, _, length uint64, w io.Writer) error {
		_, err := w.Write(make([]byte, length-1))
		return err
	}
	rd := &objectReader{fetch: short, path: "x", size: 10, chunk: 10}
	if _, err := io.ReadAll(rd); err == nil || !strings.Contains(err.Error(), "want 10") {
		t.Errorf("want a short read reported, got %v", err)
	}

	failing := func(context.Context, uint64, uint64, io.Writer) error { return errStore }
	rd = &objectReader{fetch: failing, path: "x", size: 10, chunk: 10}
	if _, err := io.ReadAll(rd); !errors.Is(err, errStore) {
		t.Errorf("want the backend's error, got %v", err)
	}
}
