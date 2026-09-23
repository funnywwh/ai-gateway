package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/browsermount"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/hostshare"
	"github.com/winger/ai-gateway/internal/dshgw/nodeclient"
	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"github.com/winger/ai-gateway/internal/dshgw/session"
	"github.com/winger/ai-gateway/internal/dshgw/sshworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type runtimeDeps struct {
	cfg       *config.Config
	reg       *registry.Registry
	validator *aigw.Client
	manager   *tenancy.Manager
	// nodes is the control plane's node records (M77) and nodeView the merged view of the
	// static configuration list plus those records. Both stay empty on a worker node and in a
	// single-machine deployment that declares nothing.
	nodes    *nodestore.Store
	nodeView []nodestore.Node
	// nodeAdmin is the shared node surface (M77): one implementation for the CLI, the admin
	// channel and (through it) the console, including the single-flight deploy jobs.
	nodeAdmin *nodeAdmin
}

// nodeViewNamed looks one node up in the merged node view (M77).
func (d *runtimeDeps) nodeViewNamed(name string) (nodestore.Node, bool) {
	return nodestore.Find(d.nodeView, name)
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
	// M77. A worker node keeps its own allocation table, so every record in it is local by
	// definition: a record naming a remote node cannot be honoured from inside a node (the
	// lifecycle layer would try to reach a node from a node). A control plane instead checks
	// its tenants against the merged node view built below, where "an unknown node" is a
	// startup failure rather than a tenant whose every request fails without a hint.
	store := nodestore.New(cfg.NodeStorePath())
	var view []nodestore.Node
	var nodeClients *nodeclient.Set
	if cfg.NodeMode() {
		if err := reg.ValidateNodes(func(string) bool { return false }); err != nil {
			return nil, err
		}
	} else {
		loaded, err := nodestore.Load(cfg.NodeStorePath())
		if err != nil {
			return nil, fmt.Errorf("load node store: %w", err)
		}
		merged, err := loaded.View(cfg.Nodes)
		if err != nil {
			return nil, err
		}
		if err := reg.ValidateNodes(nodestore.NamesKnown(merged)); err != nil {
			return nil, err
		}
		built, err := nodeClientsFor(cfg, merged)
		if err != nil {
			return nil, err
		}
		store, view, nodeClients = loaded, merged, built
	}
	client := &aigw.Client{BaseURL: cfg.AigwBaseURL, HTTP: &http.Client{Timeout: cfg.ValidateTimeout.Duration()}}
	manager := &tenancy.Manager{Config: cfg, Registry: reg, Activity: &activity.Store{Path: cfg.ActivityPath}, Nodes: nodeClients}
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
	// The logger, not nil: the service's warnings are the only narrative of a mount it refused
	// or had to break (a self-nesting source, a wedge it aborted), and nil silently discards
	// them — the audit stream records the same events, but an operator reads the log first.
	// slog.Default() is the process logger in every command shape, serve included.
	ssh, err := sshWorkspaceService(cfg, manager, slog.Default())
	if err != nil {
		return nil, err
	}
	manager.SSHWorkspaces = ssh
	// M71: the operator-declared host directories. No binaries, no polling, no kernel mounts —
	// the service only resolves configuration into bindings and materializes their sandbox
	// paths, so it is built unconditionally and stays nil when nothing is declared.
	shares, err := hostShareService(cfg, slog.Default())
	if err != nil {
		return nil, err
	}
	manager.HostShares = shares
	// CLI processes cannot detach mounts owned by the live browser transport.
	// Guard even while disabled: a config toggle does not remove existing kernel mounts.
	manager.BrowserWorkspaces = &browsermount.DetachedGuard{Registry: reg}
	deps := &runtimeDeps{cfg: cfg, reg: reg, validator: client, manager: manager, nodes: store, nodeView: view}
	deps.nodeAdmin = newNodeAdmin(deps, version, revision)
	return deps, nil
}

