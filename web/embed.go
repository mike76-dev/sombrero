// Package web serves the Sombrero web UI. The UI is built separately with
// `npm --prefix web run build`, which writes into the `dist` directory that
// this package embeds.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the built web UI. Only a placeholder is checked in — the `all:`
// prefix is what picks it up — so that the server still builds from a fresh
// clone, where the UI has never been built.
//
//go:embed all:dist
var dist embed.FS

const notBuilt = `The Sombrero web UI was not built into this binary.

Build it with:

    npm --prefix web install
    npm --prefix web run build

and then build the server again. The API itself is unaffected and is
served under /api.
`

// Handler returns an http.Handler that serves the web UI. The handler is
// meant to be mounted at the root, below the API: it serves no secrets, so
// that the UI can load and ask for the API password itself rather than
// leaving it to the browser's basic auth prompt.
func Handler() http.Handler {
	assets, err := fs.Sub(dist, "dist")
	if err != nil {
		// The embedded directory is always present, so this cannot happen.
		panic(err)
	}
	return handler(assets)
}

// handler serves the UI out of the given filesystem.
func handler(assets fs.FS) http.Handler {
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(notBuilt))
		})
	}

	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(path.Clean(req.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		if _, err := fs.Stat(assets, name); err != nil {
			// A route within the single-page app rather than a file on
			// disk: hand it the index and let the app work out what to
			// show, so that a deep link survives a page reload.
			req = req.Clone(req.Context())
			req.URL.Path = "/"
			files.ServeHTTP(w, req)
			return
		}

		// The asset file names carry a hash of their contents, so a name
		// that resolves once always resolves to the same bytes.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		files.ServeHTTP(w, req)
	})
}
