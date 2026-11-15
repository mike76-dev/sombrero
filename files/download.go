package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
)

// fileHandler handles the GET and HEAD /share/:name/file calls, sending the
// file the path query names. Ranges are honoured, so a viewer may seek in it.
func (h *Handler) fileHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}

	p, ok := cleanPath(req.URL.Query().Get("path"))
	if !ok || p == "/" {
		writeError(w, "invalid path", http.StatusBadRequest)
		return
	}

	c, partSize, err := h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, s.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return
	}

	ctx := req.Context()
	key := strings.TrimPrefix(p, "/")
	oi, err := c.Object(ctx, s.account, key)
	if errors.Is(err, stores.ErrNotFound) {
		writeError(w, "file not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("file API: failed to look up %s on %s: %v", p, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	if strings.HasSuffix(oi.Key, "/") {
		writeError(w, "not a file", http.StatusBadRequest)
		return
	}

	contentHeaders(w, req, path.Base(p))

	// The chunk follows the share's part size the way the SMB read cache's does.
	rd := &objectReader{
		ctx: ctx,
		fetch: func(ctx context.Context, offset, length uint64, w io.Writer) error {
			return c.Read(ctx, s.account, key, offset, length, w)
		},
		path:  key,
		size:  oi.Size,
		chunk: h.chunkSize(partSize),
	}
	http.ServeContent(w, req, "", oi.ModifiedAt, rd)
}

// contentHeaders names the file in the response and says whether it is saved
// or shown. Shown, a file that could carry code of its own is sandboxed: it
// is somebody's upload, and it must not run as the server's origin, where the
// sessions and the administration's password live.
func contentHeaders(w http.ResponseWriter, req *http.Request, name string) {
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	disposition := "attachment"
	if req.URL.Query().Get("inline") != "" {
		disposition = "inline"
		if !selfContained(ctype) {
			w.Header().Set("Content-Security-Policy", "sandbox")
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
}

// selfContained reports whether a browser shows a file of the type with no
// code of the file's own: such a file needs no sandbox, and a PDF viewer does
// not work in one.
func selfContained(ctype string) bool {
	return ctype == "application/pdf" ||
		(strings.HasPrefix(ctype, "image/") && ctype != "image/svg+xml") ||
		strings.HasPrefix(ctype, "video/") ||
		strings.HasPrefix(ctype, "audio/")
}

// firstFetch is how much a reader fetches at first; the fetches double while
// the reads stay sequential, up to the chunk. A request that reads a little
// and leaves, as a player seeking through a file does, costs little that way,
// and one that streams soon reads in whole chunks.
const firstFetch = 1 << 20

// objectReader reads a file off its share a chunk at a time, which is what
// lets a download be served without holding the whole file. Once the reads
// are past the middle of the chunk in hand, the next is fetched meanwhile, so
// that a stream does not stop for every fetch.
type objectReader struct {
	ctx context.Context
	// fetch reads a range of the file into the writer, from wherever it is.
	fetch func(ctx context.Context, offset, length uint64, w io.Writer) error
	path  string
	size  uint64
	chunk uint64

	pos   int64
	buf   []byte
	bufAt int64
	next  uint64
	ahead *fetched
}

// fetched is a fetch under way or done: data or err is set once done is closed.
type fetched struct {
	at     uint64
	data   []byte
	err    error
	done   chan struct{}
	cancel context.CancelFunc
}

// start fetches n bytes from at in the background.
func (r *objectReader) start(at, n uint64) *fetched {
	ctx, cancel := context.WithCancel(r.context())
	f := &fetched{at: at, done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(f.done)
		var b bytes.Buffer
		b.Grow(int(n))
		if err := r.fetch(ctx, at, n, &b); err != nil {
			f.err = err
		} else if uint64(b.Len()) != n {
			f.err = fmt.Errorf("read %d bytes of %s at %d, want %d", b.Len(), r.path, at, n)
		} else {
			f.data = b.Bytes()
		}
	}()
	return f
}

func (r *objectReader) context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// Read implements io.Reader. A read outside the chunk in hand takes the one
// fetched ahead if it starts there, and otherwise fetches from where the read
// is, starting small again unless it follows on from the chunk in hand.
func (r *objectReader) Read(p []byte) (int, error) {
	if r.pos >= int64(r.size) {
		return 0, io.EOF
	}
	if r.buf == nil || r.pos < r.bufAt || r.pos >= r.bufAt+int64(len(r.buf)) {
		start := uint64(r.pos)
		f := r.ahead
		r.ahead = nil
		if f != nil && f.at != start {
			f.cancel()
			f = nil
		}
		if f == nil {
			if r.buf == nil || start != uint64(r.bufAt)+uint64(len(r.buf)) {
				r.next = min(firstFetch, r.chunk)
			}
			f = r.start(start, min(r.next, r.size-start))
			r.next = min(r.next*2, r.chunk)
		}
		<-f.done
		if f.err != nil {
			log.Printf("file API: failed to read %s at %d: %v", r.path, f.at, f.err)
			return 0, f.err
		}
		r.buf, r.bufAt = f.data, int64(f.at)
	}
	k := copy(p, r.buf[r.pos-r.bufAt:])
	r.pos += int64(k)
	if end := uint64(r.bufAt) + uint64(len(r.buf)); r.ahead == nil && end < r.size && r.pos-r.bufAt > int64(len(r.buf))/2 {
		r.ahead = r.start(end, min(r.next, r.size-end))
		r.next = min(r.next*2, r.chunk)
	}
	return k, nil
}

// Seek implements io.Seeker.
func (r *objectReader) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = r.pos + offset
	case io.SeekEnd:
		pos = int64(r.size) + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = pos
	return pos, nil
}
