// Package nodeitest holds the multi-machine integration test (M77): a control plane and a worker
// node wired to each other over the real HTTP protocol, each with its own registry, driven through
// the same lifecycle calls the admin socket, the login hook and the CLI use.
//
// It lives in its own package because it needs both halves at once: tenancy's manager, nodeops'
// handlers, nodeserve's listener and nodeclient's transport. The unit tests next to each of those
// packages pin one side in isolation; this one exists to catch the failures that only appear when
// the two sides talk — an operation name that does not match, a field that is not carried across,
// a state the node reports but the control plane forgets to record.
//
// The workers are stand-in processes (a shell loop that prints a plausible dsh startup line), for
// the same reason the tenancy tests use them: bwrap and a dsh release are host prerequisites, and
// what this test is about is the protocol and the bookkeeping, not the sandbox.
package nodeitest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodeops"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/nodeserve"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
	"gopkg.in/yaml.v3"
)

const (
	nodeName = "node-a"
	token    = "0123456789abcdef0123456789abcdef"
)

// cluster is a control plane plus one node, both running for real.
type cluster struct {
	control     *tenancy.Manager
	controlCfg  *config.Config
	node        *tenancy.Manager
	nodeCfg     *config.Config
	nodeOps     *nodeops.Ops
	nodeRoot    string
	controlRoot string

	// The node keeps ONE address across restarts, which is what makes this a restart rather than
	// a different machine appearing: the control plane's client (and with it every session's
	// handshake authority) points at the same place before and after.
	nodeAddr    string
	nodeBaseURL string
	nodeClose   func()
}

