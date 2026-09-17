package webui

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The console is a release binary's biggest readable surface, and the compressed branch of
// the handler only exists when the build shipped sidecars — which `go test` never does.
// These tests therefore drive the handler over an injected tree: without that, the branch
// that decides what every client actually downloads would be untested (M50 §9's lesson,
// applied before it could bite again). See docs/design/m55-console-transfer-compression.md.

const (
	appJS     = "export const renderShell = () => 'console';\n"
	appCSS    = "--accent: #4af;\n"
	shellHTML = "<div id=\"app\"></div>\n"
)

// consoleTree is a miniature console. Every asset named in sidecars gets a ".gz" twin built
// the same way cmd/minifyui builds them, so the handler is exercised against real gzip
// bytes rather than a stub.
func consoleTree(t *testing.T, sidecars ...string) fstest.MapFS {
	t.Helper()
	files := map[string]string{
		"index.html":  shellHTML,
		"app.css":     appCSS,
		"js/app.js":   appJS,
		"favicon.svg": "<svg/>",
	}
	tree := fstest.MapFS{}
	for name, body := range files {
		tree[name] = &fstest.MapFile{Data: []byte(body)}
	}
	for _, name := range sidecars {
		body, ok := files[name]
		if !ok {
			t.Fatalf("consoleTree: no such asset %q", name)
		}
		tree[name+".gz"] = &fstest.MapFile{Data: gzipBytes(t, body)}
	}
	return tree
}

func gzipBytes(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestConsoleServesGzipOnlyWhenTheClientAsks pins the whole negotiation table. The header
// that matters most is the one that is easy to forget: a response that can be answered two
// ways must say so in *both* branches, or a shared cache will eventually hand the
// compressed bytes to a client that never asked for them.
func TestConsoleServesGzipOnlyWhenTheClientAsks(t *testing.T) {
	cases := []struct {
		name     string
		accept   string
		wantGzip bool
		wantVary bool
	}{
		{"gzip", "gzip", true, true},
		{"gzip with quality", "gzip;q=1.0, deflate", true, true},
		{"gzip among others", "deflate, gzip, br, zstd", true, true},
		{"wildcard stands in for the coding nobody named", "br, *;q=1", true, true},
		{"legacy x-gzip", "x-gzip", true, true},
		{"explicit refusal", "gzip;q=0", false, true},
		{"refusal beside accepted codings", "gzip;q=0, deflate", false, true},
		{"wildcard refusal", "*;q=0", false, true},
		{"brotli only", "br", false, true},
		{"identity", "identity", false, true},
		{"absent", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(handler(consoleTree(t, "js/app.js")))
			defer srv.Close()

			req, err := http.NewRequest("GET", srv.URL+"/js/app.js", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.accept != "" {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			// The transport would add its own Accept-Encoding and transparently decode
			// gzip, which is exactly the behaviour under test, so it is switched off.
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET /js/app.js: %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			got := resp.Header.Get("Content-Encoding")
			if tc.wantGzip {
				if got != "gzip" {
					t.Fatalf("Content-Encoding = %q, want gzip", got)
				}
				if want := len(gzipBytes(t, appJS)); resp.ContentLength != int64(want) {
					t.Errorf("Content-Length = %d, want the compressed size %d", resp.ContentLength, want)
				}
				if string(body) == appJS {
					t.Error("body is the uncompressed asset; the sidecar was not served")
				}
				if got := decompress(t, body); got != appJS {
					t.Errorf("decompressed body = %q, want %q", got, appJS)
				}
			} else {
				if got != "" {
					t.Errorf("Content-Encoding = %q, want none", got)
				}
				if string(body) != appJS {
					t.Errorf("body = %q, want the asset as written", body)
				}
			}
			if vary := resp.Header.Get("Vary"); tc.wantVary && !strings.Contains(vary, "Accept-Encoding") {
				t.Errorf("Vary = %q, want it to mention Accept-Encoding", vary)
			}
			// The payload is JavaScript in both branches; nothing may leak the fact that
			// the file on disk happens to end in ".gz".
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
				t.Errorf("Content-Type = %q, want the type of the payload", ct)
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
				t.Errorf("Cache-Control = %q, want the asset policy unchanged", cc)
			}
		})
	}
}

// TestConsoleWithoutSidecarsIsUnchanged is the "source build" case, and the reason Vary is
// conditional: a build that carries no compressed representation has nothing to vary on,
// and its responses must be byte for byte what they were before this feature existed.
func TestConsoleWithoutSidecarsIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(handler(consoleTree(t)))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/js/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatalf("GET /js/app.js: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none without a sidecar", got)
	}
	if got := resp.Header.Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want none without a sidecar", got)
	}
	if string(body) != appJS {
		t.Errorf("body = %q, want the asset as written", body)
	}
}

// TestConsoleShellAndFallbackAreNegotiated covers the two paths that do not go through
// http.FileServer. The shell carries the console's Content-Security-Policy, so a compressed
// shell that lost the policy would be a silently weaker console.
func TestConsoleShellAndFallbackAreNegotiated(t *testing.T) {
	srv := httptest.NewServer(handler(consoleTree(t, "index.html")))
	defer srv.Close()

	for _, path := range []string{"/", "/requests"} {
		t.Run(path, func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if resp.Header.Get("Content-Encoding") != "gzip" {
				t.Errorf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
			}
			if got := decompress(t, body); got != shellHTML {
				t.Errorf("decompressed shell = %q, want %q", got, shellHTML)
			}
			if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
				t.Errorf("CSP = %q, want the console policy on the compressed shell", resp.Header.Get("Content-Security-Policy"))
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache for the shell", cc)
			}
			if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				t.Errorf("Content-Type = %q, want text/html", resp.Header.Get("Content-Type"))
			}
		})
	}
}

// TestConsoleHEADAndMissesStillBehave: the compressed branch hands the response to
// http.FileServer, so HEAD, ranges and 404s must be the ones it already produced.
func TestConsoleHEADAndMissesStillBehave(t *testing.T) {
	srv := httptest.NewServer(handler(consoleTree(t, "js/app.js", "app.css")))
	defer srv.Close()

	req, _ := http.NewRequest("HEAD", srv.URL+"/js/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatalf("HEAD /js/app.js: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD returned %d body bytes, want none", len(body))
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("HEAD Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	if want := int64(len(gzipBytes(t, appJS))); resp.ContentLength != want {
		t.Errorf("HEAD Content-Length = %d, want %d", resp.ContentLength, want)
	}

	missing, err := http.Get(srv.URL + "/js/missing.js")
	if err != nil {
		t.Fatalf("GET /js/missing.js: %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("GET /js/missing.js status = %d, want 404", missing.StatusCode)
	}
}

// TestAssetCountIgnoresSidecars: AssetCount answers "how many assets does this console
// have", which must not change with how the binary was built.
func TestAssetCountIgnoresSidecars(t *testing.T) {
	tree := consoleTree(t, "js/app.js", "app.css", "index.html")
	if got, want := countAssets(tree, "."), 4; got != want {
		t.Errorf("countAssets = %d, want %d (the sidecars are not assets)", got, want)
	}
	if AssetCount() < 8 {
		t.Errorf("AssetCount() = %d, want the embedded console", AssetCount())
	}
}

func decompress(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(plain)
}
