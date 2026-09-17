// Command minifyui writes the compressed mirror of the console assets that a release
// binary embeds, plus the `go build -overlay` file that points the compiler at it.
//
//	make ui-dist          # wraps this command
//	minifyui -src internal/webui/static -out .cache/ui-dist/static -overlay .cache/ui-dist/overlay.json
//
// It is a separate command rather than a subcommand of cmd/aigw on purpose: esbuild's Go
// API would add roughly 10 MB to the server binary, and the server has no reason to carry
// a minifier. See docs/design/m50-frontend-minify.md.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/winger/ai-gateway/internal/webui/minify"
)

func main() {
	src := flag.String("src", "internal/webui/static", "console source assets (the truth)")
	out := flag.String("out", ".cache/ui-dist/static", "where the compressed mirror is written")
	overlay := flag.String("overlay", ".cache/ui-dist/overlay.json", "go build -overlay file (empty to skip)")
	gz := flag.Bool("gzip", true, "also write .gz sidecars for the assets worth compressing")
	verbose := flag.Bool("v", false, "print every file, not just the summary")
	flag.Parse()

	res, err := minify.Run(minify.Options{SourceDir: *src, OutputDir: *out, OverlayPath: *overlay, Gzip: *gz})
	if err != nil {
		fmt.Fprintf(os.Stderr, "minifyui: %v\n", err)
		os.Exit(1)
	}

	if *verbose {
		for _, f := range res.Files {
			change := "copied"
			if f.Changed {
				change = fmt.Sprintf("-%d%%", res.PercentOf(f))
			}
			gzipped := ""
			if f.GzipBytes > 0 {
				gzipped = fmt.Sprintf("  gz %7d", f.GzipBytes)
			}
			fmt.Printf("  %-34s %7d -> %7d (%s)%s\n", f.Path, f.SourceBytes, f.OutputBytes, change, gzipped)
		}
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "minifyui: warning: %s\n", w)
	}

	// One line, in the shape `make build` prints for `make build-src` to answer: the log
	// has to say which of the two shapes this binary carries.
	fmt.Printf("ui: minified %d files %d -> %d bytes (-%d%%)%s in %s\n",
		len(res.Files), res.SourceBytes, res.OutputBytes, res.Percent(), gzipSummary(res, *gz), res.Elapsed.Round(1e6))
	if *overlay != "" {
		fmt.Printf("ui: overlay -> %s (%d entries)\n", *overlay, res.OverlayEntries())
	}
}

// gzipSummary is the second half of the summary line: the mirror size answers "what is
// embedded", this answers "what a client actually downloads", and conflating the two is how
// a compression change looks like it did nothing (the embedded bytes go up).
//
// "Off" and "on but nothing qualified" are different facts and are reported differently: a
// reader who asked for gzip and sees no sidecar has to be able to tell the flag from the rule.
func gzipSummary(res minify.Result, requested bool) string {
	if !requested {
		return "; gzip: disabled"
	}
	if res.GzipFiles == 0 {
		return "; gzip: on, but no asset cleared the floor"
	}
	return fmt.Sprintf("; gzip %d files %d -> %d bytes (-%d%%)",
		res.GzipFiles, res.GzipRaw, res.GzipBytes, res.PercentGzip())
}
