package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// admin-serve is the M52 local provisioning channel: a root daemon listening on a UNIX
// socket so the aigw console can enable/disable dsh tenants without any root shell. It
// exposes nothing but tenant lifecycle operations, reuses the exact Manager code paths the
// CLI uses (lifecycle locks included), and refuses every peer whose UID is not explicitly
// allow-listed in the config. There is deliberately no TCP listener.

const adminOpTimeout = 5 * time.Minute

// AdminOps is the operational surface the daemon exposes. It is an interface so the
// protocol/peer-credential layer can be regression-tested without systemd or root.
type AdminOps interface {
	Create(ctx context.Context, name, key string, allowEmptyModels bool) (registry.Tenant, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	SetKey(ctx context.Context, name, key string) error
	List() []registry.Tenant
}

type managerOps struct {
	m         *tenancy.Manager
	validator *aigw.Client
	cfg       *config.Config
}

func (o managerOps) models(ctx context.Context, key string) ([]string, bool, error) {
	models, err := o.validator.ValidateKey(ctx, key)
	if err != nil {
		return nil, false, err
	}
	return models, len(models) == 0, nil
}

func (o managerOps) Create(ctx context.Context, name, key string, allowEmptyModels bool) (registry.Tenant, error) {
	models, empty, err := o.models(ctx, key)
	if err != nil {
		return registry.Tenant{}, fmt.Errorf("key validation: %w", err)
	}
	if empty && !allowEmptyModels {
		return registry.Tenant{}, errors.New("key has no available models; configure the account's model grants first, then enable dsh again")
	}
	// Same port-occupancy guard the CLI create runs: a worker must never be parked on a
	// port something else already listens on.
	ports, err := listeningPorts(ctx)
	if err != nil {
		return registry.Tenant{}, err
	}
	o.m.Taken = func(port int) bool { return ports[port] }
	return o.m.Create(ctx, name, key, models, tenancy.CreateOptions{
		AllowEmptyModels: empty,
		DirectoryPicker:  o.cfg.DirectoryPicker,
		PluginBrowserFS:  o.cfg.PluginBrowserFS,
	})
}

func (o managerOps) Start(ctx context.Context, name string) error {
	t, ok := o.m.Registry.Get(name)
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	return o.m.StartWorker(ctx, t)
}

func (o managerOps) Stop(ctx context.Context, name string) error {
	t, ok := o.m.Registry.Get(name)
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	return o.m.StopWorker(ctx, t)
}

func (o managerOps) SetKey(ctx context.Context, name, key string) error {
	t, ok := o.m.Registry.Get(name)
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	models, _, err := o.models(ctx, key)
	if err != nil {
		return fmt.Errorf("key validation: %w", err)
	}
	return o.m.RotateKey(ctx, t, key, models, false)
}

func (o managerOps) List() []registry.Tenant { return o.m.Registry.List() }

type adminRequest struct {
	ID               int64  `json:"id"`
	Op               string `json:"op"`
	Name             string `json:"name"`
	Key              string `json:"key"`
	AllowEmptyModels bool   `json:"allow_empty_models"`
}

type adminResponse struct {
	ID      int64          `json:"id"`
	OK      bool           `json:"ok"`
	Result  map[string]any `json:"result,omitempty"`
	Error   string         `json:"error,omitempty"`
	ErrType string         `json:"error_type,omitempty"`
}

// AdminServer answers one newline-delimited JSON request per connection.
type AdminServer struct {
	Ops        AdminOps
	AllowedUID []int
	Log        logger
}

type logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

type stdLogger struct{ w io.Writer }

func (l stdLogger) log(level, msg string, args ...any) {
	parts := []string{level, msg}
	for i := 0; i+1 < len(args); i += 2 {
		parts = append(parts, fmt.Sprint(args[i], "=", args[i+1]))
	}
	fmt.Fprintln(l.w, strings.Join(parts, " "))
}
func (l stdLogger) Warn(msg string, args ...any) { l.log("WARN", msg, args...) }
func (l stdLogger) Info(msg string, args ...any) { l.log("INFO", msg, args...) }

func (s *AdminServer) allowed(uid int) bool {
	if uid == 0 {
		return true
	}
	for _, candidate := range s.AllowedUID {
		if candidate == uid {
			return true
		}
	}
	return false
}

func (s *AdminServer) log() logger {
	if s.Log != nil {
		return s.Log
	}
	return stdLogger{w: os.Stderr}
}

// Serve accepts connections until the listener closes. Each connection carries exactly one
// request; a slow or stuck client cannot hold up the next one because the lifecycle lock
// inside the Manager serializes the mutations themselves.
func (s *AdminServer) Serve(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			s.log().Warn("admin accept failed", "err", err)
			return
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *AdminServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	uid, ok := peerUID(conn)
	if !ok || !s.allowed(uid) {
		s.log().Warn("admin socket rejected peer", "uid", uid)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(adminOpTimeout))
	line, err := bufio.NewReaderSize(conn, 1<<16).ReadString('\n')
	if err != nil {
		return
	}
	var req adminRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		writeAdminResponse(conn, adminResponse{Error: "invalid request", ErrType: "bad_request"})
		return
	}
	resp := s.dispatch(ctx, req)
	writeAdminResponse(conn, resp)
}

