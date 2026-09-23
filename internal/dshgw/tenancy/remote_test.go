package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/hostshare"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// fakeNode is a stand-in worker node: it records which operations the control plane sent and
// answers with the values a real node would.
//
// The point of these tests is the control plane's half of the protocol — which operation each
// lifecycle call turns into, what it sends, and what it records from the answer — so the node
// side is deliberately dumb here and the real node side is exercised by nodeops' own tests.
type fakeNode struct {
	server *httptest.Server
	token  string

	mu       sync.Mutex
	ops      []string
	requests map[string][]byte

	respond func(op string, body []byte) (int, any)
}

func newFakeNode(t *testing.T, respond func(op string, body []byte) (int, any)) *fakeNode {
	t.Helper()
	node := &fakeNode{token: "node-token", requests: map[string][]byte{}, respond: respond}
	mux := http.NewServeMux()
	mux.HandleFunc(nodeproto.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, node.token) {
			nodeproto.WriteError(w, http.StatusUnauthorized, nodeproto.CodeAuthFailed, "bad token")
			return
		}
		nodeproto.WriteValue(w, http.StatusOK, nodeproto.Health{
			Name: "node-a", Version: "test", Revision: "abc1234",
			Protocol: nodeproto.Version, StartedAt: time.Now().UTC(),
		})
	})
	mux.HandleFunc(nodeproto.ControlPath, func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, node.token) {
			nodeproto.WriteError(w, http.StatusUnauthorized, nodeproto.CodeAuthFailed, "bad token")
			return
		}
		op := strings.TrimPrefix(r.URL.Path, nodeproto.ControlPath)
		body, _ := io.ReadAll(r.Body)
		node.mu.Lock()
		node.ops = append(node.ops, op)
		node.requests[op] = body
		node.mu.Unlock()
		status, value := node.respond(op, body)
		if protoErr, ok := value.(*nodeproto.Error); ok {
			nodeproto.WriteError(w, status, protoErr.Code, protoErr.Message)
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		if value == nil {
			value = map[string]any{"ok": true}
		}
		nodeproto.WriteValue(w, status, value)
	})
	node.server = httptest.NewServer(mux)
	t.Cleanup(node.server.Close)
	return node
}

func authorized(r *http.Request, token string) bool {
	presented, ok := nodeproto.BearerToken(r.Header.Get("Authorization"))
	return ok && nodeproto.TokenMatches(token, presented)
}

func (f *fakeNode) calledOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *fakeNode) request(op string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw := f.requests[op]
	if raw == nil {
		return nil
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return decoded
}

// remoteFixture returns a manager whose node-a is the fake node, plus the fake itself.
func remoteFixture(t *testing.T, respond func(op string, body []byte) (int, any)) (*Manager, *WorkerRunner, *fakeNode) {
	t.Helper()
	m, runner, _ := managerFixture(t)
	node := newFakeNode(t, respond)
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: node.server.URL, Token: node.token}})
	if err != nil {
		t.Fatal(err)
	}
	m.Nodes = set
	return m, runner, node
}

// nodeState is the answer most operations need: a tenant running on the node.
func nodeState(name string, worker int) nodeproto.TenantState {
	return nodeproto.TenantState{
		Name: name, WorkerPort: worker, PublicPort: 32601,
		DshHome: "/node/tenants/" + name + "/.dsh", Workspace: "/node/srv/" + name,
		Running: true, Handshake: "ok",
	}
}

func defaultNodeResponder(t *testing.T) func(string, []byte) (int, any) {
	t.Helper()
	return func(op string, body []byte) (int, any) {
		switch op {
		case "status":
			return http.StatusOK, nodeproto.Status{
				Health:  nodeproto.Health{Name: "node-a", Protocol: nodeproto.Version, Tenants: 1, Running: 1},
				Tenants: []nodeproto.TenantState{nodeState("alice", 32901)},
			}
		case "tenant-create":
			return http.StatusOK, nodeState("alice", 32901)
		case "tenant-capture-url":
			return http.StatusOK, nodeproto.TenantCaptureURLResult{URL: "http://127.0.0.1:32901/?token=abcdefghijabcdefghijabcdefghijabcdefghijabc"}
		case "tenant-logout-stop":
			return http.StatusOK, nodeproto.TenantLogoutResult{MountsDetached: 2, MountsLeftover: []string{"/mnt/x"}, WorkerStopped: true}
		case "tenant-ensure-running":
			return http.StatusOK, nodeproto.TenantEnsureRunningResult{Started: true}
		case "tenant-ensure-provisioned":
			return http.StatusOK, nodeproto.TenantEnsureProvisionedResult{Provisioned: true}
		default:
			return http.StatusOK, nil
		}
	}
}