// hostShareService builds the M71 service from the deployment's declarations, or returns a nil
// hook when none are configured. It never fails for a missing share: config validation already
// refused a path that is not a directory, and a directory that disappears later is skipped at
// worker start rather than taking the whole gateway down.
func hostShareService(cfg *config.Config, logger *slog.Logger) (tenancy.HostShareHook, error) {
	if !cfg.HostShares.Enabled || len(cfg.HostShares.Shares) == 0 {
		return nil, nil
	}
	declarations := make([]hostshare.Declaration, 0, len(cfg.HostShares.Shares))
	for _, share := range cfg.HostShares.Shares {
		source := share.Path
		// The bind uses the resolved path, so a symlinked declaration cannot become a second
		// path to something the validation did not look at.
		if resolved, err := filepath.EvalSymlinks(share.Path); err == nil {
			source = resolved
		}
		declarations = append(declarations, hostshare.Declaration{
			Name:     share.Name,
			Source:   source,
			ReadOnly: share.EffectiveReadOnly(),
			Tenants:  share.Tenants,
		})
	}
	return hostshare.New(hostshare.Options{Subdir: cfg.HostShares.Subdir, Declarations: declarations}, logger)
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
		MountSubdir:    cfg.SSHWorkspaces.MountSubdir,
		SSHBin:         cfg.SSHWorkspaces.SSHBin,
		SSHFSBin:       cfg.SSHWorkspaces.SSHFSBin,
		IdentityDir:    cfg.SSHWorkspaces.IdentityDir,
		SSHConfigDir:   cfg.SSHWorkspaces.SSHConfigDir,
		Hosts:          cfg.SSHWorkspaces.Hosts,
		ConnectTimeout: cfg.SSHWorkspaces.ConnectTimeout.Duration(),
		MaxEntries:     cfg.SSHWorkspaces.MaxEntries,
		SSHFSOptions:   cfg.SSHWorkspaces.SSHFSOptions,
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
func validateKey(ctx context.Context, client *aigw.Client, key string) ([]aigw.Model, error) {
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

// nodeClientsFor builds one client per node from the merged node view (configuration + records).
//
// The token is resolved here: from the record, from a 0600 file the record names, or — for a node
// that has been registered but not deployed — missing entirely, which is legal and simply means
// every call to it is refused until the deploy installs one.
func nodeClientsFor(cfg *config.Config, nodes []nodestore.Node) (*nodeclient.Set, error) {
	specs := make([]nodeclient.Spec, 0, len(nodes))
	for _, node := range nodes {
		token := ""
		switch {
		case strings.TrimSpace(node.Token) != "":
			token = strings.TrimSpace(node.Token)
		case strings.TrimSpace(node.TokenFile) != "":
			// An operator who keeps the secret in a file on the control plane: a file that is not
			// there is a configuration error the gateway must report, not paper over.
			resolved, err := cfg.NodeToken(config.Node{Name: node.Name, Token: node.Token, TokenFile: node.TokenFile})
			if err != nil {
				return nil, err
			}
			token = resolved
		default:
		}
		specs = append(specs, nodeclient.Spec{Name: node.Name, BaseURL: node.URL, Token: token})
	}
	return nodeclient.NewSet(specs)
}

// nodeStoreFingerprint is what the serve loop compares to notice that the node store changed
// underneath it (a deploy, a rotation, a console registration).
func nodeStoreFingerprint(cfg *config.Config) (time.Time, int64) {
	info, err := os.Stat(cfg.NodeStorePath())
	if err != nil {
		return time.Time{}, -1
	}
	return info.ModTime(), info.Size()
}

// refreshNodeClients re-reads the node store and updates the running gateway's clients.
//
// Without it a deploy or a rotation made through this gateway's own admin channel (the console's
// button) would not reach the clients already in memory, and every tenant on that node would fail
// until a restart — the one thing a "one-click" button must not require.
func refreshNodeClients(cfg *config.Config, store *nodestore.Store, clients *nodeclient.Set) error {
	nodes, err := store.View(cfg.Nodes)
	if err != nil {
		return err
	}
	fresh, err := nodeClientsFor(cfg, nodes)
	if err != nil {
		return err
	}
	specs := make([]nodeclient.Spec, 0, fresh.Len())
	fresh.Each(func(client *nodeclient.Client) {
		specs = append(specs, nodeclient.Spec{Name: client.Name, BaseURL: client.BaseAddress(), Token: client.Secret()})
	})
	clients.Refresh(specs)
	return nil
}
