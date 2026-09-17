package webui

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Transfer compression.
//
// A release build ships a gzip sidecar next to every asset worth compressing
// (cmd/minifyui writes them, `go build -overlay` embeds them — see
// docs/design/m55-console-transfer-compression.md). The server's whole job is to hand out
// the compressed twin when, and only when, the client said it would take it: the rule for
// what is worth compressing lives in the build, so this file never repeats it and the two
// cannot drift apart.
//
// Two things here are easy to get subtly wrong and are therefore shared by every path that
// serves a file, rather than written twice:
//
//   - Vary: Accept-Encoding. A response that can be answered two ways must say so, in both
//     branches. Getting this wrong does not break the client that asked for gzip; it
//     breaks the next client through a shared cache, which is much harder to diagnose.
//   - Content-Type. http.FileServer sniffs a type only when the header is unset, and the
//     file it is serving here ends in ".gz". The client is receiving JavaScript.

// negotiate prepares the response headers for path and returns the asset to serve: the
// sidecar when the deployment carries one and the client accepts gzip, and path itself
// otherwise.
func negotiate(w http.ResponseWriter, r *http.Request, root fs.FS, asset string) (serve string, compressed bool) {
	if !hasSidecar(root, asset) {
		// A source build carries no sidecars, and its responses must stay exactly what
		// they were before this feature existed — Vary included.
		return asset, false
	}
	w.Header().Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r) {
		return asset, false
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Type", contentTypeFor(asset))
	return asset + ".gz", true
}

// hasSidecar reports whether the deployment carries a compressed twin of asset.
func hasSidecar(root fs.FS, asset string) bool {
	_, ok := assetSize(root, asset+".gz")
	return ok
}

// assetSize reports the size of an asset in the tree.
func assetSize(root fs.FS, asset string) (int64, bool) {
	info, err := fs.Stat(root, asset)
	if err != nil || info.IsDir() {
		return 0, false
	}
	return info.Size(), true
}

// acceptsGzip reports whether the request's Accept-Encoding allows gzip.
//
// An explicit "gzip" wins, at whatever quality it carries ("gzip;q=0" is a refusal). With
// no explicit mention, a wildcard stands in for every coding the client did not name
// (RFC 9110 §12.5.3), so "br, *;q=1" accepts gzip while "br" alone does not.
func acceptsGzip(r *http.Request) bool {
	var explicit, wildcard bool
	var explicitQ, wildcardQ float64
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		for _, param := range strings.Split(params, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				q = parsed
			}
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "gzip", "x-gzip":
			explicit, explicitQ = true, q
		case "*":
			wildcard, wildcardQ = true, q
		}
	}
	if explicit {
		return explicitQ > 0
	}
	return wildcard && wildcardQ > 0
}

// contentTypeFor names the type of the asset the client is receiving, not of the file on
// disk: the sidecar is gzip, the payload is JavaScript (or CSS, or HTML).
func contentTypeFor(asset string) string {
	if ct := mime.TypeByExtension(path.Ext(asset)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// withPath retargets a request at the sidecar. The URL is cloned rather than mutated:
// the handler below still needs the original path for cache headers and logging.
func withPath(r *http.Request, asset string) *http.Request {
	clone := r.Clone(r.Context())
	clone.URL.Path = "/" + asset
	return clone
}
