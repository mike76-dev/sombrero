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
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	proto "go.sia.tech/core/rhp/v4"
)

// fileHandler handles the GET /share/:name/file calls, sending the file the
// path query names. Ranges are honoured, so a viewer may seek in it.
func (h *Handler) fileHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}

	p, ok := cleanPath(req.FormValue("path"))
	if !ok || p == "/" {
		writeError(w, "invalid path", http.StatusBadRequest)
		return
	}

	c, err := h.server.ShareClient(share, s.account.Workgroup)
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

	name := path.Base(p)
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	disposition := "attachment"
	if req.FormValue("inline") != "" {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))

	rd := &objectReader{ctx: ctx, client: c, account: s.account, path: key, size: oi.Size, chunk: h.chunkSize(ctx, c)}
	http.ServeContent(w, req, "", oi.ModifiedAt, rd)
}

// storageChunk is how much of a file is fetched at a time: the SMB layer's
// chunk for the share's slab size, or its smallest where the backend will not
// say.
func storageChunk(ctx context.Context, c client.Client) uint64 {
	si, err := c.Storage(ctx)
	if err != nil || si.MinShards <= 0 {
		return client.ReadChunkSize(0)
	}
	return client.ReadChunkSize(uint64(si.MinShards) * proto.SectorSize)
}

// objectReader reads a file off its share a chunk at a time, which is what
// lets a download be served without holding the whole file.
type objectReader struct {
	ctx     context.Context
	client  client.Client
	account stores.Account
	path    string
	size    uint64
	chunk   uint64

	pos   int64
	buf   []byte
	bufAt int64
}

// Read implements io.Reader. A read outside the chunk in hand fetches the
// chunk starting where the read does, so a sequential read stays contiguous.
func (r *objectReader) Read(p []byte) (int, error) {
	if r.pos >= int64(r.size) {
		return 0, io.EOF
	}
	if r.buf == nil || r.pos < r.bufAt || r.pos >= r.bufAt+int64(len(r.buf)) {
		start := uint64(r.pos)
		n := min(r.chunk, r.size-start)
		var b bytes.Buffer
		b.Grow(int(n))
		if err := r.client.Read(r.ctx, r.account, r.path, start, n, &b); err != nil {
			log.Printf("file API: failed to read %s at %d: %v", r.path, start, err)
			return 0, err
		}
		if uint64(b.Len()) != n {
			return 0, fmt.Errorf("read %d bytes of %s at %d, want %d", b.Len(), r.path, start, n)
		}
		r.buf, r.bufAt = b.Bytes(), int64(start)
	}
	k := copy(p, r.buf[r.pos-r.bufAt:])
	r.pos += int64(k)
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
