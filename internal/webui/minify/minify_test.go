package minify

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file pins the properties the shipped console depends on. The size win is the point
// of the package, but these are the facts that keep a compressed mirror *usable*: same file
// set, same exported names, same import graph, same CSS selectors. A silent break in any of
// them ships a console that is missing a page or a style, and nothing else in the repository
// would notice — the Go tests and the harness read the readable source, not the mirror.

// mirror runs the package over the real console assets into t.TempDir and returns the
// result plus both directories.
func mirror(t *testing.T) (Result, string, string) {
	t.Helper()
	sourceDir := filepath.Join("..", "static")
	dir := t.TempDir()
	outDir := filepath.Join(dir, "static")
	overlayPath := filepath.Join(dir, "overlay.json")

	// Gzip on, because that is what a release build produces: the contract tests below must
	// describe the mirror that actually gets embedded, sidecars included.
	res, err := Run(Options{SourceDir: sourceDir, OutputDir: outDir, OverlayPath: overlayPath, Gzip: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, sourceDir, dir
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

// TestMirrorCoversEverySourceFile is the first contract: the mirror is a 1:1 image, plus
// exactly the gzip sidecars it reports. A file the walk forgot would 404 in the console, a
// file that grew would mean esbuild was handed something it should not have been, and a
// sidecar nobody accounted for would be a second encoding of an asset with no owner.
func TestMirrorCoversEverySourceFile(t *testing.T) {
	res, sourceDir, dir := mirror(t)
	outDir := filepath.Join(dir, "static")

	want := append(listFiles(t, sourceDir), res.Sidecars...)
	sort.Strings(want)
	got := listFiles(t, outDir)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Fatalf("mirror file set differs\n source+sidecars: %v\n mirror: %v", want, got)
	}
	if len(res.Files) != len(listFiles(t, sourceDir)) {
		t.Fatalf("Run reported %d files, the tree has %d", len(res.Files), len(listFiles(t, sourceDir)))
	}
	if res.GzipFiles == 0 {
		t.Fatal("no sidecar was written; the gzip half of the mirror is not being exercised")
	}

	for _, f := range res.Files {
		source, err := os.ReadFile(filepath.Join(sourceDir, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		ext := strings.ToLower(filepath.Ext(f.Path))
		if ext != ".js" && ext != ".css" {
			// Everything else is copied verbatim, by design: index.html is the shell whose
			// literals embed_test asserts on, and favicon.svg is an image.
			if string(source) != string(output) {
				t.Errorf("%s: non-asset file was rewritten", f.Path)
			}
			if f.Changed {
				t.Errorf("%s: reported as changed, but it is copied", f.Path)
			}
			continue
		}
		if !f.Changed {
			t.Errorf("%s: %s was not transformed", f.Path, ext)
		}
		if len(output) == 0 {
			t.Errorf("%s: the mirror is empty", f.Path)
		}
		if len(output) >= len(source) {
			t.Errorf("%s: %d -> %d bytes, minification made it bigger", f.Path, len(source), len(output))
		}
	}
}

var (
	exportFuncRe = regexp.MustCompile(`export\s+(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	exportDeclRe = regexp.MustCompile(`export\s+(?:const|let|var|class)\s+([A-Za-z_$][\w$]*)`)
	exportListRe = regexp.MustCompile(`export\s*\{([^}]*)\}`)
)

// exportedNames reads a module's exported identifiers. It understands the shapes the
// console uses (`export function f`, `export const c`) plus the list form esbuild emits
// after renaming (`export{a as f}`), which is why the same function reads both trees.
func exportedNames(source string) map[string]bool {
	names := map[string]bool{}
	for _, re := range []*regexp.Regexp{exportFuncRe, exportDeclRe} {
		for _, match := range re.FindAllStringSubmatch(source, -1) {
			names[match[1]] = true
		}
	}
	for _, match := range exportListRe.FindAllStringSubmatch(source, -1) {
		for _, part := range strings.Split(match[1], ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			fields := strings.Fields(part)
			if len(fields) == 3 && fields[1] == "as" {
				names[strings.TrimSpace(fields[2])] = true
			} else {
				names[fields[0]] = true
			}
		}
	}
	return names
}

// TestMirrorKeepsExportedNames is the contract the pages, the shell and the browser harness
// all depend on: they import each other by name, and internal/webui/embed_test.go asserts on
// those names too. esbuild renames module-local bindings and re-exports under the original
// name; this test is what makes that a requirement rather than an observation.
func TestMirrorKeepsExportedNames(t *testing.T) {
	_, sourceDir, dir := mirror(t)
	outDir := filepath.Join(dir, "static")

	checked := 0
	for _, rel := range listFiles(t, sourceDir) {
		if !strings.HasSuffix(rel, ".js") {
			continue
		}
		source := readFile(t, filepath.Join(sourceDir, filepath.FromSlash(rel)))
		want := exportedNames(source)
		if len(want) == 0 {
			continue
		}
		got := exportedNames(readFile(t, filepath.Join(outDir, filepath.FromSlash(rel))))
		for name := range want {
			if !got[name] {
				t.Errorf("%s: the mirror no longer exports %q (importers would get undefined)", rel, name)
			}
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d modules were checked; this test is not reading the tree it thinks", checked)
	}
}

var (
	staticImportRe  = regexp.MustCompile(`(?:^|[^\w$.])import\s*(?:[\w${},*\s]*\s*from\s*)?['"]([^'"]+)['"]`)
	dynamicImportRe = regexp.MustCompile(`import\(\s*['"]([^'"]+)['"]\s*\)`)
)

// importSpecifiers returns the module specifiers a file imports, in a stable order.
func importSpecifiers(source string) []string {
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{staticImportRe, dynamicImportRe} {
		for _, match := range re.FindAllStringSubmatch(source, -1) {
			seen[match[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for spec := range seen {
		out = append(out, spec)
	}
	sort.Strings(out)
	return out
}

// TestMirrorKeepsTheImportGraph is the second half of "no page went missing": the mirror
// must import exactly what the source imported, and every relative specifier must resolve
// inside the mirror. The console loads pages lazily through `import(route.module)`, so a
// module that is present but unreachable, or missing while the shell still asks for it,
// only shows up when someone opens that page in production.
func TestMirrorKeepsTheImportGraph(t *testing.T) {
	_, sourceDir, dir := mirror(t)
	outDir := filepath.Join(dir, "static")

	relative := 0
	for _, rel := range listFiles(t, sourceDir) {
		if !strings.HasSuffix(rel, ".js") {
			continue
		}
		source := readFile(t, filepath.Join(sourceDir, filepath.FromSlash(rel)))
		mirrored := readFile(t, filepath.Join(outDir, filepath.FromSlash(rel)))
		want := importSpecifiers(source)
		got := importSpecifiers(mirrored)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("%s: import specifiers changed\n source: %v\n mirror: %v", rel, want, got)
		}
		for _, spec := range got {
			if !strings.HasPrefix(spec, ".") {
				continue
			}
			relative++
			target := filepath.Join(filepath.Dir(filepath.Join(outDir, filepath.FromSlash(rel))), spec)
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s imports %q, which the mirror does not have: %v", rel, spec, err)
			}
		}
	}
	if relative < 20 {
		t.Fatalf("only %d relative imports were resolved; the extraction is wrong", relative)
	}
	// The router is the one place that decides which pages exist; if it ever stopped
	// matching the files, the console would render "页面加载失败" for a route it advertises.
	router := readFile(t, filepath.Join(outDir, "js", "router.js"))
	for _, spec := range []string{"./pages/dashboard.js", "./pages/keys.js", "./pages/chat.js"} {
		if !strings.Contains(router, spec) {
			t.Errorf("the mirrored router no longer references %s", spec)
		}
	}
	// And the lazy loading itself has to survive as a dynamic import: turning it into a
	// static one would be a behaviour change nobody asked for. The specifier is the
	// variable `route.module`, so this checks the call shape, not a literal path.
	if !regexp.MustCompile(`\bimport\s*\(`).MatchString(router) {
		t.Error("the mirrored router lost its dynamic import; pages would have to be bundled")
	}
}

// TestMirrorKeepsCSSSelectors pins the stylesheet contract. Selectors are compared with
// whitespace normalized so `a > b` and `a>b` count as the same rule, which is exactly the
// rewrite minification is allowed to make — and nothing else.
func TestMirrorKeepsCSSSelectors(t *testing.T) {
	_, sourceDir, dir := mirror(t)
	source := readFile(t, filepath.Join(sourceDir, "app.css"))
	mirrored := readFile(t, filepath.Join(dir, "static", "app.css"))

	want, got := selectorsOf(source), selectorsOf(mirrored)
	for sel := range want {
		if !got[sel] {
			t.Errorf("app.css: the mirror lost the selector %q", sel)
		}
	}
	for sel := range got {
		if !want[sel] {
			t.Errorf("app.css: the mirror invented the selector %q", sel)
		}
	}
	// The console's theming is entirely custom properties; a renamed one would leave every
	// control unstyled rather than fail loudly.
	for _, prop := range []string{"--bg", "--panel", "--panel-2", "--line", "--fg", "--muted", "--accent", "--ok", "--warn", "--danger", "--radius"} {
		if !strings.Contains(mirrored, prop+":") {
			t.Errorf("app.css: the mirror lost the custom property %s", prop)
		}
	}
	if n := strings.Count(source, "@media"); strings.Count(mirrored, "@media") != n {
		t.Errorf("app.css: %d @media blocks in the source, %d in the mirror",
			n, strings.Count(mirrored, "@media"))
	}
}

var (
	cssCommentRe  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	combinatorGap = strings.NewReplacer(" > ", ">", " + ", "+", " ~ ", "~")
)

// normalizeSelector collapses the two rewrites minification is allowed to make — runs of
// whitespace, and the spaces around a combinator — so that `a > b` and `a>b` compare equal
// while `a b` (a descendant) stays different from `a>b`.
func normalizeSelector(sel string) string {
	return combinatorGap.Replace(strings.Join(strings.Fields(sel), " "))
}

func selectorsOf(css string) map[string]bool {
	css = cssCommentRe.ReplaceAllString(css, "")
	out := map[string]bool{}
	for _, match := range regexp.MustCompile(`([^{}]+)\{`).FindAllStringSubmatch(css, -1) {
		block := strings.TrimSpace(match[1])
		if strings.HasPrefix(block, "@") || block == "" {
			continue
		}
		for _, part := range strings.Split(block, ",") {
			out[normalizeSelector(part)] = true
		}
	}
	return out
}

// TestOverlayPointsAtEveryChangedFile keeps the build wiring honest: an entry that is
// missing means `go build` would embed the readable source for that file, and an empty
// overlay (nothing changed) means the tool did nothing while reporting success.
func TestOverlayPointsAtEveryChangedFile(t *testing.T) {
	res, sourceDir, dir := mirror(t)

	blob, err := os.ReadFile(filepath.Join(dir, "overlay.json"))
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Replace map[string]string `json:"Replace"`
	}
	if err := json.Unmarshal(blob, &overlay); err != nil {
		t.Fatalf("overlay is not the JSON go build -overlay expects: %v", err)
	}
	if len(overlay.Replace) == 0 {
		t.Fatal("the overlay is empty; every build would embed the readable source")
	}

	wantAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, f := range res.Files {
		if !f.Changed {
			if _, listed := overlay.Replace[filepath.Join(wantAbs, filepath.FromSlash(f.Path))]; listed {
				t.Errorf("%s is copied verbatim but listed in the overlay", f.Path)
			}
			continue
		}
		changed++
		target, listed := overlay.Replace[filepath.Join(wantAbs, filepath.FromSlash(f.Path))]
		if !listed {
			t.Errorf("%s was transformed but is missing from the overlay", f.Path)
			continue
		}
		if _, err := os.Stat(target); err != nil {
			t.Errorf("%s points at %s, which does not exist: %v", f.Path, target, err)
		}
	}
	for _, rel := range res.Sidecars {
		// The key is a source path that does not exist on disk, which is the whole trick:
		// it is how a directory embed pattern learns about a file the source tree never had.
		key := filepath.Join(wantAbs, filepath.FromSlash(rel))
		target, listed := overlay.Replace[key]
		if !listed {
			t.Errorf("sidecar %s is missing from the overlay; the compiler would not embed it", rel)
			continue
		}
		if _, err := os.Stat(target); err != nil {
			t.Errorf("sidecar %s points at %s, which does not exist: %v", rel, target, err)
		}
		if _, err := os.Stat(key); err == nil {
			t.Errorf("sidecar key %s exists in the source tree; the mirror must not write there", key)
		}
	}
	if changed+len(res.Sidecars) != len(overlay.Replace) {
		t.Errorf("the overlay lists %d entries, %d files were transformed and %d sidecars written",
			len(overlay.Replace), changed, len(res.Sidecars))
	}
	// The strings a release must keep are the ones the server contract is written in.
	keys := readFile(t, filepath.Join(dir, "static", "js", "pages", "keys.js"))
	if !strings.Contains(keys, "record_output_text") {
		t.Error("the mirrored keys page lost the recording mode the server accepts")
	}
}

// TestMirrorIsDeterministicAndReplacesTheOldTree covers the two ways a build directory can
// go wrong: a mirror that differs between runs (an unreproducible binary) and one that
// keeps files from a previous run (a page that no longer exists still being served).
func TestMirrorIsDeterministicAndReplacesTheOldTree(t *testing.T) {
	sourceDir := filepath.Join("..", "static")
	dir := t.TempDir()
	outDir := filepath.Join(dir, "static")

	opts := Options{SourceDir: sourceDir, OutputDir: outDir, OverlayPath: filepath.Join(dir, "overlay.json"), Gzip: true}
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	first := listFiles(t, outDir)
	firstJS := readFile(t, filepath.Join(outDir, "js", "app.js"))
	firstGz := readFile(t, filepath.Join(outDir, "js", "app.js.gz"))

	// A stale file from an earlier layout must not survive the next run.
	stale := filepath.Join(outDir, "js", "pages", "removed-page.js")
	if err := os.WriteFile(stale, []byte("export function gone() {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a file from the previous mirror survived the rebuild")
	}
	if got := listFiles(t, outDir); strings.Join(got, ",") != strings.Join(first, ",") {
		t.Errorf("the second run produced a different tree\n first: %v\n second: %v", first, got)
	}
	if got := readFile(t, filepath.Join(outDir, "js", "app.js")); got != firstJS {
		t.Error("the same source produced different bytes on the second run")
	}
	// A gzip stream may carry a modification time, and a build that stamps one into every
	// asset is not reproducible. The writer leaves it zeroed; this is what keeps it that way.
	if got := readFile(t, filepath.Join(outDir, "js", "app.js.gz")); got != firstGz {
		t.Error("the same source produced different gzip bytes on the second run")
	}
	for _, leftover := range []string{outDir + ".tmp", outDir + ".old"} {
		if matches, _ := filepath.Glob(leftover + "*"); len(matches) > 0 {
			t.Errorf("run left temporary directories behind: %v", matches)
		}
	}
}

// TestBrokenAssetFailsTheRun is the failure mode that matters most: a syntax error must stop
// the build with the offending file named, and must not leave an output directory that a
// later `go build -overlay` would happily embed.
func TestBrokenAssetFailsTheRun(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "source")
	if err := os.MkdirAll(filepath.Join(sourceDir, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "js", "ok.js"), []byte("export const ok = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "js", "broken.js"), []byte("export function (\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "static")
	_, err := Run(Options{SourceDir: sourceDir, OutputDir: outDir, OverlayPath: filepath.Join(dir, "overlay.json")})
	if err == nil {
		t.Fatal("a broken asset must fail the run; falling back to the source would ship readable code")
	}
	if !strings.Contains(err.Error(), "broken.js") {
		t.Errorf("the error must name the file to fix, got: %v", err)
	}
	if _, statErr := os.Stat(outDir); statErr == nil {
		t.Error("the failed run left an output directory behind")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "overlay.json")); statErr == nil {
		t.Error("the failed run wrote an overlay")
	}
}

// TestGzipSidecarsAreTheMirrorBytesCompressed is the property the server relies on: it
// hands the sidecar to a client that asked for gzip, so a sidecar that is not exactly the
// mirrored asset compressed would serve different JavaScript depending on Accept-Encoding.
func TestGzipSidecarsAreTheMirrorBytesCompressed(t *testing.T) {
	res, _, dir := mirror(t)
	outDir := filepath.Join(dir, "static")

	if len(res.Sidecars) == 0 {
		t.Fatal("no sidecar was written")
	}
	for _, rel := range res.Sidecars {
		packed, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read sidecar %s: %v", rel, err)
		}
		plain, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(strings.TrimSuffix(rel, ".gz"))))
		if err != nil {
			t.Fatalf("read asset for sidecar %s: %v", rel, err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(packed))
		if err != nil {
			t.Fatalf("%s is not a gzip stream: %v", rel, err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gunzip %s: %v", rel, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("%s does not decompress to its asset", rel)
		}
	}
	if res.GzipBytes >= res.GzipRaw {
		t.Errorf("sidecars total %d bytes for %d bytes of assets; compression is not earning its keep",
			res.GzipBytes, res.GzipRaw)
	}
	if res.PercentGzip() < 50 {
		t.Errorf("gzip saved only %d%%; the design's premise (about -60%%) no longer holds", res.PercentGzip())
	}
	// Sorting and counting are reported facts, so they must agree with the tree.
	if !sort.StringsAreSorted(res.Sidecars) {
		t.Error("Sidecars is not sorted; the summary line would not be reproducible")
	}
}

// TestGzipSkipsWhatIsNotWorthIt pins the two halves of "worth it": only text kinds get a
// sidecar, and only when the saving clears the floor. Both rules live in one function on
// purpose — the server never repeats them, it serves a sidecar when one exists.
func TestGzipSkipsWhatIsNotWorthIt(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "source")
	writeFile(t, sourceDir, "logo.png", strings.Repeat("\x89PNG", 400)) // already compressed: never
	writeFile(t, sourceDir, "tiny.js", "export const a = 1;\n")         // below the floor
	// Text that compresses well, which is what makes a sidecar worth a second file; a file
	// that is merely big but incompressible would be skipped by the floor, not by its size.
	writeFile(t, sourceDir, "big.js", "export const payload = \""+strings.Repeat("abcdefghij", 60)+"\";\n")
	writeFile(t, sourceDir, "shell.html", strings.Repeat("<p>console shell</p>\n", 100)) // html is eligible

	outDir := filepath.Join(dir, "static")
	res, err := Run(Options{SourceDir: sourceDir, OutputDir: outDir, OverlayPath: "", Gzip: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := map[string]bool{}
	for _, rel := range res.Sidecars {
		got[rel] = true
	}
	for _, want := range []string{"big.js.gz", "shell.html.gz"} {
		if !got[want] {
			t.Errorf("no sidecar for %s; a large text asset must be compressed", want)
		}
	}
	for _, unwanted := range []string{"logo.png.gz", "tiny.js.gz"} {
		if got[unwanted] {
			t.Errorf("%s has a sidecar; it saves too little to be worth a second file", unwanted)
		}
	}
	for _, f := range res.Files {
		if f.GzipBytes > 0 && f.GzipBytes > f.OutputBytes-gzipSavingsFloor {
			t.Errorf("%s: sidecar is %d B against %d B of asset, under the floor", f.Path, f.GzipBytes, f.OutputBytes)
		}
	}
}

// TestGzipOffReproducesTheM50Mirror keeps the switch honest: with gzip off the mirror must
// be byte for byte the one M50 shipped, so "did compression change anything else?" is a
// question a build can answer instead of a claim someone has to believe.
func TestGzipOffReproducesTheM50Mirror(t *testing.T) {
	sourceDir := filepath.Join("..", "static")
	dir := t.TempDir()

	plainDir := filepath.Join(dir, "plain")
	packedDir := filepath.Join(dir, "packed")
	if _, err := Run(Options{SourceDir: sourceDir, OutputDir: plainDir, Gzip: false}); err != nil {
		t.Fatalf("Run(gzip off): %v", err)
	}
	if _, err := Run(Options{SourceDir: sourceDir, OutputDir: packedDir, Gzip: true}); err != nil {
		t.Fatalf("Run(gzip on): %v", err)
	}

	for _, rel := range listFiles(t, plainDir) {
		if strings.HasSuffix(rel, ".gz") {
			t.Fatalf("gzip off wrote %s", rel)
		}
		if readFile(t, filepath.Join(plainDir, filepath.FromSlash(rel))) !=
			readFile(t, filepath.Join(packedDir, filepath.FromSlash(rel))) {
			t.Errorf("%s differs between the two runs; gzip changed more than the sidecars", rel)
		}
	}
	sidecars := listFiles(t, packedDir)
	for _, rel := range sidecars {
		if !strings.HasSuffix(rel, ".gz") {
			continue
		}
		if _, err := os.Stat(filepath.Join(plainDir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("gzip off produced the sidecar %s", rel)
		}
	}
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(blob)
}
