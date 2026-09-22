package tenancy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"gopkg.in/yaml.v3"
)

// sshWorkspaceRowID is the loader entry id the tenant-side ssh plugin is rendered under.
const sshWorkspaceRowID = "ssh-workspace"

// sshWorkspaceRow builds the tenant profile row that mounts the ssh-workspace plugin (M64).
//
// One row covers both halves: the file URL is an ordinary loader entry whose package directory
// also carries the browser bundle (package.json's dsh.client plus exports["./client"]), which
// is how dsh's client-module scan discovers a web plugin. Only the account's own HOME is named
// — the plugin mounts under it — so nothing machine-specific has to be rendered here.
func sshWorkspaceRow(cfg *config.Config) map[string]any {
	return map[string]any{
		"id":   sshWorkspaceRowID,
		"name": pluginFileURL(filepath.Join(filepath.Dir(cfg.Deploy.PluginPath), "ssh-workspace", "index.js")),
		"config": map[string]any{
			"mountSubdir":      cfg.SSHWorkspaces.MountSubdir,
			"hosts":            cfg.SSHWorkspaces.Hosts,
			"maxEntries":       cfg.SSHWorkspaces.MaxEntries,
			"connectTimeoutMs": int(cfg.SSHWorkspaces.ConnectTimeout.Duration() / 1e6),
		},
	}
}

// EnsureSSHWorkspaceRow makes one tenant's rendered profile patch match the current
// ssh-workspace switch: the row appears (and is refreshed) when the feature is on, and is
// removed when it is off.
//
// Why this exists: the patch is otherwise written only when a tenant is created or its
// credentials rotate, so flipping the switch would silently reach only new accounts — and
// switching it off would leave existing ones with a plugin whose mount requests nothing
// answers. Only the patch is touched: re-rendering the artifacts would rewrite workspace.json
// and discard the workspaces a person added in the UI.
//
// A tenant that has no patch yet is left alone (it has never been provisioned), and a tenant
// whose patch cannot carry the row is reported as a warning rather than a failure: an optional
// feature must never stop an account from starting. Only a patch file that cannot be read or
// parsed is an error.
func EnsureSSHWorkspaceRow(cfg *config.Config, t registry.Tenant) (string, error) {
	return ensureWorkspaceRow(t, sshWorkspaceRowID, cfg.SSHWorkspaces.Enabled, sshWorkspaceRow(cfg))
}

const browserWorkspaceRowID = "browser-workspace"

func browserWorkspaceRow(cfg *config.Config) map[string]any {
	return map[string]any{"id": browserWorkspaceRowID, "name": pluginFileURL(filepath.Join(filepath.Dir(cfg.Deploy.PluginPath), "browser-workspace", "index.js")), "config": map[string]any{"mountSubdir": "browser"}}
}

// EnsureBrowserWorkspaceRow refreshes only the plugin row, preserving user workspaces.
func EnsureBrowserWorkspaceRow(cfg *config.Config, t registry.Tenant) (string, error) {
	return ensureWorkspaceRow(t, browserWorkspaceRowID, cfg.BrowserWorkspaces.Enabled, browserWorkspaceRow(cfg))
}

const accountCardRowID = "dshgw-account-card"

// accountCardRow builds the loader entry for the sidebar's identity row (M67).
//
// It is a browser-only plugin: the row reads dshgw's own /dshgw/session/ and posts to
// /dshgw/logout/ under the tenant's own origin, so there is nothing for the tenant's node
// side to do and no config to pass — which half of dshgw's features a tenant may call is the
// gateway's decision, expressed by account_card.enabled, not the plugin's.
//
// The row names index.js, not client.js: a row's `name` is a module the HOST imports, and
// the browser bundle calls `window.__ModuleLoader__.load` at import time, so pointing a row
// at it kills the tenant's plugin tree ("window is not defined", measured on a real tenant).
// client.js is discovered through the package's dsh.client declaration instead — the same
// dual-face shape the other two workspace plugins use.
func accountCardRow(cfg *config.Config) map[string]any {
	return map[string]any{
		"id":   accountCardRowID,
		"name": pluginFileURL(filepath.Join(filepath.Dir(cfg.Deploy.PluginPath), "account-card", "index.js")),
	}
}

// EnsureAccountCardRow adds or removes that row in one tenant's rendered patch, the same way
// the ssh and browser rows are kept in step with their switches.
func EnsureAccountCardRow(cfg *config.Config, t registry.Tenant) (string, error) {
	return ensureWorkspaceRow(t, accountCardRowID, cfg.AccountCard.Enabled, accountCardRow(cfg))
}

