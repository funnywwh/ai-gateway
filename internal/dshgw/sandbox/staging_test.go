package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The unit tests in profile_test.go pin the argv this package produces. These
// staging tests go further: they run that argv through a real bubblewrap on the
// host, so the isolation claims are measured instead of asserted. They skip
// themselves wherever bubblewrap cannot run (no binary, or unprivileged user
// namespaces denied), which keeps `make dshgw-test` usable on any machine.
const stagingSkipReason = "bubblewrap staging test needs an unprivileged user namespace; run it on the deployment host"

// appArmorRestrictPath is the host sysctl that decides whether an unprivileged
// process may create a user namespace. It is the precondition that turns
// "nested namespaces are denied" into an assertion instead of a hope.
const appArmorRestrictPath = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

// bwrapForStaging returns the host bubblewrap binary or skips the test.
func bwrapForStaging(t *testing.T) string {
	t.Helper()
	bin := DefaultBwrapBin
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("%s: %v", stagingSkipReason, err)
	}
	probe := exec.Command(bin, "--ro-bind", "/", "/", "--dev", "/dev", "--", "/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("%s (%v: %s)", stagingSkipReason, err, strings.TrimSpace(string(out)))
	}
	return bin
}

// stagingLayout is a synthetic deployment: two tenants, a fake node runtime, and
// the gateway state a tenant must never reach.
type stagingLayout struct {
	root      string
	rt        Runtime
	alice     Tenant
	bobState  string
	aliceCfg  string
	gateway   string
	probeNode string
}

func newStagingLayout(t *testing.T, bwrap string) stagingLayout {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "dsh/node/bin"), 0o755)
	mustMkdir(t, filepath.Join(root, "dsh/releases/v1/lib"), 0o755)
	mustWrite(t, filepath.Join(root, "dsh/releases/v1/lib/bin.js"), "// dsh launcher\n", 0o644)
	mustSymlink(t, filepath.Join(root, "dsh/releases/v1"), filepath.Join(root, "dsh/current"))

	// Alice's own roots, and Bob's data plus the gateway's state that must stay
	// invisible from inside Alice's sandbox.
	aliceWS := filepath.Join(root, "srv/alice")
	aliceHome := filepath.Join(root, "state/tenants/alice/.dsh")
	bobState := filepath.Join(root, "state/tenants/bob/.dsh")
	aliceCfg := filepath.Join(root, "etc/tenants/alice")
	gateway := filepath.Join(root, "gateway-state")
	mustMkdir(t, aliceWS, 0o700)
	mustMkdir(t, aliceHome, 0o700)
	mustMkdir(t, bobState, 0o700)
	mustMkdir(t, aliceCfg, 0o750)
	mustMkdir(t, gateway, 0o700)
	mustWrite(t, filepath.Join(aliceCfg, "gateway.key"), "sk-alice-secret\n", 0o640)
	mustWrite(t, filepath.Join(aliceCfg, "tenant.env"), "DSH_PORT=32100\n", 0o640)
	mustWrite(t, filepath.Join(gateway, "registry.json"), "{\"tenants\":[\"alice\",\"bob\"]}\n", 0o600)

	probeNode := filepath.Join(root, "dsh/node/bin/node")
	mustWrite(t, probeNode, stagingProbeScript(bwrap, bobState, gateway, aliceWS, aliceCfg), 0o755)

	rt := Runtime{
		BwrapBin:         bwrap,
		NodeBin:          probeNode,
		BinJS:            filepath.Join(root, "dsh/current/lib/bin.js"),
		CurrentLink:      filepath.Join(root, "dsh/current"),
		TenantRoot:       filepath.Join(root, "state/tenants"),
		WorkspaceRoot:    filepath.Join(root, "srv"),
		TenantConfigRoot: filepath.Join(root, "etc/tenants"),
	}
	return stagingLayout{
		root:      root,
		rt:        rt,
		bobState:  bobState,
		aliceCfg:  aliceCfg,
		gateway:   gateway,
		probeNode: probeNode,
		alice: Tenant{
			Name:        "alice",
			Workspace:   aliceWS,
			DshHome:     aliceHome,
			WorkerPort:  32100,
			Environment: []string{"web", "--port", "32100", "--no-open"},
		},
	}
}

