// Package tenancy owns tenant provisioning and the on-disk artifacts it renders.
// In the aigw-supervised shape (M58) that means files under dshgw's own state
// directory only: no OS accounts, no systemd units, no edge configuration.
package tenancy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"gopkg.in/yaml.v3"
)

type Artifact struct {
	Path string
	Data []byte
	Mode os.FileMode
}

type TenantOptions struct {
	DirectoryPicker string
	PluginBrowserFS string
}

func resolveOptions(cfg *config.Config, opt TenantOptions) TenantOptions {
	if opt.DirectoryPicker == "" {
		opt.DirectoryPicker = cfg.DirectoryPicker
	}
	if opt.PluginBrowserFS == "" {
		opt.PluginBrowserFS = cfg.PluginBrowserFS
	}
	return opt
}

func RenderTenantArtifacts(cfg *config.Config, t registry.Tenant, key string, models []aigw.Model, opt TenantOptions, now time.Time) ([]Artifact, error) {
	opt = resolveOptions(cfg, opt)
	if opt.DirectoryPicker != "clamp" && opt.DirectoryPicker != "browse" {
		return nil, errors.New("directory picker must be clamp or browse")
	}
	if opt.PluginBrowserFS != "on" && opt.PluginBrowserFS != "off" {
		return nil, errors.New("plugin_browser_fs must be on or off")
	}
	settings, err := renderSettings(cfg, nil, models)
	if err != nil {
		return nil, err
	}
	credentials, err := yaml.Marshal(struct {
		Version int               `yaml:"version"`
		Refs    map[string]string `yaml:"refs"`
		Records map[string]any    `yaml:"records"`
	}{1, map[string]string{AIGWAPIKeyRef: key}, map[string]any{}})
	if err != nil {
		return nil, err
	}
	patch, err := renderPatch(cfg, t, opt)
	if err != nil {
		return nil, err
	}
	workspace, err := renderWorkspace(cfg, t, now)
	if err != nil {
		return nil, err
	}
	return []Artifact{
		{filepath.Join(t.DshHome, "settings.yaml"), settings, 0o600},
		{filepath.Join(t.DshHome, ".credentials.yaml"), credentials, 0o600},
		{filepath.Join(t.DshHome, "profiles", "web", "cordis.patch.yml"), patch, 0o600},
		{filepath.Join(t.DshHome, "storages", "workspace.json"), workspace, 0o600},
		{filepath.Join(cfg.Deploy.TenantConfigRoot, t.Name, "gateway.key"), []byte(key + "\n"), 0o640},
	}, nil
}

func WriteArtifacts(artifacts []Artifact) error {
	for _, a := range artifacts {
		if err := securefile.WriteAtomic(a.Path, a.Data, a.Mode); err != nil {
			return fmt.Errorf("write %s: %w", a.Path, err)
		}
	}
	return nil
}

type providerCompat struct {
	SupportsStrictMode bool `yaml:"supportsStrictMode"`
}

type aigwProvider struct {
	APIKeyEnv string          `yaml:"apiKeyEnv"`
	API       string          `yaml:"api"`
	BaseURL   string          `yaml:"baseURL"`
	Models    []providerModel `yaml:"models"`
	Compat    providerCompat  `yaml:"compat"`
	// MaxRequestImageBytes is rendered only when a model on this route accepts images: it is
	// the bound dsh prunes history against, and without it dsh's own 20 MiB default exceeds
	// aigw's request body cap (10 MiB by default), where the gateway truncates rather than
	// refusing with a size it can name (M68).
	MaxRequestImageBytes int `yaml:"maxRequestImageBytes,omitempty"`
}

func renderSettings(cfg *config.Config, existing []byte, models []aigw.Model) ([]byte, error) {
	root := map[string]any{}
	if len(existing) > 0 {
		if err := yaml.Unmarshal(existing, &root); err != nil {
			return nil, fmt.Errorf("decode existing settings: %w", err)
		}
	}
	out := dshModels(models)
	namespace := map[string]any{}
	if value, exists := root["llm-pi-ai"]; exists {
		var ok bool
		namespace, ok = value.(map[string]any)
		if !ok {
			return nil, errors.New("existing llm-pi-ai setting is not a mapping")
		}
	}
	providers := map[string]any{}
	if value, exists := namespace["providers"]; exists {
		var ok bool
		providers, ok = value.(map[string]any)
		if !ok {
			return nil, errors.New("existing llm-pi-ai.providers setting is not a mapping")
		}
	}
	if len(out) == 0 {
		delete(providers, "aigw")
		if len(providers) == 0 {
			delete(namespace, "providers")
		} else {
			namespace["providers"] = providers
		}
		if len(namespace) == 0 {
			delete(root, "llm-pi-ai")
		} else {
			root["llm-pi-ai"] = namespace
		}
		if current, ok := root["agent-default-model"].(map[string]any); ok && current["provider"] == "aigw" {
			delete(root, "agent-default-model")
		}
		return yaml.Marshal(root)
	}
	provider := aigwProvider{
		APIKeyEnv: AIGWAPIKeyRef, API: "openai-responses",
		BaseURL: strings.TrimRight(cfg.AigwBaseURL, "/") + "/v1",
		Models:  out, Compat: providerCompat{SupportsStrictMode: true},
	}
	if anyImages(out) {
		provider.MaxRequestImageBytes = cfg.EffectiveImageRequestMaxBytes()
	}
	providers["aigw"] = provider
	namespace["providers"] = providers
	root["llm-pi-ai"] = namespace
	current, exists := root["agent-default-model"].(map[string]any)
	if !exists || current["provider"] == "aigw" && !containsModel(out, fmt.Sprint(current["model"])) {
		root["agent-default-model"] = map[string]any{"provider": "aigw", "model": out[0].ID}
	}
	return yaml.Marshal(root)
}

