// Package webui serves the embedded administration console. The source has no build
// step: the assets are plain ES modules that a browser runs directly, and the tests
// read them as they are. A release build (`make build`) minifies a copy of that tree and
// pre-compresses it with gzip, then compiles this package with `-overlay`, so the binary
// carries the stripped copy plus its ".gz" twins while the working tree keeps the readable
// source. Which of the two a response gets is decided per request in encoding.go, from the
// client's own Accept-Encoding. See docs/design/m50-frontend-minify.md and
// docs/design/m55-console-transfer-compression.md.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strconv"
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
	return handler(root)
}

// handler is Handler over an arbitrary asset tree. The split exists for the tests: a
// `go test` run has no -overlay, so the embedded tree never contains a sidecar and the
// compressed branch would otherwise be unreachable — and an untestable branch in the
// response path is one nobody can trust (see docs/design/m50-frontend-minify.md §9).
func handler(root fs.FS) http.Handler {
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
		// Cache headers describe the asset, so they are set from the asset's own path and
		// not from the sidecar the request may be answered with.
		setCacheHeaders(w, path)
		if target, compressed := negotiate(w, r, root, path); compressed {
			// http.FileServer refuses to set Content-Length on an encoded body — it cannot
			// know the size of bytes it did not encode — and without one every asset would
			// go out chunked, where before this feature they all carried a length. The
			// sidecar's size is exactly what will follow, so it is stated here. Ranges are
			// left alone: a partial response has its own length.
			if size, ok := assetSize(root, target); ok && r.Header.Get("Range") == "" {
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			}
			// FileServer writes the body and the range/HEAD handling: all of it is the
			// same as serving any other file, and none of it is worth reimplementing.
			files.ServeHTTP(w, withPath(r, target))
			return
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	// The shell is negotiated like any other asset: it is small today, so a release build
	// leaves it uncompressed, but the branch is the same one the assets take and a shell
	// that grew past the threshold would be served compressed without a second code path.
	target, _ := negotiate(w, r, root, "index.html")
	data, err := fs.ReadFile(root, target)
	if err != nil {
		http.Error(w, "console assets are unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Stated rather than inferred by the server from a single Write: the shell is the one
	// asset written by hand, and its length must not depend on how it was encoded.
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
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

// AssetCount reports how many console assets are embedded (used by tests and /stats).
//
// Gzip sidecars are not assets: they are second encodings of the assets above them, and a
// release build carries one per compressible file. Counting them would make the "how many
// assets does this console have" answer depend on how the binary was built.
func AssetCount() int { return countAssets(assets, "static") }

// countAssets is AssetCount over an arbitrary tree, so the sidecar rule above is covered
// by a test that does not need a release build to see a ".gz" file.
func countAssets(fsys fs.FS, root string) int {
	count := 0
	_ = fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasSuffix(path, ".gz") {
			count++
		}
		return nil
	})
	return count
}