func TestRemoteCreateRecordsWhatTheNodeChose(t *testing.T) {
	m, runner, node := remoteFixture(t, defaultNodeResponder(t))
	key := "sk-remotecreate1234567890"
	models := []aigw.Model{{ID: "deepseek-flash", ContextWindow: 1000000}}

	created, err := m.Create(context.Background(), "alice", key, models, CreateOptions{Node: "node-a", Account: "chen"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Node != "node-a" {
		t.Fatalf("created.Node = %q", created.Node)
	}
	if created.WorkerPort != 32901 || created.DshHome != "/node/tenants/alice/.dsh" || created.Workspace != "/node/srv/alice" {
		t.Fatalf("created = %+v", created)
	}
	if created.PublicPort < 32601 || created.PublicPort > 32605 {
		t.Fatalf("the control plane must allocate the public port, got %d", created.PublicPort)
	}
	if created.KeyPrefix != key[:12] || created.Handshake != registry.HandshakeOK {
		t.Fatalf("created = %+v", created)
	}

	// What the control plane sent: identity + the port it owns + the credential and the models.
	request := node.request("tenant-create")
	if request == nil {
		t.Fatal("the node was never asked to create the tenant")
	}
	spec, _ := request["spec"].(map[string]any)
	if spec == nil || spec["name"] != "alice" || spec["account"] != "chen" {
		t.Fatalf("create request spec = %+v", request["spec"])
	}
	if got, _ := spec["public_port"].(float64); int(got) != created.PublicPort {
		t.Fatalf("create request public_port = %v, want %d", spec["public_port"], created.PublicPort)
	}
	if request["key"] != key {
		t.Fatalf("create request key = %v", request["key"])
	}
	modelsSent, _ := request["models"].([]any)
	if len(modelsSent) != 1 {
		t.Fatalf("create request models = %+v", request["models"])
	}

	// The registry entry and the control plane's copy of the credential.
	stored, ok := m.Registry.Get("alice")
	if !ok || stored.Node != "node-a" || stored.WorkerPort != 32901 {
		t.Fatalf("stored tenant = %+v (ok=%t)", stored, ok)
	}
	data, err := os.ReadFile(filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice", "gateway.key"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != key {
		t.Fatalf("gateway key copy = %q", data)
	}
	// Nothing was started on this machine.
	if running := runner.Running(); len(running) != 0 {
		t.Fatalf("a remote create started a local worker: %+v", running)
	}
}

func TestRemoteCreateFailureLeavesNoTrace(t *testing.T) {
	m, _, _ := remoteFixture(t, func(op string, body []byte) (int, any) {
		if op == "tenant-create" {
			return http.StatusServiceUnavailable, nodeproto.Errorf(nodeproto.CodeInternal, "the node could not start a worker")
		}
		return http.StatusOK, nil
	})
	_, err := m.Create(context.Background(), "alice", "sk-remotefail1234567890ab", nil, CreateOptions{Node: "node-a", AllowEmptyModels: true})
	if err == nil || !strings.Contains(err.Error(), "could not start a worker") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := m.Registry.Get("alice"); ok {
		t.Fatal("a failed remote create left a registry entry behind")
	}
	if _, err := os.Stat(filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice")); err == nil {
		t.Fatal("a failed remote create left the credential copy behind")
	}
}

// TestLifecycleDispatchesToTheNode walks every lifecycle method whose tenant may be remote and
// asserts the operation name it turns into. This is the table that keeps a later change from
// quietly executing a remote tenant's lifecycle on the control plane.
func TestLifecycleDispatchesToTheNode(t *testing.T) {
	m, runner, node := remoteFixture(t, defaultNodeResponder(t))
	alice := fixtureTenant(t, m, "alice", 32901)
	alice.PublicPort = 32601
	alice.Node = "node-a"
	if err := m.Registry.Put(alice); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := m.StartWorker(ctx, alice); err != nil {
		t.Fatalf("StartWorker: %v", err)
	}
	if err := m.StopWorker(ctx, alice); err != nil {
		t.Fatalf("StopWorker: %v", err)
	}
	if err := m.Restart(ctx, alice); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := m.ProbeWorker(ctx, alice); err != nil {
		t.Fatalf("ProbeWorker: %v", err)
	}
	if started, err := m.EnsureRunning(ctx, alice); err != nil || !started {
		t.Fatalf("EnsureRunning = %t, %v", started, err)
	}
	if err := m.SyncModels(alice, []aigw.Model{{ID: "m"}}); err != nil {
		t.Fatalf("SyncModels: %v", err)
	}
	if _, err := m.CaptureURL(ctx, alice); err != nil {
		t.Fatalf("CaptureURL: %v", err)
	}
	if _, err := m.EnsureProvisioned(ctx, alice, "sk-remoteensure12345678ab", nil); err != nil {
		t.Fatalf("EnsureProvisioned: %v", err)
	}
	if err := m.RotateKey(ctx, alice, "sk-remoterotate12345678ab", nil, true); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if err := m.Enable(ctx, alice, false); err != nil {
		t.Fatalf("Enable(false): %v", err)
	}
	if err := m.Enable(ctx, alice, true); err != nil {
		t.Fatalf("Enable(true): %v", err)
	}
	if state, err := m.Status(ctx, alice); err != nil || !state.Running {
		t.Fatalf("Status = %+v, %v", state, err)
	}
	if result, err := m.StopForLogout(ctx, alice); err != nil || result.MountsDetached != 2 || !result.WorkerStopped {
		t.Fatalf("StopForLogout = %+v, %v", result, err)
	}
	if _, err := m.SandboxProfile(alice); err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("SandboxProfile for a remote tenant = %v", err)
	}
	if err := m.SandboxProfileReady(alice); err == nil || !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("SandboxProfileReady for a remote tenant = %v", err)
	}

	if running := runner.Running(); len(running) != 0 {
		t.Fatalf("a remote lifecycle call started a local worker: %+v", running)
	}
	// The rotation updated the key map prefix and the credential copy, and kept the outgoing
	// prefix valid (keepPrevious), which is what stops a request signed with the old key from
	// failing during the changeover.
	stored, _ := m.Registry.Get("alice")
	if stored.KeyPrefix != "sk-remoterot" {
		t.Fatalf("key prefix after rotation = %q", stored.KeyPrefix)
	}
	if _, ok := m.Registry.ByPrefix(stored.KeyPrefix); !ok {
		t.Fatalf("the rotated prefix does not resolve: %+v", m.Registry.List())
	}
	if _, ok := m.Registry.ByPrefix("sk-aaaaaaaaa"); !ok {
		t.Fatal("keepPrevious did not retain the outgoing prefix")
	}
	data, err := os.ReadFile(filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice", "gateway.key"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "sk-remoterotate12345678ab" {
		t.Fatalf("gateway key copy = %q", data)
	}
	// The sync-models request carried the model list, not just a name.
	if request := node.request("tenant-sync-models"); request == nil || request["models"] == nil {
		t.Fatalf("sync-models request = %+v", request)
	}
	// Removal is the one operation that also forgets the control plane's own copies.
	if snapshot, err := m.Remove(ctx, alice, false); err != nil || snapshot != "" {
		t.Fatalf("Remove = %q, %v", snapshot, err)
	}
	if _, ok := m.Registry.Get("alice"); ok {
		t.Fatal("a removed remote tenant is still in the registry")
	}
	if _, err := os.Stat(filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice")); err == nil {
		t.Fatal("a removed remote tenant's credential copy is still on the control plane")
	}
	if ops := node.calledOps(); ops[len(ops)-1] != "tenant-remove" {
		t.Fatalf("last operation = %q", ops[len(ops)-1])
	}
	want := []string{
		"tenant-start", "tenant-stop", "tenant-restart", "status", "tenant-ensure-running",
		"tenant-sync-models", "tenant-capture-url", "tenant-ensure-provisioned", "tenant-set-key",
		"tenant-stop", "tenant-start", "status", "tenant-logout-stop", "tenant-remove",
	}
	got := node.calledOps()
	if len(got) != len(want) {
		t.Fatalf("node operations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("operation %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

}

// TestLocalTenantNeverCallsANode is the other half of the dispatch: a single-machine deployment
// (and a local tenant in a multi-machine one) must not touch the network.
func TestLocalTenantNeverCallsANode(t *testing.T) {
	m, runner, node := remoteFixture(t, defaultNodeResponder(t))
	alice := fixtureTenant(t, m, "alice", 32100)
	if err := m.Registry.Put(alice); err != nil {
		t.Fatal(err)
	}
	// The lifecycle's gate reloads the registry from disk (that is how a CLI process and the
	// serving process agree), so a test that drives it must persist first.
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.StartWorker(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := m.StopWorker(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if ops := node.calledOps(); len(ops) != 0 {
		t.Fatalf("a local tenant's lifecycle reached the node: %v", ops)
	}
	// The local path really did run: the stand-in worker went up and back down through the
	// runner, which is what a remote tenant's lifecycle must never touch.
	if running := runner.Running(); len(running) != 0 {
		t.Fatalf("after StopWorker the local runner still reports %+v", running)
	}
	if status := runner.Status(alice); status.Running {
		t.Fatalf("the local runner still reports a running process for a stopped tenant: %+v", status)
	}
}

func TestUnknownNodeIsRefusedNotFallenBack(t *testing.T) {
	m, runner, node := remoteFixture(t, defaultNodeResponder(t))
	alice := fixtureTenant(t, m, "alice", 32901)
	alice.Node = "node-z"
	if err := m.Registry.Put(alice); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	err := m.StartWorker(context.Background(), alice)
	if err == nil || !strings.Contains(err.Error(), "node-z") {
		t.Fatalf("err = %v", err)
	}
	if ops := node.calledOps(); len(ops) != 0 {
		t.Fatalf("an unknown node name reached node-a: %v", ops)
	}
	if running := runner.Running(); len(running) != 0 {
		t.Fatalf("a tenant on an unknown node was started locally: %+v", running)
	}

	// A control plane with no nodes configured must fail the same way rather than assume local.
	m.Nodes = nil
	err = m.StartWorker(context.Background(), alice)
	if err == nil || !strings.Contains(err.Error(), "no nodes configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdoptOnNode(t *testing.T) {
	m, _, node := remoteFixture(t, func(op string, body []byte) (int, any) {
		switch op {
		case "tenant-adopt":
			return http.StatusOK, nodeproto.TenantState{
				Name: "alice", WorkerPort: 32907, PublicPort: 32601,
				DshHome: "/node/tenants/alice/.dsh", Workspace: "/node/srv/alice",
				Handshake: "pending",
			}
		default:
			return http.StatusOK, nodeState("alice", 32907)
		}
	})
	alice := fixtureTenant(t, m, "alice", 32100)
	alice.Node = ""
	if err := m.Registry.Put(alice); err != nil {
		t.Fatal(err)
	}
	if err := m.AdoptOnNode(context.Background(), alice, "node-a"); err != nil {
		t.Fatal(err)
	}
	stored, _ := m.Registry.Get("alice")
	if stored.Node != "node-a" || stored.WorkerPort != 32907 || stored.Workspace != "/node/srv/alice" {
		t.Fatalf("stored = %+v", stored)
	}
	if request := node.request("tenant-adopt"); request == nil || request["name"] != "alice" {
		t.Fatalf("adopt request = %+v", request)
	}
	// The node's refusal to adopt data it does not have must reach the operator.
	node.respond = func(op string, body []byte) (int, any) {
		if op == "tenant-adopt" {
			return http.StatusBadRequest, nodeproto.Errorf(nodeproto.CodeBadRequest, "no tenant data to adopt at /node/tenants/bob/.dsh")
		}
		return http.StatusOK, nil
	}
	bob := fixtureTenant(t, m, "bob", 32101)
	bob.KeyPrefix = "sk-bbbbbbbbb"
	if err := m.Registry.Put(bob); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	err := m.AdoptOnNode(context.Background(), bob, "node-a")
	if err == nil || !strings.Contains(err.Error(), "no tenant data") {
		t.Fatalf("err = %v", err)
	}
	if stored, _ := m.Registry.Get("bob"); !m.Config.IsLocalNode(stored.Node) {
		t.Fatalf("a refused adoption still moved the tenant: %+v", stored)
	}
}

func TestReconcileNodeAppliesWhatTheNodeReports(t *testing.T) {
	m, _, node := remoteFixture(t, func(op string, body []byte) (int, any) {
		if op == "reconcile" {
			return http.StatusOK, nodeproto.ReconcileResult{
				Status: nodeproto.Status{
					Health: nodeproto.Health{Name: "node-a", Protocol: nodeproto.Version, Tenants: 1, Running: 1},
					Tenants: []nodeproto.TenantState{{
						Name: "alice", WorkerPort: 32908, PublicPort: 32602,
						DshHome: "/node/tenants/alice/.dsh", Workspace: "/node/srv/alice",
						Running: true, Handshake: "ok",
					}},
				},
			}
		}
		return http.StatusOK, nil
	})
	alice := fixtureTenant(t, m, "alice", 32100)
	alice.Node = "node-a"
	alice.Account = "chen"
	if err := m.Registry.Put(alice); err != nil {
		t.Fatal(err)
	}
	bob := fixtureTenant(t, m, "bob", 32101)
	bob.KeyPrefix = "sk-bbbbbbbbb"
	if err := m.Registry.Put(bob); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}

	result, err := m.ReconcileNode(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Status.Tenants) != 1 {
		t.Fatalf("result = %+v", result)
	}
	request := node.request("reconcile")
	if request == nil {
		t.Fatal("the node was never asked to reconcile")
	}
	tenants, _ := request["tenants"].([]any)
	if len(tenants) != 1 {
		t.Fatalf("the reconcile list must contain exactly the node's tenants: %+v", request["tenants"])
	}
	entry, _ := tenants[0].(map[string]any)
	if entry["name"] != "alice" || entry["account"] != "chen" {
		t.Fatalf("reconcile entry = %+v", entry)
	}
	if request["prune"] != true {
		t.Fatalf("reconcile must ask the node to prune: %+v", request)
	}
	// The port and paths the node reported are now what the control plane records.
	stored, _ := m.Registry.Get("alice")
	if stored.WorkerPort != 32908 || stored.DshHome != "/node/tenants/alice/.dsh" {
		t.Fatalf("stored after reconcile = %+v", stored)
	}
	// The local tenant was not touched.
	if other, _ := m.Registry.Get("bob"); !m.Config.IsLocalNode(other.Node) || other.WorkerPort != 32101 {
		t.Fatalf("bob = %+v", other)
	}
}

func TestApplyNodeStatusReportsUnknownTenants(t *testing.T) {
	m, _, _ := remoteFixture(t, defaultNodeResponder(t))
	alice := fixtureTenant(t, m, "alice", 32100)
	alice.Node = "node-a"
	if err := m.Registry.Put(alice); err != nil {
		t.Fatal(err)
	}
	status := nodeproto.Status{Tenants: []nodeproto.TenantState{
		{Name: "alice", WorkerPort: 32100, DshHome: alice.DshHome, Workspace: alice.Workspace},
		{Name: "ghost", WorkerPort: 32999, DshHome: "/node/g/.dsh", Workspace: "/node/srv/g"},
	}}
	drift := m.applyNodeStatus("node-a", status)
	if len(drift) != 1 || !strings.Contains(drift[0], "ghost") {
		t.Fatalf("drift = %v", drift)
	}
	if _, ok := m.Registry.Get("ghost"); ok {
		t.Fatal("a tenant only the node knows about must not be adopted silently")
	}
}

func TestRemoteUnreachableIsReportedAsSuch(t *testing.T) {
	m, _, node := remoteFixture(t, defaultNodeResponder(t))
	alice := fixtureTenant(t, m, "alice", 32901)
	alice.Node = "node-a"
	if err := m.Registry.Put(alice); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	// Pick the address up before closing the server, so the failure is a dial failure and not a
	// malformed address.
	node.server.Close()
	err := m.StartWorker(context.Background(), alice)
	if err == nil || !nodeproto.IsCode(errors.Unwrap(err), nodeproto.CodeUnreachable) {
		var proto *nodeproto.Error
		if !errors.As(err, &proto) || proto.Code != nodeproto.CodeUnreachable {
			t.Fatalf("err = %v (code %q)", err, nodeproto.CodeOf(err))
		}
	}
	if !strings.Contains(err.Error(), "node-a") {
		t.Fatalf("the error must name the node: %v", err)
	}
}

func TestNodeClientsRequirePlaceholdersToBeReal(t *testing.T) {
	if _, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: "192.168.190.87:18400"}}); err == nil {
		t.Fatal("a node without an http:// address must be refused")
	}
	if _, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: "http://x:1"}, {Name: "node-a", BaseURL: "http://y:1"}}); err == nil {
		t.Fatal("a duplicate node name must be refused")
	}
	set, err := nodeclient.NewSet([]nodeclient.Spec{{Name: "node-a", BaseURL: "http://x:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 1 || len(set.Names()) != 1 || set.Names()[0] != "node-a" {
		t.Fatalf("set = %+v", set.Names())
	}
	if _, ok := set.Get("node-b"); ok {
		t.Fatal("Get returned an unknown node")
	}
	var empty *nodeclient.Set
	if empty.Len() != 0 || len(empty.Names()) != 0 {
		t.Fatal("a nil set must behave as empty")
	}
	empty.Each(func(*nodeclient.Client) { t.Fatal("a nil set must not visit anything") })
}

// TestConcurrentLifecycleUsesOneNodeRecord keeps the multi-machine path honest about the shared
// registry: two operations for the same node must not corrupt the placement record.
func TestConcurrentLifecycleUsesOneNodeRecord(t *testing.T) {
	m, _, _ := remoteFixture(t, func(op string, body []byte) (int, any) {
		if op == "tenant-create" {
			return http.StatusOK, nodeState("alice", 32901)
		}
		return http.StatusOK, nil
	})
	created, err := m.Create(context.Background(), "alice", "sk-concurrent1234567890ab", nil, CreateOptions{Node: "node-a", AllowEmptyModels: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.Node != "node-a" || created.WorkerPort != 32901 {
		t.Fatalf("created = %+v", created)
	}
	if !m.Config.IsLocalNode("") {
		t.Fatal("the fixture's default placement must stay local")
	}
}

// Guard the fixture itself: these tests are meaningless if the deployment fixture is a node.
func TestRemoteFixtureIsAControlPlane(t *testing.T) {
	m, _, _ := remoteFixture(t, defaultNodeResponder(t))
	if m.Config.NodeMode() {
		t.Fatal("the fixture must be a control plane")
	}
	if m.Nodes.Len() != 1 {
		t.Fatalf("fixture nodes = %v", m.Nodes.Names())
	}
	if !m.Config.IsLocalNode("") || m.Config.IsLocalNode("node-a") {
		t.Fatal("placement helpers disagree with the fixture")
	}
	_ = config.LocalNodeName
}

// TestCreateWithHostSharesMaterializesTheBindRootFirst is the regression for a create-time ordering
// bug the multi-machine acceptance found: the profile binds the host-share container read-only, and
// rendering it before that container existed failed the whole create.
func TestCreateWithHostSharesMaterializesTheBindRootFirst(t *testing.T) {
	m, _, _ := managerFixture(t)
	shareSource := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(shareSource, 0o755); err != nil {
		t.Fatal(err)
	}
	service, err := hostshare.New(hostshare.Options{
		Subdir: "host",
		Declarations: []hostshare.Declaration{{
			Name: "shared", Source: shareSource, ReadOnly: true, Tenants: []string{"alice"},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.HostShares = service

	created, err := m.Create(context.Background(), "alice", "sk-hostsahre1234567890ab", nil, CreateOptions{
		AllowEmptyModels: true, DirectoryPicker: "clamp", PluginBrowserFS: "off",
	})
	if err != nil {
		t.Fatalf("creating a tenant in a deployment with host shares failed: %v", err)
	}
	target := filepath.Join(created.Workspace, "host", "shared")
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("the share target was not materialized: %v", err)
	}
	container := filepath.Join(created.Workspace, "host")
	if info, err := os.Stat(container); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the share container must be private to the account: %v (%v)", info, err)
	}
	// The profile really carries the binding (which is what required the root to exist).
	profile, err := m.SandboxProfile(created)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(profile, " ")
	if !strings.Contains(joined, container) || !strings.Contains(joined, target) {
		t.Fatalf("the profile does not bind the share: %v", profile)
	}
}
