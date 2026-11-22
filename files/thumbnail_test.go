package files

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
)

var (
	red  = color.NRGBA{R: 220, G: 20, B: 20, A: 255}
	blue = color.NRGBA{R: 20, G: 20, B: 220, A: 255}
)

// halves is an image of the size with its left half red and its right half
// blue, or its top half red and its bottom half blue.
func halves(w, h int, vertical bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := red
			if (!vertical && x >= w/2) || (vertical && y >= h/2) {
				c = blue
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// encodeJPEG encodes the image with an EXIF segment saying the orientation.
func encodeJPEG(t *testing.T, img image.Image, orientation int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	tiff := []byte("MM\x00*\x00\x00\x00\x08\x00\x01")
	entry := make([]byte, 12)
	binary.BigEndian.PutUint16(entry[0:], 0x0112)
	binary.BigEndian.PutUint16(entry[2:], 3)
	binary.BigEndian.PutUint32(entry[4:], 1)
	binary.BigEndian.PutUint16(entry[8:], uint16(orientation))
	tiff = append(tiff, entry...)
	tiff = append(tiff, 0, 0, 0, 0)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(len(seg)+2))
	out := append([]byte{}, data[:2]...)
	out = append(out, app1...)
	out = append(out, seg...)
	return append(out, data[2:]...)
}

// imageClient is a client with the files, each with the given bytes.
func imageClient(files map[string][]byte, modified time.Time) *mockClient {
	return &mockClient{
		object: func(p string) (client.ObjectInfo, error) {
			if p == "Photos" || p == "Shots.png" {
				return client.ObjectInfo{Key: "/" + p + "/"}, nil
			}
			if data, ok := files[p]; ok {
				return client.ObjectInfo{Key: "/" + p, Size: uint64(len(data)), ModifiedAt: modified}, nil
			}
			return client.ObjectInfo{}, stores.ErrNotFound
		},
		read: func(p string, offset, length uint64, w io.Writer) error {
			data, ok := files[p]
			if !ok || offset+length > uint64(len(data)) {
				return stores.ErrNotFound
			}
			_, err := w.Write(data[offset : offset+length])
			return err
		},
	}
}

func thumbnailOf(h http.Handler, from *httptest.ResponseRecorder, query string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/share/docs/thumbnail?"+query, nil)
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

// decodeThumbnail decodes the answer and checks its size.
func decodeThumbnail(t *testing.T, r *httptest.ResponseRecorder, w, h int) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(r.Body.Bytes()))
	if err != nil {
		t.Fatalf("decoding the thumbnail: %v", err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("want %dx%d, got %dx%d", w, h, b.Dx(), b.Dy())
	}
	return img
}

// isRed and isBlue judge a pixel loosely, as JPEG shifts the colours a little.
func isRed(c color.Color) bool {
	r, _, b, _ := c.RGBA()
	return r>>8 > 150 && b>>8 < 100
}

func isBlue(c color.Color) bool {
	r, _, b, _ := c.RGBA()
	return b>>8 > 150 && r>>8 < 100
}

// TestThumbnailOfAnImage verifies that an image comes back small, in
// proportion, and once fetched is served from the cache until the file
// changes, with a tag a browser checks its copy by.
func TestThumbnailOfAnImage(t *testing.T) {
	modified := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	files := map[string][]byte{"Photos/chart.png": encodePNG(t, halves(600, 300, false))}
	mc := imageClient(files, modified)
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")
	if err := h.SetThumbnailDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	r := thumbnailOf(h, w, "path=/Photos/chart.png", nil)
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if got := r.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("want a PNG of a PNG, got %q", got)
	}
	if got := r.Header().Get("Cache-Control"); got != "private, max-age=86400" {
		t.Errorf("Cache-Control: got %q", got)
	}
	etag := r.Header().Get("ETag")
	if etag == "" {
		t.Error("want a tag, got none")
	}
	img := decodeThumbnail(t, r, 256, 128)
	if !isRed(img.At(10, 64)) || !isBlue(img.At(245, 64)) {
		t.Errorf("want the halves kept, got %v and %v", img.At(10, 64), img.At(245, 64))
	}
	if len(mc.reads) != 1 {
		t.Fatalf("want the file read once, got %v", mc.reads)
	}

	// The next request is served from the cache, and one with the tag with
	// nothing at all.
	r = thumbnailOf(h, w, "path=/Photos/chart.png", nil)
	if r.Code != http.StatusOK || len(mc.reads) != 1 {
		t.Errorf("again: want %d from the cache, got %d after the reads %v", http.StatusOK, r.Code, mc.reads)
	}
	r = thumbnailOf(h, w, "path=/Photos/chart.png", map[string]string{"If-None-Match": etag})
	if r.Code != http.StatusNotModified || r.Body.Len() != 0 {
		t.Errorf("with the tag: want %d and no body, got %d and %d bytes", http.StatusNotModified, r.Code, r.Body.Len())
	}

	// The file written anew is another file, with another tag.
	mc.object = imageClient(files, modified.Add(time.Minute)).object
	r = thumbnailOf(h, w, "path=/Photos/chart.png", map[string]string{"If-None-Match": etag})
	if r.Code != http.StatusOK || len(mc.reads) != 2 {
		t.Errorf("changed: want %d after a read, got %d after the reads %v", http.StatusOK, r.Code, mc.reads)
	}
	if got := r.Header().Get("ETag"); got == etag {
		t.Error("changed: want another tag, got the same")
	}
}

