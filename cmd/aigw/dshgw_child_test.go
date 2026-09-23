package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/config"
	dshgwconfig "github.com/winger/ai-gateway/internal/dshgw/config"
	dshgwfeishu "github.com/winger/ai-gateway/internal/dshgw/feishu"
)

// childFixture is a configuration that enables the supervised child, with every
// path pointing into a temporary directory and the dsh runtime supplied by the
// environment (which is how the repository's own scripts describe a staged dsh).
func childFixture(t *testing.T) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	node := filepath.Join(root, "dsh/node/bin/node")
	release := filepath.Join(root, "dsh/releases/r1")
	for _, dir := range []string{filepath.Dir(node), filepath.Join(release, "lib"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{node, filepath.Join(release, "lib", "bin.js")} {
		if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	aigwBinary := filepath.Join(root, "bin/aigw")
	dshgwBinary := filepath.Join(root, "bin/dshgw")
	for _, file := range []string{aigwBinary, dshgwBinary} {
		if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DSHGW_NODE", node)
	t.Setenv("DSHGW_DSH_ROOT", release)
	t.Setenv("DSHGW_TEMPLATE_HOME", filepath.Join(root, "template-home"))
	cfg := &config.Config{
		Server:   config.Server{Listen: ":8088"},
		Database: config.Database{Path: filepath.Join(root, "data/aigw.db")},
		Dshgw: config.Dshgw{
			Enabled: true, PublicHost: "localhost", Listen: "127.0.0.1:31699",
			PortalPort: 31000, TenantPortLo: 31001, TenantPortHi: 31299,
			WorkerPortLo: 31300, WorkerPortHi: 31599,
			// The picker plugin has no default on purpose (M63): the child's default
			// directory picker is clamp, and an unset path used to fall through to the
			// deleted root install's /opt/dshgw path.
			PluginPath: filepath.Join(root, "cmd/dshgw/plugin/picker-clamp.js"),
		},
	}
	return cfg, aigwBinary
}

func TestBuildDshgwChildDerivesTheWholeSurface(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(filepath.Dir(cfg.Database.Path), "dshgw")
	if child.binary != filepath.Join(filepath.Dir(aigwBinary), "dshgw") {
		t.Fatalf("binary = %q, want the sibling of aigw", child.binary)
	}
	if child.configPath != filepath.Join(stateDir, "config.yaml") {
		t.Fatalf("config path = %q", child.configPath)
	}
	generated := child.config
	checks := map[string]string{
		"state dir":      generated.StateDir,
		"workspace root": generated.WorkspaceRoot,
		"tenant config":  generated.Deploy.TenantConfigRoot,
		"admin socket":   generated.AdminSocket,
		"child config":   generated.Deploy.ConfigPath,
		"backup dir":     generated.Deploy.BackupDir,
		"aigw base url":  generated.AigwBaseURL,
		"node bin":       generated.Dsh.NodeBin,
		"current link":   generated.Dsh.CurrentLink,
	}
	wants := map[string]string{
		"state dir":      stateDir,
		"workspace root": filepath.Join(stateDir, "workspaces"),
		"tenant config":  filepath.Join(stateDir, "tenant-config"),
		"admin socket":   filepath.Join(stateDir, "admin.sock"),
		"child config":   filepath.Join(stateDir, "config.yaml"),
		"backup dir":     filepath.Join(stateDir, "backups"),
		"aigw base url":  "http://127.0.0.1:8088",
		"node bin":       os.Getenv("DSHGW_NODE"),
		"current link":   os.Getenv("DSHGW_DSH_ROOT"),
	}
	for label, want := range wants {
		if checks[label] != want {
			t.Errorf("%s = %q, want %q", label, checks[label], want)
		}
	}
	// Every tenant worker runs as aigw's own account: no service account, no root.
	if generated.Deploy.WorkerUser == "" || generated.Deploy.WorkerUser == "root" {
		t.Fatalf("worker account = %q; workers must run as the invoking account", generated.Deploy.WorkerUser)
	}
}

// The roots, the path-mode switch and the port-mode default are all configuration
// the deployment reads back from the generated file. A key that is accepted but
// silently ignored is worse than a rejected one — the migration script and the
// single-domain proxy both depend on these arriving.
func TestBuildDshgwChildCarriesRootsAndPathMode(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Dshgw.TenantRoot = "/srv/old-state/tenants"
	cfg.Dshgw.WorkspaceRoot = "/srv/old-state/workspaces"
	cfg.Dshgw.PublicHost = "chat.example" // the base URL's host must agree with it
	cfg.Dshgw.PublicBaseURL = "https://chat.example/"
	cfg.Dshgw.TenantPathPrefix = "/t"
	cfg.Dshgw.PortalPathPrefix = "/dshgw"

	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	generated := child.config
	if generated.TenantRoot != "/srv/old-state/tenants" || generated.WorkspaceRoot != "/srv/old-state/workspaces" {
		t.Fatalf("configured roots ignored: tenant=%q workspace=%q", generated.TenantRoot, generated.WorkspaceRoot)
	}
	// A trailing slash on the base URL would produce "https://host//t/alice/".
	if generated.PublicBaseURL != "https://chat.example" {
		t.Fatalf("public base url = %q, want it without a trailing slash", generated.PublicBaseURL)
	}
	if generated.TenantPathPrefix != "/t" || generated.PortalPathPrefix != "/dshgw" {
		t.Fatalf("prefixes = %q / %q", generated.TenantPathPrefix, generated.PortalPathPrefix)
	}

	// Unset means port mode plus state_dir-derived roots, not empty paths.
	defaults, err := buildDshgwChild(childFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.config.PublicBaseURL != "" {
		t.Fatalf("path mode enabled by default: %q", defaults.config.PublicBaseURL)
	}
	if defaults.config.TenantRoot == "" || defaults.config.WorkspaceRoot == "" {
		t.Fatalf("default roots are empty: %q / %q", defaults.config.TenantRoot, defaults.config.WorkspaceRoot)
	}

	// A base URL whose host disagrees with public_host is refused: the generated
	// links would be rejected by the gateway's own Host fence.
	cfg.Dshgw.PublicBaseURL = "https://other.example"
	if _, err := buildDshgwChild(cfg, aigwBinary); err == nil {
		t.Fatal("a public_base_url with a different host was accepted")
	}
}

// The public scheme decides the child's session cookie Secure attribute, so failing
// to pass it through is not cosmetic: on a plain-HTTP deployment the browser drops
// the cookie and every login looks like "authenticated, then back at the portal".
func TestBuildDshgwChildCarriesThePublicScheme(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Dshgw.PublicScheme = "http"
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.PublicScheme != "http" {
		t.Fatalf("public scheme = %q, want the configured http", child.config.PublicScheme)
	}
	// Unset stays unset: the child's own "auto" default is what port-mode HTTPS
	// deployments rely on, and writing "auto" out would only add noise.
	cfg.Dshgw.PublicScheme = ""
	child, err = buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.PublicScheme != "" {
		t.Fatalf("public scheme = %q, want it omitted when unset", child.config.PublicScheme)
	}
	rendered, err := child.config.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "public_scheme") {
		t.Fatalf("an unset public_scheme still reached the child configuration:\n%s", rendered)
	}
	// A scheme that contradicts the base URL is refused before the child restarts
	// into it.
	cfg.Dshgw.PublicScheme = "https"
	cfg.Dshgw.PublicHost = "chat.example"
	cfg.Dshgw.PublicBaseURL = "http://chat.example"
	if _, err := buildDshgwChild(cfg, aigwBinary); err == nil {
		t.Fatal("a public_scheme contradicting public_base_url was accepted")
	}
}

func TestBuildDshgwChildPrefersExplicitOverridesAndBasePath(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Server.BasePath = "/aigw/"
	cfg.Server.Listen = "127.0.0.1:9099"
	cfg.Dshgw.AigwBaseURL = "http://aigw.internal:8088"
	cfg.Dshgw.StateDir = filepath.Join(t.TempDir(), "custom-state")
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.AigwBaseURL != "http://aigw.internal:8088" {
		t.Fatalf("base url = %q, want the explicit override", child.config.AigwBaseURL)
	}
	if child.config.StateDir != cfg.Dshgw.StateDir {
		t.Fatalf("state dir = %q, want the configured one", child.config.StateDir)
	}

	// Without an override, aigw's own listener is used, including its mount prefix:
	// the child calls aigw's own API, not the console.
	cfg.Dshgw.AigwBaseURL = ""
	child, err = buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.AigwBaseURL != "http://127.0.0.1:9099/aigw" {
		t.Fatalf("base url = %q, want the loopback listener with the mount prefix", child.config.AigwBaseURL)
	}
}

// Without a dsh runtime there is nothing for the child to run tenants from; the
// failure must name what is missing instead of surfacing as a restart loop.
func TestBuildDshgwChildRequiresTheDSHRuntime(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	t.Setenv("DSHGW_NODE", "")
	cfg.Dshgw.NodeBin = ""
	_, err := buildDshgwChild(cfg, aigwBinary)
	if err == nil {
		t.Fatal("missing node binary accepted")
	}
	if !strings.Contains(err.Error(), "node_bin") {
		t.Fatalf("error does not name the missing setting: %v", err)
	}
	// A missing sibling binary is refused too: enabled means "aigw starts dshgw".
	if err := os.Remove(filepath.Join(filepath.Dir(aigwBinary), "dshgw")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildDshgwChild(cfg, aigwBinary); err == nil {
		t.Fatal("missing dshgw binary accepted")
	}
}

func TestPrepareWritesTheChildConfigOnce(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := child.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first prepare reported no change")
	}
	info, err := os.Stat(child.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("child config mode = %04o, want 0600", perm)
	}
	changed, err = child.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical configuration rewritten")
	}
}

// Enabling the Feishu login must hand the child exactly what it needs to redeem a ticket:
// the URL to send people to, and the key that must match aigw's own signer. Both are derived,
// so an operator never has to keep two files in sync.
func TestBuildDshgwChildInjectsTheFeishuHandoff(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.CredentialsKey = "credentials-key-for-tests"
	cfg.Feishu.Enabled = true
	cfg.Feishu.DSHLogin = true
	cfg.Feishu.AppID = "cli_test"
	cfg.Feishu.AppSecret = "secret"
	cfg.Feishu.CallbackURL = "http://192.168.190.86:8090/feishu/callback"
	// The timeouts and endpoints come from config.Default() in a real deployment (Load starts
	// there); this fixture builds the struct literally, so it states them.
	cfg.Feishu.StateTTLS = 600
	cfg.Feishu.TicketTTLS = 120
	cfg.Feishu.TimeoutS = 5
	defaults := config.Default().Feishu
	cfg.Feishu.AuthorizeURL = defaults.AuthorizeURL
	cfg.Feishu.TokenURL = defaults.TokenURL
	cfg.Feishu.UserInfoURL = defaults.UserInfoURL

	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.Feishu == nil || !child.config.Feishu.Enabled {
		t.Fatal("the child was not told to serve Feishu login")
	}
	if got, want := child.config.Feishu.AigwLoginURL, "http://192.168.190.86:8090/feishu/login"; got != want {
		t.Fatalf("aigw_login_url = %q, want %q", got, want)
	}
	// The injected secret must be the one aigw signs with, or every login would fail
	// verification in the child.
	deps, err := buildFeishuDeps(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, _, err := deps.Tickets.Issue("alice", 7, 3, "ou_alice", "nonce-parity")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := dshgwfeishu.New([]byte(child.config.Feishu.TicketSecret))
	if err != nil {
		t.Fatalf("the injected secret is unusable by the child: %v", err)
	}
	ticket, err := verifier.Verify(wire)
	if err != nil {
		t.Fatalf("the child cannot verify a ticket signed by aigw: %v", err)
	}
	if ticket.Tenant != "alice" || ticket.OpenID != "ou_alice" {
		t.Fatalf("ticket decoded differently on the two sides: %+v", ticket)
	}

	// While the feature is off the block must not appear at all: a deployment that does not
	// use Feishu generates byte-identical configuration to before.
	cfg.Feishu.Enabled = false
	plain, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if plain.config.Feishu != nil {
		t.Fatal("a disabled Feishu block still reached the child configuration")
	}
	rendered, err := plain.config.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "feishu") {
		t.Fatalf("the generated configuration mentions Feishu while it is off:\n%s", rendered)
	}
}

// The parent must dial the same socket it told the child to bind: with an unset
// dshgw.admin_socket the console's DSH buttons (and the automatic enable a Feishu binding
// performs) would otherwise dial an empty address and report "missing address".
func TestBuildDshgwChildDerivesTheAdminSocketForBothSides(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(filepath.Dir(cfg.Database.Path), "dshgw")
	if child.config.AdminSocket != filepath.Join(stateDir, "admin.sock") {
		t.Fatalf("child admin socket = %q", child.config.AdminSocket)
	}
	if child.adminSocket != child.config.AdminSocket {
		t.Fatalf("the parent would dial %q while the child binds %q", child.adminSocket, child.config.AdminSocket)
	}
	// An explicit setting wins on both sides.
	cfg.Dshgw.AdminSocket = "/run/dshgw/elsewhere.sock"
	child, err = buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.adminSocket != "/run/dshgw/elsewhere.sock" || child.config.AdminSocket != "/run/dshgw/elsewhere.sock" {
		t.Fatalf("explicit socket ignored: parent=%q child=%q", child.adminSocket, child.config.AdminSocket)
	}
}

// M63: the state root hangs off the database's directory, and the shipped defaults spell
// that directory relatively (./data). Before this milestone the derived value was rejected
// as "not absolute", so the supervised shape could not start at all with the default
// configuration — the child's own validation requires clean absolute paths, so aigw is the
// side that resolves the deployment's relative data root.
func TestBuildDshgwChildResolvesTheRelativeDataRoot(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Database.Path = "./data/aigw.db"
	cfg.Dshgw.StateDir = ""
	cfg.Dshgw.TenantRoot = ""
	cfg.Dshgw.WorkspaceRoot = ""
	cfg.Dshgw.TemplateHome = ""
	// The fixture exports DSHGW_TEMPLATE_HOME; clear it so the derived default is what is
	// exercised here (the environment still wins when it is set, which is how the
	// repository's own template script points at a prepared profile).
	t.Setenv("DSHGW_TEMPLATE_HOME", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wantState := filepath.Join(wd, "data", "dshgw")

	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	generated := child.config
	checks := map[string]string{
		"state dir":      generated.StateDir,
		"tenant root":    generated.TenantRoot,
		"workspace root": generated.WorkspaceRoot,
		"template home":  generated.Deploy.TemplateHome,
		"config path":    child.configPath,
	}
	for label, got := range checks {
		if got != wantState && !strings.HasPrefix(got, wantState+string(filepath.Separator)) {
			t.Errorf("%s = %q, want it inside %q", label, got, wantState)
		}
	}
	if !filepath.IsAbs(generated.Deploy.TemplateHome) {
		t.Errorf("template home = %q, want an absolute path: it used to be left empty, which made the child fall back to its own /opt/dshgw default", generated.Deploy.TemplateHome)
	}

	// Round trip: what aigw renders must be loadable by the child. This is the guard
	// against "aigw writes a path the child rejects", which is how the relative default
	// failed before.
	rendered, err := generated.Render()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "child.yaml")
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dshgwconfig.Load(path); err != nil {
		t.Fatalf("the generated child configuration does not load: %v\n--- rendered ---\n%s", err, rendered)
	}
}

// The picker plugin has no default: the child's default directory picker is clamp, and an
// unset path used to fall through to the deleted root install's
// /opt/dshgw/share/dsh-plugin/picker-clamp.js. A missing value is a startup error naming
// the setting, not a tenant session with a picker pointing nowhere.
func TestBuildDshgwChildRequiresThePickerPlugin(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Dshgw.PluginPath = ""
	_, err := buildDshgwChild(cfg, aigwBinary)
	if err == nil {
		t.Fatal("an empty dshgw.plugin_path was accepted")
	}
	if !strings.Contains(err.Error(), "plugin_path") {
		t.Fatalf("error does not name the missing setting: %v", err)
	}
}

// A relative path may point deeper into the deployment, never back out of it.
func TestBuildDshgwChildRejectsParentTraversal(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Dshgw.StateDir = "./data/../elsewhere"
	if _, err := buildDshgwChild(cfg, aigwBinary); err == nil {
		t.Fatal("a state dir climbing out of the deployment root was accepted")
	}
}

// The ssh-workspace block is the child's own feature (it is the side that runs ssh), so the
// supervised shape must carry it across and put its paths in the deployment's terms. A key
// that is accepted by aigw but never reaches the child would look like a feature that does
// nothing at all.
func TestBuildDshgwChildCarriesSSHWorkspaces(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Dshgw.SSHWorkspaces = config.DshgwSSHWorkspaces{
		Enabled:        true,
		MountSubdir:    "ssh",
		IdentityDir:    "./keys",
		Hosts:          []string{"gpt001"},
		ConnectTimeout: "7s",
		PollInterval:   "1s",
		MaxEntries:     50,
		SSHFSOptions:   []string{"reconnect"},
	}
	// A relative key directory is resolved against the deployment root here, so the child
	// never has to guess which directory it meant.
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(working, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Join(working, "keys"))
	if err := os.WriteFile(filepath.Join(working, "keys", "dsh-colin"), []byte("PRIVATE KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	ssh := child.config.SSHWorkspaces
	if ssh == nil || !ssh.Enabled {
		t.Fatal("the ssh workspace block did not reach the child")
	}
	if ssh.IdentityDir != filepath.Join(working, "keys") {
		t.Errorf("identity_dir = %q, want the resolved deployment path", ssh.IdentityDir)
	}
	if ssh.ConnectTimeout != "7s" || ssh.PollInterval != "1s" {
		t.Errorf("durations were rewritten: %q / %q", ssh.ConnectTimeout, ssh.PollInterval)
	}
	if ssh.MaxEntries != 50 || len(ssh.Hosts) != 1 || ssh.Hosts[0] != "gpt001" {
		t.Errorf("ssh workspace values = %+v", ssh)
	}

	// Disabled means absent: a generated file should not carry a block nobody asked for.
	cfg.Dshgw.SSHWorkspaces = config.DshgwSSHWorkspaces{}
	child, err = buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.SSHWorkspaces != nil {
		t.Errorf("a disabled ssh workspace block was generated: %+v", child.config.SSHWorkspaces)
	}
}

// M75: unlike the blocks above, the tenant-side plugin switches are written even when they are all
// off. The child's own defaults for them are ON, so a parent that stayed silent after an operator
// wrote `enabled: false` would have the child quietly turn the plugin back on — the one failure
// mode of a default-on switch that crosses a process boundary.
func TestBuildDshgwChildCarriesTheTenantPluginSwitches(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Dshgw.TenantPlugins = config.DshgwTenantPlugins{
		WebTTY:         config.DshgwPluginSwitch{Enabled: false},
		WorkspaceFiles: config.DshgwPluginSwitch{Enabled: true},
		GitDiff:        config.DshgwPluginSwitch{Enabled: false},
		RootLabel:      "工作区",
	}
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	plugins := child.config.TenantPlugins
	if plugins == nil {
		t.Fatal("the tenant plugin block did not reach the child")
	}
	if plugins.WebTTY.Enabled || !plugins.WorkspaceFiles.Enabled || plugins.GitDiff.Enabled {
		t.Fatalf("switches did not travel as written: %+v", plugins)
	}
	if plugins.RootLabel != "工作区" {
		t.Fatalf("root_label = %q", plugins.RootLabel)
	}

	// Every switch off still generates the block: silence would mean "on" on the child's side.
	cfg.Dshgw.TenantPlugins = config.DshgwTenantPlugins{}
	child, err = buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	plugins = child.config.TenantPlugins
	if plugins == nil {
		t.Fatal("an all-off block must still be written, or the child's own defaults turn it back on")
	}
	if plugins.WebTTY.Enabled || plugins.WorkspaceFiles.Enabled || plugins.GitDiff.Enabled {
		t.Fatalf("the child would enable a plugin nobody asked for: %+v", plugins)
	}
}

// TestCurrentAccountNeverFallsBackToAUid pins the fallback: a numeric worker_user is rejected by
// dshgw's own configuration validator, so a process started without USER/LOGNAME (a sandbox, a
// stripped service environment) must still name a real account — otherwise aigw writes a child
// configuration the child cannot load, and the failure surfaces as a dshgw startup error far from
// its cause.
func TestCurrentAccountNeverFallsBackToAUid(t *testing.T) {
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")
	account := currentAccount()
	if account == "" {
		t.Fatal("currentAccount returned nothing")
	}
	if _, err := strconv.Atoi(account); err == nil {
		t.Fatalf("currentAccount fell back to a numeric id (%s), which dshgw rejects as worker_user", account)
	}
}
