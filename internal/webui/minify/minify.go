// Package minify produces the copy of the console assets that a release binary embeds.
//
// The source tree (internal/webui/static) stays exactly as written: plain ES modules a
// browser runs directly and the tests read as text. This package turns it into a mirror
// with the same file names, the same relative import specifiers and the same exported
// names, but stripped of comments, whitespace and local identifier names — which is what
// `make build` embeds through `go build -overlay` (see docs/design/m50-frontend-minify.md).
//
// Two properties matter more than the size win:
//
//   - Exported names survive. The pages, the console shell and the browser harness all
//     import each other by name, and internal/webui/embed_test.go asserts on those names;
//     esbuild renames only module-local bindings and re-exports under the original name.
//   - A file that fails to parse fails the build. Falling back to the source would ship
//     readable code while every log line claimed success.
package minify

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/evanw/esbuild/pkg/api"
)

// Options describes one mirror run. OverlayPath may be empty, in which case no overlay
// file is written (the mirror is still produced).
type Options struct {
	SourceDir   string // e.g. internal/webui/static
	OutputDir   string // e.g. .cache/ui-dist/static
	OverlayPath string // e.g. .cache/ui-dist/overlay.json
	// Gzip writes a "<name>.gz" sidecar next to every asset worth compressing, so the
	// server can answer a request with the compressed bytes and a Content-Encoding the
	// client asked for. It is part of the mirror rather than of the server because that
	// keeps per-request cost at zero and the compression rule in one auditable place;
	// see docs/design/m55-console-transfer-compression.md. False keeps the M50 mirror
	// byte for byte, which is what an A/B comparison wants.
	Gzip bool
}

// FileResult is one file's outcome. Changed is false for anything that is not JavaScript
// or CSS, because those are copied byte for byte and must never be rewritten.
type FileResult struct {
	Path        string // slash-separated, relative to the source root
	SourceBytes int
	OutputBytes int
	Changed     bool
	// GzipBytes is the size of this file's ".gz" sidecar, or 0 when it has none (below
	// the saving floor, not a compressible kind, or gzip was switched off).
	GzipBytes int
}

// Result summarizes a run so the CLI can print it and the tests can assert on it.
type Result struct {
	Files       []FileResult // sorted by Path, so a run is reproducible line for line
	SourceBytes int
	OutputBytes int
	// GzipFiles counts the ".gz" sidecars, and GzipRaw/GzipBytes are the sizes of the
	// files that carry one: the numbers on the wire are what the summary reports, so
	// "the mirror is smaller" and "what a client actually downloads" stay separate facts.
	GzipFiles int
	GzipRaw   int
	GzipBytes int
	// Sidecars lists the sidecar paths (slash-separated, relative to the mirror root),
	// sorted. The overlay needs them: the compiler must embed files the source tree does
	// not contain.
	Sidecars []string
	Warnings []string // esbuild warnings: reported, never fatal
	Elapsed  time.Duration
}

// Percent returns how much smaller the mirror is, rounded to a whole percent.
func (r Result) Percent() int { return percentSaved(r.SourceBytes, r.OutputBytes) }

// PercentGzip returns the saving a client sees for the files that carry a sidecar.
func (r Result) PercentGzip() int { return percentSaved(r.GzipRaw, r.GzipBytes) }

// OverlayEntries counts what the overlay file carries: every rewritten file plus every
// sidecar. It is derived rather than stored so the number printed by the CLI cannot
// disagree with what writeOverlay actually wrote.
func (r Result) OverlayEntries() int {
	n := len(r.Sidecars)
	for _, f := range r.Files {
		if f.Changed {
			n++
		}
	}
	return n
}

// PercentOf returns one file's saving, the same way.
func (r Result) PercentOf(f FileResult) int { return percentSaved(f.SourceBytes, f.OutputBytes) }

// percentSaved rounds rather than truncates: 41.07% off must read as -41%, not -42%,
// because the number is quoted in the design document and the release notes.
func percentSaved(source, output int) int {
	if source <= 0 {
		return 0
	}
	return int(math.Round(100 - 100*float64(output)/float64(source)))
}

