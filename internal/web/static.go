package web

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// http.FileServer types files from the OS's MIME table, and Windows usually has
// no entry for .webmanifest, so the manifest went out as text/plain and the
// browser would not offer to install the app.
func init() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// staticHandler serves the embedded PWA.
//
// It is a single-page app, so any path the build does not contain falls back to
// index.html and the client router takes over. The one exception is /api/, which
// must 404: letting an unknown API path return the app shell would turn a typo
// in a fetch into a confusing JSON parse error instead of a clear 404.
func (s *Server) staticHandler() http.Handler {
	if s.deps.Static == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A binary built without the PWA still serves the API; say so
			// rather than returning a blank page.
			WriteError(w, http.StatusNotFound, CodeNotFound, "the web app is not bundled in this build")
		})
	}

	fileServer := http.FileServer(http.FS(s.deps.Static))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := path.Clean("/" + r.URL.Path)

		if strings.HasPrefix(upath, "/api/") {
			WriteError(w, http.StatusNotFound, CodeNotFound, "no such endpoint")
			return
		}

		// Hashed build assets are safe to cache hard; the shell is not, or an
		// update would never be picked up.
		switch {
		case strings.HasPrefix(upath, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case upath == "/sw.js":
			// The service worker itself must always be revalidated, or a bad
			// worker could pin itself in place.
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}

		if exists(s.deps.Static, upath) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = upath
			fileServer.ServeHTTP(w, r2)
			return
		}

		// Unknown path: serve the shell so a deep link such as /files works on
		// a cold load.
		serveShell(w, r, s.deps.Static)
	})
}

// exists reports whether the embedded build contains a file at upath.
func exists(fsys fs.FS, upath string) bool {
	name := strings.TrimPrefix(upath, "/")
	if name == "" {
		name = "index.html"
	}
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	return true
}

// serveShell writes index.html for a client-routed path.
func serveShell(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	f, err := fsys.Open("index.html")
	if err != nil {
		WriteError(w, http.StatusNotFound, CodeNotFound, "the web app is not bundled in this build")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not read the web app")
		return
	}
	rs, ok := f.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not read the web app")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", st.ModTime(), rs)
}
