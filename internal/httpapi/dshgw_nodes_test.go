package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/localdshgw"
)

// fakeDshgwNodes is the node half of the channel as the console sees it.
type fakeDshgwNodes struct {
	listed       []bool
	added        []localdshgw.NodeSpec
	updated      []localdshgw.NodeSpec
	removed      []string
	purged       []bool
	deployed     []string
	deployOpts   []localdshgw.DeployOptions
	rotated      []string
	reconciled   []string
	audited      []string
	auditLines   []int
	probed       []string
	probeResults []localdshgw.NodeView
	status       localdshgw.DeployStatus
	failWith     error
}

func (f *fakeDshgwNodes) ListNodes(_ context.Context, probe bool) ([]localdshgw.NodeView, error) {
	f.listed = append(f.listed, probe)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return []localdshgw.NodeView{{
		Name: "node-a", State: "ready", Reachable: probe, TokenState: "set", Tenants: 2, Default: true,
		Features: localdshgw.NodeFeatures{TenantPlugins: []string{"web-tty"}, HostShares: 1},
	}}, nil
}

func (f *fakeDshgwNodes) AddNode(_ context.Context, spec localdshgw.NodeSpec) (localdshgw.NodeView, error) {
	if f.failWith != nil {
		return localdshgw.NodeView{}, f.failWith
	}
	f.added = append(f.added, spec)
	return localdshgw.NodeView{Name: spec.Name, SSHHost: spec.SSHHost, SSHUser: spec.SSHUser, State: "pending", TokenState: "missing"}, nil
}

func (f *fakeDshgwNodes) UpdateNode(_ context.Context, spec localdshgw.NodeSpec) (localdshgw.NodeView, error) {
	f.updated = append(f.updated, spec)
	return localdshgw.NodeView{Name: spec.Name, State: "pending"}, nil
}

func (f *fakeDshgwNodes) RemoveNode(_ context.Context, name string, purge bool) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.removed = append(f.removed, name)
	f.purged = append(f.purged, purge)
	return nil
}

func (f *fakeDshgwNodes) DeployNode(_ context.Context, name string, opts localdshgw.DeployOptions) (localdshgw.DeployStatus, error) {
	if f.failWith != nil {
		return localdshgw.DeployStatus{}, f.failWith
	}
	f.deployed = append(f.deployed, name)
	f.deployOpts = append(f.deployOpts, opts)
	return localdshgw.DeployStatus{Node: name, Running: true, State: "deploying", Phase: "preflight"}, nil
}

func (f *fakeDshgwNodes) RotateNodeToken(_ context.Context, name string, opts localdshgw.DeployOptions) (localdshgw.DeployStatus, error) {
	f.rotated = append(f.rotated, name)
	return localdshgw.DeployStatus{Node: name, Running: true, State: "deploying", Rotated: true}, nil
}

func (f *fakeDshgwNodes) NodeDeployStatus(_ context.Context, name string) (localdshgw.DeployStatus, error) {
	if f.status.Node == "" {
		f.status = localdshgw.DeployStatus{
			Node: name, State: "ready", Systemd: true,
			Phases:  []localdshgw.DeployPhase{{Name: "preflight", OK: true, Millis: 5}},
			LogTail: "upload-ok\n", LogPath: "/var/lib/dshgw/node-deploy/node-a.log",
		}
	}
	return f.status, nil
}

func (f *fakeDshgwNodes) ProbeNode(_ context.Context, name string) (localdshgw.NodeView, error) {
	f.probed = append(f.probed, name)
	if f.failWith != nil {
		return localdshgw.NodeView{Name: name, State: "unreachable"}, f.failWith
	}
	return localdshgw.NodeView{Name: name, State: "ready", Reachable: true}, nil
}

func (f *fakeDshgwNodes) ReconcileNode(_ context.Context, name string) (localdshgw.ReconcileResult, error) {
	if f.failWith != nil {
		return localdshgw.ReconcileResult{}, f.failWith
	}
	f.reconciled = append(f.reconciled, name)
	return localdshgw.ReconcileResult{Started: []string{"alice"}, Pruned: []string{"ghost"}}, nil
}

func (f *fakeDshgwNodes) NodeAudit(_ context.Context, name string, lines int) ([]string, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.audited = append(f.audited, name)
	f.auditLines = append(f.auditLines, lines)
	return []string{`{"kind":"ssh-mount-refused","tenant":"alice"}`, `{"kind":"logout_mount_detach"}`}, nil
}

