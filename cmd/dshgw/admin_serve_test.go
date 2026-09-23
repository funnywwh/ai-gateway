package main

import (
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/nodedep"
	"github.com/winger/ai-gateway/internal/dshgw/nodeproto"

	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// M52: the admin socket is the console's provisioning channel. The protocol and the
// peer-credential gate are tested here against a real UNIX socket; the Manager operations
// behind AdminOps are the same ones the CLI regression tests cover.

type stubOps struct {
	created []string
	// accounts records the account label every create/set-key carried (M67), so the
	// protocol test can pin that it survives the socket hop.
	accounts []string
	started  []string
	stopped  []string
	keys     []string
	tenants  []registry.Tenant

	// M77: the node surface.
	nodes        []string
	addedNodes   []string
	updatedNodes []string
	removedNodes []string
	purged       []string
	deployed     []string
	deployOpts   []DeployOptions
	probed       []string
	probedNodes  []bool
	reconciled   []string
	audited      []string
	auditLines   []int
	restarted    []string
	moved        []string
}

func (s *stubOps) Create(_ context.Context, name, account, key, node string, _ bool) (registry.Tenant, error) {
	s.created = append(s.created, name)
	s.accounts = append(s.accounts, account)
	s.keys = append(s.keys, key)
	s.nodes = append(s.nodes, node)
	created := registry.Tenant{Name: name, Account: account, Node: node, PublicPort: 32601, UID: 1234}
	s.tenants = append(s.tenants, created)
	return created, nil
}

func (s *stubOps) Restart(_ context.Context, name string) error {
	s.restarted = append(s.restarted, name)
	return nil
}

func (s *stubOps) SetNode(_ context.Context, tenant, node string) error {
	s.moved = append(s.moved, tenant+"->"+node)
	return nil
}

func (s *stubOps) ListTenants(_ context.Context) ([]TenantRow, error) {
	rows := make([]TenantRow, 0, len(s.tenants))
	for _, tenant := range s.tenants {
		rows = append(rows, TenantRow{Name: tenant.Name, Account: tenant.Account, Node: tenant.Node, PublicPort: tenant.PublicPort})
	}
	return rows, nil
}

func (s *stubOps) ListNodes(_ context.Context, probe bool) ([]NodeView, error) {
	s.probedNodes = append(s.probedNodes, probe)
	return []NodeView{{
		Name: "node-a", State: "ready", Reachable: true, TokenState: "set",
		Features: nodeproto.Features{TenantPlugins: []string{"web-tty"}, HostShares: 1},
	}}, nil
}

func (s *stubOps) AddNode(_ context.Context, spec NodeSpec) (NodeView, error) {
	s.addedNodes = append(s.addedNodes, spec.Name)
	return NodeView{Name: spec.Name, SSHHost: spec.SSHHost, SSHUser: spec.SSHUser, State: "pending", TokenState: "missing"}, nil
}

func (s *stubOps) UpdateNode(_ context.Context, spec NodeSpec) (NodeView, error) {
	s.updatedNodes = append(s.updatedNodes, spec.Name)
	return NodeView{Name: spec.Name, State: "pending"}, nil
}

func (s *stubOps) RemoveNode(_ context.Context, name string, purge bool) error {
	s.removedNodes = append(s.removedNodes, name)
	if purge {
		s.purged = append(s.purged, name)
	}
	return nil
}

func (s *stubOps) DeployNode(_ context.Context, name string, opts DeployOptions) (DeployStatus, error) {
	s.deployed = append(s.deployed, name)
	s.deployOpts = append(s.deployOpts, opts)
	return DeployStatus{Node: name, Running: true, State: "deploying", Phase: nodedep.PhaseUpload}, nil
}

func (s *stubOps) NodeDeployStatus(_ context.Context, name string) (DeployStatus, error) {
	return DeployStatus{
		Node: name, State: "ready", Systemd: true, Fingerprint: "SHA256:abc", Rotated: true,
		Phases:  []DeployPhase{{Name: nodedep.PhasePreflight, OK: true}, {Name: nodedep.PhaseVerify, OK: true}},
		LogTail: "line one\nline two\n",
	}, nil
}

func (s *stubOps) ProbeNode(_ context.Context, name string) (NodeView, error) {
	s.probed = append(s.probed, name)
	return NodeView{Name: name, State: "ready", Reachable: true}, nil
}

func (s *stubOps) ReconcileNode(_ context.Context, name string) (nodeproto.ReconcileResult, error) {
	s.reconciled = append(s.reconciled, name)
	return nodeproto.ReconcileResult{Status: nodeproto.Status{Health: nodeproto.Health{Tenants: 2}}, Started: []string{"alice"}}, nil
}

func (s *stubOps) NodeAudit(_ context.Context, name string, lines int) ([]string, error) {
	s.audited = append(s.audited, name)
	s.auditLines = append(s.auditLines, lines)
	return []string{`{"kind":"ssh-mount-refused"}`}, nil
}
func (s *stubOps) Start(_ context.Context, name string) error {
	s.started = append(s.started, name)
	return nil
}
func (s *stubOps) Stop(_ context.Context, name string) error {
	s.stopped = append(s.stopped, name)
	return nil
}
func (s *stubOps) SetKey(_ context.Context, name, account, key string) error {
	s.accounts = append(s.accounts, account)
	s.keys = append(s.keys, key)
	return nil
}
func (s *stubOps) List() []registry.Tenant { return s.tenants }

func startTestAdminServer(t *testing.T, ops AdminOps, ownerUID int) (string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{Ops: ops, OwnerUID: ownerUID}
	ctx, cancel := context.WithCancel(context.Background())
	go server.Serve(ctx, ln)
	return path, func() { cancel(); ln.Close() }
}

func adminCall(t *testing.T, path string, req adminRequest) adminResponse {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp adminResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAdminServerLifecycleOpsAndPeerGate(t *testing.T) {
	ops := &stubOps{}
	path, stop := startTestAdminServer(t, ops, os.Geteuid())
	defer stop()

	resp := adminCall(t, path, adminRequest{ID: 1, Op: "ping"})
	if !resp.OK {
		t.Fatalf("ping: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 2, Op: "tenant-create", Name: "alice", Account: "李智超(colin)", Key: "sk-gw-test-key-000001", AllowEmptyModels: true})
	if !resp.OK || resp.Result["name"] != "alice" {
		t.Fatalf("create: %+v", resp)
	}
	if len(ops.created) != 1 || ops.created[0] != "alice" || len(ops.keys) != 1 {
		t.Fatalf("ops recorded: %+v", ops)
	}
	// The account label is display data the tenant's sidebar shows (M67): it must survive the
	// socket hop unchanged, non-ASCII included.
	if len(ops.accounts) != 1 || ops.accounts[0] != "李智超(colin)" {
		t.Fatalf("account label did not cross the channel: %+v", ops.accounts)
	}
	resp = adminCall(t, path, adminRequest{ID: 3, Op: "tenant-stop", Name: "alice"})
	if !resp.OK {
		t.Fatalf("stop: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 4, Op: "tenant-start", Name: "alice"})
	if !resp.OK {
		t.Fatalf("start: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 5, Op: "tenant-set-key", Name: "alice", Account: "李智超(colin)", Key: "sk-gw-test-key-000002"})
	if !resp.OK {
		t.Fatalf("set-key: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 6, Op: "tenant-create", Name: "../escape", Key: "sk-gw-test-key-000003"})
	if resp.OK || resp.ErrType != "bad_request" {
		t.Fatalf("path traversal must be a bad request: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 7, Op: "tenant-create", Name: "alice"})
	if resp.OK || resp.ErrType != "bad_request" {
		t.Fatalf("missing key must be a bad request: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 8, Op: "nope"})
	if resp.OK || resp.ErrType != "bad_request" {
		t.Fatalf("unknown op: %+v", resp)
	}
	if len(ops.created) != 1 {
		t.Fatalf("rejected ops must not mutate: %+v", ops)
	}
}

func TestAdminServerRejectsForeignPeerUID(t *testing.T) {
	// SO_PEERCRED reports the connecting process's real UID. The test process
	// connects as itself, so a socket owned by another account must refuse it
	// before any protocol byte is read.
	ops := &stubOps{}
	path, stop := startTestAdminServer(t, ops, os.Geteuid()+1)
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	data, _ := json.Marshal(adminRequest{ID: 1, Op: "ping"})
	// The server may refuse and close before this write lands, so a failed write is one of the
	// expected outcomes here (it is a race, and a flaky assertion about it says nothing about
	// the guard under test). What matters is that no answer ever comes back.
	_, _ = conn.Write(append(data, '\n'))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
		t.Fatal("foreign peer must not receive an answer")
	}
	if len(ops.created) != 0 {
		t.Fatal("foreign peer must not trigger ops")
	}
}

func TestListenAdminRequiresConfig(t *testing.T) {
	dir := t.TempDir()
	empty := &config.Config{}
	if _, err := ListenAdmin(empty); err == nil {
		t.Fatal("empty socket path must be rejected")
	}
	relative := &config.Config{AdminSocket: "admin.sock"}
	if _, err := ListenAdmin(relative); err == nil {
		t.Fatal("relative socket path must be rejected")
	}
	// Real bind: the socket file must exist and be mode 0660 with the single allowed UID
	// as owner, so the aigw user can connect and nobody else can even find it writable.
	cfg := &config.Config{AdminSocket: filepath.Join(dir, "admin.sock"), AdminAllowedUIDs: []int{os.Geteuid()}}
	ln, err := ListenAdmin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	info, err := os.Stat(cfg.AdminSocket)
	if err != nil {
		t.Fatal(err)
	}
	// 0600: the channel admits only the account that owns the socket, so it must
	// not be reachable by anyone else in the first place.
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
		t.Fatalf("socket owner uid %d", st.Uid)
	}
}

// TestAdminNodeOpsCrossTheSocket is the P6 protocol test for the node surface: every operation
// reaches the shared implementation with its arguments intact, and the read-only ones do not
// announce a tenant change (which would make the console's public surface reconcile on every poll).
func TestAdminNodeOpsCrossTheSocket(t *testing.T) {
	ops := &stubOps{}
	path, stop := startTestAdminServer(t, ops, os.Getuid())
	defer stop()

	list := adminCall(t, path, adminRequest{Op: "node-list", Probe: true})
	if !list.OK {
		t.Fatalf("node-list failed: %+v", list)
	}
	nodes, _ := list.Result["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %+v", list.Result)
	}
	first, _ := nodes[0].(map[string]any)
	if first["name"] != "node-a" || first["state"] != "ready" || first["token_state"] != "set" {
		t.Fatalf("node row = %+v", first)
	}
	features, _ := first["features"].(map[string]any)
	if features["host_shares"] != float64(1) {
		t.Fatalf("features did not survive the socket: %+v", features)
	}
	if len(ops.probedNodes) != 1 || !ops.probedNodes[0] {
		t.Fatalf("probe flag = %v", ops.probedNodes)
	}

	added := adminCall(t, path, adminRequest{Op: "node-add", Spec: &NodeSpec{
		Name: "node-b", SSHHost: "10.0.0.9", SSHUser: "deployer", Listen: "10.0.0.9:18400",
	}})
	if !added.OK {
		t.Fatalf("node-add failed: %+v", added)
	}
	if len(ops.addedNodes) != 1 || ops.addedNodes[0] != "node-b" {
		t.Fatalf("added = %v", ops.addedNodes)
	}

	updated := adminCall(t, path, adminRequest{Op: "node-update", Spec: &NodeSpec{Name: "node-b", BwrapBin: "/opt/bwrap"}})
	if !updated.OK || len(ops.updatedNodes) != 1 {
		t.Fatalf("node-update: %+v %v", updated, ops.updatedNodes)
	}
	// The name may also arrive as the request's Name field, which is how the console sends it.
	if resp := adminCall(t, path, adminRequest{Op: "node-update", Name: "node-b", Spec: &NodeSpec{}}); !resp.OK || len(ops.updatedNodes) != 2 {
		t.Fatalf("node-update by name: %+v %v", resp, ops.updatedNodes)
	}

	deployed := adminCall(t, path, adminRequest{Op: "node-deploy", Name: "node-b", Deploy: &DeployOptions{AcceptHostKey: "SHA256:abc"}})
	if !deployed.OK {
		t.Fatalf("node-deploy failed: %+v", deployed)
	}
	if len(ops.deployOpts) != 1 || ops.deployOpts[0].AcceptHostKey != "SHA256:abc" || ops.deployOpts[0].RotateToken {
		t.Fatalf("deploy options = %+v", ops.deployOpts)
	}
	rotated := adminCall(t, path, adminRequest{Op: "node-rotate-token", Name: "node-b"})
	if !rotated.OK || len(ops.deployOpts) != 2 || !ops.deployOpts[1].RotateToken {
		t.Fatalf("rotate-token did not set the flag: %+v %+v", rotated, ops.deployOpts)
	}

	status := adminCall(t, path, adminRequest{Op: "node-deploy-status", Name: "node-b"})
	if !status.OK {
		t.Fatalf("node-deploy-status failed: %+v", status)
	}
	job, _ := status.Result["deploy"].(map[string]any)
	if job["state"] != "ready" || job["systemd"] != true || job["rotated_token"] != true {
		t.Fatalf("deploy status = %+v", job)
	}
	if !strings.Contains(job["log_tail"].(string), "line two") {
		t.Fatalf("log tail = %q", job["log_tail"])
	}
	phases, _ := job["phases"].([]any)
	if len(phases) != 2 {
		t.Fatalf("phases = %+v", phases)
	}

	if resp := adminCall(t, path, adminRequest{Op: "node-probe", Name: "node-b"}); !resp.OK || resp.Result["reachable"] != true {
		t.Fatalf("node-probe: %+v", resp)
	}
	if resp := adminCall(t, path, adminRequest{Op: "node-reconcile", Name: "node-b"}); !resp.OK || len(ops.reconciled) != 1 {
		t.Fatalf("node-reconcile: %+v", resp)
	}
	audit := adminCall(t, path, adminRequest{Op: "node-audit", Name: "node-b", Lines: 25})
	if !audit.OK || len(ops.auditLines) != 1 || ops.auditLines[0] != 25 {
		t.Fatalf("node-audit: %+v %v", audit, ops.auditLines)
	}
	lines, _ := audit.Result["lines"].([]any)
	if len(lines) != 1 || !strings.Contains(lines[0].(string), "ssh-mount-refused") {
		t.Fatalf("audit lines = %+v", lines)
	}

	removed := adminCall(t, path, adminRequest{Op: "node-remove", Name: "node-b", Purge: true})
	if !removed.OK || len(ops.purged) != 1 || ops.purged[0] != "node-b" {
		t.Fatalf("node-remove: %+v %v", removed, ops.purged)
	}

	restart := adminCall(t, path, adminRequest{Op: "tenant-restart", Name: "alice"})
	if !restart.OK || len(ops.restarted) != 1 {
		t.Fatalf("tenant-restart: %+v %v", restart, ops.restarted)
	}
	move := adminCall(t, path, adminRequest{Op: "tenant-set-node", Name: "alice", Node: "node-b"})
	if !move.OK || len(ops.moved) != 1 || ops.moved[0] != "alice->node-b" {
		t.Fatalf("tenant-set-node: %+v %v", move, ops.moved)
	}
	// A placement without a node is refused before it reaches the implementation.
	if resp := adminCall(t, path, adminRequest{Op: "tenant-set-node", Name: "alice"}); resp.OK || resp.ErrType != "bad_request" {
		t.Fatalf("tenant-set-node without a node = %+v", resp)
	}

	// The tenant list carries the placement, so the console can show which machine hosts what.
	create := adminCall(t, path, adminRequest{Op: "tenant-create", Name: "carol", Account: "Carol", Key: "sk-x", Node: "node-b", AllowEmptyModels: true})
	if !create.OK {
		t.Fatalf("tenant-create failed: %+v", create)
	}
	if len(ops.nodes) != 1 || ops.nodes[0] != "node-b" {
		t.Fatalf("the placement did not reach Create: %v", ops.nodes)
	}
	if create.Result["node"] != "node-b" {
		t.Fatalf("create result = %+v", create.Result)
	}
	listing := adminCall(t, path, adminRequest{Op: "tenant-list"})
	rows, _ := listing.Result["tenants"].([]any)
	if len(rows) != 1 {
		t.Fatalf("tenants = %+v", listing.Result)
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != "carol" || row["node"] != "node-b" {
		t.Fatalf("tenant row = %+v", row)
	}
}

// TestAdminNodeOpsValidateTheirArguments keeps the socket's contract narrow: a malformed request is
// refused by name instead of reaching the implementation.
func TestAdminNodeOpsValidateTheirArguments(t *testing.T) {
	ops := &stubOps{}
	path, stop := startTestAdminServer(t, ops, os.Getuid())
	defer stop()
	for _, request := range []adminRequest{
		{Op: "node-add"},
		{Op: "node-update"},
		{Op: "node-deploy", Name: "../etc"},
		{Op: "node-remove", Name: ""},
		{Op: "node-probe", Name: "bad name"},
		{Op: "node-deploy-status", Name: ".."},
		{Op: "node-audit", Name: "x/y"},
		{Op: "node-set-token", Name: "node-a"},
	} {
		response := adminCall(t, path, request)
		if response.OK {
			t.Fatalf("%+v was accepted", request)
		}
	}
	if len(ops.addedNodes)+len(ops.removedNodes)+len(ops.deployed)+len(ops.probed) != 0 {
		t.Fatal("a malformed request reached the implementation")
	}
}
