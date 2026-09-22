package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"github.com/winger/ai-gateway/internal/dshgw/session"
)

type Command struct {
	Path string
	Args []string
	Env  []string
}
type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s failed: %w: %s", c.Path, strings.Join(c.Args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// WorkerState is what the lifecycle commands report about one tenant's worker:
// the process state and the operator's durable intent behind it.
type WorkerState struct {
	Running   bool
	Suspended bool
	Detail    string
	PID       int
	StartURL  string
	StartedAt time.Time
}
type Manager struct {
	Config   *config.Config
	Registry *registry.Registry
	Sessions session.Store
	Activity interface{ Delete(string) error }
	// Workers owns every tenant worker process. M58 replaced systemd units with
	// direct child processes, so lifecycle operations are process operations.
	// Inject before concurrent use; workersMu protects lazy initialization only.
	Workers   *WorkerRunner
	workersMu sync.Mutex
	Taken     func(int) bool
	Probe     func(context.Context, registry.Tenant) error
	Now       func() time.Time
	// ModelRefresh refreshes one tenant's model list from aigw before its worker
	// starts. The composition root injects it: tenancy decides *when* a refresh
	// is mandatory, the CLI owns the aigw client and the key source.
	ModelRefresh func(context.Context, registry.Tenant) error
	// Logger receives worker lifecycle events.
	Logger *slog.Logger
	// WorkerAccount validates the unprivileged account a worker runs as.
	// Tests inject a stub; production uses sandbox.ValidateWorkerAccount, which
	// additionally requires the account to exist on the host.
	WorkerAccount func(string) error
	// RuntimeCheck validates the host-side bindings a bwrap profile needs.
	// Tests inject sandbox.ValidateBindings; production uses
	// sandbox.ValidateRuntime, which also requires the host's linker layout.
	RuntimeCheck func(sandbox.Runtime) error
	// SSHWorkspaces is the slice of the ssh-workspace service (M64) the lifecycle needs:
	// the mount points to bind into a worker, the per-account ssh identity to provision,
	// and the mounts to detach when an account goes away. Nil disables the feature, which
	// is what a deployment that does not configure it gets.
	SSHWorkspaces     SSHWorkspaceHook
	HostShares        HostShareHook
	BrowserWorkspaces BrowserWorkspaceHook
}

// HostShareHook is the host-share surface the lifecycle needs (M71): the bindings one account's
// profile carries, and the sandbox paths they need to exist before a worker starts. Nil
// disables the feature.
type HostShareHook interface {
	// ContainerFor is the gateway-managed container inside one account's workspace.
	ContainerFor(workspace string) string
	// SharesFor lists the bindings one account's sandbox must carry.
	SharesFor(tenant, workspace string) []sandbox.HostShare
	// Ensure creates the container, every target and the account's mirror of the list.
	Ensure(tenant, workspace, dshHome string) error
}

// BrowserWorkspaceHook supplies explicit binds and lifecycle cleanup for browser mounts.
type BrowserWorkspaceHook interface {
	MountsFor(string) []string
	DropTenant(context.Context, string) error
	// DetachTenant is the logout half (M76): this account's mounts are excluded from every
	// worker profile and forced out of the mount table, and no worker is started or stopped for
	// them — the logout path stops the tenant's dsh LAST.
	DetachTenant(context.Context, string) error
	// AttachedMounts lists what is still attached right now, so a teardown can report what it
	// took and what it could not.
	AttachedMounts(string) []string
}

// SSHWorkspaceHook is the ssh-workspace surface the tenancy lifecycle depends on. It is an
// interface rather than the concrete service so this package stays independent of it (and
// so the lifecycle tests can hand in a stub).
type SSHWorkspaceHook interface {
	// MountsFor lists the mount points to bind for one account.
	MountsFor(tenant string) []string
	// EnsureIdentity provisions the account's ssh key when it has none.
	EnsureIdentity(tenant, workspace, dshHome string) error
	// DropTenant detaches every mount the account owns.
	DropTenant(ctx context.Context, tenant string) error
	// DetachTenant is the logout half (M76): the account's mounts are detached while its records,
	// its mirror and its mount points stay, so the next sign-in can put them back.
	DetachTenant(ctx context.Context, tenant string) error
	// AttachedMounts lists what is still attached right now.
	AttachedMounts(tenant string) []string
	// Restore re-mounts the account's recorded mounts that are not attached. Login calls it
	// BEFORE the worker starts, because a worker's profile binds the mount points that exist
	// when it starts.
	Restore(ctx context.Context, tenant, workspace, dshHome string) error
}

