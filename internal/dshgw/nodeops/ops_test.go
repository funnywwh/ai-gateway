package nodeops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/session"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// fixture builds a node-side Ops around a real manager whose workers are stand-in processes.
//
// It is the same shape as the tenancy tests' fixture, on purpose: the node's handlers are thin,
// and what they need to be tested against is the real lifecycle (directories, registry, handshake,
// suspension) rather than a mock of it.
func fixture(t *testing.T) (*Ops, *tenancy.Manager, *registry.Registry, string) {
	t.Helper()
	root := t.TempDir()
	tpl := filepath.Join(root, "template")
	writeTemplate(t, tpl)
	release := filepath.Join(root, "dsh", "releases", "r1")
	nodeBin := filepath.Join(root, "dsh", "node", "bin", "node")
	currentLink := filepath.Join(root, "dsh", "current")
	for _, dir := range []string{filepath.Join(release, "lib"), filepath.Dir(nodeBin)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(release, "lib", "bin.js"), []byte("// dsh launcher\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, currentLink); err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	picker := filepath.Join(plugin, "picker-clamp.js")
	if err := os.WriteFile(picker, []byte("// picker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		PublicHost: "node.test", PortalPort: 32600, TenantPortLo: 32601, TenantPortHi: 32605,
		WorkerPortLo: 32900, WorkerPortHi: 32910, Listen: "127.0.0.1:3099",
		AigwBaseURL: "http://aigw", SessionTTL: config.Duration(1), DirectoryPicker: "clamp",
		PluginBrowserFS: "on", WorkspaceSeed: []string{"work"}, ReservedNames: []string{"login"},
		Dsh:      config.DshRuntime{NodeBin: nodeBin, BinJS: filepath.Join(release, "lib", "bin.js"), CurrentLink: currentLink},
		StateDir: filepath.Join(root, "state"), TenantRoot: filepath.Join(root, "state/tenants"),
		WorkspaceRoot: filepath.Join(root, "srv"), HandshakeDir: filepath.Join(root, "handshake"),
		RegistryPath: filepath.Join(root, "state/registry.json"), KeyMapPath: filepath.Join(root, "state/keys.map"),
		Deploy: config.DeployConfig{TemplateHome: tpl, PluginPath: picker, ConfigPath: filepath.Join(root, "etc/dshgw-node.yaml"),
			TenantConfigRoot: filepath.Join(root, "state/tenant-config"), BackupDir: filepath.Join(root, "backups"),
			WorkerUser: "dshgw", BwrapBin: "/usr/bin/bwrap"},
	}
	for _, dir := range []string{cfg.StateDir, cfg.HandshakeDir, filepath.Dir(cfg.Deploy.ConfigPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.Deploy.ConfigPath, []byte("test: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(root, "fake-worker.sh")
	script := "#!/bin/sh\nport=\"$1\"\necho \"dsh web: http://127.0.0.1:$port/?token=" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\ntrap 'exit 0' TERM INT\nwhile :; do sleep 0.2; done\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &tenancy.WorkerRunner{
		Config: cfg,
		Profile: func(tn registry.Tenant) ([]string, error) {
			return []string{worker, itoa(tn.WorkerPort)}, nil
		},
		StopTimeout: 5 * time.Second,
	}
	reg := registry.New(cfg.RegistryPath, cfg.KeyMapPath)
	manager := &tenancy.Manager{
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
	ops := &Ops{Config: cfg, Manager: manager, Registry: reg, Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}
	return ops, manager, reg, root
}

func writeTemplate(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, "profiles/web")
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `{"dependencies":{"dsh-browser-fs":"0.2.0"},"dsh":{"profile":{"bundles":["@deepseek-ai/dsh-base","@deepseek-ai/dsh-web-app","dsh-browser-fs"]}}}`
	if err := os.WriteFile(filepath.Join(p, "package.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "cordis.patch.yml"), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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

// createTenant runs the create handler the way the control plane would.
func createTenant(t *testing.T, ops *Ops, name, key string, public int) nodeproto.TenantState {
	t.Helper()
	body, err := json.Marshal(nodeproto.TenantCreateRequest{
		Spec:            nodeproto.TenantSpec{Name: name, PublicPort: public, Account: "chen"},
		Key:             key,
		Models:          nodeproto.ModelsToSpec([]aigw.Model{{ID: "deepseek-flash", ContextWindow: 1000000}}),
		DirectoryPicker: "clamp",
		PluginBrowserFS: "on",
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := ops.Handlers()["tenant-create"](context.Background(), body)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	state, ok := value.(nodeproto.TenantState)
	if !ok {
		t.Fatalf("create answered %T", value)
	}
	return state
}

func call(t *testing.T, ops *Ops, op string, request any) (any, error) {
	t.Helper()
	handler, ok := ops.Handlers()[op]
	if !ok {
		t.Fatalf("operation %q is not registered", op)
	}
	var body []byte
	if request != nil {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		body = encoded
	}
	return handler(context.Background(), body)
}

// TestHandlersCoverEveryProtocolOperation keeps the table honest: a protocol operation nobody
// registered answers 501 to the control plane, which looks like an old node build.
func TestHandlersCoverEveryProtocolOperation(t *testing.T) {
	ops, _, _, _ := fixture(t)
	handlers := ops.Handlers()
	for _, op := range []string{"status", "reconcile", "tenant-create", "tenant-adopt", "tenant-start",
		"tenant-stop", "tenant-restart", "tenant-remove", "tenant-set-key", "tenant-sync-models",
		"tenant-ensure-running", "tenant-ensure-provisioned", "tenant-capture-url", "tenant-logout-stop",
		"handshake", "audit-tail"} {
		if handlers[op] == nil {
			t.Errorf("operation %q has no handler", op)
		}
	}
	if len(handlers) != 16 {
		t.Errorf("handler table has %d entries", len(handlers))
	}
}

func TestCreateUsesTheControlPlanesPublicPortAndItsOwnWorkerPort(t *testing.T) {
	ops, _, reg, _ := fixture(t)
	state := createTenant(t, ops, "alice", "sk-nodecreate1234567890ab", 32603)

	if state.Name != "alice" || state.PublicPort != 32603 {
		t.Fatalf("state = %+v", state)
	}
	if state.WorkerPort < 32900 || state.WorkerPort > 32910 {
		t.Fatalf("the node must allocate its own worker port, got %d", state.WorkerPort)
	}
	if !state.Running {
		t.Fatalf("a created tenant's worker must be running: %+v", state)
	}
	if !strings.HasPrefix(state.DshHome, ops.Config.TenantRoot) || !strings.HasPrefix(state.Workspace, ops.Config.WorkspaceRoot) {
		t.Fatalf("the node must derive its own paths: %+v", state)
	}
	stored, ok := reg.Get("alice")
	if !ok {
		t.Fatal("the tenant is not in the node's allocation table")
	}
	// A node-local record is local by definition, and carries no key prefix (that belongs to the
	// control plane).
	if !ops.Config.IsLocalNode(stored.Node) {
		t.Fatalf("a node recorded a placement for itself: %+v", stored)
	}
	if stored.KeyPrefix != "" {
		t.Fatalf("a node recorded a key prefix: %q", stored.KeyPrefix)
	}
	// The record must survive a save/load cycle, which is what a node restart does.
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := registry.Load(ops.Config.RegistryPath, ops.Config.KeyMapPath)
	if err != nil {
		t.Fatalf("a node's allocation table must reload: %v", err)
	}
	if _, ok := reloaded.Get("alice"); !ok {
		t.Fatal("alice did not survive the reload")
	}
}

func TestCreateRefusesIncompleteOrDuplicateRequests(t *testing.T) {
	ops, _, _, _ := fixture(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		request nodeproto.TenantCreateRequest
		code    string
	}{
		{"no name", nodeproto.TenantCreateRequest{Spec: nodeproto.TenantSpec{PublicPort: 32601}, Key: "k"}, nodeproto.CodeBadRequest},
		{"no key", nodeproto.TenantCreateRequest{Spec: nodeproto.TenantSpec{Name: "alice", PublicPort: 32601}}, nodeproto.CodeBadRequest},
		{"no public port", nodeproto.TenantCreateRequest{Spec: nodeproto.TenantSpec{Name: "alice"}, Key: "k"}, nodeproto.CodeBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.request)
			_, err := ops.Handlers()["tenant-create"](ctx, body)
			if !nodeproto.IsCode(err, tc.code) {
				t.Fatalf("err = %v (code %q)", err, nodeproto.CodeOf(err))
			}
		})
	}
	if _, err := ops.Handlers()["tenant-create"](ctx, []byte("{not json")); !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("malformed body err = %v", err)
	}
	if _, err := ops.Handlers()["tenant-create"](ctx, nil); !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("empty body err = %v", err)
	}

	createTenant(t, ops, "alice", "sk-nodedupe1234567890abcd", 32601)
	body, _ := json.Marshal(nodeproto.TenantCreateRequest{Spec: nodeproto.TenantSpec{Name: "alice", PublicPort: 32602}, Key: "sk-nodedupe1234567890abcd"})
	if _, err := ops.Handlers()["tenant-create"](ctx, body); !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("duplicate create err = %v", err)
	}
}

func TestUnknownTenantIsReportedAsSuch(t *testing.T) {
	ops, _, _, _ := fixture(t)
	for _, op := range []string{"tenant-start", "tenant-stop", "tenant-restart", "tenant-ensure-running", "tenant-capture-url", "tenant-logout-stop"} {
		_, err := call(t, ops, op, nodeproto.TenantRef{Name: "ghost"})
		if !nodeproto.IsCode(err, nodeproto.CodeTenantUnknown) {
			t.Errorf("%s on an unknown tenant = %v (code %q)", op, err, nodeproto.CodeOf(err))
		}
	}
	if _, err := call(t, ops, "tenant-remove", nodeproto.TenantRemoveRequest{Name: "ghost"}); !nodeproto.IsCode(err, nodeproto.CodeTenantUnknown) {
		t.Errorf("remove on an unknown tenant = %v", err)
	}
	if _, err := call(t, ops, "tenant-sync-models", nodeproto.TenantSyncModelsRequest{Name: "ghost"}); !nodeproto.IsCode(err, nodeproto.CodeTenantUnknown) {
		t.Errorf("sync-models on an unknown tenant = %v", err)
	}
	if _, err := call(t, ops, "tenant-set-key", nodeproto.TenantSetKeyRequest{Name: "ghost", Key: "k"}); !nodeproto.IsCode(err, nodeproto.CodeTenantUnknown) {
		t.Errorf("set-key on an unknown tenant = %v", err)
	}
	if _, err := call(t, ops, "tenant-ensure-provisioned", nodeproto.TenantEnsureProvisionedRequest{Name: "ghost", Key: "k"}); !nodeproto.IsCode(err, nodeproto.CodeTenantUnknown) {
		t.Errorf("ensure-provisioned on an unknown tenant = %v", err)
	}
}

func TestLifecycleHandlersDriveTheWorkers(t *testing.T) {
	ops, manager, reg, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-nodelifecycle1234567ab", 32601)

	value, err := call(t, ops, "tenant-stop", nodeproto.TenantRef{Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if state := value.(nodeproto.TenantState); state.Running || !state.Suspended {
		t.Fatalf("after stop: %+v", state)
	}
	// The operator's intent is durable: a node restart must not bring the tenant back.
	stored, _ := reg.Get("alice")
	if !stored.Suspended {
		t.Fatal("stop did not record the operator's intent")
	}

	if _, err := call(t, ops, "tenant-ensure-running", nodeproto.TenantRef{Name: "alice"}); err == nil {
		t.Fatal("ensure-running must refuse a suspended tenant")
	}
	if _, err := call(t, ops, "tenant-start", nodeproto.TenantRef{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if stored, _ := reg.Get("alice"); stored.Suspended {
		t.Fatal("start did not clear the suspension")
	}
	if value, err = call(t, ops, "tenant-ensure-running", nodeproto.TenantRef{Name: "alice"}); err != nil {
		t.Fatal(err)
	} else if result := value.(nodeproto.TenantEnsureRunningResult); result.Started {
		t.Fatal("ensure-running started a worker that was already running")
	}
	if _, err := call(t, ops, "tenant-restart", nodeproto.TenantRef{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, ops, "tenant-capture-url", nodeproto.TenantRef{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	// The logout teardown runs the whole ordered sequence on this machine.
	if _, err := call(t, ops, "tenant-logout-stop", nodeproto.TenantRef{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if status, _ := manager.Status(context.Background(), stored); status.Running {
		t.Fatal("logout-stop left the worker running")
	}
	// A stopped-by-logout tenant is not suspended; the next login starts it again.
	if stored, _ := reg.Get("alice"); stored.Suspended {
		t.Fatal("logout-stop must not record an operator suspension")
	}
}

func TestStatusListsEveryTenantWithItsRuntime(t *testing.T) {
	ops, _, _, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-nodestatus1234567890ab", 32601)
	createTenant(t, ops, "bob", "sk-nodestatus2234567890ab", 32602)
	if _, err := call(t, ops, "tenant-stop", nodeproto.TenantRef{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	value, err := call(t, ops, "status", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(nodeproto.Status)
	if status.Health.Tenants != 2 || status.Health.Running != 1 || status.Health.Suspended != 1 {
		t.Fatalf("health = %+v", status.Health)
	}
	byName := map[string]nodeproto.TenantState{}
	for _, tenant := range status.Tenants {
		byName[tenant.Name] = tenant
	}
	if !byName["alice"].Running || byName["bob"].Running {
		t.Fatalf("tenants = %+v", status.Tenants)
	}
	if byName["alice"].WorkerPort == 0 || byName["alice"].DshHome == "" {
		t.Fatalf("alice = %+v", byName["alice"])
	}
}

// TestReconcileAdoptsIdentityAndOwnsPorts is the node's half of the drift policy.
func TestReconcileAdoptsIdentityAndOwnsPorts(t *testing.T) {
	ops, _, reg, _ := fixture(t)
	state := createTenant(t, ops, "alice", "sk-nodereconcile1234567ab", 32601)
	workerPort := state.WorkerPort

	// The control plane now believes something different (another public port and account) and
	// lists a tenant this node has never provisioned.
	value, err := call(t, ops, "reconcile", nodeproto.ReconcileRequest{
		Tenants: []nodeproto.TenantSpec{
			{Name: "alice", PublicPort: 32604, Account: "li"},
			{Name: "bob", PublicPort: 32605, Account: "chen"},
		},
		Prune: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(nodeproto.ReconcileResult)
	stored, ok := reg.Get("alice")
	if !ok {
		t.Fatal("reconcile dropped a tenant the control plane listed")
	}
	if stored.PublicPort != 32604 || stored.Account != "li" {
		t.Fatalf("reconcile did not adopt the control plane's identity: %+v", stored)
	}
	if stored.WorkerPort != workerPort {
		t.Fatalf("reconcile changed the worker port from %d to %d", workerPort, stored.WorkerPort)
	}
	// The unknown tenant is reported, not invented.
	if len(result.Status.Tenants) != 2 {
		t.Fatalf("result = %+v", result)
	}
	var sawBob bool
	for _, tenant := range result.Status.Tenants {
		if tenant.Name == "bob" {
			sawBob = true
			if tenant.Running {
				t.Fatalf("bob must not be reported as running: %+v", tenant)
			}
		}
	}
	if !sawBob {
		t.Fatal("bob is missing from the reconcile answer")
	}
	if _, exists := reg.Get("bob"); exists {
		t.Fatal("reconcile registered a tenant it cannot provision")
	}
}

func TestReconcileAppliesSuspensionAndPrunesUnknownRecords(t *testing.T) {
	ops, manager, reg, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-nodeprune1234567890ab", 32601)
	createTenant(t, ops, "orphan", "sk-nodeprune2234567890ab", 32602)
	alice, _ := reg.Get("alice")

	// The control plane suspends alice and no longer knows about orphan.
	value, err := call(t, ops, "reconcile", nodeproto.ReconcileRequest{
		Tenants: []nodeproto.TenantSpec{{Name: "alice", PublicPort: alice.PublicPort, Account: alice.Account, Suspended: true}},
		Prune:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(nodeproto.ReconcileResult)
	if len(result.Stopped) != 2 {
		t.Fatalf("reconcile stopped %v, want alice and orphan", result.Stopped)
	}
	if len(result.Pruned) != 1 || result.Pruned[0] != "orphan" {
		t.Fatalf("pruned = %v", result.Pruned)
	}
	if _, ok := reg.Get("orphan"); ok {
		t.Fatal("the pruned record is still in the allocation table")
	}
	// The pruned tenant's data is untouched: reconciling is not a purge.
	if _, err := os.Stat(filepath.Join(ops.Config.WorkspaceRoot, "orphan")); err != nil {
		t.Fatalf("reconcile deleted a pruned tenant's data: %v", err)
	}
	stored, _ := reg.Get("alice")
	if !stored.Suspended {
		t.Fatal("reconcile did not record the suspension")
	}
	if status, _ := manager.Status(context.Background(), stored); status.Running {
		t.Fatal("a suspended tenant's worker is still running")
	}
	// Nothing is pruned unless asked.
	createTenant(t, ops, "carol", "sk-nodekeep1234567890ab", 32603)
	if _, err := call(t, ops, "reconcile", nodeproto.ReconcileRequest{Tenants: nil}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("carol"); !ok {
		t.Fatal("a reconcile without prune removed a local record")
	}
	// A tenant the control plane no longer lists is not started again just because it exists.
	if status, _ := manager.Status(context.Background(), stored); status.Running {
		t.Fatal("reconcile started a suspended tenant")
	}
}

func TestAdoptRequiresTheDataAndNeverStartsAWorker(t *testing.T) {
	ops, manager, reg, _ := fixture(t)
	ctx := context.Background()

	// No data: refused, with the path the operator has to copy to.
	_, err := call(t, ops, "tenant-adopt", nodeproto.TenantSpec{Name: "alice", PublicPort: 32601})
	if !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("adopt without data = %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(ops.Config.TenantRoot, "alice")) {
		t.Fatalf("the refusal must name the path to copy to: %v", err)
	}

	// The operator copies the tenant's directories over; now adoption registers it.
	dshHome := filepath.Join(ops.Config.TenantRoot, "alice", ".dsh")
	workspace := filepath.Join(ops.Config.WorkspaceRoot, "alice")
	for _, dir := range []string{dshHome, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dshHome, "settings.yaml"), []byte("llm-pi-ai: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := call(t, ops, "tenant-adopt", nodeproto.TenantSpec{Name: "alice", PublicPort: 32602, Account: "chen"})
	if err != nil {
		t.Fatal(err)
	}
	state := value.(nodeproto.TenantState)
	if state.Running {
		t.Fatalf("adoption must not start a worker: %+v", state)
	}
	if state.PublicPort != 32602 || state.DshHome != dshHome || state.Workspace != workspace {
		t.Fatalf("state = %+v", state)
	}
	stored, ok := reg.Get("alice")
	if !ok {
		t.Fatal("the adopted tenant is not in the allocation table")
	}
	if stored.WorkerPort < 32900 || stored.WorkerPort > 32910 {
		t.Fatalf("adopt did not allocate a worker port: %+v", stored)
	}
	if stored.KeyPrefix != "" {
		t.Fatalf("an adopted record invented a key prefix: %q", stored.KeyPrefix)
	}
	if status, _ := manager.Status(ctx, stored); status.Running {
		t.Fatal("the adopted tenant's worker was started")
	}
	// Adopting twice is a mistake worth reporting, not a silent second registration.
	if _, err := call(t, ops, "tenant-adopt", nodeproto.TenantSpec{Name: "alice", PublicPort: 32602}); !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("second adopt = %v", err)
	}
	// The explicit start that follows works.
	if _, err := call(t, ops, "tenant-start", nodeproto.TenantRef{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if status, _ := manager.Status(ctx, stored); !status.Running {
		t.Fatal("the adopted tenant did not start")
	}
}

func TestRemoveKeepsDataUnlessPurged(t *testing.T) {
	ops, _, reg, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-noderemove1234567890ab", 32601)
	if _, err := call(t, ops, "tenant-stop", nodeproto.TenantRef{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	value, err := call(t, ops, "tenant-remove", nodeproto.TenantRemoveRequest{Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(nodeproto.TenantRemoveResult)
	if result.Snapshot == "" {
		t.Fatal("removal must snapshot before dropping anything")
	}
	if _, ok := reg.Get("alice"); ok {
		t.Fatal("the removed tenant is still registered")
	}
	if _, err := os.Stat(filepath.Join(ops.Config.WorkspaceRoot, "alice")); err != nil {
		t.Fatalf("a removal without purge deleted the workspace: %v", err)
	}
}

func TestSetKeyAndSyncModelsReachTheTenantsFiles(t *testing.T) {
	ops, _, reg, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-nodekey1234567890abcd", 32601)
	newKey := "sk-nodekey2234567890abcd"
	if _, err := call(t, ops, "tenant-set-key", nodeproto.TenantSetKeyRequest{Name: "alice", Account: "li", Key: newKey, KeepPrevious: true}); err != nil {
		t.Fatal(err)
	}
	stored, _ := reg.Get("alice")
	if stored.Account != "li" {
		t.Fatalf("set-key did not record the account: %+v", stored)
	}
	credentials, err := os.ReadFile(filepath.Join(stored.DshHome, ".credentials.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(credentials), newKey) {
		t.Fatalf("the rotated credential is not in .credentials.yaml: %s", credentials)
	}
	// The credential file is written on the node, under its own paths.
	if _, err := call(t, ops, "tenant-sync-models", nodeproto.TenantSyncModelsRequest{
		Name: "alice", Models: nodeproto.ModelsToSpec([]aigw.Model{{ID: "deepseek-flash", ContextWindow: 200000}}),
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(stored.DshHome, "settings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(settings), "deepseek-flash") {
		t.Fatalf("settings.yaml does not carry the model list: %s", settings)
	}
	if _, err := call(t, ops, "tenant-set-key", nodeproto.TenantSetKeyRequest{Name: "alice"}); !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("set-key without a key = %v", err)
	}
}

func TestEnsureProvisionedWritesWhatIsMissing(t *testing.T) {
	ops, _, reg, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-nodeprov1234567890abcd", 32601)
	stored, _ := reg.Get("alice")
	// Simulate the state the op exists for: a tenant whose dsh never got its settings.
	for _, name := range []string{"settings.yaml", ".credentials.yaml"} {
		if err := os.Remove(filepath.Join(stored.DshHome, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	value, err := call(t, ops, "tenant-ensure-provisioned", nodeproto.TenantEnsureProvisionedRequest{
		Name: "alice", Key: "sk-nodeprov1234567890abcd",
		Models: nodeproto.ModelsToSpec([]aigw.Model{{ID: "deepseek-flash"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !value.(nodeproto.TenantEnsureProvisionedResult).Provisioned {
		t.Fatal("ensure-provisioned did not report writing the files")
	}
	if _, err := os.Stat(filepath.Join(stored.DshHome, "settings.yaml")); err != nil {
		t.Fatalf("settings.yaml was not written: %v", err)
	}
	// It is idempotent: a provisioned tenant reports "nothing to do".
	value, err = call(t, ops, "tenant-ensure-provisioned", nodeproto.TenantEnsureProvisionedRequest{
		Name: "alice", Key: "sk-nodeprov1234567890abcd",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.(nodeproto.TenantEnsureProvisionedResult).Provisioned {
		t.Fatal("ensure-provisioned rewrote a provisioned tenant")
	}
}

// TestHandshakeRunsOnThisMachine is the multi-machine handshake (M77): the token file and the
// loopback socket are here, so the exchange is here — and it must use the authority the control
// plane will present on every forwarded request.
func TestHandshakeRunsOnThisMachine(t *testing.T) {
	ops, _, reg, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-nodehandshake1234567ab", 32601)
	tenant, _ := reg.Get("alice")

	var askedURL, askedAuthority string
	ops.TokenURL = func(name string) (string, error) {
		return "http://127.0.0.1:" + itoa(tenant.WorkerPort) + "/?token=" + strings.Repeat("a", 43), nil
	}
	ops.Exchange = func(_ context.Context, tokenURL, authority string) (*session.Upstream, error) {
		askedURL, askedAuthority = tokenURL, authority
		return &session.Upstream{Name: "dsh-auth-test", Value: "cookie", Authority: authority, ExpiresAt: time.Unix(1700003600, 0)}, nil
	}
	authority := "127.0.0.1:" + itoa(tenant.WorkerPort)
	value, err := call(t, ops, "handshake", nodeproto.TenantHandshakeRequest{Name: "alice", Authority: authority})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(nodeproto.TenantHandshakeResult)
	if result.Name != "dsh-auth-test" || result.Value != "cookie" || result.Authority != authority {
		t.Fatalf("result = %+v", result)
	}
	if askedAuthority != authority || !strings.Contains(askedURL, "token=") {
		t.Fatalf("exchange called with url=%q authority=%q", askedURL, askedAuthority)
	}
	if result.ExpiresAt.IsZero() {
		t.Fatal("the expiry must travel back: the control plane stores it with the cookie")
	}

	// A stale control-plane record (another worker port) is refused: answering anyway would hand
	// back a cookie its own requests cannot use.
	if _, err := call(t, ops, "handshake", nodeproto.TenantHandshakeRequest{Name: "alice", Authority: "127.0.0.1:9999"}); !nodeproto.IsCode(err, nodeproto.CodeBadRequest) {
		t.Fatalf("mismatched authority = %v", err)
	}
	// An unknown tenant never reaches the token file.
	if _, err := call(t, ops, "handshake", nodeproto.TenantHandshakeRequest{Name: "ghost", Authority: authority}); !nodeproto.IsCode(err, nodeproto.CodeTenantUnknown) {
		t.Fatalf("unknown tenant = %v", err)
	}
	// A missing startup token means the worker is not running.
	ops.TokenURL = func(string) (string, error) { return "", errNoToken }
	if _, err := call(t, ops, "handshake", nodeproto.TenantHandshakeRequest{Name: "alice", Authority: authority}); !nodeproto.IsCode(err, nodeproto.CodeWorkerNotRunning) {
		t.Fatalf("missing token = %v", err)
	}
	// A failed exchange is reported as "no worker", not as an internal failure: the control plane
	// answers 503 and the next request re-handshakes.
	ops.TokenURL = func(name string) (string, error) {
		return "http://127.0.0.1:" + itoa(tenant.WorkerPort) + "/?token=" + strings.Repeat("a", 43), nil
	}
	ops.Exchange = func(context.Context, string, string) (*session.Upstream, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	if _, err := call(t, ops, "handshake", nodeproto.TenantHandshakeRequest{Name: "alice", Authority: authority}); !nodeproto.IsCode(err, nodeproto.CodeWorkerNotRunning) {
		t.Fatalf("failed exchange = %v", err)
	}
}

var errNoToken = errors.New("no handshake file")

// TestHandshakeResultIsNeverEmpty pins the check the control plane also makes: a node must not
// answer with a half-credential, because that would be stored as a session and then fail.
func TestHandshakeResultIsNeverEmpty(t *testing.T) {
	ops, _, reg, _ := fixture(t)
	createTenant(t, ops, "alice", "sk-nodehandshake2234567ab", 32601)
	tenant, _ := reg.Get("alice")
	ops.TokenURL = func(string) (string, error) {
		return "http://127.0.0.1:" + itoa(tenant.WorkerPort) + "/?token=" + strings.Repeat("a", 43), nil
	}
	ops.Exchange = func(_ context.Context, _ string, authority string) (*session.Upstream, error) {
		return &session.Upstream{Name: "dsh-auth-test", Value: "cookie", Authority: "127.0.0.1:1"}, nil
	}
	authority := "127.0.0.1:" + itoa(tenant.WorkerPort)
	if _, err := call(t, ops, "handshake", nodeproto.TenantHandshakeRequest{Name: "alice", Authority: authority}); err == nil {
		t.Fatal("an exchanger that answers with another authority must be refused")
	}
}

// TestAuditTailServesThisNodesEvents is the node half of the single-audit-stream promise (M77):
// the control plane reads the node's file through a cursor, and the first read starts at the end
// so a gateway never imports a node's whole history on first contact.
func TestAuditTailServesThisNodesEvents(t *testing.T) {
	ops, _, _, root := fixture(t)
	path := filepath.Join(root, "state", "gateway")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(path, "audit.jsonl")
	ops.AuditPath = auditPath

	// Nothing written yet: an empty answer with a usable cursor.
	value, err := call(t, ops, "audit-tail", nodeproto.AuditTailRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first := value.(nodeproto.AuditTailResult)
	if len(first.Lines) != 0 || first.Cursor == "" {
		t.Fatalf("first = %+v", first)
	}

	// Two events appear; the first read with that cursor returns exactly them.
	write := func(kind string) {
		t.Helper()
		file, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteString(`{"time":"2026-09-23T01:00:00Z","kind":"` + kind + `","tenant":"alice","reason":"x","status":403}` + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	write("ssh-mount-refused")
	write("browsermount.expire")

	value, err = call(t, ops, "audit-tail", nodeproto.AuditTailRequest{Cursor: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	batch := value.(nodeproto.AuditTailResult)
	if len(batch.Lines) != 2 || !strings.Contains(batch.Lines[0], "ssh-mount-refused") || !strings.Contains(batch.Lines[1], "browsermount.expire") {
		t.Fatalf("batch = %+v", batch)
	}
	// Reading again with the new cursor yields nothing: the cursor advanced.
	value, err = call(t, ops, "audit-tail", nodeproto.AuditTailRequest{Cursor: batch.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if again := value.(nodeproto.AuditTailResult); len(again.Lines) != 0 {
		t.Fatalf("re-read returned %+v", again)
	}

	// Last asks for the newest lines and does not consume the cursor.
	value, err = call(t, ops, "audit-tail", nodeproto.AuditTailRequest{Last: 1})
	if err != nil {
		t.Fatal(err)
	}
	tail := value.(nodeproto.AuditTailResult)
	if len(tail.Lines) != 1 || !strings.Contains(tail.Lines[0], "browsermount.expire") {
		t.Fatalf("tail = %+v", tail)
	}

	// A file that shrank (rotation) restarts at the beginning and reports the gap.
	if err := os.WriteFile(auditPath, []byte(`{"time":"2026-09-23T02:00:00Z","kind":"after-rotation","reason":"x","status":200}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err = call(t, ops, "audit-tail", nodeproto.AuditTailRequest{Cursor: batch.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	rotated := value.(nodeproto.AuditTailResult)
	if len(rotated.Lines) != 1 || !strings.Contains(rotated.Lines[0], "after-rotation") || rotated.Dropped == 0 {
		t.Fatalf("rotated = %+v", rotated)
	}

	// A half-written trailing line is left for the next read instead of being served as JSON.
	file, err := os.OpenFile(auditPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"time":"2026-09-23T03:00:00Z","kind":"partial"`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	value, err = call(t, ops, "audit-tail", nodeproto.AuditTailRequest{Cursor: rotated.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if partial := value.(nodeproto.AuditTailResult); len(partial.Lines) != 0 {
		t.Fatalf("a partial line was served: %+v", partial)
	}
}

func TestAuditTailWithoutAFileIsEmpty(t *testing.T) {
	ops, _, _, root := fixture(t)
	ops.AuditPath = filepath.Join(root, "absent", "audit.jsonl")
	value, err := call(t, ops, "audit-tail", nodeproto.AuditTailRequest{})
	if err != nil {
		t.Fatalf("a node that has never logged anything must answer empty, not fail: %v", err)
	}
	if result := value.(nodeproto.AuditTailResult); len(result.Lines) != 0 || result.Cursor == "" {
		t.Fatalf("result = %+v", result)
	}
}
