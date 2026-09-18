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
	"sort"
	"strings"
	"time"

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

func RenderTenantArtifacts(cfg *config.Config, t registry.Tenant, key string, models []string, opt TenantOptions, now time.Time) ([]Artifact, error) {
	opt = resolveOptions(cfg, opt)
	if opt.DirectoryPicker != "clamp" && opt.DirectoryPicker != "browse" {
		return nil, errors.New("directory picker must be clamp or browse")
	}
	if opt.PluginBrowserFS != "on" && opt.PluginBrowserFS != "off" {
		return nil, errors.New("plugin_browser_fs must be on or off")
	}
	settings, err := renderSettings(nil, cfg.AigwBaseURL, models)
	if err != nil {
		return nil, err
	}
	credentials, err := yaml.Marshal(struct {
		Version int               `yaml:"version"`
		Refs    map[string]string `yaml:"refs"`
		Records map[string]any    `yaml:"records"`
	}{1, map[string]string{"AIGW_API_KEY": key}, map[string]any{}})
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

type providerModel struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name,omitempty"`
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
}

func renderSettings(existing []byte, base string, models []string) ([]byte, error) {
	root := map[string]any{}
	if len(existing) > 0 {
		if err := yaml.Unmarshal(existing, &root); err != nil {
			return nil, fmt.Errorf("decode existing settings: %w", err)
		}
	}
	unique := append([]string(nil), models...)
	sort.Strings(unique)
	out := unique[:0]
	for _, id := range unique {
		if id != "" && (len(out) == 0 || out[len(out)-1] != id) {
			out = append(out, id)
		}
	}
	modelRows := make([]providerModel, 0, len(out))
	for _, id := range out {
		modelRows = append(modelRows, providerModel{ID: id, Name: id})
	}
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
	providers["aigw"] = aigwProvider{APIKeyEnv: "AIGW_API_KEY", API: "openai-responses", BaseURL: strings.TrimRight(base, "/") + "/v1", Models: modelRows, Compat: providerCompat{SupportsStrictMode: true}}
	namespace["providers"] = providers
	root["llm-pi-ai"] = namespace
	current, exists := root["agent-default-model"].(map[string]any)
	if !exists || current["provider"] == "aigw" && !containsModel(out, fmt.Sprint(current["model"])) {
		root["agent-default-model"] = map[string]any{"provider": "aigw", "model": out[0]}
	}
	return yaml.Marshal(root)
}

func containsModel(models []string, want string) bool {
	for _, model := range models {
		if model == want {
			return true
		}
	}
	return false
}

func UpdateSettings(path, base string, models []string) error {
	return withDSHFileLock(path, func() error {
		return updateSettingsLocked(path, base, models)
	})
}

// updateSettingsLocked updates settings while the caller owns path.lock.
// Lifecycle operations use this lockless form while holding a larger
// credentials/settings transaction lock, so rotation cannot observe a
// half-updated pair of files.
func updateSettingsLocked(path, base string, models []string) error {
	var existing []byte
	if data, err := securefile.ReadLimitedRegular(path, 16<<20); err == nil {
		existing = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := renderSettings(existing, base, models)
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(path, data, 0o600)
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
	doc.Refs["AIGW_API_KEY"] = key
	next, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(path, next, 0o600)
}

func withDSHFileLock(filename string, operation func() error) error {
	lockPath := filename + ".lock"
	deadline := time.Now().Add(10 * time.Second)
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
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for dsh writer lock %s", lockPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func renderPatch(cfg *config.Config, t registry.Tenant, opt TenantOptions) ([]byte, error) {
	pickerName := "@deepseek-ai/dsh-host-directory-picker-browse"
	pickerID := "picker-browse"
	picker := map[string]any{"id": pickerID, "name": pickerName}
	if opt.DirectoryPicker == "clamp" {
		pickerID = "picker-clamp"
		pickerURL := (&url.URL{Scheme: "file", Path: cfg.Deploy.PluginPath}).String()
		picker = map[string]any{"id": pickerID, "name": pickerURL, "config": map[string]any{"root": t.Workspace}}
	}
	rows := []map[string]any{
		{"id": "directory-picker", "name": "@deepseek-ai/dsh-host-directory-picker-auto", "disabled": true},
		{"insert": []map[string]any{
			picker,
			{"id": pickerID + "-ui", "name": "@deepseek-ai/dsh-client-ui-directory-picker-browse"},
		}},
	}
	if cfg.SSHWorkspaces.Enabled {
		// The account-side half of M64: ssh browsing, remote mkdir, and the mount mailbox.
		//
		// One row covers both halves. The file URL is an ordinary loader entry whose package
		// directory also carries the browser bundle (package.json's dsh.client plus
		// exports["./client"]), which is how dsh's client-module scan discovers a web plugin
		// — the same mechanism the shipped directory-picker surface uses.
		sshPluginURL := (&url.URL{
			Scheme: "file",
			Path:   filepath.Join(filepath.Dir(cfg.Deploy.PluginPath), "ssh-workspace", "index.js"),
		}).String()
		rows[1]["insert"] = append(rows[1]["insert"].([]map[string]any), map[string]any{
			"id":   "ssh-workspace",
			"name": sshPluginURL,
			"config": map[string]any{
				// The account's HOME inside the sandbox is its workspace, so the plugin needs
				// no paths: it mounts under $HOME/<mount_subdir>/… exactly like the gateway.
				"mountSubdir":      cfg.SSHWorkspaces.MountSubdir,
				"hosts":            cfg.SSHWorkspaces.Hosts,
				"maxEntries":       cfg.SSHWorkspaces.MaxEntries,
				"connectTimeoutMs": int(cfg.SSHWorkspaces.ConnectTimeout.Duration() / time.Millisecond),
			},
		})
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
		target := filepath.Join(t.Workspace, seed)
		sum := sha256.Sum256([]byte(target))
		id := "dshgw-" + hex.EncodeToString(sum[:12])
		doc.Global.WorkspaceIDs = append(doc.Global.WorkspaceIDs, id)
		doc.Tables.Workspaces[id] = workspaceRecord{Path: target, Title: filepath.Base(target), SessionIDs: []string{}, CreatedAt: stamp, UpdatedAt: stamp}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	return append(data, '\n'), err
}
