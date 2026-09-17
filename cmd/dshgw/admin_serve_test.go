package main

import (
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
	started []string
	stopped []string
	keys    []string
	tenants []registry.Tenant
}

func (s *stubOps) Create(_ context.Context, name, key string, _ bool) (registry.Tenant, error) {
	s.created = append(s.created, name)
	s.keys = append(s.keys, key)
	return registry.Tenant{Name: name, PublicPort: 32601, UID: 1234}, nil
}
func (s *stubOps) Start(_ context.Context, name string) error {
	s.started = append(s.started, name)
	return nil
}
func (s *stubOps) Stop(_ context.Context, name string) error {
	s.stopped = append(s.stopped, name)
	return nil
}
func (s *stubOps) SetKey(_ context.Context, name, key string) error {
	s.keys = append(s.keys, key)
	return nil
}
func (s *stubOps) List() []registry.Tenant { return s.tenants }

func startTestAdminServer(t *testing.T, ops AdminOps, allowedUIDs []int) (string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &AdminServer{Ops: ops, AllowedUID: allowedUIDs}
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
	path, stop := startTestAdminServer(t, ops, []int{os.Geteuid()})
	defer stop()

	resp := adminCall(t, path, adminRequest{ID: 1, Op: "ping"})
	if !resp.OK {
		t.Fatalf("ping: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 2, Op: "tenant-create", Name: "alice", Key: "sk-gw-test-key-000001", AllowEmptyModels: true})
	if !resp.OK || resp.Result["name"] != "alice" {
		t.Fatalf("create: %+v", resp)
	}
	if len(ops.created) != 1 || ops.created[0] != "alice" || len(ops.keys) != 1 {
		t.Fatalf("ops recorded: %+v", ops)
	}
	resp = adminCall(t, path, adminRequest{ID: 3, Op: "tenant-stop", Name: "alice"})
	if !resp.OK {
		t.Fatalf("stop: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 4, Op: "tenant-start", Name: "alice"})
	if !resp.OK {
		t.Fatalf("start: %+v", resp)
	}
	resp = adminCall(t, path, adminRequest{ID: 5, Op: "tenant-set-key", Name: "alice", Key: "sk-gw-test-key-000002"})
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
	// SO_PEERCRED reports the connecting process's real UID. The test process connects as
	// itself, so an allowlist that does not contain that UID must be refused before any
	// protocol byte is read.
	ops := &stubOps{}
	path, stop := startTestAdminServer(t, ops, []int{os.Geteuid() + 1})
	defer stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	data, _ := json.Marshal(adminRequest{ID: 1, Op: "ping"})
	if _, err := conn.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	// The server closes the connection without answering.
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
	relative := &config.Config{AdminSocket: "admin.sock", AdminAllowedUIDs: []int{os.Geteuid()}}
	if _, err := ListenAdmin(relative); err == nil {
		t.Fatal("relative socket path must be rejected")
	}
	noUIDs := &config.Config{AdminSocket: filepath.Join(dir, "admin.sock")}
	if _, err := ListenAdmin(noUIDs); err == nil {
		t.Fatal("empty uid allowlist must be rejected")
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
	if info.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %v", info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
		t.Fatalf("socket owner uid %d", st.Uid)
	}
}