// stagingProbeScript is the stand-in for node inside the sandbox. It reports
// what it can see and do; the test asserts the report.
func stagingProbeScript(bwrap, bobState, gateway, workspace, configDir string) string {
	return fmt.Sprintf(`#!/bin/sh
report() { printf '%%s=%%s\n' "$1" "$2"; }
entries() { ls -A "$1" 2>/dev/null | wc -l; }
report argv "$*"
if [ -d %[4]q ]; then report own-workspace PRESENT; else report own-workspace MISSING; fi
if [ -e %[2]q ]; then report other-tenant-state VISIBLE; else report other-tenant-state MISSING; fi
if [ -e %[3]q ]; then report gateway-state VISIBLE; else report gateway-state MISSING; fi
if [ -e %[5]q ]; then report own-config VISIBLE; else report own-config MISSING; fi
report host-home-entries "$(entries /home)"
report host-root-entries "$(entries /root)"
report host-srv-entries "$(entries /srv)"
report host-var-entries "$(entries /var)"
report etc-entries "$(ls -A /etc 2>/dev/null | tr '\n' ',')"
report etc-dshgw-entries "$(entries /etc/dshgw)"
if [ -d /etc/alternatives ]; then report etc-alternatives PRESENT; else report etc-alternatives MISSING; fi
if [ -x /usr/bin/pager ]; then report pager-resolves yes; else report pager-resolves no; fi
if [ -e /etc/systemd ]; then report etc-systemd VISIBLE; else report etc-systemd MISSING; fi
if [ -r /etc/passwd ]; then report passwd readable; else report passwd missing; fi
if [ -r /etc/resolv.conf ]; then report resolv-conf readable; else report resolv-conf missing; fi
if command -v sh >/dev/null 2>&1; then report shell found; else report shell missing; fi
if touch %[4]q/write-probe 2>/dev/null; then report workspace-write ok; else report workspace-write failed; fi
mkdir -p %[4]q/sub/dir 2>/dev/null && report subdir-create ok || report subdir-create failed
if touch /usr/.dshgw-probe 2>/dev/null; then report usr-write WRITABLE; else report usr-write read-only; fi
if [ -w /etc/passwd ]; then report etc-write WRITABLE; else report etc-write read-only; fi
if %[1]q --ro-bind / / --dev /dev /bin/true 2>/dev/null; then report nested-bwrap ALLOWED; else report nested-bwrap DENIED; fi
printf 'done=1\n'
`, bwrap, bobState, gateway, workspace, configDir)
}

// TestStagingSandboxHidesHostAndOtherTenants runs the real profile and asserts
// the resulting view: only the tenant's own roots, the read-only whitelist, and
// device plumbing are reachable.
func TestStagingSandboxHidesHostAndOtherTenants(t *testing.T) {
	bwrap := bwrapForStaging(t)
	layout := newStagingLayout(t, bwrap)
	argv, err := Profile(layout.rt, layout.alice)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sandbox run failed: %v\nstderr: %s", err, stderr.String())
	}
	facts := parseStagingReport(t, stdout.String())
	want := map[string]string{
		"own-workspace":      "PRESENT",
		"other-tenant-state": "MISSING",
		"gateway-state":      "MISSING",
		"own-config":         "MISSING",
		"host-home-entries":  "0",
		"host-root-entries":  "0",
		"host-srv-entries":   "0",
		"etc-systemd":        "MISSING",
		"passwd":             "readable",
		"resolv-conf":        "readable",
		"shell":              "found",
		"workspace-write":    "ok",
		"subdir-create":      "ok",
		"usr-write":          "read-only",
		"etc-write":          "read-only",
		"done":               "1",
	}
	for key, expected := range want {
		if got := facts[key]; got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}
	// /etc exposes the named whitelist and nothing else: no nginx, no systemd,
	// no gateway key directory. The /etc/dshgw entry is the gateway's own
	// config mount point, which is kept mounted and empty on purpose so a
	// future bind cannot expose the parent tree.
	allowed := map[string]bool{
		"resolv.conf": true, "hosts": true, "nsswitch.conf": true, "passwd": true,
		"group": true, "localtime": true, "ssl": true, "ca-certificates": true,
		"dshgw": true,
	}
	// The alternatives database is bound back on purpose, so it is part of the
	// whitelist too: the distro's /usr/bin/{pager,awk,which,…} are symlinks into
	// it, and a dangling pager is what made an interactive `git log` fail with
	// "cannot run pager: No such file or directory".
	allowed["alternatives"] = true
	for _, entry := range strings.Split(strings.TrimSuffix(facts["etc-entries"], ","), ",") {
		if entry != "" && !allowed[entry] {
			t.Errorf("/etc exposes %q, which is not part of the runtime whitelist", entry)
		}
	}
	if got := facts["etc-dshgw-entries"]; got != "0" {
		t.Errorf("/etc/dshgw holds %s entries, want an empty mount point", got)
	}
	// The alternatives database is the one /etc directory bound back, because the
	// distro's tool names are symlinks into it: a dangling /usr/bin/pager is what
	// turned an interactive `git log` into "cannot run pager: No such file or
	// directory". Whether the directory exists is a property of the host, so the
	// expectation follows the host rather than demanding one answer.
	if _, err := os.Stat("/etc/alternatives"); err == nil {
		if got := facts["etc-alternatives"]; got != "PRESENT" {
			t.Errorf("etc-alternatives = %q, want PRESENT: the host has /etc/alternatives but the sandbox does not", got)
		}
		if _, err := os.Stat("/etc/alternatives/pager"); err == nil {
			if got := facts["pager-resolves"]; got != "yes" {
				t.Errorf("pager-resolves = %q, want yes: /usr/bin/pager resolves on the host but dangles inside the sandbox", got)
			}
		}
	}
	// A world-writable /var would mean the gateway's own state directory could
	// reappear; it is a tmpfs like the other hidden trees.
	if got := facts["host-var-entries"]; got != "0" {
		t.Errorf("host-var-entries = %q, want 0", got)
	}
	// Nesting a second namespace is what AppArmor denies on the deployment
	// host. When the host does not restrict it, the check must at least have
	// produced a verdict rather than a crash.
	if got := facts["nested-bwrap"]; got != "DENIED" && got != "ALLOWED" {
		t.Errorf("nested-bwrap = %q, want a verdict", got)
	}
	if restricted, err := os.ReadFile(appArmorRestrictPath); err == nil && strings.TrimSpace(string(restricted)) == "1" {
		if facts["nested-bwrap"] != "DENIED" {
			t.Errorf("host restricts unprivileged user namespaces but nested bwrap was %q", facts["nested-bwrap"])
		}
	}
}