// Transform compiles one asset. isCSS selects the stylesheet pipeline; everything
// esbuild-specific lives here so no other file has to know the option set.
func Transform(source []byte, isCSS bool) (code []byte, warnings []string, err error) {
	opts := api.TransformOptions{
		MinifyWhitespace: true,
		MinifySyntax:     true,
		LegalComments:    api.LegalCommentsNone,
		// UTF-8 rather than ASCII: the console's UI text is Chinese, and escaping it would
		// grow the embedded copy by ~9% for no benefit (the server always sends charset).
		Charset: api.CharsetUTF8,
	}
	if isCSS {
		opts.Loader = api.LoaderCSS
		// NOT MinifyIdentifiers: a class or id renamed here would still match the markup
		// the JS builds, but it is the one transform whose failure mode is a silently
		// unstyled control, and the win is a few hundred bytes.
	} else {
		opts.Loader = api.LoaderJS
		opts.Format = api.FormatESModule
		// ES2020 rather than ESNext: the console already uses `?.`/`??`, so nothing newer
		// is needed today, and pinning the target turns "someone writes ES2022 syntax"
		// into an explicit decision here instead of a browser compatibility surprise.
		opts.Target = api.ES2020
		opts.MinifyIdentifiers = true
	}

	res := api.Transform(string(source), opts)
	if len(res.Errors) > 0 {
		return nil, nil, errors.New(describeMessages(res.Errors))
	}
	for _, msg := range res.Warnings {
		warnings = append(warnings, describeMessages([]api.Message{msg}))
	}
	return res.Code, warnings, nil
}

// Run writes the mirror and the overlay it needs. The mirror is published atomically: a
// build that is interrupted (^C, a full disk) must never leave a half-written tree for the
// compiler to embed, because the result would be a console missing pages.
func Run(opts Options) (Result, error) {
	start := time.Now()
	res := Result{}

	info, err := os.Stat(opts.SourceDir)
	if err != nil {
		return res, fmt.Errorf("source assets: %w", err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("source assets: %s is not a directory", opts.SourceDir)
	}
	if opts.OutputDir == "" {
		return res, errors.New("output directory is required")
	}

	tmp := opts.OutputDir + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.RemoveAll(tmp); err != nil {
		return res, fmt.Errorf("clear %s: %w", tmp, err)
	}

	if err := writeMirror(opts.SourceDir, tmp, opts.Gzip, &res); err != nil {
		_ = os.RemoveAll(tmp)
		return res, err
	}
	if err := publish(tmp, opts.OutputDir); err != nil {
		_ = os.RemoveAll(tmp)
		return res, err
	}
	if opts.OverlayPath != "" {
		if err := writeOverlay(opts.OverlayPath, opts.SourceDir, opts.OutputDir, res.Files, res.Sidecars); err != nil {
			return res, err
		}
	}

	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path })
	sort.Strings(res.Sidecars)
	res.Elapsed = time.Since(start)
	return res, nil
}

// writeMirror walks the source tree and writes every file into dst, transforming the two
// kinds this package understands and copying the rest. The 1:1 file set is a contract the
// tests enforce: a file that silently failed to be copied would 404 in the console.
func writeMirror(srcDir, dstDir string, gzipOn bool, res *Result) error {
	return filepath.WalkDir(srcDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		output := source
		changed := false
		switch strings.ToLower(filepath.Ext(path)) {
		case ".js", ".css":
			code, warnings, err := Transform(source, strings.EqualFold(filepath.Ext(path), ".css"))
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			output, changed = code, true
			for _, w := range warnings {
				res.Warnings = append(res.Warnings, rel+": "+w)
			}
		}

		target := filepath.Join(dstDir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, output, 0o644); err != nil {
			return err
		}

		file := FileResult{
			Path:        filepath.ToSlash(rel),
			SourceBytes: len(source),
			OutputBytes: len(output),
			Changed:     changed,
		}

		if gzipOn && gzipEligible(rel) {
			packed, err := compress(output)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			// The floor is what keeps a sidecar from being a pessimisation: below it the
			// second file costs more (a request that may be made, an entry in the mirror,
			// a line in the overlay) than the bytes it saves. The server never repeats
			// this test — it serves a sidecar when one exists.
			if len(packed) <= len(output)-gzipSavingsFloor {
				if err := os.WriteFile(target+".gz", packed, 0o644); err != nil {
					return err
				}
				file.GzipBytes = len(packed)
				res.Sidecars = append(res.Sidecars, file.Path+".gz")
				res.GzipFiles++
				res.GzipRaw += len(output)
				res.GzipBytes += len(packed)
			}
		}

		res.Files = append(res.Files, file)
		res.SourceBytes += len(source)
		res.OutputBytes += len(output)
		return nil
	})
}

