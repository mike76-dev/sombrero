package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/renterd/v2/api"
)

// uploadHandler handles the PUT /share/:name/file calls, storing the request
// body as the file the path query names, over whatever was there. The body
// goes to the share a part at a time, so a file of any size is taken.
func (h *Handler) uploadHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Write {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}

	// The query, never FormValue: a form-typed body would be read as a form.
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

	// A backend that knows the folder rights is asked up front, so that a
	// refusal comes before the body is read rather than as a failed upload.
	ctx := req.Context()
	key := strings.TrimPrefix(p, "/")
	if ac, ok := c.(client.AccessChecker); ok {
		pa, err := ac.PathAccess(ctx, s.account, key)
		if errors.Is(err, stores.ErrNotFound) {
			writeError(w, "folder not found", http.StatusNotFound)
			return
		} else if err != nil {
			log.Printf("file API: failed to check %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !pa.Write {
			writeError(w, "access denied", http.StatusForbidden)
			return
		}
	}
	if oi, err := c.Object(ctx, s.account, key); err == nil && strings.HasSuffix(oi.Key, "/") {
		writeError(w, "not a file", http.StatusBadRequest)
		return
	}

	size, failure := h.storeBody(ctx, c, s.account, share.Name, key, partSize, req.Body)
	if failure != nil {
		writeError(w, failure.message, failure.code)
		return
	}

	entry := Entry{Name: path.Base(p), Path: p, Size: size}
	if oi, err := c.Object(ctx, s.account, key); err == nil {
		entry.CreatedAt, entry.ModifiedAt = oi.CreatedAt, oi.ModifiedAt
	}
	writeJSONStatus(w, http.StatusCreated, entry)
}

// failure is what a store that did not go through answers with.
type failure struct {
	code    int
	message string
}

// storeBody stores the body as the file at the key, over whatever was there,
// a part at a time, and returns the size stored. Nothing empty can be stored,
// so the first part is read before the upload is started, and an empty body
// starts none.
func (h *Handler) storeBody(ctx context.Context, c client.Client, acc stores.Account, share, key string, partSize uint64, body io.Reader) (uint64, *failure) {
	buf := make([]byte, partSize)
	n, err := readPart(body, buf)
	if err != nil {
		return 0, &failure{http.StatusBadRequest, "failed to read the body"}
	}
	if n == 0 {
		return 0, &failure{http.StatusBadRequest, "an empty file cannot be stored"}
	}

	id, err := c.StartUpload(ctx, acc, key)
	if errors.Is(err, stores.ErrNotFound) {
		return 0, &failure{http.StatusConflict, "the file is being written by somebody else"}
	} else if err != nil {
		log.Printf("file API: failed to start the upload of %s on %s: %v", key, share, err)
		return 0, &failure{http.StatusInternalServerError, "internal error"}
	}

	// The parts are sent one after another: what bounds the memory is one
	// part, and what bounds the speed is the backend, not this loop.
	var parts []api.MultipartCompletedPart
	var offset uint64
	for number := 1; n > 0; number++ {
		eTag, err := c.Write(ctx, bytes.NewReader(buf[:n]), key, id, number, offset, uint64(n))
		if err != nil {
			log.Printf("file API: failed to store part %d of %s on %s: %v", number, key, share, err)
			h.abortUpload(ctx, c, key, id)
			return 0, &failure{http.StatusInternalServerError, "failed to store the file"}
		}
		parts = append(parts, api.MultipartCompletedPart{PartNumber: number, ETag: eTag})
		offset += uint64(n)

		n, err = readPart(body, buf)
		if err != nil {
			h.abortUpload(ctx, c, key, id)
			return 0, &failure{http.StatusBadRequest, "failed to read the body"}
		}
	}

	if err := c.FinishUpload(ctx, key, id, parts); err != nil {
		log.Printf("file API: failed to finish the upload of %s on %s: %v", key, share, err)
		h.abortUpload(ctx, c, key, id)
		return 0, &failure{http.StatusInternalServerError, "failed to store the file"}
	}
	return offset, nil
}

// readPart fills the buffer from the body as far as the body goes. Only its
// clean end is an end: a body cut short, as by a cancelled upload, reports
// io.ErrUnexpectedEOF, which io.ReadFull would make indistinguishable from a
// short last part, and which must not be stored as the whole file.
func readPart(body io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := body.Read(buf[n:])
		n += m
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// abortUpload drops an upload that will not be finished. The request may be
// gone by now, so the abort runs on a context of its own.
func (h *Handler) abortUpload(ctx context.Context, c client.Client, key, id string) {
	if err := c.AbortUpload(context.WithoutCancel(ctx), key, id); err != nil {
		log.Printf("file API: failed to abort the upload of %s: %v", key, err)
	}
}
