package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
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

type UnitStatus struct {
	Active        bool
	Enabled       bool
	Detail        string
	UnitFileState string
}
type Manager struct {
	Config   *config.Config
	Registry *registry.Registry
	Sessions session.Store
	Activity interface{ Delete(string) error }
	Runner   Runner
	Taken    func(int) bool
	Probe    func(context.Context, registry.Tenant) error
	Now      func() time.Time
}

func (m *Manager) runner() Runner {
	if m.Runner != nil {
		return m.Runner
	}
	return ExecRunner{}
}
func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}
func (m *Manager) run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return m.runner().Run(ctx, Command{Path: path, Args: args})
}
func (m *Manager) user(name string) string { return m.Config.Deploy.DshUserPrefix + name }
func (m *Manager) unit(name string) string {
	template := m.Config.Deploy.WorkerUnit
	if template == "" {
		template = "dsh-worker@.service"
	}
	return strings.Replace(template, "@.service", "@"+name+".service", 1)
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
	created = registry.Tenant{Name: name, PublicPort: pub, WorkerPort: worker, KeyPrefix: key[:12], DshHome: filepath.Join(m.Config.TenantRoot, name, ".dsh"), Workspace: filepath.Join(m.Config.WorkspaceRoot, name), CreatedAt: m.now(), Handshake: registry.HandshakePending, DirectoryPicker: picker, PluginBrowserFS: browser, ModelsPending: len(models) == 0}
	paths := []string{filepath.Dir(created.DshHome), created.Workspace, filepath.Join(m.Config.Deploy.TenantConfigRoot, name)}
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
	user := m.user(name)
	userCreated := false
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
			if _, stopErr := m.run(rollbackCtx, "systemctl", "disable", "--now", m.unit(name)); stopErr != nil {
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
			if nginxErr := m.InstallNginx(rollbackCtx, true); nginxErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback nginx: %w", nginxErr))
			}
		}
		if userCreated && safeToRemove {
			// Never let userdel infer a home directory outside our claimed paths.
			if _, deleteErr := m.run(rollbackCtx, "userdel", user); deleteErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback user (data retained): %w", deleteErr))
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
		parentMode := os.FileMode(0o750)
		if filepath.Dir(path) == m.Config.TenantRoot {
			parentMode = 0o711
		}
		if err = os.MkdirAll(filepath.Dir(path), parentMode); err != nil {
			return created, err
		}
		mode := os.FileMode(0o700)
		if path == filepath.Join(m.Config.Deploy.TenantConfigRoot, name) {
			mode = 0o750
		}
		if err = os.Mkdir(path, mode); err != nil {
			return created, err
		}
		ownedPaths = append(ownedPaths, path)
		// An operator may use umask 077 after preparing key files. Mkdir's
		// masked mode must not silently remove the gateway group's search bit.
		if err = os.Chmod(path, mode); err != nil {
			return created, err
		}
	}
	if _, err = m.run(ctx, "useradd", "--system", "--user-group", "--no-create-home", "--home-dir", created.Workspace, "--shell", "/usr/sbin/nologin", user); err != nil {
		return created, err
	}
	userCreated = true
	uidOut, runErr := m.run(ctx, "id", "-u", user)
	if runErr != nil {
		err = runErr
		return created, err
	}
	created.UID, err = strconv.Atoi(strings.TrimSpace(string(uidOut)))
	if err != nil {
		return created, fmt.Errorf("parse uid: %w", err)
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
	if _, err = m.run(ctx, "chown", "-R", user+":"+user, filepath.Join(m.Config.TenantRoot, name), created.Workspace); err != nil {
		return created, err
	}
	if _, err = m.run(ctx, "chown", "-R", "root:"+m.Config.Deploy.GatewayGroup, filepath.Join(m.Config.Deploy.TenantConfigRoot, name)); err != nil {
		return created, err
	}
	// Root's ability to read the artifact is not proof that the independent
	// worker UID can traverse shared parents. Test access as that exact UID
	// before publishing it or starting systemd; no secret content is printed.
	for _, probe := range []struct{ flag, path string }{
		{"-r", filepath.Join(created.DshHome, "settings.yaml")},
		{"-r", filepath.Join(created.DshHome, ".credentials.yaml")},
		{"-w", created.DshHome},
		{"-w", created.Workspace},
	} {
		if _, err = m.run(ctx, "runuser", "-u", user, "--", "/usr/bin/test", probe.flag, probe.path); err != nil {
			return created, fmt.Errorf("worker UID cannot access %s; check shared parent search permissions: %w", probe.path, err)
		}
	}
	if err = m.Registry.Put(created); err != nil {
		return created, err
	}
	registryAdded = true
	if err = m.Registry.Save(); err != nil {
		return created, err
	}
	if _, err = m.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return created, err
	}
	startAttempted = true
	if _, err = m.run(ctx, "systemctl", "start", m.unit(name)); err != nil {
		return created, err
	}
	probe := m.Probe
	if probe == nil {
		probe = m.probeWorker
	}
	if err = probe(ctx, created); err != nil {
		return created, fmt.Errorf("worker readiness probe: %w", err)
	}
	if err = m.Registry.SetHandshake(name, registry.HandshakeOK); err != nil {
		return created, err
	}
	if err = m.Registry.Save(); err != nil {
		return created, err
	}
	created.Handshake = registry.HandshakeOK
	if _, err = m.run(ctx, "systemctl", "enable", m.unit(name)); err != nil {
		return created, err
	}
	if err = m.InstallNginx(ctx, true); err != nil {
		return created, err
	}
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

