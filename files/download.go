package files

import (
	"context"
	"errors"
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

	contentHeaders(w, path.Base(p), req.URL.Query().Get("inline") != "")

	// The chunk follows the share's part size the way the SMB read cache's does.
	rd := h.newReader(fileKey{share: share.Name, key: key, size: oi.Size, modified: oi.ModifiedAt}, partSize,
		func(ctx context.Context, offset, length uint64, w io.Writer) error {
			return c.Read(ctx, s.account, key, offset, length, w)
		})
	http.ServeContent(w, req, "", oi.ModifiedAt, rd)
}

// contentHeaders names the file in the response and says whether it is saved
// or shown. Shown, a file that could carry code of its own is sandboxed: it
// is somebody's upload, and it must not run as the server's origin, where the
// sessions and the administration's password live.
func contentHeaders(w http.ResponseWriter, name string, inline bool) {
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	// A copy the browser keeps is used again only once the server has
	// agreed, so that it goes with the session or the link that fetched it.
	w.Header().Set("Cache-Control", "private, no-cache")
	disposition := "attachment"
	if inline {
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

// objectReader reads a file off its share through the read cache a piece at a
// time, which is what lets a download be served without holding the whole
// file. Once the reads are past the middle of the piece in hand, the next is
// fetched meanwhile, so that a stream does not stop for every fetch.
type objectReader struct {
	cache *readCache
	file  fileKey
	fetch fetchFunc
	chunk uint64

	pos int64
	seg *segment
}

// newReader reads the file with the handler's cache.
func (h *Handler) newReader(file fileKey, partSize uint64, fetch fetchFunc) *objectReader {
	return &objectReader{cache: h.cache, file: file, fetch: fetch, chunk: h.chunkSize(partSize)}
}

// Read implements io.Reader.
func (r *objectReader) Read(p []byte) (int, error) {
	if r.pos >= int64(r.file.size) {
		return 0, io.EOF
	}
	pos := uint64(r.pos)
	if r.seg == nil || pos < r.seg.at || pos >= r.seg.end() {
		s := r.cache.segmentAt(r.file, pos, r.chunk, r.fetch)
		<-s.done
		if s.err != nil {
			log.Printf("file API: failed to read %s at %d: %v", r.file.key, s.at, s.err)
			return 0, s.err
		}
		r.seg = s
	}
	k := copy(p, r.seg.data[pos-r.seg.at:])
	r.pos += int64(k)
	if end := r.seg.end(); end < r.file.size && uint64(r.pos)-r.seg.at > r.seg.n/2 {
		r.cache.ahead(r.file, end, r.chunk, r.fetch)
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
		pos = int64(r.file.size) + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = pos
	return pos, nil
}