// gzipSavingsFloor is the smallest saving that earns a sidecar.
const gzipSavingsFloor = 256

// gzipEligible reports whether a file kind may get a sidecar. Text assets only: the
// console is JavaScript, CSS and an HTML shell plus one SVG logo, and wrapping an
// already-compressed format (or a binary one) would only spend a request to save nothing.
// Widen this list deliberately, never by default.
func gzipEligible(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".css", ".html", ".svg":
		return true
	}
	return false
}

// compress returns the gzip encoding of data. The header carries no file name and no
// modification time, which is what makes two runs byte-identical: the build must be
// reproducible, and a timestamp inside every asset would break that for no gain.
func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// publish swaps the finished mirror into place. The previous tree is moved aside first so
// the rename cannot fail on a non-empty destination, and so there is no moment where the
// build directory simply does not exist.
func publish(tmp, outputDir string) error {
	old := outputDir + ".old-" + strconv.Itoa(os.Getpid())
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("clear %s: %w", old, err)
	}
	if _, err := os.Stat(outputDir); err == nil {
		if err := os.Rename(outputDir, old); err != nil {
			return fmt.Errorf("replace %s: %w", outputDir, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(outputDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, outputDir); err != nil {
		return fmt.Errorf("publish %s: %w", outputDir, err)
	}
	_ = os.RemoveAll(old)
	return nil
}

// writeOverlay records where the mirror differs from the source, in the format
// `go build -overlay` reads. Only changed files are listed: everything else is already
// identical, and a shorter list is easier to read when a build goes wrong.
//
// Sidecars get an entry of their own whose *source* path does not exist on disk. That is
// deliberate and load-bearing: a directory embed pattern is expanded through the overlay
// filesystem, which merges overlaid entries into the directory listing, so this is what
// puts "static/js/app.js.gz" in the embedded file set. Without it the server would look
// for a sidecar that was never embedded. See docs/design/m55-console-transfer-compression.md.
func writeOverlay(path, srcDir, outDir string, files []FileResult, sidecars []string) error {
	replace := map[string]string{}
	abs := func(root, rel string) (string, error) {
		return filepath.Abs(filepath.Join(root, filepath.FromSlash(rel)))
	}
	for _, f := range files {
		if !f.Changed {
			continue
		}
		source, err := abs(srcDir, f.Path)
		if err != nil {
			return err
		}
		output, err := abs(outDir, f.Path)
		if err != nil {
			return err
		}
		replace[source] = output
	}
	for _, rel := range sidecars {
		// The key is the source path plus ".gz": the file the compiler would look for if
		// the sidecar lived in the source tree, which is exactly the lookup to redirect.
		source, err := abs(srcDir, rel)
		if err != nil {
			return err
		}
		output, err := abs(outDir, rel)
		if err != nil {
			return err
		}
		replace[source] = output
	}
	if len(replace) == 0 {
		return errors.New("no asset was transformed; refusing to write an empty overlay")
	}

	blob, err := json.Marshal(struct {
		Replace map[string]string `json:"Replace"`
	}{Replace: replace})
	if err != nil {
		return err
	}
	blob = append(blob, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// describeMessages renders esbuild's diagnostics as one line each, with the file and the
// position, because that is what a failed build has to show.
func describeMessages(msgs []api.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		where := ""
		if msg.Location != nil {
			where = fmt.Sprintf("%s:%d:%d: ", msg.Location.File, msg.Location.Line, msg.Location.Column)
		}
		parts = append(parts, where+msg.Text)
	}
	return strings.Join(parts, "; ")
}
