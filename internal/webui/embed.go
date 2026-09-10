// Package webui serves the embedded administration console. It has no build step:
// the assets are plain ES modules that a browser runs directly, embedded into the
// binary so the console ships with the server and needs no network access.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var assets embed.FS

// Handler serves the console. Request paths are relative to the console root,
// so mount it with http.StripPrefix("/admin/ui/", webui.Handler()).
func Handler() http.Handler {
	root, err := fs.Sub(assets, "static")
	if err != nil {
		panic("webui: embedded assets are missing: " + err.Error())
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		info, statErr := fs.Stat(root, path)
		if statErr != nil || info.IsDir() {
			// Single-page fallback: unknown paths render the shell, which then routes
			// by hash. Assets that 404 would be a bug, so keep them honest.
			if strings.Contains(path, ".") {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, root)
			return
		}
		setCacheHeaders(w, path)
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	data, err := fs.ReadFile(root, "index.html")
	if err != nil {
		http.Error(w, "console assets are unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The console is a static shell with no inline data, so a strict policy is cheap.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
	_, _ = w.Write(data)
}

// setCacheHeaders keeps the shell fresh while letting immutable assets be cached.
func setCacheHeaders(w http.ResponseWriter, path string) {
	if strings.HasSuffix(path, ".html") {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// AssetCount reports how many files are embedded (used by tests and /stats).
func AssetCount() int {
	count := 0
	_ = fs.WalkDir(assets, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count++
		}
		return nil
	})
	return count
}
