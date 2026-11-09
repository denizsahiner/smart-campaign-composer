package api

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// WithStatic makes the server also serve the built frontend (web/dist) from dir, so
// the browser, the API and the event streams share one origin. Empty dir: API only, as
// in development where Vite serves the frontend and proxies /api.
func (s *Server) WithStatic(dir string) *Server {
	s.staticDir = dir
	return s
}

// staticHandler serves files from dir and answers every other path with index.html, so
// client-side routes (/campaigns/7) survive a reload. Missing files under /assets/ stay
// 404: a stale bundle name must fail as a missing script, not load HTML as JavaScript.
// /api/ never reaches it except for unknown API paths, which get a JSON 404.
func staticHandler(dir string) http.Handler {
	index := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not_found", "No such endpoint.")
			return
		}
		// path.Clean on a rooted path removes any "..", so the join stays inside dir.
		name := filepath.Join(dir, filepath.FromSlash(path.Clean("/"+r.URL.Path)))
		if fi, err := os.Stat(name); err == nil && !fi.IsDir() {
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				// Vite puts a content hash in every asset name.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFile(w, r, name)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		// index.html names the current bundle, so it must never be cached.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}