// LogoutResult is what one tenant's logout teardown did (M76). The proxy audits it, so it says
// what an operator would ask afterwards: how many mounts went away, which ones did not, and
// whether the dsh itself is really gone.
type LogoutResult struct {
	MountsDetached int
	MountsLeftover []string
	WorkerStopped  bool
}

// The teardown budgets of a logout. The mounts go first and the worker LAST (M76), so each
// phase gets its own bound: a remote or a wedged FUSE must not eat the time the worker stop
// needs, and the whole sequence stays well inside the proxy's response budget.
const (
	logoutMountBudget = 15 * time.Second
	logoutStopBudget  = 30 * time.Second
)

// log returns the manager's logger, falling back to the default one. Manager is constructed by
// several entry points — serve, each CLI command, tests — and not all of them set Logger, so
// logging must never be the reason a lifecycle step panics.
func (m *Manager) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// ensureSSHIdentity provisions one account's ssh material when the feature is on.
func (m *Manager) ensureSSHIdentity(t registry.Tenant) error {
	if m.SSHWorkspaces == nil {
		return nil
	}
	return m.SSHWorkspaces.EnsureIdentity(t.Name, t.Workspace, t.DshHome)
}

// ensureHostShares materializes one account's host-share container and targets. It must run
// before the profile is rendered: the profile binds those paths, and --bind-try silently skips
// a target that does not exist yet.
func (m *Manager) ensureHostShares(t registry.Tenant) error {
	if m.HostShares == nil {
		return nil
	}
	return m.HostShares.Ensure(t.Name, t.Workspace, t.DshHome)
}

// workers returns the worker runner, creating it on first use. Tests inject their
// own runner (with a stand-in profile) instead of starting real sandboxes.
func (m *Manager) workers() *WorkerRunner {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	if m.Workers == nil {
		m.Workers = &WorkerRunner{
			Config: m.Config, Profile: m.SandboxProfile, Probe: m.ProbeWorker, Logger: m.Logger,
			CanStart: m.workerStartAllowed,
			Limits: WorkerLimits{
				MemoryHighBytes: m.Config.WorkerLimits.MemoryHighBytes,
				MemoryMaxBytes:  m.Config.WorkerLimits.MemoryMaxBytes,
				TasksMax:        m.Config.WorkerLimits.TasksMax,
				CPUQuotaPercent: m.Config.WorkerLimits.CPUQuotaPercent,
			},
		}
	}
	return m.Workers
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) WithLifecycleLock(operation func() error) error {
	lockPath := filepath.Join(filepath.Dir(m.Registry.Path()), "lifecycle.lock")
	return securefile.WithLock(lockPath, func() error {
		if err := m.Registry.Reload(); err != nil {
			return err
		}
		return operation()
	})
}

type CreateOptions struct {
	AllowEmptyModels bool
	DirectoryPicker  string
	PluginBrowserFS  string
	// Account is the aigw account this tenant belongs to (M67), recorded so the tenant's
	// sidebar can name the person signed in. Empty is accepted: the CLI creates tenants
	// without an account, and the sidebar falls back to the tenant name.
	Account string
}

func (m *Manager) Create(ctx context.Context, name, key string, models []aigw.Model, opt CreateOptions) (created registry.Tenant, err error) {
	err = m.WithLifecycleLock(func() error {
		var createErr error
		created, createErr = m.createLocked(ctx, name, key, models, opt)
		return createErr
	})
	return created, err
}

