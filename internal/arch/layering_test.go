// Package arch_test turns the layering rules of docs/architecture.md into executable
// assertions. Reading the real import graph means a single wrong import fails a test
// instead of quietly eroding the design.
package arch_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/winger/ai-gateway/"

// allowed lists, per package, the module-internal packages it may import.
// An entry of nil means: no module-internal imports at all.
var allowed = map[string][]string{
	"internal/domain":     nil,
	"internal/ids":        nil,
	"internal/secret":     nil,
	"internal/logx":       nil,
	"internal/balancer":   nil,
	"internal/pricing":    {"internal/domain"},
	"internal/arch":       nil,
	"internal/creds":      nil,
	"internal/webui":      nil,
	"pkg/pluginapi":       nil,
	"internal/config":     {"internal/logx"},
	"pkg/providerkit":     {"pkg/pluginapi"},
	"internal/pluginhost": {"pkg/pluginapi"},
	"internal/registry":   {"internal/domain"},
	"internal/quota":      {"internal/domain"},
	// Retention is a leaf: persistence arrives through a port, so it imports nothing.
	"internal/retention":   nil,
	"internal/backup":      {"internal/domain"},
	"internal/mcpsrv":      {"internal/domain", "internal/registry"},
	"internal/hook":        {"internal/domain", "internal/ids", "internal/logx"},
	"internal/usage":       {"internal/domain", "pkg/pluginapi"},
	"internal/responses":   {"internal/domain", "internal/ids", "pkg/pluginapi"},
	"internal/sessionauth": {"internal/domain", "internal/ids", "internal/secret"},
	"internal/portal":      {"internal/domain", "internal/sessionauth"},
	"internal/store":       {"internal/config", "internal/domain", "internal/secret"},
	"internal/modelmap":    {"internal/domain", "internal/registry"},
	"internal/routing":     {"internal/balancer", "internal/domain", "internal/modelmap", "internal/registry"},
	"internal/providers": {
		"internal/providers/openaichat", "internal/providers/openairesponses",
		"internal/providers/testecho", "pkg/pluginapi",
	},
	"internal/providers/httpx":           {"pkg/pluginapi"},
	"internal/providers/openaichat":      {"internal/providers/httpx", "pkg/pluginapi", "pkg/providerkit"},
	"internal/providers/openairesponses": {"internal/providers/httpx", "pkg/pluginapi", "pkg/providerkit"},
	"internal/providers/testecho":        {"pkg/pluginapi", "pkg/providerkit"},
	"internal/runtime": {
		"internal/balancer", "internal/creds", "internal/domain", "internal/pluginhost",
		"internal/providers", "internal/registry", "pkg/pluginapi",
	},
	"internal/billing": {
		"internal/domain", "internal/ids", "internal/pricing", "internal/secret", "internal/store",
	},
	"internal/admin": {"internal/domain", "internal/ids", "internal/secret", "internal/sessionauth", "internal/store"},
	// Example provider plugins are published code: the public protocol only.
	"examples/provider-replay": {"pkg/pluginapi", "pkg/providerkit"},
	"examples/provider-codex":  {"pkg/pluginapi", "pkg/providerkit"},
	// Example provider plugins: published code, so they may only use the public protocol.
	"internal/apikey": {"internal/domain", "internal/secret"},

	// The transport layer may compose business packages, but must reach the plugin host
	// and the credential store only through ports (Prober, Sealer).
	"internal/httpapi": {
		"internal/admin", "internal/apikey", "internal/backup", "internal/billing",
		"internal/config", "internal/domain", "internal/ids", "internal/mcpsrv",
		"internal/modelmap", "internal/portal", "internal/pricing", "internal/providers",
		"internal/quota", "internal/registry", "internal/responses", "internal/retention",
		"internal/routing",
		"internal/runtime", "internal/secret", "internal/store", "internal/usage", "pkg/pluginapi",
	},
}

