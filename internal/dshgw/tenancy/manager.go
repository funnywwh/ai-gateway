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
	Workers *WorkerRunner
	Taken   func(int) bool
	Probe   func(context.Context, registry.Tenant) error
	Now     func() time.Time
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
}

// workers returns the worker runner, creating it on first use. Tests inject their
// own runner (with a stand-in profile) instead of starting real sandboxes.
func (m *Manager) workers() *WorkerRunner {
	if m.Workers == nil {
		m.Workers = &WorkerRunner{Config: m.Config, Profile: m.SandboxProfile, Probe: m.ProbeWorker, Logger: m.Logger}
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
}

func (m *Manager) Create(ctx context.Context, name, key string, models []string, opt CreateOptions) (created registry.Tenant, err error) {
	err = m.WithLifecycleLock(func() error {
		var createErr error
		created, createErr = m.createLocked(ctx, name, key, models, opt)
		return createErr
	})
	return created, err
}

func (m *Manager) createLocked(ctx context.Context, name, key string, models []string, opt CreateOptions) (created registry.Tenant, err error) {
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
	if err := ValidateTemplate(m.Config.Deploy.TemplateHome, browser == "on"); err != nil {
		return created, err
	}
	pub, worker, err := m.Registry.AssignPorts(m.Config, m.Taken)
	if err != nil {
		return created, err
	}
	created = registry.Tenant{Name: name, PublicPort: pub, WorkerPort: worker, KeyPrefix: key[:12], DshHome: filepath.Join(m.Config.TenantRoot, name, ".dsh"), Workspace: filepath.Join(m.Config.WorkspaceRoot, name), CreatedAt: m.now(), Handshake: registry.HandshakePending, DirectoryPicker: picker, PluginBrowserFS: browser, ModelsPending: len(models) == 0, Isolation: registry.IsolationBwrap}
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
	return m.workers().Stop(ctx, t)
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

func (m *Manager) RotateKey(ctx context.Context, t registry.Tenant, key string, models []string, keepPrevious bool) error {
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

func (m *Manager) rotateKeyLocked(ctx context.Context, t registry.Tenant, key string, models []string, keepPrevious bool) (err error) {
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
			if err = updateSettingsLocked(settingsPath, m.Config.AigwBaseURL, models); err != nil {
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

func (m *Manager) SyncModels(t registry.Tenant, models []string) error {
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
			if err = updateSettingsLocked(settingsPath, m.Config.AigwBaseURL, models); err != nil {
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