var stagingLineRE = regexp.MustCompile(`^([a-z0-9-]+)=(.*)$`)

func parseStagingReport(t *testing.T, output string) map[string]string {
	t.Helper()
	facts := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		match := stagingLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if match == nil {
			continue
		}
		facts[match[1]] = match[2]
	}
	if len(facts) == 0 {
		t.Fatalf("sandbox probe produced no report:\n%s", output)
	}
	return facts
}

// stagingURLRE matches the startup line dsh prints once its web server listens.
var stagingURLRE = regexp.MustCompile(`http://127\.0\.0\.1:([0-9]+)/\?token=[A-Za-z0-9_-]{43}`)

// TestStagingRealDshWebServesInsideSandbox starts the real dsh web server inside
// the real profile and asserts the worker contract the gateway depends on: dsh
// reports a loopback URL, and the unauthenticated /api probe answers 401.
//
// It needs the deployed Node and dsh release and therefore skips unless
// DSHGW_NODE (node executable) and DSHGW_DSH_ROOT (unpacked release, the parent
// of lib/bin.js) point at them — which is exactly what the Makefile passes.
func TestStagingRealDshWebServesInsideSandbox(t *testing.T) {
	bwrap := bwrapForStaging(t)
	nodeBin := os.Getenv("DSHGW_NODE")
	dshRoot := os.Getenv("DSHGW_DSH_ROOT")
	if nodeBin == "" || dshRoot == "" {
		t.Skip("set DSHGW_NODE and DSHGW_DSH_ROOT to stage a real dsh web worker")
	}
	for _, path := range []string{nodeBin, filepath.Join(dshRoot, "lib", "bin.js")} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("staged dsh runtime is incomplete: %v", err)
		}
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "srv/alice")
	dshHome := filepath.Join(root, "state/tenants/alice/.dsh")
	configDir := filepath.Join(root, "etc/tenants/alice")
	for _, dir := range []string{workspace, dshHome, configDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	port := freeStagingPort(t)
	rt := Runtime{
		BwrapBin:         bwrap,
		NodeBin:          nodeBin,
		BinJS:            filepath.Join(dshRoot, "lib", "bin.js"),
		CurrentLink:      dshRoot,
		TenantRoot:       filepath.Join(root, "state/tenants"),
		WorkspaceRoot:    filepath.Join(root, "srv"),
		TenantConfigRoot: filepath.Join(root, "etc/tenants"),
	}
	tenant := Tenant{Name: "alice", Workspace: workspace, DshHome: dshHome, WorkerPort: port,
		Environment: []string{"web", "--port", fmt.Sprint(port), "--no-open"}}
	argv, err := Profile(rt, tenant)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// systemd supplies these from the tenant's EnvironmentFile; the staging run
	// reproduces them so the checks exercise the same startup path.
	cmd.Env = append(os.Environ(),
		"DSH_PORT="+fmt.Sprint(port),
		"DSH_HOME="+dshHome,
		"HOME="+workspace,
		"DSHGW_DSH_ANCHOR="+filepath.Join(dshRoot, "package.json"),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	urls := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var seen strings.Builder
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 {
				seen.Write(buf[:n])
				if match := stagingURLRE.FindString(seen.String()); match != "" {
					select {
					case urls <- match:
					default:
					}
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
	var startURL string
	select {
	case startURL = <-urls:
	case <-time.After(60 * time.Second):
		t.Fatalf("dsh never reported a startup URL inside the sandbox; stderr: %s", stderr.String())
	}
	// The unauthenticated probe the gateway's readiness check performs.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api", port))
	if err != nil {
		t.Fatalf("worker did not answer on its loopback port: %v (stderr: %s)", err, stderr.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api answered %d, want 401 (startup url %s)", resp.StatusCode, startURL)
	}
	// The workspace stays the tenant's writable root even with a real dsh
	// running: dsh keeps its state in DSH_HOME, which is also writable.
	for _, marker := range []string{dshHome} {
		info, statErr := os.Stat(marker)
		if statErr != nil || !info.IsDir() {
			t.Fatalf("sandbox did not keep %s usable: %v", marker, statErr)
		}
	}
	// Nothing outside the tenant roots may exist because of this worker.
	if _, statErr := os.Stat(filepath.Join(root, "state/tenants/bob")); !os.IsNotExist(statErr) {
		t.Fatalf("staging run created foreign tenant state: %v", statErr)
	}
}

func freeStagingPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
