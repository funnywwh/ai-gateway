package tenancy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