// apiAnswer is one admin call's outcome, with the body already read (a fixture response body is
// consumed once, and every assertion here wants both).
type apiAnswer struct {
	Code int
	Body string
}

func adminAnswer(t *testing.T, f *adminFixture, method, path string, body any) apiAnswer {
	t.Helper()
	return callAs(t, f, method, path, body, adminUser)
}

func viewerAnswer(t *testing.T, f *adminFixture, method, path string, body any) apiAnswer {
	t.Helper()
	return callAs(t, f, method, path, body, "reader")
}

func callAs(t *testing.T, f *adminFixture, method, path string, body any, user string) apiAnswer {
	t.Helper()
	cookie := f.login(t, user, adminPassword)
	payload := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = string(encoded)
	}
	resp := f.call(t, method, path, payload, cookie)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return apiAnswer{Code: resp.StatusCode, Body: string(data)}
}

// nodesFixture is the admin fixture with the node half wired.
func nodesFixture(t *testing.T) (*adminFixture, *fakeDshgwNodes) {
	t.Helper()
	nodes := &fakeDshgwNodes{}
	fixture := newAdminFixtureWith(t, "", func(deps *Deps) { deps.DshgwNodes = nodes })
	return fixture, nodes
}

func TestDshgwNodeListDefaultsToNoProbe(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	response := adminAnswer(t, fixture, "GET", "/admin/api/v1/dshgw/nodes", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(nodes.listed) != 1 || nodes.listed[0] {
		t.Fatalf("probe flags = %v (a page render must not walk the fleet)", nodes.listed)
	}
	var body struct {
		Nodes []localdshgw.NodeView `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Nodes) != 1 || body.Nodes[0].Name != "node-a" || body.Nodes[0].Features.HostShares != 1 {
		t.Fatalf("nodes = %+v", body.Nodes)
	}
	// probe=true asks for the live check.
	if response := adminAnswer(t, fixture, "GET", "/admin/api/v1/dshgw/nodes?probe=true", nil); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if !nodes.listed[1] {
		t.Fatalf("probe=true did not reach the daemon: %v", nodes.listed)
	}
}

func TestDshgwNodeAddRequiresTheSSHTarget(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	for _, body := range []map[string]any{
		{"name": "node-b"},
		{"name": "node-b", "listen": "10.0.0.9:18400"},
		{"name": "node-b", "listen": "10.0.0.9:18400", "ssh_host": "10.0.0.9"},
	} {
		response := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/nodes", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %+v: status = %d (%s)", body, response.Code, response.Body)
		}
	}
	if len(nodes.added) != 0 {
		t.Fatal("an incomplete registration reached the daemon")
	}
	response := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/nodes", map[string]any{
		"name": "node-b", "listen": "10.0.0.9:18400", "ssh_host": "10.0.0.9", "ssh_user": "deployer",
		"worker_port_lo": 32900, "default": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(nodes.added) != 1 || nodes.added[0].WorkerPortLo != 32900 || !nodes.added[0].Default {
		t.Fatalf("spec = %+v", nodes.added)
	}
}

func TestDshgwNodeUpdateOnlyChangesWhatWasSent(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	response := adminAnswer(t, fixture, "PATCH", "/admin/api/v1/dshgw/nodes/node-a", map[string]any{"bwrap_bin": "/opt/bwrap"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(nodes.updated) != 1 || nodes.updated[0].Name != "node-a" || nodes.updated[0].BwrapBin != "/opt/bwrap" {
		t.Fatalf("spec = %+v", nodes.updated)
	}
	if nodes.updated[0].SSHHost != "" {
		t.Fatalf("an absent field became a value: %+v", nodes.updated[0])
	}
}

func TestDshgwNodeRemoveNeedsConfirmationToPurge(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	if response := adminAnswer(t, fixture, "DELETE", "/admin/api/v1/dshgw/nodes/node-a?purge=true", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("an unconfirmed purge was accepted: %d", response.Code)
	}
	if len(nodes.removed) != 0 {
		t.Fatal("an unconfirmed purge reached the daemon")
	}
	if response := adminAnswer(t, fixture, "DELETE", "/admin/api/v1/dshgw/nodes/node-a", nil); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if len(nodes.removed) != 1 || nodes.purged[0] {
		t.Fatalf("removed = %v purged = %v", nodes.removed, nodes.purged)
	}
	if response := adminAnswer(t, fixture, "DELETE", "/admin/api/v1/dshgw/nodes/node-a?purge=true&confirm=node-a", nil); response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(nodes.purged) != 2 || !nodes.purged[1] {
		t.Fatalf("purged = %v", nodes.purged)
	}
}

func TestDshgwNodeDeployIsAsynchronous(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	response := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/nodes/node-a/deploy", map[string]any{
		"accept_host_key": "SHA256:abc", "with_packages": true,
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s (a deploy returns as soon as the job starts)", response.Code, response.Body)
	}
	if len(nodes.deployed) != 1 || nodes.deployOpts[0].AcceptHostKey != "SHA256:abc" || !nodes.deployOpts[0].WithPackages {
		t.Fatalf("deploy = %v %+v", nodes.deployed, nodes.deployOpts)
	}
	var body struct {
		Deploy localdshgw.DeployStatus `json:"deploy"`
	}
	if err := json.Unmarshal([]byte(response.Body), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Deploy.Running || body.Deploy.Phase != "preflight" {
		t.Fatalf("deploy status = %+v", body.Deploy)
	}

	status := adminAnswer(t, fixture, "GET", "/admin/api/v1/dshgw/nodes/node-a/deploy", nil)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d", status.Code)
	}
	if err := json.Unmarshal([]byte(status.Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Deploy.Phases) != 1 || !strings.Contains(body.Deploy.LogTail, "upload-ok") || !body.Deploy.Systemd {
		t.Fatalf("deploy status = %+v", body.Deploy)
	}

	if rotated := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/nodes/node-a/rotate-token", nil); rotated.Code != http.StatusAccepted {
		t.Fatalf("rotate status = %d", rotated.Code)
	}
	if len(nodes.rotated) != 1 {
		t.Fatalf("rotated = %v", nodes.rotated)
	}
}

func TestDshgwNodeReconcileAndAudit(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	response := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/nodes/node-a/reconcile", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(nodes.reconciled) != 1 {
		t.Fatalf("reconciled = %v", nodes.reconciled)
	}
	audit := adminAnswer(t, fixture, "GET", "/admin/api/v1/dshgw/nodes/node-a/audit?lines=25", nil)
	if audit.Code != http.StatusOK {
		t.Fatalf("status = %d", audit.Code)
	}
	if len(nodes.auditLines) != 1 || nodes.auditLines[0] != 25 {
		t.Fatalf("audit lines = %v", nodes.auditLines)
	}
	if tooBig := adminAnswer(t, fixture, "GET", "/admin/api/v1/dshgw/nodes/node-a/audit?lines=5000", nil); tooBig.Code != http.StatusBadRequest {
		t.Fatalf("an unbounded tail read was accepted: %d", tooBig.Code)
	}
}

func TestDshgwTenantRestartAndMove(t *testing.T) {
	fixture, _ := nodesFixture(t)
	response := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/tenants/alice/restart", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(fixture.dshgwAdmin.restarted) != 1 || fixture.dshgwAdmin.restarted[0] != "alice" {
		t.Fatalf("restarted = %v", fixture.dshgwAdmin.restarted)
	}
	move := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/tenants/alice/node", map[string]any{"node": "node-a"})
	if move.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", move.Code, move.Body)
	}
	if len(fixture.dshgwAdmin.moved) != 1 || fixture.dshgwAdmin.moved[0] != "alice->node-a" {
		t.Fatalf("moved = %v", fixture.dshgwAdmin.moved)
	}
	// A move without a node is refused here, before it reaches dshgw.
	if missing := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/tenants/alice/node", map[string]any{}); missing.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", missing.Code)
	}
	if len(fixture.dshgwAdmin.moved) != 1 {
		t.Fatalf("a request without a node reached the daemon: %v", fixture.dshgwAdmin.moved)
	}
}

func TestDshgwNodeRoutesRequireTheChannel(t *testing.T) {
	// The node half unwired (a single-machine deployment): the routes say so instead of panicking.
	fixture := newAdminFixture(t)
	for _, path := range []string{"/admin/api/v1/dshgw/nodes", "/admin/api/v1/dshgw/nodes/node-a/deploy"} {
		response := adminAnswer(t, fixture, "GET", path, nil)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status = %d body = %s", path, response.Code, response.Body)
		}
		if !strings.Contains(response.Body, "not configured") {
			t.Fatalf("%s: body = %s", path, response.Body)
		}
	}
}

func TestDshgwNodeRoutesRequireAdmin(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	// A viewer may look, and may not act.
	for _, action := range []struct {
		method, path string
		body         map[string]any
	}{
		{"POST", "/admin/api/v1/dshgw/nodes", map[string]any{"name": "n", "listen": "1.2.3.4:1", "ssh_host": "1.2.3.4", "ssh_user": "u"}},
		{"PATCH", "/admin/api/v1/dshgw/nodes/node-a", map[string]any{"bwrap_bin": "/x"}},
		{"DELETE", "/admin/api/v1/dshgw/nodes/node-a", nil},
		{"POST", "/admin/api/v1/dshgw/nodes/node-a/deploy", nil},
		{"POST", "/admin/api/v1/dshgw/nodes/node-a/rotate-token", nil},
		{"POST", "/admin/api/v1/dshgw/nodes/node-a/reconcile", nil},
		{"POST", "/admin/api/v1/dshgw/tenants/alice/restart", nil},
		{"POST", "/admin/api/v1/dshgw/tenants/alice/node", map[string]any{"node": "node-a"}},
	} {
		response := viewerAnswer(t, fixture, action.method, action.path, action.body)
		if response.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s: status = %d", action.method, action.path, response.Code)
		}
	}
	if len(nodes.added)+len(nodes.removed)+len(nodes.deployed)+len(nodes.rotated)+len(nodes.reconciled) != 0 {
		t.Fatal("a viewer's request reached the daemon")
	}
	if response := viewerAnswer(t, fixture, "GET", "/admin/api/v1/dshgw/nodes", nil); response.Code != http.StatusOK {
		t.Fatalf("a viewer must be able to look: %d", response.Code)
	}
}

// TestDshgwNodeErrorsReachTheConsole: a daemon refusal (a deploy already running, a node with
// tenants) must arrive as the daemon's own sentence, not as a generic failure.
func TestDshgwNodeErrorsReachTheConsole(t *testing.T) {
	fixture, nodes := nodesFixture(t)
	nodes.failWith = errDshgwBusy{}
	response := adminAnswer(t, fixture, "POST", "/admin/api/v1/dshgw/nodes/node-a/deploy", nil)
	if response.Code == http.StatusOK || response.Code == http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
	if !strings.Contains(response.Body, "already has a deploy running") {
		t.Fatalf("the daemon's message did not reach the console: %s", response.Body)
	}
}

type errDshgwBusy struct{}

func (errDshgwBusy) Error() string { return "node node-a already has a deploy running (phase upload)" }

// TestDshgwAccountEnableCarriesThePlacement: the console's 启用 DSH dialog can name the node a new
// tenant lands on (M77), and an empty value stays "the deployment's default".
func TestDshgwAccountEnableCarriesThePlacement(t *testing.T) {
	fixture, _ := nodesFixture(t)
	// A tenant name that cannot collide with the fake's existing rows.
	response := adminAnswer(t, fixture, "POST", "/admin/api/v1/accounts/1/dsh",
		map[string]any{"enabled": true, "tenant": "acme-dsh", "node": "node-a"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(fixture.dshgwAdmin.nodes) != 1 || fixture.dshgwAdmin.nodes[0] != "node-a" {
		t.Fatalf("the placement did not reach dshgw: %v", fixture.dshgwAdmin.nodes)
	}
	// Disabling and re-enabling the same account must not create a second tenant: a placement only
	// applies when a tenant is created (moving one is the explicit tenant/node route, because the
	// data has to be carried across machines by hand).
	if response := adminAnswer(t, fixture, "POST", "/admin/api/v1/accounts/1/dsh", map[string]any{"enabled": false}); response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if response := adminAnswer(t, fixture, "POST", "/admin/api/v1/accounts/1/dsh", map[string]any{"enabled": true}); response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(fixture.dshgwAdmin.nodes) != 1 {
		t.Fatalf("re-enabling created another tenant: %v", fixture.dshgwAdmin.nodes)
	}
	if len(fixture.dshgwAdmin.Started) != 1 || fixture.dshgwAdmin.Started[0] != "acme-dsh" {
		t.Fatalf("re-enabling did not restart the existing tenant: %v", fixture.dshgwAdmin.Started)
	}
}