// forbiddenByRule names the checks that exist because a specific refactor made them true.
var forbiddenByRule = []struct {
	pkg     string
	forbid  []string
	because string
}{
	{"internal/httpapi", []string{"internal/pluginhost"},
		"the transport layer must probe plugins through the Prober port, not the host"},
	{"internal/httpapi", []string{"internal/creds"},
		"credentials must be sealed through the Sealer port, never by httpapi itself"},
	{"internal/domain", []string{"internal/"},
		"the domain layer is the bottom of the graph"},
	{"internal/store", []string{"internal/httpapi", "internal/routing", "internal/billing", "internal/mcpsrv"},
		"persistence must not know about transport, routing or billing"},
	{"internal/routing", []string{"internal/store", "internal/httpapi"},
		"routing plans against the in-memory snapshot only"},
	{"pkg/pluginapi", []string{"internal/"},
		"the plugin protocol is published to plugin authors and cannot depend on the gateway"},
}

type packageInfo struct {
	path    string
	imports []string
}

func loadGraph(t *testing.T) []packageInfo {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go tool is not on PATH: skipping the layering check")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	format := "{{.ImportPath}}|{{join .Imports \" \"}}"
	cmd := exec.Command("go", "list", "-f", format, "./...")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		t.Skipf("go list is unavailable here: %v", err)
	}
	graph := []packageInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		info := packageInfo{path: strings.TrimPrefix(parts[0], modulePath)}
		if len(parts) == 2 {
			for _, imported := range strings.Fields(parts[1]) {
				if strings.HasPrefix(imported, modulePath) {
					info.imports = append(info.imports, strings.TrimPrefix(imported, modulePath))
				}
			}
		}
		sort.Strings(info.imports)
		graph = append(graph, info)
	}
	if len(graph) == 0 {
		t.Skip("no packages were listed")
	}
	return graph
}

func TestLayeringAllowsOnlyDeclaredImports(t *testing.T) {
	graph := loadGraph(t)
	for _, pkg := range graph {
		if strings.HasPrefix(pkg.path, "cmd/") {
			continue // the composition root may import anything
		}
		permitted, declared := allowed[pkg.path]
		if !declared {
			t.Errorf("package %s is not declared in the layering table; add it with the imports it is allowed to make", pkg.path)
			continue
		}
		for _, imported := range pkg.imports {
			if !contains(permitted, imported) {
				t.Errorf("%s imports %s, which is not in its allowed set %v", pkg.path, imported, permitted)
			}
		}
	}
}

func TestDecouplingInvariants(t *testing.T) {
	graph := loadGraph(t)
	byPath := map[string][]string{}
	for _, pkg := range graph {
		byPath[pkg.path] = pkg.imports
	}

	for _, rule := range forbiddenByRule {
		imports, ok := byPath[rule.pkg]
		if !ok {
			t.Fatalf("rule references an unknown package %s", rule.pkg)
		}
		for _, imported := range imports {
			for _, banned := range rule.forbid {
				if imported == banned || (strings.HasSuffix(banned, "/") && strings.HasPrefix(imported, banned)) {
					t.Errorf("%s must not import %s: %s", rule.pkg, imported, rule.because)
				}
			}
		}
	}

	// Nothing may depend on the composition root.
	for _, pkg := range graph {
		for _, imported := range pkg.imports {
			if strings.HasPrefix(imported, "cmd/") {
				t.Errorf("%s imports the command package %s", pkg.path, imported)
			}
		}
	}

	// Only the composition root may know both the store and the transport layer.
	for _, pkg := range graph {
		if strings.HasPrefix(pkg.path, "cmd/") {
			continue
		}
		if contains(pkg.imports, "internal/store") && contains(pkg.imports, "internal/httpapi") {
			t.Errorf("%s imports both the store and httpapi; only cmd/aigw may wire those together", pkg.path)
		}
	}
}

func TestPackageTreeIsComplete(t *testing.T) {
	graph := loadGraph(t)
	seen := map[string]bool{}
	for _, pkg := range graph {
		seen[pkg.path] = true
	}
	for _, required := range []string{
		"internal/domain", "internal/store", "internal/routing", "internal/httpapi",
		"internal/billing", "internal/backup", "internal/webui", "pkg/pluginapi", "cmd/aigw",
	} {
		if !seen[required] {
			t.Errorf("expected package %s to exist", required)
		}
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
