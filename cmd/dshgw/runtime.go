package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"github.com/winger/ai-gateway/internal/dshgw/session"
	"github.com/winger/ai-gateway/internal/dshgw/sshworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
)

type runtimeDeps struct {
	cfg       *config.Config
	reg       *registry.Registry
	validator *aigw.Client
	manager   *tenancy.Manager
}

func (c *cli) loadRuntime(withSessions bool) (*runtimeDeps, error) {
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return nil, &codedError{code: 2, err: fmt.Errorf("configuration error: %w", err)}
	}
	reg, err := registry.Load(cfg.RegistryPath, cfg.KeyMapPath)
	if err != nil {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	client := &aigw.Client{BaseURL: cfg.AigwBaseURL, HTTP: &http.Client{Timeout: cfg.ValidateTimeout.Duration()}}
	manager := &tenancy.Manager{Config: cfg, Registry: reg, Activity: &activity.Store{Path: cfg.ActivityPath}}
	if withSessions {
		store, err := session.NewFileStoreWithLimit(cfg.SessionPath, cfg.MaxSessions)
		if err != nil {
			return nil, fmt.Errorf("load sessions: %w", err)
		}
		manager.Sessions = store
	}
	// The lifecycle refreshes a tenant's model list from aigw right before its
	// worker starts, so dsh is configured from current grants rather than from
	// whenever `sync-models` was last run by hand.
	manager.ModelRefresh = modelRefreshHook(cfg, client, manager, nil)
	// M64: the same hook every process path needs. A tenant is removed by a *CLI* invocation
	// as often as from the serving process, and a mount is kernel state: without this hook
	// such a removal purges the workspace while the mount is still attached, which fails with
	// a bare EBUSY (and, worse, would walk through a live mount if it did not). Constructing
	// the service here — a pure struct, no listeners, no polling — keeps both paths honest;
	// `serve` additionally fails hard when sshfs is missing, while a CLI command that has
	// nothing to do with ssh only warns.
	ssh, err := sshWorkspaceService(cfg, manager, nil)
	if err != nil {
		return nil, err
	}
	manager.SSHWorkspaces = ssh
	return &runtimeDeps{cfg: cfg, reg: reg, validator: client, manager: manager}, nil
}

// sshWorkspaceService builds the M64 service when the feature is configured, or returns a nil
// hook when it is not. The restart callback is the manager's own restart so a new mount can
// become visible inside the account's sandbox (its profile binds mount points at worker start).
func sshWorkspaceService(cfg *config.Config, manager *tenancy.Manager, logger *slog.Logger) (tenancy.SSHWorkspaceHook, error) {
	if !cfg.SSHWorkspaces.Enabled {
		return nil, nil
	}
	restart := func(ctx context.Context, tenant string) error {
		current, ok := manager.Registry.Get(tenant)
		if !ok {
			return fmt.Errorf("unknown tenant %s", tenant)
		}
		return manager.Restart(ctx, current)
	}
	service, err := sshworkspace.New(sshworkspace.Options{
		MountSubdir:     cfg.SSHWorkspaces.MountSubdir,
		SSHBin:          cfg.SSHWorkspaces.SSHBin,
		SSHFSBin:        cfg.SSHWorkspaces.SSHFSBin,
		IdentitySource:  cfg.SSHWorkspaces.IdentitySource,
		IdentityDir:     cfg.SSHWorkspaces.IdentityDir,
		SSHConfigSource: cfg.SSHWorkspaces.SSHConfigSource,
		Hosts:           cfg.SSHWorkspaces.Hosts,
		ConnectTimeout:  cfg.SSHWorkspaces.ConnectTimeout.Duration(),
		MaxEntries:      cfg.SSHWorkspaces.MaxEntries,
		SSHFSOptions:    cfg.SSHWorkspaces.SSHFSOptions,
	}, sshworkspace.NewStore(filepath.Join(cfg.StateDir, "ssh-mounts.json")), restart, &audit.JSONL{Path: cfg.AuditPath}, logger)
	if err != nil {
		return nil, fmt.Errorf("ssh workspaces: %w", err)
	}
	return service, nil
}
func readKey(input io.Reader, keyFile string) (string, error) {
	var data []byte
	var err error
	if keyFile != "" {
		if !filepath.IsAbs(keyFile) {
			return "", errors.New("--key-file must be absolute")
		}
		if err = securefile.CheckPermissions(keyFile, 0o600); err != nil {
			return "", err
		}
		data, err = securefile.ReadLimitedRegular(keyFile, 8193)
	} else {
		data, err = io.ReadAll(io.LimitReader(input, 8193))
	}
	if err != nil {
		return "", err
	}
	if len(data) > 8192 {
		return "", errors.New("API key input exceeds 8192 bytes")
	}
	return aigw.NormalizeKey(string(data))
}
func validateKey(ctx context.Context, client *aigw.Client, key string) ([]string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, client.HTTP.Timeout)
	defer cancel()
	models, err := client.ValidateKey(checkCtx, key)
	if errors.Is(err, aigw.ErrInvalidKey) {
		return nil, errors.New("API key is invalid or disabled")
	}
	return models, err
}
func listeningPorts(ctx context.Context) (map[int]bool, error) {
	cmd := exec.CommandContext(ctx, "ss", "-H", "-ltn")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect listening ports with ss: %w", err)
	}
	return registry.ListeningPorts(out), nil
}
func tenantByName(reg *registry.Registry, name string) (registry.Tenant, error) {
	tenant, ok := reg.Get(name)
	if !ok {
		return tenant, fmt.Errorf("tenant %q not found", name)
	}
	return tenant, nil
}
func cleanPrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if len(prefix) != 12 {
		return "", errors.New("prefix must contain exactly 12 printable ASCII characters")
	}
	for _, b := range []byte(prefix) {
		if b < 0x21 || b > 0x7e {
			return "", errors.New("prefix must contain exactly 12 printable ASCII characters")
		}
	}
	return prefix, nil
}
