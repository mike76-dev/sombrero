package files

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

// archiveItem is one entry of an archive to be written: where it goes in
// the archive and what it is on the share.
type archiveItem struct {
	name     string
	key      string
	dir      bool
	size     uint64
	modified time.Time
}

// archiveHandler handles the GET and POST /share/:name/archive calls, sending
// what the path parameters name, folders with everything in them, as one zip
// archive. A folder alone names the archive; otherwise its parent does, and
// the share where that is the root.
func (h *Handler) archiveHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
	share, r, ok := h.shareFor(w, s, ps.ByName("name"))
	if !ok {
		return
	}
	if !r.Read {
		writeError(w, "access denied", http.StatusForbidden)
		return
	}
	if err := req.ParseForm(); err != nil {
		writeError(w, "malformed request", http.StatusBadRequest)
		return
	}
	var paths []string
	for _, raw := range req.Form["path"] {
		p, ok := cleanPath(raw)
		if !ok {
			writeError(w, "invalid path", http.StatusBadRequest)
			return
		}
		paths = append(paths, p)
	}
	// The root is downloaded whole, so it is named alone, where it is named.
	if len(paths) == 0 || (len(paths) > 1 && slices.Contains(paths, "/")) {
		writeError(w, "invalid path", http.StatusBadRequest)
		return
	}

	c, partSize, err := h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		log.Printf("file API: share %s unavailable to workgroup %s: %v", share.Name, s.account.Workgroup, err)
		writeError(w, "share unavailable", http.StatusServiceUnavailable)
		return
	}

	// Everything is looked up before the first byte goes out, so that what
	// is not there is refused with a status rather than a broken archive.
	ctx := req.Context()
	var items []archiveItem
	for _, p := range paths {
		if p == "/" {
			items, err = h.collect(ctx, c, s.account, "", "", items)
			break
		}
		key := strings.TrimPrefix(p, "/")
		var oi client.ObjectInfo
		oi, err = c.Object(ctx, s.account, key)
		if errors.Is(err, stores.ErrNotFound) {
			writeError(w, "not found", http.StatusNotFound)
			return
		} else if err != nil {
			log.Printf("file API: failed to look up %s on %s: %v", p, share.Name, err)
			writeError(w, "internal error", http.StatusInternalServerError)
			return
		}
		name := path.Base(p)
		if !strings.HasSuffix(oi.Key, "/") {
			items = append(items, archiveItem{name: name, key: key, size: oi.Size, modified: oi.ModifiedAt})
			continue
		}
		items = append(items, archiveItem{name: name + "/", key: key, dir: true, modified: oi.ModifiedAt})
		if items, err = h.collect(ctx, c, s.account, key, name+"/", items); err != nil {
			break
		}
	}
	if err != nil {
		log.Printf("file API: failed to list the archive of %v on %s: %v", paths, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// A folder downloaded alone names the archive; otherwise the parent of
	// what is downloaded does, and the share where that is the root.
	name := share.Name
	if len(paths) == 1 && paths[0] != "/" && items[0].dir {
		name = path.Base(paths[0])
	} else if parent := path.Dir(paths[0]); parent != "/" {
		name = path.Base(parent)
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))

	// A read that fails midway cannot be reported any more; the connection is
	// dropped instead, so that the client gets no archive that looks whole.
	zw := zip.NewWriter(w)
	for _, item := range items {
		fh := &zip.FileHeader{Name: item.name, Method: zip.Deflate, Modified: item.modified}
		if item.dir {
			fh.SetMode(0o755 | fs.ModeDir)
		} else {
			fh.SetMode(0o644)
			if !compressible(item.name) {
				fh.Method = zip.Store
			}
		}
		fw, err := zw.CreateHeader(fh)
		if err != nil {
			log.Printf("file API: failed to write the archive of %s: %v", share.Name, err)
			panic(http.ErrAbortHandler)
		}
		if item.dir {
			continue
		}
		key := item.key
		rd := h.newReader(fileKey{share: share.Name, key: key, size: item.size, modified: item.modified}, partSize,
			func(ctx context.Context, offset, length uint64, w io.Writer) error {
				return c.Read(ctx, s.account, key, offset, length, w)
			})
		if _, err := io.Copy(fw, rd); err != nil {
			log.Printf("file API: failed to archive %s on %s: %v", key, share.Name, err)
			panic(http.ErrAbortHandler)
		}
	}
	if err := zw.Close(); err != nil {
		log.Printf("file API: failed to finish the archive of %s: %v", share.Name, err)
		panic(http.ErrAbortHandler)
	}
}

// collect lists the folder at key, the root where it is empty, into items
// under the prefix, and its folders after it. A folder the account may not
// list, or one gone meanwhile, stays empty.
func (h *Handler) collect(ctx context.Context, c client.Client, acc stores.Account, key, prefix string, items []archiveItem) ([]archiveItem, error) {
	listKey := ""
	if key != "" {
		listKey = key + "/"
	}
	ois, err := c.List(ctx, acc, listKey)
	if errors.Is(err, stores.ErrAccessDenied) || errors.Is(err, stores.ErrNotFound) {
		return items, nil
	} else if err != nil {
		return nil, err
	}
	for _, oi := range ois {
		k := strings.TrimPrefix(oi.Key, "/")
		dir := strings.HasSuffix(k, "/")
		k = strings.TrimSuffix(k, "/")
		if k == "" || k == key {
			continue
		}
		name := prefix + path.Base(k)
		if !dir {
			items = append(items, archiveItem{name: name, key: k, size: oi.Size, modified: oi.ModifiedAt})
			continue
		}
		items = append(items, archiveItem{name: name + "/", key: k, dir: true, modified: oi.ModifiedAt})
		if items, err = h.collect(ctx, c, acc, k, name+"/", items); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// stored lists the kinds of file that are compressed already, which an
// archive takes as they are rather than spend the time on them again.
var stored = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".avif": true, ".heic": true,
	".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true,
	".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".oga": true, ".opus": true, ".flac": true,
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true, ".7z": true, ".rar": true, ".jar": true,
	".docx": true, ".xlsx": true, ".pptx": true, ".odt": true, ".ods": true, ".odp": true, ".pdf": true,
}

func compressible(name string) bool {
	return !stored[strings.ToLower(path.Ext(name))]
}