func (m *Manager) createLocked(ctx context.Context, name, key string, models []aigw.Model, opt CreateOptions) (created registry.Tenant, err error) {
	if !config.ValidTenantName(name) {
		return created, fmt.Errorf("invalid tenant name %q", name)
	}
	if m.Config.IsReservedTenant(name) {
		return created, fmt.Errorf("tenant name %q is reserved", name)
	}
	if _, ok := m.Registry.Get(name); ok {
		return created, fmt.Errorf("tenant %q already exists", name)
	}
	key, err = aigw.NormalizeKey(key)
	if err != nil {
		return created, err
	}
	if len(models) == 0 && !opt.AllowEmptyModels {
		return created, errors.New("valid API key currently has no available models; grant models or pass --allow-empty-models")
	}
	picker := opt.DirectoryPicker
	if picker == "" {
		picker = m.Config.DirectoryPicker
	}
	if picker != "clamp" && picker != "browse" {
		return created, fmt.Errorf("invalid directory picker %q", picker)
	}
	browser := opt.PluginBrowserFS
	if browser == "" {
		browser = m.Config.PluginBrowserFS
	}
	if browser != "on" && browser != "off" {
		return created, fmt.Errorf("invalid browser-fs mode %q", browser)
	}
	if existing, ok := m.Registry.ByPrefix(key[:12]); ok {
		return created, fmt.Errorf("key prefix already belongs to tenant %q", existing.Name)
	}
	account := strings.TrimSpace(opt.Account)
	if account != "" {
		// Checked before anything is written: a label that cannot be stored must fail the
		// create rather than leave a half-provisioned tenant behind.
		if err := registry.ValidateAccountLabel(account); err != nil {
			return created, err
		}
	}
	if err := ValidateTemplate(m.Config.Deploy.TemplateHome, browser == "on"); err != nil {
		return created, err
	}
	pub, worker, err := m.Registry.AssignPorts(m.Config, m.Taken)
	if err != nil {
		return created, err
	}
	created = registry.Tenant{Name: name, PublicPort: pub, WorkerPort: worker, KeyPrefix: key[:12], DshHome: filepath.Join(m.Config.TenantRoot, name, ".dsh"), Workspace: filepath.Join(m.Config.WorkspaceRoot, name), CreatedAt: m.now(), Handshake: registry.HandshakePending, DirectoryPicker: picker, PluginBrowserFS: browser, ModelsPending: len(models) == 0, Isolation: registry.IsolationBwrap, Account: strings.TrimSpace(opt.Account)}
	// The tenant's own roots. They are deduplicated because a deployment may point
	// tenant_config_root and tenant_root at the same directory: the layout is the
	// operator's business, and creating the same path twice is not an error worth
	// failing a tenant over.
	paths := dedupePaths(filepath.Dir(created.DshHome), created.Workspace, filepath.Join(m.Config.Deploy.TenantConfigRoot, name))
	handshakePath := filepath.Join(m.Config.HandshakeDir, name+".url")
	// A removed tenant may deliberately retain its data. Never adopt or clean
	// up an existing destination, including a dangling symlink.
	for _, path := range append(append([]string(nil), paths...), handshakePath) {
		if _, statErr := os.Lstat(path); statErr == nil {
			return created, fmt.Errorf("tenant destination already exists: %s", path)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return created, statErr
		}
	}
	registryAdded := false
	startAttempted := false
	var ownedPaths []string
	defer func() {
		if err == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		safeToRemove := true
		if startAttempted {
			if stopErr := m.workers().Stop(rollbackCtx, created); stopErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback stop worker (data retained): %w", stopErr))
				safeToRemove = false
			}
		}
		if registryAdded {
			m.Registry.Delete(name)
			if saveErr := m.Registry.Save(); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback registry: %w", saveErr))
				safeToRemove = false
			}
		}
		if safeToRemove {
			for i := len(ownedPaths) - 1; i >= 0; i-- {
				if removeErr := os.RemoveAll(ownedPaths[i]); removeErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback directory %s: %w", ownedPaths[i], removeErr))
				}
			}
			if startAttempted {
				if removeErr := os.Remove(handshakePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					err = errors.Join(err, fmt.Errorf("rollback handshake: %w", removeErr))
				}
			}
		}
	}()
	for _, path := range paths {
		// Tenant directories belong to the account dshgw itself runs as: with no
		// per-tenant identity there is nothing to chown, and 0700 keeps an
		// unrelated local user out of the second line of defence behind the mount
		// namespace.
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return created, err
		}
		if err = os.Mkdir(path, 0o700); err != nil {
			return created, err
		}
		ownedPaths = append(ownedPaths, path)
		if err = os.Chmod(path, 0o700); err != nil {
			return created, err
		}
	}
	created.UID = os.Geteuid()
	if err = m.SandboxProfileReady(created); err != nil {
		return created, err
	}
	if err = copyProfileTemplate(m.Config.Deploy.TemplateHome, created.DshHome); err != nil {
		return created, err
	}
	// The account's ssh identity, so an ssh workspace can be created the first time someone
	// asks for one. A missing or too-broad key source is a configuration error and fails the
	// create: a half-provisioned account is what produces "the button does nothing" later.
	if err = m.ensureSSHIdentity(created); err != nil {
		return created, err
	}
	// The account's host-share container and targets (M71), for the same reason: the first
	// worker start must find them, and the profile binds what is in the configuration.
	if err = m.ensureHostShares(created); err != nil {
		return created, err
	}
	for _, seed := range m.Config.WorkspaceSeed {
		if err = os.MkdirAll(filepath.Join(created.Workspace, seed), 0o700); err != nil {
			return created, err
		}
	}
	artifacts, renderErr := RenderTenantArtifacts(m.Config, created, key, models, TenantOptions{DirectoryPicker: picker, PluginBrowserFS: browser}, m.now())
	if renderErr != nil {
		err = renderErr
		return created, err
	}
	if err = WriteArtifacts(artifacts); err != nil {
		return created, err
	}
	if err = m.Registry.Put(created); err != nil {
		return created, err
	}
	registryAdded = true
	if err = m.Registry.Save(); err != nil {
		return created, err
	}
	startAttempted = true
	if err = m.workers().Start(ctx, created); err != nil {
		return created, err
	}
	probe := m.Probe
	if probe == nil {
		probe = m.probeWorker
	}
	if err = probe(ctx, created); err != nil {
		return created, fmt.Errorf("worker readiness probe: %w (worker output: %s)", err, m.workers().Output(created.Name))
	}
	if err = m.Registry.SetHandshake(name, registry.HandshakeOK); err != nil {
		return created, err
	}
	if err = m.Registry.Save(); err != nil {
		return created, err
	}
	created.Handshake = registry.HandshakeOK
	return created, nil
}

