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
	verbose := flag.Bool("v", false, "print every file, not just the summary")
	flag.Parse()

	res, err := minify.Run(minify.Options{SourceDir: *src, OutputDir: *out, OverlayPath: *overlay})
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
			fmt.Printf("  %-34s %7d -> %7d (%s)\n", f.Path, f.SourceBytes, f.OutputBytes, change)
		}
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "minifyui: warning: %s\n", w)
	}

	// One line, in the shape `make build` prints for `make build-src` to answer: the log
	// has to say which of the two shapes this binary carries.
	fmt.Printf("ui: minified %d files %d -> %d bytes (-%d%%) in %s\n",
		len(res.Files), res.SourceBytes, res.OutputBytes, res.Percent(), res.Elapsed.Round(1e6))
	if *overlay != "" {
		fmt.Printf("ui: overlay -> %s\n", *overlay)
	}
}
