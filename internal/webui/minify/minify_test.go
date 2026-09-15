package minify

import (
	"encoding/json"
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

	res, err := Run(Options{SourceDir: sourceDir, OutputDir: outDir, OverlayPath: overlayPath})
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

// TestMirrorCoversEverySourceFile is the first contract: the mirror is a 1:1 image. A file
// the walk forgot would 404 in the console, and a file that grew would mean esbuild was
// handed something it should not have been.
func TestMirrorCoversEverySourceFile(t *testing.T) {
	res, sourceDir, dir := mirror(t)
	outDir := filepath.Join(dir, "static")

	want := listFiles(t, sourceDir)
	got := listFiles(t, outDir)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Fatalf("mirror file set differs\n source: %v\n mirror: %v", want, got)
	}
	if len(res.Files) != len(want) {
		t.Fatalf("Run reported %d files, the tree has %d", len(res.Files), len(want))
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
	if changed != len(overlay.Replace) {
		t.Errorf("the overlay lists %d files, %d were transformed", len(overlay.Replace), changed)
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

	opts := Options{SourceDir: sourceDir, OutputDir: outDir, OverlayPath: filepath.Join(dir, "overlay.json")}
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	first := listFiles(t, outDir)
	firstJS := readFile(t, filepath.Join(outDir, "js", "app.js"))

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

func readFile(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(blob)
}