func writeAdminResponse(conn net.Conn, resp adminResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(data, '\n'))
}

func (s *AdminServer) dispatch(ctx context.Context, req adminRequest) adminResponse {
	resp := adminResponse{ID: req.ID}
	result, err := s.runOp(ctx, req)
	if err != nil {
		resp.Error = err.Error()
		var invalid invalidRequestError
		if errors.As(err, &invalid) {
			resp.ErrType = "bad_request"
		} else {
			resp.ErrType = "internal"
		}
		return resp
	}
	resp.OK = true
	resp.Result = result
	return resp
}

type invalidRequestError struct{ msg string }

func (e invalidRequestError) Error() string { return e.msg }

func (s *AdminServer) runOp(ctx context.Context, req adminRequest) (map[string]any, error) {
	switch req.Op {
	case "ping":
		return map[string]any{"ok": true}, nil
	case "tenant-list":
		tenants := s.Ops.List()
		rows := make([]map[string]any, 0, len(tenants))
		for _, t := range tenants {
			rows = append(rows, map[string]any{
				"name": t.Name, "public_port": t.PublicPort, "worker_port": t.WorkerPort,
				"uid": t.UID, "models_pending": t.ModelsPending,
			})
		}
		return map[string]any{"tenants": rows}, nil
	case "tenant-create":
		if err := validOpName(req.Name); err != nil {
			return nil, err
		}
		if strings.TrimSpace(req.Key) == "" {
			return nil, invalidRequestError{"key is required"}
		}
		t, err := s.Ops.Create(ctx, req.Name, req.Key, req.AllowEmptyModels)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": t.Name, "public_port": t.PublicPort, "uid": t.UID}, nil
	case "tenant-start":
		if err := validOpName(req.Name); err != nil {
			return nil, err
		}
		return map[string]any{"started": req.Name}, s.Ops.Start(ctx, req.Name)
	case "tenant-stop":
		if err := validOpName(req.Name); err != nil {
			return nil, err
		}
		return map[string]any{"stopped": req.Name}, s.Ops.Stop(ctx, req.Name)
	case "tenant-set-key":
		if err := validOpName(req.Name); err != nil {
			return nil, err
		}
		if strings.TrimSpace(req.Key) == "" {
			return nil, invalidRequestError{"key is required"}
		}
		return map[string]any{"key_set": req.Name}, s.Ops.SetKey(ctx, req.Name, req.Key)
	default:
		return nil, invalidRequestError{"unknown op " + req.Op}
	}
}

func validOpName(name string) error {
	if !config.ValidTenantName(name) {
		return invalidRequestError{"invalid tenant name"}
	}
	return nil
}

// peerUID returns the peer's effective UID over SO_PEERCRED. Anything unusual (non-UNIX
// socket, absent credentials) fails closed.
func peerUID(conn net.Conn) (int, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, false
	}
	uid := -1
	ctrlErr := raw.Control(func(fd uintptr) {
		ucred, sockErr := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if sockErr != nil {
			return
		}
		uid = int(ucred.Uid)
	})
	if ctrlErr != nil || uid < 0 {
		return -1, false
	}
	return uid, true
}

// ListenAdmin binds the configured socket path with tight file permissions: the directory
// is created 0755 root-owned, the socket itself 0660 and — when exactly one peer UID is
// configured — owned by that UID, so only aigw (and root) can even connect.
func ListenAdmin(cfg *config.Config) (net.Listener, error) {
	if strings.TrimSpace(cfg.AdminSocket) == "" {
		return nil, errors.New(`admin_socket is not configured; admin-serve stays disabled`)
	}
	if !filepath.IsAbs(cfg.AdminSocket) {
		return nil, errors.New("admin_socket must be an absolute path")
	}
	if len(cfg.AdminAllowedUIDs) == 0 {
		return nil, errors.New("admin_allowed_uids must list at least one peer UID (typically the aigw runtime user)")
	}
	dir := filepath.Dir(cfg.AdminSocket)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("admin socket directory %q is not a directory", dir)
	}
	_ = os.Remove(cfg.AdminSocket)
	ln, err := net.Listen("unix", cfg.AdminSocket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.AdminSocket, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	if len(cfg.AdminAllowedUIDs) == 1 {
		if err := os.Chown(cfg.AdminSocket, cfg.AdminAllowedUIDs[0], -1); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

func (c *cli) adminServe(ctx context.Context) error {
	if err := requireRoot(); err != nil {
		return err
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	ln, err := ListenAdmin(deps.cfg)
	if err != nil {
		return err
	}
	defer ln.Close()
	server := &AdminServer{
		Ops:        managerOps{m: deps.manager, validator: deps.validator, cfg: deps.cfg},
		AllowedUID: deps.cfg.AdminAllowedUIDs,
	}
	fmt.Fprintf(c.stdout, "dshgw admin channel listening on %s (allowed uids: %v)\n", ln.Addr(), deps.cfg.AdminAllowedUIDs)
	// admin-serve is a long-running daemon: the generic 2-minute command timeout does not
	// apply, the caller passes a context without a deadline.
	go func() { <-ctx.Done(); ln.Close() }()
	server.Serve(ctx, ln)
	return nil
}
