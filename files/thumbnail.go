package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/mike76-dev/sombrero/stores"
	"golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	_ "image/gif"
)

const (
	// thumbnailSide is the longest side of a thumbnail, in pixels.
	thumbnailSide = 256

	// thumbnailMaxSize bounds the files a thumbnail is made from, and
	// thumbnailMaxPixels the images, as one is decoded whole.
	thumbnailMaxSize   = 64 << 20
	thumbnailMaxPixels = 50_000_000

	// thumbnailWorkers is how many thumbnails are made at a time.
	thumbnailWorkers = 3

	// thumbnailAge is how long a thumbnail nobody asks for is kept, and
	// thumbnailPruneEvery how often the cache is looked through for such.
	thumbnailAge        = 60 * 24 * time.Hour
	thumbnailPruneEvery = time.Hour
)

// errNoThumbnail says that no thumbnail can be made of the file.
var errNoThumbnail = errors.New("no thumbnail")

// thumbnailable lists the kinds of image a thumbnail is made of, which are
// those the server decodes.
var thumbnailable = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true}

// thumbnailCache keeps the thumbnails on disk, each under the id of the file
// it was made of, in a folder per leading byte of the id. A file no thumbnail
// can be made of is remembered as such, so that it is not fetched again.
type thumbnailCache struct {
	dir   string
	slots chan struct{}

	mu     sync.Mutex
	pruned time.Time
}

// SetThumbnailDir keeps the thumbnails of the images in the directory.
func (h *Handler) SetThumbnailDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create the thumbnail directory: %w", err)
	}
	h.thumbs = &thumbnailCache{dir: dir, slots: make(chan struct{}, thumbnailWorkers)}
	return nil
}

// thumbnailHandler handles the GET /share/:name/thumbnail calls, sending a
// small image of the file the path query names. The browser may keep it: the
// id changes with the file, and a changed file is a new address to a client
// that checks with the tag it was given.
func (h *Handler) thumbnailHandler(w http.ResponseWriter, req *http.Request, ps httprouter.Params, s session) {
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
	if h.thumbs == nil || !thumbnailable[strings.ToLower(path.Ext(p))] {
		writeError(w, "no thumbnail", http.StatusNotFound)
		return
	}

	c, _, err := h.server.ShareClient(share, s.account.Workgroup)
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
	if oi.Size > thumbnailMaxSize {
		writeError(w, "no thumbnail", http.StatusNotFound)
		return
	}

	id := thumbnailID(share.Name, key, oi.Size, oi.ModifiedAt)
	etag := `"` + id + `"`
	if req.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, ctype, err := h.thumbs.get(id)
	if errors.Is(err, fs.ErrNotExist) {
		data, ctype, err = h.thumbs.make(ctx, id, path.Ext(p), func(w io.Writer) error {
			return c.Read(ctx, s.account, key, 0, oi.Size, w)
		})
	}
	if errors.Is(err, errNoThumbnail) {
		writeError(w, "no thumbnail", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("file API: failed to make a thumbnail of %s on %s: %v", p, share.Name, err)
		writeError(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

// thumbnailID names the thumbnail of the file as it is: another size or time
// is another file.
func thumbnailID(share, key string, size uint64, modified time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", share, key, size, modified.UnixNano())))
	return hex.EncodeToString(sum[:16])
}

// The thumbnails are JPEG where the image is, and PNG otherwise, as the other
// kinds may be transparent.
var thumbnailTypes = map[string]string{".jpg": "image/jpeg", ".png": "image/png", ".none": ""}

func (t *thumbnailCache) file(id, ext string) string {
	return filepath.Join(t.dir, id[:2], id+ext)
}

// get reads the thumbnail with the id, or reports fs.ErrNotExist where none
// has been made and errNoThumbnail where none could be. A thumbnail asked
// for is kept for another while.
func (t *thumbnailCache) get(id string) ([]byte, string, error) {
	for ext, ctype := range thumbnailTypes {
		name := t.file(id, ext)
		data, err := os.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, "", err
		}
		now := time.Now()
		os.Chtimes(name, now, now)
		if ext == ".none" {
			return nil, "", errNoThumbnail
		}
		return data, ctype, nil
	}
	return nil, "", fs.ErrNotExist
}

// make reads the file with read and keeps its thumbnail under the id, or a
// note that none can be made of it. Only so many are made at a time, as one
// takes the whole image in memory.
func (t *thumbnailCache) make(ctx context.Context, id, ext string, read func(io.Writer) error) ([]byte, string, error) {
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	t.maybePrune()

	var buf bytes.Buffer
	if err := read(&buf); err != nil {
		return nil, "", err
	}
	data, ctype, err := thumbnail(buf.Bytes(), strings.ToLower(ext) == ".jpg" || strings.ToLower(ext) == ".jpeg")
	if errors.Is(err, errNoThumbnail) {
		t.store(id, ".none", nil)
		return nil, "", err
	} else if err != nil {
		return nil, "", err
	}
	outExt := ".png"
	if ctype == "image/jpeg" {
		outExt = ".jpg"
	}
	if err := t.store(id, outExt, data); err != nil {
		return nil, "", err
	}
	return data, ctype, nil
}

// store writes the thumbnail whole or not at all, so that a reader never
// finds half of one.
func (t *thumbnailCache) store(id, ext string, data []byte) error {
	name := t.file(id, ext)
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), id+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// maybePrune drops the thumbnails nobody has asked for in a long while, once
// in a while, in the background.
func (t *thumbnailCache) maybePrune() {
	t.mu.Lock()
	due := time.Since(t.pruned) >= thumbnailPruneEvery
	if due {
		t.pruned = time.Now()
	}
	t.mu.Unlock()
	if !due {
		return
	}
	go func() {
		cutoff := time.Now().Add(-thumbnailAge)
		filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
				os.Remove(p)
			}
			return nil
		})
	}()
}