func (m *Manager) Restart(ctx context.Context, t registry.Tenant) error {
	_, err := m.run(ctx, "systemctl", "restart", m.unit(t.Name))
	return err
}

// StopWorker halts the tenant's worker without touching its identity, data or unit
// enablement: the M52 console "disable" keeps everything and just frees the resources.
func (m *Manager) StopWorker(ctx context.Context, t registry.Tenant) error {
	_, err := m.run(ctx, "systemctl", "stop", m.unit(t.Name))
	return err
}

// StartWorker reverses StopWorker for a tenant that already exists.
func (m *Manager) StartWorker(ctx context.Context, t registry.Tenant) error {
	_, err := m.run(ctx, "systemctl", "start", m.unit(t.Name))
	return err
}
func (m *Manager) Enable(ctx context.Context, t registry.Tenant, on bool) error {
	verb := "disable"
	if on {
		verb = "enable"
	}
	_, err := m.run(ctx, "systemctl", verb, m.unit(t.Name))
	return err
}
func (m *Manager) Status(ctx context.Context, t registry.Tenant) (UnitStatus, error) {
	return m.unitStatus(ctx, m.unit(t.Name))
}

// Unlike is-active/is-enabled, show exits successfully for an inactive or
// disabled unit. Command errors must never be interpreted as a stopped worker.
func (m *Manager) unitStatus(ctx context.Context, unit string) (UnitStatus, error) {
	var status UnitStatus
	out, err := m.run(ctx, "systemctl", "show", "--property=LoadState", "--property=ActiveState", "--property=UnitFileState", unit)
	if err != nil {
		return status, fmt.Errorf("inspect unit %s: %w", unit, err)
	}
	props := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return status, fmt.Errorf("invalid systemd status for %s", unit)
		}
		if _, duplicate := props[key]; duplicate {
			return status, fmt.Errorf("duplicate systemd property %s for %s", key, unit)
		}
		props[key] = value
	}
	if props["LoadState"] != "loaded" {
		return status, fmt.Errorf("unit %s is not loaded (%q)", unit, props["LoadState"])
	}
	status.Detail = props["ActiveState"]
	switch status.Detail {
	case "active", "reloading":
		status.Active = true
	case "inactive", "failed":
	default:
		return status, fmt.Errorf("unit %s has transitional or unknown active state %q", unit, status.Detail)
	}
	status.UnitFileState = props["UnitFileState"]
	switch status.UnitFileState {
	case "enabled", "enabled-runtime":
		status.Enabled = true
	case "disabled", "static", "indirect", "generated", "transient", "masked", "masked-runtime", "linked", "linked-runtime", "alias":
	default:
		return status, fmt.Errorf("unit %s has unknown unit file state %q", unit, status.UnitFileState)
	}
	return status, nil
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
			if _, err = m.run(ctx, "chown", "root:"+m.Config.Deploy.GatewayGroup, gatewayPath); err != nil {
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

func (m *Manager) ReconcileNginx(ctx context.Context, reload bool) error {
	return m.WithLifecycleLock(func() error { return m.InstallNginx(ctx, reload) })
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

func (m *Manager) CaptureURL(ctx context.Context, t registry.Tenant, pid string) error {
	if pid == "" {
		out, err := m.run(ctx, "systemctl", "show", "--property", "MainPID", "--value", m.unit(t.Name))
		if err != nil {
			return err
		}
		pid = strings.TrimSpace(string(out))
	}
	if parsed, err := strconv.Atoi(pid); err != nil || parsed <= 1 {
		return fmt.Errorf("invalid worker MainPID %q", pid)
	}
	target := filepath.Join(m.Config.HandshakeDir, t.Name+".url")
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, err := m.run(ctx, "journalctl", "-b", "-u", m.unit(t.Name), "_PID="+pid, "-n", "50", "--no-pager", "-o", "cat")
		if err == nil {
			matches := startupURLRE.FindAllStringSubmatch(string(out), -1)
			if len(matches) > 1 {
				return errors.New("worker process emitted multiple startup URLs")
			}
			if len(matches) == 1 {
				port, _ := strconv.Atoi(matches[0][2])
				if port != t.WorkerPort {
					return fmt.Errorf("worker startup URL port %d does not match registry port %d", port, t.WorkerPort)
				}
				if err := securefile.WriteAtomic(target, []byte(matches[0][1]+"\n"), 0o640); err != nil {
					return err
				}
				_, err = m.run(ctx, "chown", "root:"+m.Config.Deploy.GatewayGroup, target)
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	// Journal output can contain bearer tokens and must not enter error logs.
	return fmt.Errorf("no matching startup URL found for PID %s in journal", pid)
}
func (m *Manager) InstallNginx(ctx context.Context, reload bool) (err error) {
	files, err := RenderNginx(m.Config, m.Registry.List())
	if err != nil {
		return err
	}
	dir := m.Config.Deploy.NginxDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	includePath := m.Config.Deploy.NginxIncludePath
	if includePath == "" {
		includePath = filepath.Join(filepath.Dir(dir), "dshgw.conf")
	}
	oldInclude, includeReadErr := os.ReadFile(includePath)
	includeExisted := includeReadErr == nil
	if includeReadErr != nil && !errors.Is(includeReadErr, os.ErrNotExist) {
		return includeReadErr
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	old := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return readErr
		}
		old[entry.Name()] = data
	}
	reloadAttempted := false
	defer func() {
		if err == nil {
			return
		}
		var restoreErr error
		entries, readErr := os.ReadDir(dir)
		restoreErr = errors.Join(restoreErr, readErr)
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".conf") {
				restoreErr = errors.Join(restoreErr, os.Remove(filepath.Join(dir, entry.Name())))
			}
		}
		for name, data := range old {
			restoreErr = errors.Join(restoreErr, securefile.WriteAtomic(filepath.Join(dir, name), data, 0o644))
		}
		if includeExisted {
			restoreErr = errors.Join(restoreErr, securefile.WriteAtomic(includePath, oldInclude, 0o644))
		} else if removeErr := os.Remove(includePath); !errors.Is(removeErr, os.ErrNotExist) {
			restoreErr = errors.Join(restoreErr, removeErr)
		}
		if reloadAttempted && restoreErr == nil {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, checkErr := m.run(rollbackCtx, m.Config.Deploy.NginxBinary, "-t"); checkErr != nil {
				restoreErr = checkErr
			} else {
				_, restoreErr = m.run(rollbackCtx, "systemctl", "reload", "nginx")
			}
		}
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore nginx configuration: %w", restoreErr))
		}
	}()
	includeLine := fmt.Sprintf("# generated by dshgw; do not edit\ninclude %s/*.conf;\n", dir)
	if err := securefile.WriteAtomic(includePath, []byte(includeLine), 0o644); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := securefile.WriteAtomic(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return err
		}
	}
	for name := range old {
		if _, keep := files[name]; !keep {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	if _, err := m.run(ctx, m.Config.Deploy.NginxBinary, "-t"); err != nil {
		return err
	}
	if reload {
		reloadAttempted = true
		if _, err := m.run(ctx, "systemctl", "reload", "nginx"); err != nil {
			return err
		}
	}
	return nil
}
