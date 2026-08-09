package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func built() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte("<!doctype html><title>Sombrero</title>")},
		"favicon.svg":             {Data: []byte("<svg/>")},
		"assets/index-abc123.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc123.css": {Data: []byte("body{}")},
	}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestHandlerServesFiles verifies that the built UI is served as it is.
func TestHandlerServesFiles(t *testing.T) {
	h := handler(built())

	for _, path := range []string{"/", "/favicon.svg", "/assets/index-abc123.js"} {
		if w := get(h, path); w.Code != http.StatusOK {
			t.Errorf("%s: want %d, got %d", path, http.StatusOK, w.Code)
		}
	}

	if w := get(h, "/"); !strings.Contains(w.Body.String(), "Sombrero") {
		t.Errorf("the root: want the index, got %q", w.Body.String())
	}

	// http.FileServer redirects the index to the canonical root.
	if w := get(h, "/index.html"); w.Code != http.StatusMovedPermanently {
		t.Errorf("/index.html: want %d, got %d", http.StatusMovedPermanently, w.Code)
	}
}

// TestHandlerFallsBackToIndex verifies that a route of the single-page app is
// answered with the index rather than a 404, so that a deep link survives a
// page reload.
func TestHandlerFallsBackToIndex(t *testing.T) {
	h := handler(built())

	for _, path := range []string{"/shares", "/workgroups/8303eeb8", "/settings"} {
		w := get(h, path)
		if w.Code != http.StatusOK {
			t.Errorf("%s: want %d, got %d", path, http.StatusOK, w.Code)
		}
		if !strings.Contains(w.Body.String(), "Sombrero") {
			t.Errorf("%s: want the index, got %q", path, w.Body.String())
		}
	}

	// A missing asset is a genuine mistake and must not be papered over
	// with the index, which would only fail later and less clearly.
	if w := get(h, "/assets/gone.js"); w.Code != http.StatusOK {
		t.Errorf("a missing asset: want the index fallback, got %d", w.Code)
	}
}

// TestHandlerCachesAssets verifies that the content-hashed assets are cached
// but the index, whose name never changes, is not.
func TestHandlerCachesAssets(t *testing.T) {
	h := handler(built())

	if got := get(h, "/assets/index-abc123.js").Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("an asset: want an immutable Cache-Control, got %q", got)
	}
	if got := get(h, "/").Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("the index: want it left uncached, got %q", got)
	}
}

// TestHandlerWithoutBuiltUI verifies that a binary built before the UI was
// ever built says so, rather than answering with a bare 404.
func TestHandlerWithoutBuiltUI(t *testing.T) {
	h := handler(fstest.MapFS{".gitkeep": {}})

	w := get(h, "/")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("want %d, got %d", http.StatusServiceUnavailable, w.Code)
	}
	if !strings.Contains(w.Body.String(), "npm --prefix web run build") {
		t.Errorf("want the build instructions, got %q", w.Body.String())
	}
}
