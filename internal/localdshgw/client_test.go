package localdshgw

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// socketServer answers one request per connection with a scripted reply, recording what arrived.
type socketServer struct {
	t        *testing.T
	path     string
	requests []request
	reply    map[string]any
	failWith string
}

func newSocketServer(t *testing.T) *socketServer {
	t.Helper()
	server := &socketServer{t: t, path: filepath.Join(t.TempDir(), "admin.sock")}
	listener, err := net.Listen("unix", server.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				var req request
				if err := json.Unmarshal([]byte(line), &req); err != nil {
					return
				}
				server.requests = append(server.requests, req)
				response := map[string]any{"id": req.ID, "ok": true, "result": server.reply}
				if server.failWith != "" {
					response = map[string]any{"id": req.ID, "ok": false, "error": server.failWith, "error_type": "internal"}
				}
				encoded, _ := json.Marshal(response)
				_, _ = conn.Write(append(encoded, '\n'))
			}()
		}
	}()
	return server
}

func (s *socketServer) last() request {
	if len(s.requests) == 0 {
		s.t.Fatal("no request reached the server")
	}
	return s.requests[len(s.requests)-1]
}

// TestNodeOpsReachTheDaemon pins the wire shape of every node operation: the daemon reads these
// fields by name, and a rename on one side would otherwise only surface as a silently ignored
// argument in production.
func TestNodeOpsReachTheDaemon(t *testing.T) {
	server := newSocketServer(t)
	client := &Client{SocketPath: server.path, Timeout: 5 * time.Second}
	ctx := context.Background()

	server.reply = map[string]any{"nodes": []map[string]any{{
		"name": "node-a", "state": "ready", "reachable": true, "token_state": "set", "tenants": 2,
		"features": map[string]any{"host_shares": 1, "tenant_plugins": []string{"web-tty"}},
	}}}
	nodes, err := client.ListNodes(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if req := server.last(); req.Op != "node-list" || !req.Probe {
		t.Fatalf("request = %+v", req)
	}
	if len(nodes) != 1 || nodes[0].Name != "node-a" || !nodes[0].Reachable || nodes[0].Tenants != 2 {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[0].Features.HostShares != 1 || len(nodes[0].Features.TenantPlugins) != 1 {
		t.Fatalf("features = %+v", nodes[0].Features)
	}

	server.reply = map[string]any{"node": map[string]any{"name": "node-b", "state": "pending", "ssh_host": "10.0.0.9", "token_state": "missing"}}
	view, err := client.AddNode(ctx, NodeSpec{Name: "node-b", SSHHost: "10.0.0.9", SSHUser: "deployer", Listen: "10.0.0.9:18400", WorkerPortLo: 32900})
	if err != nil {
		t.Fatal(err)
	}
	req := server.last()
	if req.Op != "node-add" || req.Name != "node-b" || req.Spec == nil {
		t.Fatalf("request = %+v", req)
	}
	if req.Spec.SSHHost != "10.0.0.9" || req.Spec.Listen != "10.0.0.9:18400" || req.Spec.WorkerPortLo != 32900 {
		t.Fatalf("spec did not survive the wire: %+v", req.Spec)
	}
	if view.Name != "node-b" || view.TokenState != "missing" {
		t.Fatalf("view = %+v", view)
	}

	server.reply = map[string]any{"deploy": map[string]any{
		"node": "node-b", "running": true, "state": "deploying", "phase": "upload",
	}}
	status, err := client.DeployNode(ctx, "node-b", DeployOptions{AcceptHostKey: "SHA256:abc", WithPackages: true})
	if err != nil {
		t.Fatal(err)
	}
	req = server.last()
	if req.Op != "node-deploy" || req.Deploy == nil || req.Deploy.AcceptHostKey != "SHA256:abc" || !req.Deploy.WithPackages {
		t.Fatalf("deploy request = %+v", req.Deploy)
	}
	if !status.Running || status.Phase != "upload" {
		t.Fatalf("status = %+v", status)
	}

	server.reply = map[string]any{"deploy": map[string]any{
		"node": "node-b", "state": "ready", "systemd": true, "rotated_token": true,
		"phases":   []map[string]any{{"name": "preflight", "ok": true, "millis": 12}, {"name": "verify", "ok": true}},
		"log_tail": "line one\nline two\n", "fingerprint": "SHA256:abc",
	}}
	status, err = client.NodeDeployStatus(ctx, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if server.last().Op != "node-deploy-status" {
		t.Fatalf("op = %s", server.last().Op)
	}
	if len(status.Phases) != 2 || status.Phases[0].Name != "preflight" || status.Phases[0].Millis != 12 {
		t.Fatalf("phases = %+v", status.Phases)
	}
	if !status.Systemd || !status.Rotated || !strings.Contains(status.LogTail, "line two") {
		t.Fatalf("status = %+v", status)
	}

	server.reply = map[string]any{"node": map[string]any{"name": "node-b", "reachable": true}}
	if _, err := client.ProbeNode(ctx, "node-b"); err != nil {
		t.Fatal(err)
	}
	if server.last().Op != "node-probe" {
		t.Fatalf("op = %s", server.last().Op)
	}

	server.reply = map[string]any{"result": map[string]any{"started": []string{"alice"}, "pruned": []string{"ghost"}}}
	result, err := client.ReconcileNode(ctx, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if server.last().Op != "node-reconcile" || len(result.Started) != 1 || len(result.Pruned) != 1 {
		t.Fatalf("result = %+v", result)
	}

	server.reply = map[string]any{"lines": []string{`{"kind":"x"}`}}
	events, err := client.NodeAudit(ctx, "node-b", 25)
	if err != nil {
		t.Fatal(err)
	}
	if server.last().Lines != 25 || len(events) != 1 {
		t.Fatalf("audit request = %+v events = %v", server.last(), events)
	}

	server.reply = map[string]any{"removed": "node-b", "purged": true}
	if err := client.RemoveNode(ctx, "node-b", true); err != nil {
		t.Fatal(err)
	}
	if req := server.last(); req.Op != "node-remove" || !req.Purge {
		t.Fatalf("remove request = %+v", req)
	}

	server.reply = map[string]any{"deploy": map[string]any{"node": "node-b", "rotated_token": true}}
	if _, err := client.RotateNodeToken(ctx, "node-b", DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if req := server.last(); req.Op != "node-rotate-token" || req.Deploy == nil || !req.Deploy.RotateToken {
		t.Fatalf("rotate request = %+v", req)
	}

	server.reply = map[string]any{"restarted": "alice"}
	if err := client.RestartTenant(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if server.last().Op != "tenant-restart" {
		t.Fatalf("op = %s", server.last().Op)
	}

	server.reply = map[string]any{"tenant": "alice", "node": "node-b"}
	if err := client.SetTenantNode(ctx, "alice", "node-b"); err != nil {
		t.Fatal(err)
	}
	if req := server.last(); req.Op != "tenant-set-node" || req.Node != "node-b" {
		t.Fatalf("set-node request = %+v", req)
	}

	server.reply = map[string]any{"name": "carol", "node": "node-b"}
	if err := client.CreateTenantIn(ctx, "carol", "Carol", "sk-x", "node-b"); err != nil {
		t.Fatal(err)
	}
	if req := server.last(); req.Op != "tenant-create" || req.Node != "node-b" || req.Account != "Carol" {
		t.Fatalf("create request = %+v", req)
	}
}

// TestDaemonErrorsSurface: a refusal from the daemon must reach the console as a message, not as an
// empty success.
func TestDaemonErrorsSurface(t *testing.T) {
	server := newSocketServer(t)
	server.failWith = "node node-a already has a deploy running (phase upload)"
	client := &Client{SocketPath: server.path, Timeout: 5 * time.Second}
	if _, err := client.DeployNode(context.Background(), "node-a", DeployOptions{}); err == nil ||
		!strings.Contains(err.Error(), "already has a deploy running") {
		t.Fatalf("err = %v", err)
	}
}

// TestMissingSocketIsReportedByName: the console's "this half is not wired" case has to name the
// socket, because that is what an operator checks first.
func TestMissingSocketIsReportedByName(t *testing.T) {
	client := &Client{SocketPath: filepath.Join(t.TempDir(), "absent.sock"), Timeout: time.Second}
	if _, err := client.ListNodes(context.Background(), false); err == nil || !strings.Contains(err.Error(), "admin channel unavailable") {
		t.Fatalf("err = %v", err)
	}
}

// TestDecodeReportsAMissingField keeps the two halves honest: an answer without the field a caller
// asked for is an error, not a zero value.
func TestDecodeReportsAMissingField(t *testing.T) {
	if _, err := decodeOne[DeployStatus](map[string]any{}, "deploy"); err == nil {
		t.Fatal("a missing field must be reported")
	}
	if _, err := decodeOne[DeployStatus](map[string]any{"deploy": "not-an-object"}, "deploy"); err == nil {
		t.Fatal("a mistyped field must be reported")
	}
}