func ValidateTemplate(root string, requireBrowserFS bool) error {
	profile := filepath.Join(root, "profiles", "web", "package.json")
	data, err := os.ReadFile(profile)
	if err != nil {
		return fmt.Errorf("template profile is unavailable (%s): %w", profile, err)
	}
	var doc struct {
		Dependencies map[string]string `json:"dependencies"`
		Dsh          struct {
			Profile struct {
				Bundles []string `json:"bundles"`
			} `json:"profile"`
		} `json:"dsh"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode template profile: %w", err)
	}
	if !requireBrowserFS {
		return nil
	}
	version := doc.Dependencies["dsh-browser-fs"]
	if version != "0.2.0" {
		return fmt.Errorf("template must pin dsh-browser-fs to 0.2.0 (got %q); run deploy/dshgw/prepare-template.sh", version)
	}
	for _, bundle := range doc.Dsh.Profile.Bundles {
		if bundle == "dsh-browser-fs" {
			return nil
		}
	}
	return errors.New("template installed dsh-browser-fs but did not activate its bundle")
}

func copyProfileTemplate(srcHome, dstHome string) error {
	src := filepath.Join(srcHome, "profiles")
	dst := filepath.Join(dstHome, "profiles")
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("destination profile already exists: %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyTree(src, dst)
}
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode().IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyTree(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	default:
		return fmt.Errorf("unsupported template file type: %s", src)
	}
}

// dedupePaths removes repeated and nested-equal paths, preserving order.
func dedupePaths(paths ...string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

func (m *Manager) probeWorker(ctx context.Context, t registry.Tenant) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(t.WorkerPort))
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+authority+"/api", nil)
		req.Host = authority
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				return nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return last
}
func (m *Manager) ProbeWorker(ctx context.Context, t registry.Tenant) error {
	if m.Probe != nil {
		return m.Probe(ctx, t)
	}
	return m.probeWorker(ctx, t)
}

// Restart replaces the tenant's worker process in place.
func (m *Manager) Restart(ctx context.Context, t registry.Tenant) error {
	if err := m.workers().Restart(ctx, t); err != nil {
		return err
	}
	if err := m.ProbeWorker(ctx, t); err != nil {
		return fmt.Errorf("worker readiness probe: %w (worker output: %s)", err, m.workers().Output(t.Name))
	}
	return nil
}

// StopWorker stops the worker and records the durable intent, so restarting aigw
// does not bring back a tenant the operator turned off. The old shape stored that
// intent in a systemd unit's enablement; M58 stores it in the registry.
func (m *Manager) StopWorker(ctx context.Context, t registry.Tenant) error {
	if err := m.setSuspended(t.Name, true); err != nil {
		return err
	}
	if err := m.workers().Stop(ctx, t); err != nil {
		return err
	}
	if m.BrowserWorkspaces != nil {
		return m.BrowserWorkspaces.DropTenant(ctx, t.Name)
	}
	return nil
}

// StartWorker clears the durable intent, refreshes the tenant's models and starts
// the process.
func (m *Manager) StartWorker(ctx context.Context, t registry.Tenant) error {
	if err := m.setSuspended(t.Name, false); err != nil {
		return err
	}
	if err := m.startWorker(ctx, t); err != nil {
		return err
	}
	if err := m.ProbeWorker(ctx, t); err != nil {
		return fmt.Errorf("worker readiness probe: %w (worker output: %s)", err, m.workers().Output(t.Name))
	}
	return nil
}

// EnsureRunning brings a tenant's worker up when it is not running, and reports whether it
// started one (M69).
//
// Login is the caller: a tenant whose last session signed out has its dsh stopped, so the next
// sign-in has to bring it back before the browser is redirected to it — otherwise the person
// lands on a 502 that heals only if somebody else starts the tenant. A worker that is already
// running is left alone: restarting it would kill whatever turn the tenant is in the middle of,
// and dsh hot-reloads settings.yaml, which is the only thing a login refresh changes.
//
// The operator's suspension is honoured rather than cleared: `suspended` says the deployment
// turned this tenant off, and a user signing in is not an operator action.
func (m *Manager) EnsureRunning(ctx context.Context, t registry.Tenant) (bool, error) {
	current, ok := m.Registry.Get(t.Name)
	if !ok {
		return false, fmt.Errorf("tenant %q not found", t.Name)
	}
	if current.Suspended {
		return false, fmt.Errorf("tenant %s is suspended by the operator; not starting its worker", t.Name)
	}
	state, err := m.Status(ctx, current)
	if err != nil {
		return false, err
	}
	if state.Running {
		return false, nil
	}
	if err := m.startWorker(ctx, current); err != nil {
		return false, err
	}
	if err := m.ProbeWorker(ctx, current); err != nil {
		return true, fmt.Errorf("worker readiness probe: %w (worker output: %s)", err, m.workers().Output(current.Name))
	}
	return true, nil
}

// StopForLogout stops a tenant's dsh because its last session signed out (M69, sequenced by M76).
//
// The order is the point of M76, and it is what the person clicking 退出 asked for: exclude and
// FORCE-DETACH the account's mounts (browser directory mounts and ssh workspaces), and only THEN
// force-stop its dsh. Detaching first is what makes a dsh parked in a FUSE request die quickly
// instead of burning its whole stop timeout, and it is safe because the forced ladder (lazy
// detach, then aborting the connection) does not need the worker's namespace to be gone.
//
// It deliberately does NOT record an operator suspension, and it no longer short-circuits: every
// phase runs, failures are aggregated, and the result says what was left behind. Before M76 a
// failed worker stop returned early and left every mount mounted (2026-09-22: three logouts in a
// row audited as failures while the dsh itself had already exited).
func (m *Manager) StopForLogout(ctx context.Context, t registry.Tenant) (LogoutResult, error) {
	current := t
	if live, ok := m.Registry.Get(t.Name); ok {
		current = live
	}
	var result LogoutResult
	var errs []error
	// detach runs one mount service's forced detach and folds its outcome into the result: what
	// was attached before, what is attached after, and what could not be taken.
	detach := func(label string, attached func() []string, run func(context.Context) error) {
		before := attached()
		detachCtx, cancel := context.WithTimeout(ctx, logoutMountBudget)
		err := run(detachCtx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
		}
		leftover := attached()
		result.MountsLeftover = append(result.MountsLeftover, leftover...)
		if gone := len(before) - len(leftover); gone > 0 {
			result.MountsDetached += gone
		}
	}
	if m.BrowserWorkspaces != nil {
		detach("browser mounts",
			func() []string { return m.BrowserWorkspaces.AttachedMounts(current.Name) },
			func(ctx context.Context) error { return m.BrowserWorkspaces.DetachTenant(ctx, current.Name) })
	}
	if m.SSHWorkspaces != nil {
		detach("ssh workspaces",
			func() []string { return m.SSHWorkspaces.AttachedMounts(current.Name) },
			func(ctx context.Context) error { return m.SSHWorkspaces.DetachTenant(ctx, current.Name) })
	}
	// LAST: the dsh itself. The runner signals TERM, escalates to SIGKILL and reaps the worker's
	// scope, so this is the force-exit the person asked for, and it is verified rather than
	// assumed.
	stopCtx, cancel := context.WithTimeout(ctx, logoutStopBudget)
	err := m.workers().Stop(stopCtx, current)
	cancel()
	if err != nil {
		errs = append(errs, fmt.Errorf("stopping %s: %w", current.Name, err))
	} else if state, statusErr := m.Status(ctx, current); statusErr == nil && !state.Running {
		result.WorkerStopped = true
	}
	return result, errors.Join(errs...)
}

// Enable is the console's durable on/off toggle for one tenant's DSH.
func (m *Manager) Enable(ctx context.Context, t registry.Tenant, on bool) error {
	if !on {
		return m.StopWorker(ctx, t)
	}
	return m.StartWorker(ctx, t)
}

// Status reports the worker process and the recorded operator intent.
func (m *Manager) Status(_ context.Context, t registry.Tenant) (WorkerState, error) {
	current := t
	if live, ok := m.Registry.Get(t.Name); ok {
		current = live
	}
	status := m.workers().Status(current)
	return WorkerState{
		Running:   status.Running,
		Suspended: current.Suspended,
		Detail:    status.Detail,
		PID:       status.PID,
		StartURL:  status.StartURL,
		StartedAt: status.StartedAt,
	}, nil
}

// startWorker refreshes the tenant's model list and then launches its process.
// The refresh happens here — not only at create time — because dsh is configured
// from settings.yaml: a tenant whose key gained or lost models in aigw must see
// the change when dsh starts, not whenever somebody remembers to run sync-models.
func (m *Manager) startWorker(ctx context.Context, t registry.Tenant) error {
	if err := m.ensureSSHIdentity(t); err != nil {
		return err
	}
	// The host-share paths exist before the profile that binds them is rendered.
	if err := m.ensureHostShares(t); err != nil {
		return err
	}
	// The profile's ssh row follows the feature switch on every start, so enabling or
	// disabling it reaches accounts that already exist (the artifacts are otherwise written
	// only at create/rotate time).
	if warning, err := EnsureSSHWorkspaceRow(m.Config, t); err != nil {
		return err
	} else if warning != "" {
		m.log().Warn(warning)
	}
	if warning, err := EnsureBrowserWorkspaceRow(m.Config, t); err != nil {
		return err
	} else if warning != "" {
		m.log().Warn(warning)
	}
	// The identity row's switch is checked on every start for the same reason: a tenant that
	// already exists must gain (or lose) the row when the operator flips account_card.
	if warning, err := EnsureAccountCardRow(m.Config, t); err != nil {
		return err
	} else if warning != "" {
		m.log().Warn(warning)
	}
	// The tenant-side web plugins (M75): same rule again, for three rows at once. An enabled but
	// undeployed plugin warns and loses its row instead of stopping the account.
	if warnings, err := EnsureTenantPlugins(m.Config, t); err != nil {
		return err
	} else {
		for _, warning := range warnings {
			m.log().Warn(warning)
		}
	}
	if err := m.refreshModelsBeforeStart(ctx, t); err != nil {
		return err
	}
	return m.workers().Start(ctx, t)
}

// refreshModelsBeforeStart runs the injected refresh. Any error it returns stops
// the start: the hook itself decides what is fatal (an invalid key is) and what
// only deserves a warning (aigw being briefly unreachable is).
func (m *Manager) refreshModelsBeforeStart(ctx context.Context, t registry.Tenant) error {
	if m.ModelRefresh == nil {
		return nil
	}
	if err := m.ModelRefresh(ctx, t); err != nil {
		return fmt.Errorf("refreshing models for %s before starting its worker: %w", t.Name, err)
	}
	return nil
}

// StartWorkers brings up every tenant the registry says should be running. It is
// the startup path of the supervised shape: no systemd enablement exists to do it,
// so dshgw itself restores the tenants that were not suspended.
//
// It refreshes each tenant's models first (through the same hook a manual start
// uses) and reports per-tenant failures instead of stopping at the first one.
func (m *Manager) StartWorkers(ctx context.Context) error {
	var failures []error
	for _, tenant := range m.Registry.List() {
		if tenant.Suspended {
			continue
		}
		if err := m.startWorker(ctx, tenant); err != nil {
			failures = append(failures, fmt.Errorf("tenant %s: %w", tenant.Name, err))
			continue
		}
		if err := m.ProbeWorker(ctx, tenant); err != nil {
			failures = append(failures, fmt.Errorf("tenant %s readiness: %w (worker output: %s)", tenant.Name, err, m.workers().Output(tenant.Name)))
		}
	}
	return errors.Join(failures...)
}

// setSuspended persists the operator's intent for one tenant.
func (m *Manager) setSuspended(name string, suspended bool) error {
	return m.WithLifecycleLock(func() error {
		current, ok := m.Registry.Get(name)
		if !ok {
			return fmt.Errorf("tenant %q not found", name)
		}
		if current.Suspended == suspended {
			return nil
		}
		current.Suspended = suspended
		if err := m.Registry.Put(current); err != nil {
			return err
		}
		return m.Registry.Save()
	})
}

func (m *Manager) RotateKey(ctx context.Context, t registry.Tenant, key string, models []aigw.Model, keepPrevious bool) error {
	return m.WithLifecycleLock(func() error {
		current, ok := m.Registry.Get(t.Name)
		if !ok {
			return fmt.Errorf("tenant %q not found", t.Name)
		}
		if err := m.validateTenantPaths(current); err != nil {
			return err
		}
		return m.rotateKeyLocked(ctx, current, key, models, keepPrevious)
	})
}

func (m *Manager) rotateKeyLocked(ctx context.Context, t registry.Tenant, key string, models []aigw.Model, keepPrevious bool) (err error) {
	key, err = aigw.NormalizeKey(key)
	if err != nil {
		return err
	}
	if existing, ok := m.Registry.ByPrefix(key[:12]); ok && existing.Name != t.Name {
		return fmt.Errorf("key prefix already belongs to tenant %q", existing.Name)
	}
	credentialsPath := filepath.Join(t.DshHome, ".credentials.yaml")
	settingsPath := filepath.Join(t.DshHome, "settings.yaml")
	gatewayPath := filepath.Join(m.Config.Deploy.TenantConfigRoot, t.Name, "gateway.key")

	// Registry-derived credentials and settings paths are trusted lifecycle
	// roots. Acquire both locks in this stable order and hold them across the
	// snapshots, every mutation, registry persistence, and rollback.
	return withDSHFileLock(credentialsPath, func() error {
		return withDSHFileLock(settingsPath, func() (err error) {
			oldCredentials, err := securefile.ReadLimitedRegular(credentialsPath, 16<<20)
			if err != nil {
				return err
			}
			oldSettings, err := securefile.ReadLimitedRegular(settingsPath, 16<<20)
			if err != nil {
				return err
			}
			oldGateway, gatewayErr := securefile.ReadLimitedRegular(gatewayPath, 16<<20)
			if gatewayErr != nil && !errors.Is(gatewayErr, os.ErrNotExist) {
				return gatewayErr
			}
			rotatedRegistry := false
			defer func() {
				if err == nil {
					return
				}
				if restoreErr := securefile.WriteAtomic(credentialsPath, oldCredentials, 0o600); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback credentials: %w", restoreErr))
				}
				if restoreErr := securefile.WriteAtomic(settingsPath, oldSettings, 0o600); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback settings: %w", restoreErr))
				}
				var restoreErr error
				if gatewayErr == nil {
					restoreErr = securefile.WriteAtomic(gatewayPath, oldGateway, 0o640)
				} else {
					restoreErr = securefile.RemoveFile(gatewayPath)
					if errors.Is(restoreErr, os.ErrNotExist) {
						restoreErr = nil
					}
				}
				if restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback gateway key: %w", restoreErr))
				}
				if rotatedRegistry {
					if restoreErr := m.Registry.Put(t); restoreErr != nil {
						err = errors.Join(err, fmt.Errorf("rollback tenant: %w", restoreErr))
					} else if restoreErr := m.Registry.Save(); restoreErr != nil {
						err = errors.Join(err, fmt.Errorf("rollback registry: %w", restoreErr))
					}
				}
			}()
			if err = rotateCredentialsLocked(credentialsPath, key); err != nil {
				return err
			}
			if err = updateSettingsLocked(m.Config, settingsPath, models); err != nil {
				return err
			}
			if err = securefile.WriteAtomic(gatewayPath, []byte(key+"\n"), 0o640); err != nil {
				return err
			}
			if err = m.Registry.RotatePrefix(t.Name, key[:12], keepPrevious); err != nil {
				return err
			}
			rotatedRegistry = true
			updated, _ := m.Registry.Get(t.Name)
			updated.ModelsPending = len(models) == 0
			if err = m.Registry.Put(updated); err != nil {
				return err
			}
			if err = m.Registry.Save(); err != nil {
				return err
			}
			return nil
		})
	})
}
func (m *Manager) BindPrefix(tenant, prefix string) error {
	return m.WithLifecycleLock(func() (err error) {
		old, ok := m.Registry.Get(tenant)
		if !ok {
			return fmt.Errorf("tenant %q not found", tenant)
		}
		if err = m.Registry.AddPrefix(tenant, prefix); err != nil {
			return err
		}
		if err = m.Registry.Save(); err != nil {
			if rollbackErr := m.Registry.Put(old); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback registry: %w", rollbackErr))
			} else if rollbackErr := m.Registry.Save(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback registry: %w", rollbackErr))
			}
			return err
		}
		return nil
	})
}

// EnsureProvisioned writes the files a tenant's dsh reads at startup when they are
// missing: settings.yaml (provider + the models this key may use), .credentials.yaml
// (the key reference), the profile patch, the workspace state and the stored gateway
// key. It returns true when it created them.
//
// Why this has to exist: a tenant can be in the registry without ever having been
// provisioned — written by hand, restored from a backup, migrated from the previous
// deployment, or its files deleted. Its worker then starts with no provider at all,
// which dsh reports as "settings are unavailable in this browser" and an empty model
// list. Starting dsh for that tenant is exactly the moment the files should appear,
// built from the key the tenant has and the models aigw says that key can call.
//
// An existing settings.yaml is never touched: while a tenant is provisioned its model
// list belongs to SyncModels, and rewriting it here would fight that path.
func (m *Manager) EnsureProvisioned(ctx context.Context, t registry.Tenant, key string, models []aigw.Model) (bool, error) {
	normalized, err := aigw.NormalizeKey(key)
	if err != nil {
		return false, err
	}
	provisioned := false
	err = m.WithLifecycleLock(func() error {
		current, ok := m.Registry.Get(t.Name)
		if !ok {
			return fmt.Errorf("tenant %q not found", t.Name)
		}
		if err := m.validateTenantPaths(current); err != nil {
			return err
		}
		settingsPath := filepath.Join(current.DshHome, "settings.yaml")
		switch _, err := os.Stat(settingsPath); {
		case err == nil:
			return nil // already provisioned; SyncModels owns the model list
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		// The profile tree is what makes the rendered patch loadable: our plugin and
		// the picker/client packages are resolved through it. A tenant that lost (or
		// never had) it would get a patch it cannot import, and dsh would exit on
		// startup — so the template is installed first, exactly as tenant creation
		// does, and a template that cannot satisfy the options still fails loudly
		// instead of producing a worker that dies.
		if err := ValidateTemplate(m.Config.Deploy.TemplateHome, current.PluginBrowserFS == "on"); err != nil {
			return fmt.Errorf("cannot provision %s: %w", current.Name, err)
		}
		if err := copyProfileTemplate(m.Config.Deploy.TemplateHome, current.DshHome); err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				return err
			}
		}
		artifacts, err := RenderTenantArtifacts(m.Config, current, normalized, models, TenantOptions{
			DirectoryPicker: current.DirectoryPicker,
			PluginBrowserFS: current.PluginBrowserFS,
		}, m.now())
		if err != nil {
			return err
		}
		if err := WriteArtifacts(artifacts); err != nil {
			return err
		}
		// The key's own prefix becomes the tenant's, with the old one kept as a
		// previous prefix so an existing binding is not silently dropped.
		if err := m.Registry.RotatePrefix(current.Name, normalized[:12], true); err != nil {
			return err
		}
		updated, _ := m.Registry.Get(current.Name)
		updated.ModelsPending = len(models) == 0
		if err := m.Registry.Put(updated); err != nil {
			return err
		}
		if err := m.Registry.Save(); err != nil {
			return err
		}
		provisioned = true
		return nil
	})
	return provisioned, err
}

// SyncModels updates a provisioned tenant's model list.
func (m *Manager) SyncModels(t registry.Tenant, models []aigw.Model) error {
	return m.WithLifecycleLock(func() error {
		current, ok := m.Registry.Get(t.Name)
		if !ok {
			return fmt.Errorf("tenant %q not found", t.Name)
		}
		if err := m.validateTenantPaths(current); err != nil {
			return err
		}
		settingsPath := filepath.Join(current.DshHome, "settings.yaml")
		return withDSHFileLock(settingsPath, func() (err error) {
			oldSettings, settingsErr := securefile.ReadLimitedRegular(settingsPath, 16<<20)
			if settingsErr != nil {
				return settingsErr
			}
			defer func() {
				if err == nil {
					return
				}
				restoreErr := securefile.WriteAtomic(settingsPath, oldSettings, 0o600)
				if restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback settings: %w", restoreErr))
				}
				if restoreErr := m.Registry.Put(current); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback tenant: %w", restoreErr))
				} else if restoreErr := m.Registry.Save(); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("rollback registry: %w", restoreErr))
				}
			}()
			if err = updateSettingsLocked(m.Config, settingsPath, models); err != nil {
				return err
			}
			updated := current
			updated.ModelsPending = len(models) == 0
			if err = m.Registry.Put(updated); err != nil {
				return err
			}
			if err = m.Registry.Save(); err != nil {
				return err
			}
			return nil
		})
	})
}

var startupURLRE = regexp.MustCompile(`(?m)^dsh web: (http://127\.0\.0\.1:([0-9]+)/\?token=[A-Za-z0-9_-]{43})(?: \(LAN: .+\))?$`)

// CaptureURL returns the startup URL a tenant worker printed. The runner reads it
// from the child's own output (M58), which replaced the journalctl scan the
// systemd shape needed.
func (m *Manager) CaptureURL(_ context.Context, t registry.Tenant) (string, error) {
	url, ok := m.workers().StartURL(t.Name)
	if !ok {
		return "", fmt.Errorf("worker for %s has not reported a startup URL", t.Name)
	}
	return url, nil
}