func ensureWorkspaceRow(t registry.Tenant, id string, enabled bool, pluginRow map[string]any) (warning string, err error) {
	path := filepath.Join(t.DshHome, "profiles", "web", "cordis.patch.yml")
	data, err := securefile.ReadLimitedRegular(path, 1<<20)
	if err != nil {
		// A tenant that was never provisioned has no patch at all — including no profiles
		// directory, which is why this unwraps rather than testing the concrete error.
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var rows []map[string]any
	if err := yaml.Unmarshal(data, &rows); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	// Drop every ssh row that is there now, so a configuration change (a new mount subdir, a
	// changed host list) propagates instead of being ignored on account of "already present".
	for _, row := range rows {
		insert, ok := row["insert"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(insert))
		for _, entry := range insert {
			if record, ok := entry.(map[string]any); ok && record["id"] == id {
				continue
			}
			kept = append(kept, entry)
		}
		row["insert"] = kept
	}
	if !enabled {
		// Nothing to add: the removal above is the whole point when the feature is off.
		if !patchMentions(data, id) {
			return "", nil
		}
		return "", writePatchRows(path, rows)
	}
	placed := false
	for _, row := range rows {
		if _, ok := row["insert"].([]any); !ok {
			continue
		}
		row["insert"] = append(row["insert"].([]any), pluginRow)
		placed = true
		break
	}
	if !placed {
		// A patch from an older shape has no insert list to add to, and rewriting the file
		// from scratch would drop whatever else it carries. The account keeps working without
		// the feature; rotating its key re-renders it.
		return fmt.Sprintf("tenant %s has a profile patch without an insert list, so the workspace surface was not added; rotate its key to re-render it", t.Name), nil
	}
	return "", writePatchRows(path, rows)
}

func patchMentions(data []byte, id string) bool {
	var rows []map[string]any
	if err := yaml.Unmarshal(data, &rows); err != nil {
		return false
	}
	for _, row := range rows {
		insert, ok := row["insert"].([]any)
		if !ok {
			continue
		}
		for _, entry := range insert {
			if record, ok := entry.(map[string]any); ok && record["id"] == id {
				return true
			}
		}
	}
	return false
}

func writePatchRows(path string, rows []map[string]any) error {
	data, err := yaml.Marshal(rows)
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(path, data, 0o600)
}

// The three tenant-side web plugins every account gets by default (M75). The row ids are the ones
// the accounts that carried these plugins by hand already used, so the gateway taking over the
// wiring changes nothing about what the browser half registers.
const (
	webTTYRowID         = "dshgw-web-tty"
	workspaceFilesRowID = "dshgw-workspace-files"
	gitDiffRowID        = "dshgw-git-diff"
)

// tenantPlugin is one shipped plugin: the directory name beside deploy.plugin_path, the loader row
// id, the current switch, and how its row is built.
type tenantPlugin struct {
	dir     string
	id      string
	enabled bool
	row     func(cfg *config.Config, t registry.Tenant) map[string]any
}

// tenantPlugins lists them in the order their rows appear in the profile.
func tenantPlugins(cfg *config.Config) []tenantPlugin {
	return []tenantPlugin{
		{dir: "web-tty", id: webTTYRowID, enabled: cfg.TenantPlugins.WebTTY.Enabled, row: webTTYRow},
		{dir: "workspace-files", id: workspaceFilesRowID, enabled: cfg.TenantPlugins.WorkspaceFiles.Enabled, row: workspaceFilesRow},
		{dir: "git-diff", id: gitDiffRowID, enabled: cfg.TenantPlugins.GitDiff.Enabled, row: gitDiffRow},
	}
}

// tenantPluginURL is the file URL of one plugin's host half, beside the picker plugin.
func tenantPluginURL(cfg *config.Config, dir string) string {
	return pluginFileURL(filepath.Join(filepath.Dir(cfg.Deploy.PluginPath), dir, "index.js"))
}

// tenantPluginState is one plugin's per-tenant runtime file inside the account's own DSH home.
//
// Not next to the module: the installed copy is shared by every account, so a trace there would
// mix accounts together, and git-diff's scan cache would hand one account's repository paths to
// another. The DSH home is bound writable into the account's sandbox, which is where these files
// have to be written from — the plugin creates the directory itself, as the account that owns it.
func tenantPluginState(t registry.Tenant, name string) string {
	return filepath.Join(t.DshHome, "plugin-state", name)
}

// pluginRootLabel is the label the workspace-scoped panels show for their root.
func pluginRootLabel(cfg *config.Config) string {
	if label := strings.TrimSpace(cfg.TenantPlugins.RootLabel); label != "" {
		return label
	}
	return config.DefaultPluginRootLabel
}

// webTTYRow builds the terminal panel's row.
//
// `cwd`/`cwdRoot` are named rather than left to the plugin's own default of the process working
// directory: the account's workspace is what a terminal inside its dsh should open in, and the
// bound is what the browser half may ask for instead.
func webTTYRow(cfg *config.Config, t registry.Tenant) map[string]any {
	return map[string]any{
		"id":   webTTYRowID,
		"name": tenantPluginURL(cfg, "web-tty"),
		"config": map[string]any{
			"cwd":       t.Workspace,
			"cwdRoot":   t.Workspace,
			"traceFile": tenantPluginState(t, "web-tty.trace.jsonl"),
			"trace":     true,
		},
	}
}

// workspaceFilesRow builds the workspace file manager's row, clamped to the account's workspace.
func workspaceFilesRow(cfg *config.Config, t registry.Tenant) map[string]any {
	return map[string]any{
		"id":   workspaceFilesRowID,
		"name": tenantPluginURL(cfg, "workspace-files"),
		"config": map[string]any{
			"root":      t.Workspace,
			"rootLabel": pluginRootLabel(cfg),
			"traceFile": tenantPluginState(t, "workspace-files.trace.jsonl"),
			"trace":     true,
		},
	}
}

// gitDiffRow builds the read-only change review's row: the same clamped root, plus a cache file of
// its own — the scan cache holds repository paths, so it may not be shared between accounts.
func gitDiffRow(cfg *config.Config, t registry.Tenant) map[string]any {
	return map[string]any{
		"id":   gitDiffRowID,
		"name": tenantPluginURL(cfg, "git-diff"),
		"config": map[string]any{
			"root":      t.Workspace,
			"rootLabel": pluginRootLabel(cfg),
			"traceFile": tenantPluginState(t, "git-diff.trace.jsonl"),
			"cacheFile": tenantPluginState(t, "git-diff.cache.json"),
			"trace":     true,
		},
	}
}

// tenantPluginInstalled reports whether one plugin's package is deployed beside the picker plugin,
// and names the exact path when it is not.
//
// Why this is checked at all: a row whose module cannot be imported costs the account its whole
// plugin tree, not just that one panel (measured on a real tenant, M67). So an enabled plugin that
// was never deployed is refused at create time (renderPatch) and skipped with a warning at start
// time — never rendered as a row pointing at nothing.
func tenantPluginInstalled(cfg *config.Config, dir string) error {
	path := filepath.Join(filepath.Dir(cfg.Deploy.PluginPath), dir, "index.js")
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("plugin %s is enabled but %s cannot be read: %w", dir, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("plugin %s is enabled but %s is not a regular file", dir, path)
	}
	return nil
}

// tenantPluginRows builds the rows of every enabled and deployed tenant-side plugin, in order.
//
// A missing plugin is an error here rather than a skipped row: this runs while a tenant is being
// created or its key rotated, which is the moment to tell the operator that the deployment is not
// what the configuration claims (the same rule deploy.plugin_path itself follows).
func tenantPluginRows(cfg *config.Config, t registry.Tenant) ([]map[string]any, error) {
	rows := make([]map[string]any, 0, 3)
	for _, plugin := range tenantPlugins(cfg) {
		if !plugin.enabled {
			continue
		}
		if err := tenantPluginInstalled(cfg, plugin.dir); err != nil {
			return nil, err
		}
		rows = append(rows, plugin.row(cfg, t))
	}
	return rows, nil
}

// EnsureTenantPlugins makes one tenant's rendered profile patch match the current tenant_plugins
// switches: each enabled plugin's row appears (and is refreshed when its configuration changes),
// and each disabled — or no longer deployed — plugin's row is removed.
//
// Run on every worker start for the same reason the ssh, browser and account-card rows are: the
// patch is otherwise written only at create/rotate time, so flipping a switch would silently reach
// new accounts only. A plugin that is switched on but not deployed is reported as a warning and
// treated as off, so a half-finished deployment cannot stop an account from starting.
func EnsureTenantPlugins(cfg *config.Config, t registry.Tenant) ([]string, error) {
	var warnings []string
	for _, plugin := range tenantPlugins(cfg) {
		enabled := plugin.enabled
		if enabled {
			if err := tenantPluginInstalled(cfg, plugin.dir); err != nil {
				warnings = append(warnings, fmt.Sprintf("%v; the row was removed so the account keeps starting", err))
				enabled = false
			}
		}
		warning, err := ensureWorkspaceRow(t, plugin.id, enabled, plugin.row(cfg, t))
		if err != nil {
			return warnings, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return warnings, nil
}