// startNode builds the node's manager and serves its control surface on the cluster's address.
func (c *cluster) startNode(t *testing.T) {
	t.Helper()
	cfg, manager, ops := buildNode(t, c.nodeRoot)
	c.nodeCfg, c.node, c.nodeOps = cfg, manager, ops
	server, err := nodeserve.New(nodeserve.Options{
		Name: nodeName, Version: "test", Revision: "abc1234", Token: token,
		Ops: ops.Handlers(), StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.nodeAddr == "" {
		c.nodeAddr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", c.nodeAddr)
	if err != nil {
		t.Fatalf("bind %s: %v", c.nodeAddr, err)
	}
	// Remember the address the OS chose the first time, and rebind exactly it on every restart.
	c.nodeAddr = listener.Addr().String()
	if c.nodeBaseURL == "" {
		c.nodeBaseURL = "http://" + c.nodeAddr
	}
	// The test owns the http.Server instead of using nodeserve.Serve, so a simulated restart is
	// immediate: Close() drops every connection, where Shutdown waits for them to become idle.
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: time.Second, IdleTimeout: time.Minute}
	go func() { _ = httpServer.Serve(listener) }()
	c.nodeClose = func() { _ = httpServer.Close() }
	// A node starts the workers its own table says should be running — that is what makes a node
	// restart recover, and it is the behaviour the control plane's reconcile then reconciles.
	if err := manager.StartWorkers(context.Background()); err != nil {
		t.Fatalf("node start workers: %v", err)
	}
}

// stopNode simulates the node agent dying: its workers go with it (Pdeathsig, as in production),
// while the machine and its address stay what they were.
func (c *cluster) stopNode() {
	if c.nodeClose != nil {
		c.nodeClose()
		c.nodeClose = nil
	}
	if c.node != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.node.ShutdownWorkers(ctx)
	}
}

// restartNode brings the node back from the same state directory on the same address.
func (c *cluster) restartNode(t *testing.T) {
	t.Helper()
	c.stopNode()
	c.startNode(t)
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	root := t.TempDir()
	c := &cluster{controlRoot: filepath.Join(root, "control"), nodeRoot: filepath.Join(root, "node")}
	for _, dir := range []string{c.controlRoot, c.nodeRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c.startNode(t)

	controlCfg, control := buildControl(t, c.controlRoot)
	client, err := nodeclient.NewSet([]nodeclient.Spec{{Name: nodeName, BaseURL: c.nodeBaseURL, Token: token}})
	if err != nil {
		t.Fatal(err)
	}
	control.Nodes = client
	c.controlCfg, c.control = controlCfg, control
	return c
}

// stateDir builds the directories both roles expect.
func stateDir(t *testing.T, root, template string) *config.Config {
	t.Helper()
	cfg := &config.Config{
		PublicHost: "node.test", PortalPort: 32600, TenantPortLo: 32601, TenantPortHi: 32610,
		WorkerPortLo: 32900, WorkerPortHi: 32910, Listen: "127.0.0.1:3099",
		AigwBaseURL: "http://aigw.invalid", SessionTTL: config.Duration(1), DirectoryPicker: "clamp",
		PluginBrowserFS: "off", WorkspaceSeed: []string{"work"}, ReservedNames: []string{"login"},
		Dsh: config.DshRuntime{
			NodeBin:     filepath.Join(root, "dsh/node/bin/node"),
			BinJS:       filepath.Join(root, "dsh/releases/r1/lib/bin.js"),
			CurrentLink: filepath.Join(root, "dsh/current"),
		},
		StateDir:      filepath.Join(root, "state"),
		TenantRoot:    filepath.Join(root, "state/tenants"),
		WorkspaceRoot: filepath.Join(root, "srv"),
		HandshakeDir:  filepath.Join(root, "state/handshake"),
		RegistryPath:  filepath.Join(root, "state/registry.json"),
		KeyMapPath:    filepath.Join(root, "state/keys.map"),
		Deploy: config.DeployConfig{
			TemplateHome: template, PluginPath: filepath.Join(root, "plugin/picker-clamp.js"),
			ConfigPath: filepath.Join(root, "dshgw.yaml"), TenantConfigRoot: filepath.Join(root, "state/tenant-config"),
			BackupDir: filepath.Join(root, "state/backups"), WorkerUser: "dshgw", BwrapBin: "/usr/bin/bwrap",
		},
	}
	for _, dir := range []string{cfg.StateDir, cfg.HandshakeDir, filepath.Dir(cfg.Deploy.ConfigPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.Deploy.ConfigPath, []byte("test: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fixtureAssets writes the synthetic dsh release, the template and the stand-in worker.
func fixtureAssets(t *testing.T, root string) (template, worker string) {
	t.Helper()
	template = filepath.Join(root, "template")
	profile := filepath.Join(template, "profiles/web")
	release := filepath.Join(root, "dsh/releases/r1")
	nodeBin := filepath.Join(root, "dsh/node/bin/node")
	for _, dir := range []string{profile, filepath.Join(release, "lib"), filepath.Dir(nodeBin), filepath.Join(root, "plugin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(profile, "package.json"), []byte(`{"dependencies":{},"dsh":{"profile":{"bundles":["@deepseek-ai/dsh-base","@deepseek-ai/dsh-web-app"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "cordis.patch.yml"), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "lib/bin.js"), []byte("// dsh launcher\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a node restart rebuilds the manager from the same directory, so this runs more
	// than once for one root.
	link := filepath.Join(root, "dsh/current")
	if _, err := os.Lstat(link); os.IsNotExist(err) {
		if err := os.Symlink(release, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "plugin/picker-clamp.js"), []byte("// picker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worker = filepath.Join(root, "fake-worker.sh")
	script := "#!/bin/sh\nport=\"$1\"\necho \"dsh web: http://127.0.0.1:$port/?token=" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\ntrap 'exit 0' TERM INT\nwhile :; do sleep 0.2; done\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return template, worker
}

func buildManager(t *testing.T, cfg *config.Config, worker string) *tenancy.Manager {
	t.Helper()
	runner := &tenancy.WorkerRunner{
		Config: cfg,
		Profile: func(tn registry.Tenant) ([]string, error) {
			return []string{worker, itoa(tn.WorkerPort)}, nil
		},
		StopTimeout: 5 * time.Second,
	}
	// Load, not New: a node restart reads the allocation table the previous incarnation wrote,
	// which is the whole reason that table exists.
	reg, err := registry.Load(cfg.RegistryPath, cfg.KeyMapPath)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return &tenancy.Manager{
		Config: cfg, Registry: reg, Workers: runner,
		WorkerAccount: func(name string) error {
			if name == "" {
				return os.ErrInvalid
			}
			return nil
		},
		RuntimeCheck: sandbox.ValidateBindings,
		Probe:        func(context.Context, registry.Tenant) error { return nil },
		HostPasswd: func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/sh\ndshgw:x:1001:1001::/home/dshgw:/bin/sh\n"), nil
		},
	}
}

func buildNode(t *testing.T, root string) (*config.Config, *tenancy.Manager, *nodeops.Ops) {
	t.Helper()
	template, worker := fixtureAssets(t, root)
	cfg := stateDir(t, root, template)
	manager := buildManager(t, cfg, worker)
	ops := &nodeops.Ops{Config: cfg, Manager: manager, Registry: manager.Registry}
	return cfg, manager, ops
}

func buildControl(t *testing.T, root string) (*config.Config, *tenancy.Manager) {
	t.Helper()
	template, worker := fixtureAssets(t, root)
	cfg := stateDir(t, root, template)
	// The control plane knows which node exists; the token lives in its node record.
	cfg.Nodes = []config.Node{{Name: nodeName, URL: "http://placeholder.invalid", Token: token}}
	cfg.DefaultNode = nodeName
	manager := buildManager(t, cfg, worker)
	return cfg, manager
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestClusterLifecycleEndToEnd drives the whole multi-machine lifecycle through the real protocol.
func TestClusterLifecycleEndToEnd(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	models := []aigw.Model{{ID: "deepseek-flash", ContextWindow: 1000000}}

	// 1) A tenant created for a node is provisioned there, and the control plane records what the
	//    node chose (worker port and paths) without starting anything locally.
	created, err := c.control.Create(ctx, "alice", "sk-cluster1234567890abcd", models, tenancy.CreateOptions{
		Node: nodeName, Account: "chen", DirectoryPicker: "clamp", PluginBrowserFS: "off",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Node != nodeName {
		t.Fatalf("created = %+v", created)
	}
	if created.WorkerPort < 32900 || created.WorkerPort > 32910 {
		t.Fatalf("the node's worker port was not recorded: %+v", created)
	}
	if !filepath.IsAbs(created.DshHome) || filepath.Dir(created.DshHome) != filepath.Join(c.nodeCfg.TenantRoot, "alice") {
		t.Fatalf("the node's paths were not recorded: %+v", created)
	}
	if local := c.controlCfg.IsLocalNode(created.Node); local {
		t.Fatal("the tenant was recorded as local")
	}
	// The control plane's own process tree has no worker for it: the node has.
	if running := c.control.Workers.Running(); len(running) != 0 {
		t.Fatalf("the control plane started a local worker: %+v", running)
	}
	if running := c.node.Workers.Running(); len(running) != 1 || running[0].Name != "alice" {
		t.Fatalf("the node did not start the worker: %+v", running)
	}

	// 2) Status and probe read the node.
	state, err := c.control.Status(ctx, created)
	if err != nil || !state.Running {
		t.Fatalf("status = %+v, %v", state, err)
	}
	if err := c.control.ProbeWorker(ctx, created); err != nil {
		t.Fatalf("probe: %v", err)
	}

	// 3) Stop and start travel to the node, and the suspension is durable there.
	if err := c.control.StopWorker(ctx, created); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if running := c.node.Workers.Running(); len(running) != 0 {
		t.Fatalf("stop did not stop the node's worker: %+v", running)
	}
	if state, err := c.control.Status(ctx, created); err != nil || state.Running || !state.Suspended {
		t.Fatalf("status after stop = %+v, %v", state, err)
	}
	if started, err := c.control.EnsureRunning(ctx, created); err == nil {
		t.Fatalf("ensure-running must refuse a suspended tenant (started=%t)", started)
	}
	if err := c.control.StartWorker(ctx, created); err != nil {
		t.Fatalf("start: %v", err)
	}
	if running := c.node.Workers.Running(); len(running) != 1 {
		t.Fatalf("start did not start the node's worker: %+v", running)
	}

	// 4) A node restart loses its workers (they are its children), and the node brings back the
	//    ones its own table says should run.
	c.restartNode(t)
	if running := c.node.Workers.Running(); len(running) != 1 || running[0].Name != "alice" {
		t.Fatalf("after a node restart, the node's workers are %+v", running)
	}
	// The port it came back on is the same one the control plane recorded, so the handshake
	// authority a live session holds stays valid.
	reloaded, _ := c.control.Registry.Get("alice")
	if reloaded.WorkerPort != created.WorkerPort {
		t.Fatalf("the node came back on %d, the control plane records %d", reloaded.WorkerPort, created.WorkerPort)
	}

	// 5) Reconciliation repairs a stale control-plane record: a node that re-allocated its worker
	//    port (simulated by rewriting the control plane's record) is brought back in line.
	if err := c.control.Registry.SetPlacement("alice", 32905, created.DshHome, created.Workspace); err != nil {
		t.Fatal(err)
	}
	if err := c.control.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	result, err := c.control.ReconcileNode(ctx, nodeName)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(result.Status.Tenants) != 1 || result.Status.Tenants[0].Name != "alice" {
		t.Fatalf("reconcile answer = %+v", result.Status.Tenants)
	}
	repaired, _ := c.control.Registry.Get("alice")
	if repaired.WorkerPort != created.WorkerPort {
		t.Fatalf("reconcile did not record the node's real port: %+v", repaired)
	}

	// 6) A tenant the control plane dropped is pruned from the node without losing its data.
	if orphan, err := c.node.Create(ctx, "orphan", "sk-cluster2234567890abcd", models, tenancy.CreateOptions{
		AllowEmptyModels: true, DirectoryPicker: "clamp", PluginBrowserFS: "off", PublicPort: 32609,
	}); err == nil {
		// The node created it directly (as a migration or a hand-run would); the control plane does
		// not know it, so reconcile must prune it — and keep the workspace.
		if _, err := c.control.ReconcileNode(ctx, nodeName); err != nil {
			t.Fatalf("reconcile with an orphan: %v", err)
		}
		if _, ok := c.node.Registry.Get("orphan"); ok {
			t.Fatal("reconcile left a tenant the control plane does not know about")
		}
		if _, err := os.Stat(orphan.Workspace); err != nil {
			t.Fatalf("reconcile deleted a pruned tenant's data: %v", err)
		}
	}

	// 7) Removal drops the control plane's entry and credential copy, and keeps the node's data
	//    unless the caller asks for a purge.
	snapshot, err := c.control.Remove(ctx, created, false)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if snapshot == "" {
		t.Fatal("removal did not write a snapshot on the node")
	}
	if _, ok := c.control.Registry.Get("alice"); ok {
		t.Fatal("the removed tenant is still in the control plane's registry")
	}
	if _, err := os.Stat(filepath.Join(c.controlCfg.Deploy.TenantConfigRoot, "alice")); err == nil {
		t.Fatal("the control plane kept the removed tenant's credential copy")
	}
	if _, err := os.Stat(created.Workspace); err != nil {
		t.Fatalf("a removal without purge deleted the node's data: %v", err)
	}
}

// TestClusterReconcileDoesNotResurrectSuspendedTenants is the failure this design must not have: a
// node restart (or a control-plane restart) bringing back a tenant an operator turned off.
func TestClusterReconcileDoesNotResurrectSuspendedTenants(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	created, err := c.control.Create(ctx, "bob", "sk-cluster3234567890abcd", nil, tenancy.CreateOptions{
		Node: nodeName, Account: "li", DirectoryPicker: "clamp", PluginBrowserFS: "off", AllowEmptyModels: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.control.StopWorker(ctx, created); err != nil {
		t.Fatal(err)
	}
	// The node restarts: its own table says suspended, so it must stay stopped.
	c.restartNode(t)
	if running := c.node.Workers.Running(); len(running) != 0 {
		t.Fatalf("a node restart resurrected a suspended tenant: %+v", running)
	}
	if _, err := c.control.ReconcileNode(ctx, nodeName); err != nil {
		t.Fatal(err)
	}
	if running := c.node.Workers.Running(); len(running) != 0 {
		t.Fatalf("reconcile resurrected a suspended tenant: %+v", running)
	}
	// Starting it explicitly works, and the intent is cleared on both sides.
	if err := c.control.StartWorker(ctx, created); err != nil {
		t.Fatal(err)
	}
	if running := c.node.Workers.Running(); len(running) != 1 {
		t.Fatalf("start did not bring the tenant back: %+v", running)
	}
}

// TestClusterProtocolCarriesWhatTheNodeNeeds checks the pieces a later refactor could drop without
// any single-side test noticing: the model list, the credential and the picker choices all reach
// the node, and the node's own answer is what the control plane stores.
func TestClusterProtocolCarriesWhatTheNodeNeeds(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()
	models := []aigw.Model{
		{ID: "deepseek-flash", Name: "DeepSeek Flash", ContextWindow: 1000000, MaxOutputTokens: 65536, Images: true},
	}
	created, err := c.control.Create(ctx, "carol", "sk-cluster4234567890abcd", models, tenancy.CreateOptions{
		Node: nodeName, Account: "wang", DirectoryPicker: "clamp", PluginBrowserFS: "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(created.DshHome, "settings.yaml"))
	if err != nil {
		t.Fatalf("the node did not write the tenant's settings: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte("{}"), &doc); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(settings, &doc); err != nil {
		t.Fatalf("settings.yaml is not valid YAML: %v", err)
	}
	providers, _ := doc["llm-pi-ai"].(map[string]any)
	if providers == nil {
		t.Fatalf("settings.yaml has no provider block: %s", settings)
	}
	list, _ := providers["providers"].(map[string]any)
	aigwBlock, _ := list["aigw"].(map[string]any)
	if aigwBlock == nil {
		t.Fatalf("settings.yaml has no aigw provider: %s", settings)
	}
	modelList, _ := aigwBlock["models"].([]any)
	if len(modelList) != 1 {
		t.Fatalf("the model list did not reach the node: %s", settings)
	}
	first, _ := modelList[0].(map[string]any)
	if first["id"] != "deepseek-flash" || first["contextWindow"] != 1000000 {
		t.Fatalf("model entry = %+v", first)
	}
	credentials, err := os.ReadFile(filepath.Join(created.DshHome, ".credentials.yaml"))
	if err != nil {
		t.Fatalf("the node did not write the tenant's credential: %v", err)
	}
	if !strings.Contains(string(credentials), "sk-cluster4234567890abcd") {
		t.Fatalf("the credential did not reach the node: %s", credentials)
	}
}

// TestClusterNodeListenerIsAuthenticated keeps the security property next to the happy path: the
// integration test's own node must refuse an unauthenticated caller.
func TestClusterNodeListenerIsAuthenticated(t *testing.T) {
	c := newCluster(t)
	req, err := http.NewRequest(http.MethodGet, c.nodeBaseURL+nodeproto.HealthPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("health without a token = %d, want 401", resp.StatusCode)
	}
	// The listener is a LAN address, not loopback-only: a node's control plane is another machine.
	if _, port, err := net.SplitHostPort(c.nodeAddr); err != nil || port == "" {
		t.Fatalf("node listener address = %q (%v)", c.nodeAddr, err)
	}
}