func containsModel(models []providerModel, want string) bool {
	for _, model := range models {
		if model.ID == want {
			return true
		}
	}
	return false
}

func UpdateSettings(cfg *config.Config, path string, models []aigw.Model) error {
	return withDSHFileLock(path, func() error {
		return updateSettingsLocked(cfg, path, models)
	})
}

// updateSettingsLocked updates settings while the caller owns path.lock.
// Lifecycle operations use this lockless form while holding a larger
// credentials/settings transaction lock, so rotation cannot observe a
// half-updated pair of files.
func updateSettingsLocked(cfg *config.Config, path string, models []aigw.Model) error {
	var existing []byte
	if data, err := securefile.ReadLimitedRegular(path, 16<<20); err == nil {
		existing = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := renderSettings(cfg, existing, models)
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(path, data, 0o600)
}

// AIGWAPIKeyRef is the credential reference the rendered aigw provider reads its key from
// (`apiKeyEnv`). It is platform-owned: the key belongs to the deployment, not to the tenant.
const AIGWAPIKeyRef = "AIGW_API_KEY"

// EnsureCredentialRef makes sure a tenant's .credentials.yaml still holds the platform's
// credential reference, and reports whether it had to write (M69).
//
// The file has two owners: dshgw writes `refs` (the platform's key) and dsh writes `records`
// (browser session grants). A tenant page can therefore end up with the reference removed —
// observed on this host, where `refs` came back empty while the provider entry was also gone —
// and nothing else would ever put it back. Only the named reference is touched: every other ref
// and every record is preserved, and an already-correct value is not rewritten (no mtime churn,
// no needless lock contention with the tenant's own writer).
func EnsureCredentialRef(path, ref, value string) (bool, error) {
	if ref == "" || value == "" {
		return false, errors.New("credential ref and value are required")
	}
	changed := false
	err := withDSHFileLock(path, func() error {
		data, err := securefile.ReadLimitedRegular(path, 16<<20)
		if err != nil {
			return err
		}
		var doc credentialsFile
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil {
			return err
		}
		if doc.Version != 1 {
			return fmt.Errorf("unsupported credentials version %d", doc.Version)
		}
		if doc.Refs == nil {
			doc.Refs = map[string]string{}
		}
		if doc.Refs[ref] == value {
			return nil
		}
		doc.Refs[ref] = value
		next, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		if err := securefile.WriteAtomic(path, next, 0o600); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

type credentialsFile struct {
	Version int               `yaml:"version"`
	Refs    map[string]string `yaml:"refs"`
	Records map[string]any    `yaml:"records"`
}

func RotateCredentials(path, key string) error {
	return withDSHFileLock(path, func() error {
		return rotateCredentialsLocked(path, key)
	})
}

// rotateCredentialsLocked updates credentials while the caller owns
// path.lock. See updateSettingsLocked for the lifecycle transaction use case.
func rotateCredentialsLocked(path, key string) error {
	data, err := securefile.ReadLimitedRegular(path, 16<<20)
	if err != nil {
		return err
	}
	var doc credentialsFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	if doc.Version != 1 {
		return fmt.Errorf("unsupported credentials version %d", doc.Version)
	}
	if doc.Refs == nil {
		doc.Refs = map[string]string{}
	}
	if doc.Records == nil {
		doc.Records = map[string]any{}
	}
	doc.Refs[AIGWAPIKeyRef] = key
	next, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(path, next, 0o600)
}

func withDSHFileLock(filename string, operation func() error) error {
	return withDSHFileLockTimeout(filename, 10*time.Second, operation)
}

// withDSHFileLockTimeout is withDSHFileLock with an explicit wait, so tests can exercise both
// outcomes (reclaim a dead owner's lock, respect a live one's) without waiting ten seconds.
func withDSHFileLockTimeout(filename string, timeout time.Duration, operation func() error) error {
	lockPath := filename + ".lock"
	deadline := time.Now().Add(timeout)
	for {
		lock, err := securefile.OpenRegular(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(lock, "%d\n", os.Getpid())
			_ = lock.Close()
			defer securefile.RemoveFile(lockPath) //nolint:errcheck
			return operation()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		// The lock records its owner for exactly this reason: a writer that died while holding
		// it (a crash, a panic, a killed process) otherwise leaves a lock nobody will ever
		// release, and every later start of that account fails with a timeout that says
		// nothing about the cause. A pid that no longer exists means the lock is stale.
		if lockOwnerGone(lockPath) {
			_ = securefile.RemoveFile(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for dsh writer lock %s (held by pid %s)", lockPath, lockOwner(lockPath))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// lockOwner reads the pid a lock file recorded, or "" when it cannot be read.
func lockOwner(path string) string {
	data, err := securefile.ReadLimitedRegular(path, 64)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// lockOwnerGone reports whether a lock file's recorded owner no longer exists.
//
// Conservative on purpose: an unreadable or just-created lock (the window between O_EXCL and
// the pid write) is treated as held, so a race can only make this wait, never steal a lock
// from a live writer.
func lockOwnerGone(path string) bool {
	raw := lockOwner(path)
	if raw == "" {
		return false
	}
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 1 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

// pluginFileURL is the file:// URL one loader row imports.
func pluginFileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func renderPatch(cfg *config.Config, t registry.Tenant, opt TenantOptions) ([]byte, error) {
	rows := []map[string]any{
		{"id": "directory-picker", "name": "@deepseek-ai/dsh-host-directory-picker-auto", "disabled": true},
		// The same pair EnsureDirectoryPickerRow refreshes on every worker start, so a tenant
		// created now and one provisioned earlier describe the picker identically.
		{"insert": pickerRows(cfg, t, opt.DirectoryPicker)},
	}
	if cfg.SSHWorkspaces.Enabled {
		// The account-side half of M64, rendered in the same shape a later refresh writes
		// (EnsureSSHWorkspaceRow), so an enabled feature looks identical on a new tenant and on
		// one that was provisioned earlier.
		rows[1]["insert"] = append(rows[1]["insert"].([]map[string]any), sshWorkspaceRow(cfg))
	}
	if cfg.BrowserWorkspaces.Enabled {
		rows[1]["insert"] = append(rows[1]["insert"].([]map[string]any), browserWorkspaceRow(cfg))
	}
	if cfg.AccountCard.Enabled {
		// Last, so the identity row lands under the workspace actions in the sidebar's foot —
		// the place a person looks for "who am I / sign out" (M67).
		rows[1]["insert"] = append(rows[1]["insert"].([]map[string]any), accountCardRow(cfg))
	}
	// The tenant-side web plugins (M75). Undeployed but enabled is an error rather than a skipped
	// row: this is the create/rotate path, where a bad deployment must be named, not rendered into
	// a profile whose row would cost the account its whole plugin tree at start.
	tenantRows, err := tenantPluginRows(cfg, t)
	if err != nil {
		return nil, err
	}
	if len(tenantRows) > 0 {
		rows[1]["insert"] = append(rows[1]["insert"].([]map[string]any), tenantRows...)
	}
	if opt.PluginBrowserFS == "off" {
		rows = append(rows, map[string]any{"id": "browser-fs", "name": "dsh-browser-fs", "disabled": true})
	}
	return yaml.Marshal(rows)
}

type workspaceRecord struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	SessionIDs []string `json:"sessionIds"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}
type workspaceDocument struct {
	Unit struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	} `json:"unit"`
	Global struct {
		Initialized        bool     `json:"initialized"`
		WorkspaceIDs       []string `json:"workspaceIds"`
		ArchivedSessionIDs []string `json:"archivedSessionIds"`
	} `json:"global"`
	Tables struct {
		Workspaces map[string]workspaceRecord `json:"workspaces"`
	} `json:"tables"`
}

func renderWorkspace(cfg *config.Config, t registry.Tenant, now time.Time) ([]byte, error) {
	doc := workspaceDocument{}
	doc.Unit.Name = "workspace"
	doc.Unit.Version = 2
	doc.Global.Initialized = true
	doc.Global.ArchivedSessionIDs = []string{}
	doc.Tables.Workspaces = map[string]workspaceRecord{}
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, seed := range cfg.WorkspaceSeed {
		// Seeded workspaces are the ones a fresh account opens first, so they are rendered at the
		// workspace's sandbox view too (M79): a seed pointing at the host path would make the
		// account's first session print the long path the view exists to remove.
		target := filepath.Join(sandboxWorkspacePath(cfg, t), seed)
		sum := sha256.Sum256([]byte(target))
		id := "dshgw-" + hex.EncodeToString(sum[:12])
		doc.Global.WorkspaceIDs = append(doc.Global.WorkspaceIDs, id)
		doc.Tables.Workspaces[id] = workspaceRecord{Path: target, Title: filepath.Base(target), SessionIDs: []string{}, CreatedAt: stamp, UpdatedAt: stamp}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	return append(data, '\n'), err
}