// thumbnail makes a thumbnail of the image: a JPEG of a JPEG, turned the way
// its camera says, and a PNG of anything else. An image too large, or not
// one the server decodes, has none.
func thumbnail(data []byte, isJPEG bool) ([]byte, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > thumbnailMaxPixels {
		return nil, "", errNoThumbnail
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", errNoThumbnail
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > thumbnailSide || h > thumbnailSide {
		if w >= h {
			h = max(1, h*thumbnailSide/w)
			w = thumbnailSide
		} else {
			w = max(1, w*thumbnailSide/h)
			h = thumbnailSide
		}
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)

	var out bytes.Buffer
	if isJPEG {
		if err := jpeg.Encode(&out, orient(dst, jpegOrientation(data)), &jpeg.Options{Quality: 82}); err != nil {
			return nil, "", err
		}
		return out.Bytes(), "image/jpeg", nil
	}
	if err := png.Encode(&out, dst); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "image/png", nil
}

// orient turns and flips the image as the EXIF orientation says the camera
// held it, so that the thumbnail is upright.
func orient(src *image.NRGBA, orientation int) *image.NRGBA {
	if orientation <= 1 || orientation > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch orientation {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			dst.SetNRGBA(x, y, src.NRGBAAt(sx, sy))
		}
	}
	return dst
}

// jpegOrientation reads the EXIF orientation of a JPEG, 1 where it has none.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(data) && data[i] == 0xFF; {
		marker := data[i+1]
		switch {
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9:
			return 1
		}
		n := int(binary.BigEndian.Uint16(data[i+2:]))
		if n < 2 || i+2+n > len(data) {
			return 1
		}
		if seg := data[i+4 : i+2+n]; marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return tiffOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

// tiffOrientation finds the orientation tag in the first directory of a TIFF
// header, which is where EXIF keeps it.
func tiffOrientation(t []byte) int {
	var bo binary.ByteOrder
	switch {
	case bytes.HasPrefix(t, []byte("II*\x00")):
		bo = binary.LittleEndian
	case bytes.HasPrefix(t, []byte("MM\x00*")):
		bo = binary.BigEndian
	default:
		return 1
	}
	if len(t) < 8 {
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off < 0 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 && bo.Uint16(t[e+2:]) == 3 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}