// TestThumbnailOfAPhotoIsUpright verifies that a JPEG comes back as a JPEG,
// turned the way its EXIF orientation says.
func TestThumbnailOfAPhotoIsUpright(t *testing.T) {
	// The camera was turned, so the image is on its side: the top of the
	// scene, red, is on the right, and the thumbnail stands it up.
	files := map[string][]byte{"photo.jpg": encodeJPEG(t, halves(400, 200, true), 6)}
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: imageClient(files, time.Now())}, "alice", "hunter2")
	if err := h.SetThumbnailDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	r := thumbnailOf(h, w, "path=/photo.jpg", nil)
	if r.Code != http.StatusOK {
		t.Fatalf("want %d, got %d (%s)", http.StatusOK, r.Code, r.Body.String())
	}
	if got := r.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("want a JPEG of a JPEG, got %q", got)
	}
	img := decodeThumbnail(t, r, 128, 256)
	if !isBlue(img.At(10, 128)) || !isRed(img.At(118, 128)) {
		t.Errorf("want the image turned clockwise, got %v on the left and %v on the right", img.At(10, 128), img.At(118, 128))
	}
}

// TestJPEGOrientation verifies the EXIF reading, in either byte order, and
// that a JPEG without any says upright.
func TestJPEGOrientation(t *testing.T) {
	plain := encodeJPEG(t, halves(8, 8, false), 0)
	plain = append(plain[:2], plain[2+4+len("Exif\x00\x00")+22:]...)
	if got := jpegOrientation(plain); got != 1 {
		t.Errorf("no EXIF: want 1, got %d", got)
	}
	for o := 1; o <= 8; o++ {
		if got := jpegOrientation(encodeJPEG(t, halves(8, 8, false), o)); got != o {
			t.Errorf("big-endian %d: got %d", o, got)
		}
	}
	little := []byte("II*\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x08\x00\x00\x00\x00\x00\x00\x00")
	if got := tiffOrientation(little); got != 8 {
		t.Errorf("little-endian: want 8, got %d", got)
	}
	if got := jpegOrientation([]byte("not a jpeg")); got != 1 {
		t.Errorf("not a JPEG: want 1, got %d", got)
	}
}

// TestThumbnailRefusals verifies what gets no thumbnail, and that nothing is
// fetched for a file that cannot have one, or had none the last time.
func TestThumbnailRefusals(t *testing.T) {
	files := map[string][]byte{
		"notes.txt":  []byte("text"),
		"broken.png": []byte("not a PNG at all"),
		"huge.png":   {},
	}
	mc := imageClient(files, time.Now())
	object := mc.object
	mc.object = func(p string) (client.ObjectInfo, error) {
		oi, err := object(p)
		if p == "huge.png" {
			oi.Size = thumbnailMaxSize + 1
		}
		return oi, err
	}
	h, w := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")
	if err := h.SetThumbnailDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name  string
		query string
		want  int
	}{
		{"a text file", "path=/notes.txt", http.StatusNotFound},
		{"a folder", "path=/Photos", http.StatusNotFound},
		{"a folder named like an image", "path=/Shots.png", http.StatusBadRequest},
		{"the root", "path=/", http.StatusBadRequest},
		{"not there", "path=/gone.png", http.StatusNotFound},
		{"too large", "path=/huge.png", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if r := thumbnailOf(h, w, tt.query, nil); r.Code != tt.want {
				t.Errorf("want %d, got %d (%s)", tt.want, r.Code, r.Body.String())
			}
		})
	}
	if len(mc.reads) != 0 {
		t.Errorf("want nothing read, got %v", mc.reads)
	}

	// A file that does not decode is read once, then remembered as having
	// no thumbnail.
	for i := 0; i < 2; i++ {
		if r := thumbnailOf(h, w, "path=/broken.png", nil); r.Code != http.StatusNotFound {
			t.Errorf("broken, try %d: want %d, got %d", i, http.StatusNotFound, r.Code)
		}
	}
	if len(mc.reads) != 1 {
		t.Errorf("broken: want one read, got %v", mc.reads)
	}

	g, gw := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "guest", "")
	if r := thumbnailOf(g, gw, "path=/broken.png", nil); r.Code != http.StatusForbidden {
		t.Errorf("a guest on docs: want %d, got %d", http.StatusForbidden, r.Code)
	}

	// Without a directory for them, there are no thumbnails.
	bare, bw := loggedIn(t, &mockStore{}, &mockServer{client: mc}, "alice", "hunter2")
	if r := thumbnailOf(bare, bw, "path=/broken.png", nil); r.Code != http.StatusNotFound {
		t.Errorf("no directory: want %d, got %d", http.StatusNotFound, r.Code)
	}
}
