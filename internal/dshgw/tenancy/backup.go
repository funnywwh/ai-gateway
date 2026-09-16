package tenancy

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

type archiveRoot struct {
	Path     string
	Name     string
	Required bool
}

type archiveSource struct {
	Path    string `json:"path"`
	Name    string `json:"archive_path"`
	Present bool   `json:"present"`
}

type archiveManifest struct {
	Version int              `json:"version"`
	Roots   []archiveSource  `json:"roots"`
	Tenant  *registry.Tenant `json:"tenant,omitempty"`
}

func (m *Manager) Backup(ctx context.Context) (path string, err error) {
	err = m.WithLifecycleLock(func() (backupErr error) {
		var active []string
		for _, tenant := range m.Registry.List() {
			status, statusErr := m.Status(ctx, tenant)
			if statusErr != nil {
				return statusErr
			}
			if status.Active {
				active = append(active, m.unit(tenant.Name))
			}
		}
		gateway, statusErr := m.unitStatus(ctx, m.Config.Deploy.GatewayUnit)
		if statusErr != nil {
			return statusErr
		}
		// Record attempts before running stop: a failed command may already
		// have stopped the process. Recovery runs even after cancellation.
		var attempted []string
		defer func() {
			restartCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for i := len(attempted) - 1; i >= 0; i-- {
				if _, restartErr := m.run(restartCtx, "systemctl", "start", attempted[i]); restartErr != nil {
					backupErr = errors.Join(backupErr, fmt.Errorf("restore unit %s after backup: %w", attempted[i], restartErr))
				}
			}
		}()
		if gateway.Active {
			attempted = append(attempted, m.Config.Deploy.GatewayUnit)
			if _, stopErr := m.run(ctx, "systemctl", "stop", m.Config.Deploy.GatewayUnit); stopErr != nil {
				return stopErr
			}
		}
		for _, unit := range active {
			attempted = append(attempted, unit)
			if _, stopErr := m.run(ctx, "systemctl", "stop", unit); stopErr != nil {
				return stopErr
			}
		}
		path, backupErr = m.backupUnlocked()
		return backupErr
	})
	return path, err
}

func (m *Manager) backupUnlocked() (string, error) {
	now := m.now()
	stamp := now.Format("20060102-150405")
	destDir := filepath.Join(m.Config.Deploy.BackupDir, now.Format("2006-01-02"))
	roots := []archiveRoot{
		{m.Config.StateDir, "state", true},
		{filepath.Dir(m.Config.Deploy.ConfigPath), "etc", true},
		{m.Config.WorkspaceRoot, "workspaces", false},
		{m.Config.Deploy.ConfigPath, "config.yaml", true},
		{m.Config.TenantRoot, "tenants", false},
		{m.Config.HandshakeDir, "handshake", false},
		{m.Config.Deploy.TenantConfigRoot, "tenant-config", false},
		{m.Registry.Path(), "registry.json", len(m.Registry.List()) > 0},
		{m.Registry.KeyMapPath(), "keys.map", false},
	}
	for _, extra := range []archiveRoot{{m.Config.SessionPath, "sessions.json", false}, {m.Config.ActivityPath, "activity.json", false}, {m.Config.AuditPath, "audit.jsonl", false}} {
		if extra.Path != "" {
			roots = append(roots, extra)
		}
	}
	// Known tenant data is mandatory, even when its configured parent is
	// outside state_dir/workspace_root. The writer deduplicates covered roots.
	for _, tenant := range m.Registry.List() {
		if err := m.validateTenantPaths(tenant); err != nil {
			return "", err
		}
		roots = append(roots,
			archiveRoot{filepath.Dir(tenant.DshHome), "tenant-" + tenant.Name + "-state", true},
			archiveRoot{tenant.Workspace, "tenant-" + tenant.Name + "-workspace", true},
			archiveRoot{filepath.Join(m.Config.Deploy.TenantConfigRoot, tenant.Name), "tenant-" + tenant.Name + "-config", true})
	}
	return writeArchive(destDir, "dshgw-"+stamp+".tar.gz", roots, nil)
}

func (m *Manager) validateTenantPaths(t registry.Tenant) error {
	if !config.ValidTenantName(t.Name) || filepath.Clean(t.DshHome) != filepath.Join(m.Config.TenantRoot, t.Name, ".dsh") || filepath.Clean(t.Workspace) != filepath.Join(m.Config.WorkspaceRoot, t.Name) {
		return fmt.Errorf("tenant %q paths do not match configured tenant roots", t.Name)
	}
	return nil
}

func (m *Manager) BackupTenant(t registry.Tenant) (string, error) {
	if err := m.validateTenantPaths(t); err != nil {
		return "", err
	}
	now := m.now()
	stamp := now.Format("20060102-150405")
	destDir := filepath.Join(m.Config.Deploy.BackupDir, now.Format("2006-01-02"))
	roots := []archiveRoot{
		{filepath.Dir(t.DshHome), "tenant-state", true},
		{t.Workspace, "workspace", true},
		{filepath.Join(m.Config.Deploy.TenantConfigRoot, t.Name), "tenant-config", true},
		{filepath.Join(m.Config.HandshakeDir, t.Name+".url"), "handshake.url", false},
	}
	return writeArchive(destDir, "tenant-"+t.Name+"-"+stamp+".tar.gz", roots, &t)
}

func writeArchive(destDir, name string, roots []archiveRoot, tenant *registry.Tenant) (path string, err error) {
	if !filepath.IsAbs(destDir) || filepath.Base(name) != name {
		return "", errors.New("archive destination must be an absolute directory and a basename")
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", err
	}
	physicalDest, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if !filepath.IsAbs(root.Path) {
			return "", fmt.Errorf("archive source must be absolute: %q", root.Path)
		}
		physicalRoot, evalErr := filepath.EvalSymlinks(root.Path)
		if evalErr != nil && !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		if evalErr == nil && pathWithin(physicalRoot, physicalDest) {
			return "", fmt.Errorf("backup destination is inside archive source %s", root.Path)
		}
	}
	tmp, err := os.CreateTemp(destDir, "."+strings.TrimSuffix(name, ".tar.gz")+"-*.tar.gz.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return "", err
	}
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	if err = writeArchiveContents(tw, roots, tenant); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return "", err
	}
	if err = tw.Close(); err != nil {
		return "", err
	}
	if err = gz.Close(); err != nil {
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	// Keep the random suffix and publish without replacement. Two snapshots
	// in one second must never destroy one another, including across processes.
	path = filepath.Join(destDir, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(tmpName), "."), ".tmp"))
	if err = os.Link(tmpName, path); err != nil {
		return "", err
	}
	dir, err := os.Open(destDir)
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeArchiveContents(tw *tar.Writer, roots []archiveRoot, tenant *registry.Tenant) error {
	manifest := archiveManifest{Version: 1, Tenant: tenant}
	var included []archiveRoot
	// Only a missing optional root is skippable. ENOENT after this preflight
	// is a snapshot consistency failure, not an excuse to silently drop data.
	for _, root := range roots {
		root.Path = filepath.Clean(root.Path)
		if !filepath.IsAbs(root.Path) || root.Name == "manifest.json" || !safeArchivePath(root.Name) {
			return fmt.Errorf("invalid archive root %q / %q", root.Path, root.Name)
		}
		source := archiveSource{Path: root.Path, Name: root.Name}
		info, statErr := os.Lstat(root.Path)
		if statErr != nil {
			if root.Required || !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("archive source %s: %w", root.Path, statErr)
			}
			manifest.Roots = append(manifest.Roots, source)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive root must not be a symlink: %s", root.Path)
		}
		source.Present = true
		covered := false
		for _, prior := range included {
			if pathWithin(prior.Path, root.Path) {
				rel, _ := filepath.Rel(prior.Path, root.Path)
				source.Name = filepath.ToSlash(filepath.Join(prior.Name, rel))
				covered = true
				break
			}
		}
		if !covered {
			included = append(included, root)
		}
		manifest.Roots = append(manifest.Roots, source)
	}
	for _, root := range included {
		if err := addArchiveRoot(tw, root.Path, root.Name); err != nil {
			return fmt.Errorf("archive source %s: %w", root.Path, err)
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

func addArchiveRoot(tw *tar.Writer, root, name string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		archiveName := name
		if rel != "." {
			archiveName = filepath.ToSlash(filepath.Join(name, rel))
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = archiveName
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = target
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		// The walker metadata and the open must refer to the same file. The
		// secure open rejects symlink/FIFO swaps and symlink ancestors.
		file, err := securefile.OpenRegular(path, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			_ = file.Close()
			if statErr != nil {
				return statErr
			}
			return fmt.Errorf("archive source changed during open: %s", path)
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

// Remove snapshots first, then removes the routing entry and system identity.
// purge only controls whether the snapshotted data directories are deleted.
func (m *Manager) Remove(ctx context.Context, t registry.Tenant, purge bool) (snapshot string, err error) {
	err = m.WithLifecycleLock(func() error {
		current, ok := m.Registry.Get(t.Name)
		if !ok {
			return fmt.Errorf("tenant %q not found", t.Name)
		}
		var removeErr error
		snapshot, removeErr = m.removeLocked(ctx, current, purge)
		return removeErr
	})
	return snapshot, err
}

func (m *Manager) removeLocked(ctx context.Context, t registry.Tenant, purge bool) (snapshot string, err error) {
	if err := m.validateTenantPaths(t); err != nil {
		return "", err
	}
	status, err := m.Status(ctx, t)
	if err != nil {
		return "", err
	}
	// These are the installation states created by dshgw. Other states (for
	// example a linked or masked unit) cannot be faithfully restored by enable.
	if status.UnitFileState != "enabled" && status.UnitFileState != "enabled-runtime" && status.UnitFileState != "disabled" {
		return "", fmt.Errorf("cannot transactionally remove unit in state %q", status.UnitFileState)
	}
	registryRemoved := false
	committed := false
	defer func() {
		if err == nil || committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if registryRemoved {
			if restoreErr := m.Registry.Put(t); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore tenant: %w", restoreErr))
			} else if restoreErr := m.Registry.Save(); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore registry: %w", restoreErr))
			}
			if restoreErr := m.InstallNginx(rollbackCtx, true); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore nginx: %w", restoreErr))
			}
		}
		if status.Enabled {
			args := []string{"enable"}
			if status.UnitFileState == "enabled-runtime" {
				args = append(args, "--runtime")
			}
			args = append(args, m.unit(t.Name))
			if _, restoreErr := m.run(rollbackCtx, "systemctl", args...); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore worker enablement: %w", restoreErr))
			}
		}
		if status.Active {
			if _, restoreErr := m.run(rollbackCtx, "systemctl", "start", m.unit(t.Name)); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore worker activity: %w", restoreErr))
			}
		}
	}()
	disableArgs := []string{"disable", "--now"}
	if status.UnitFileState == "enabled-runtime" {
		disableArgs = append(disableArgs, "--runtime")
	}
	disableArgs = append(disableArgs, m.unit(t.Name))
	if _, err = m.run(ctx, "systemctl", disableArgs...); err != nil {
		return "", err
	}
	snapshot, err = m.BackupTenant(t)
	if err != nil {
		return "", fmt.Errorf("tenant snapshot failed: %w", err)
	}
	m.Registry.Delete(t.Name)
	registryRemoved = true
	if err = m.Registry.Save(); err != nil {
		return snapshot, err
	}
	if m.Sessions != nil {
		if err = m.Sessions.DeleteTenant(t.Name); err != nil {
			return snapshot, err
		}
	}
	if m.Activity != nil {
		if err = m.Activity.Delete(t.Name); err != nil {
			return snapshot, err
		}
	}
	if err = m.InstallNginx(ctx, true); err != nil {
		return snapshot, err
	}
	// userdel is not transactional: even a failing command may already have
	// removed the identity. Do not resurrect routes/start a missing identity.
	// Preserve the snapshot and data for manual recovery on any failure here.
	committed = true
	if _, err = m.run(ctx, "userdel", m.user(t.Name)); err != nil {
		return snapshot, fmt.Errorf("tenant routes removed; user deletion incomplete (data retained): %w", err)
	}
	if purge {
		for _, path := range []string{filepath.Dir(t.DshHome), t.Workspace, filepath.Join(m.Config.Deploy.TenantConfigRoot, t.Name), filepath.Join(m.Config.HandshakeDir, t.Name+".url")} {
			if removeErr := os.RemoveAll(path); removeErr != nil {
				return snapshot, removeErr
			}
		}
	}
	return snapshot, nil
}

func safeArchivePath(name string) bool {
	return name != "" && name != "." && !filepath.IsAbs(name) && filepath.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

func ListArchive(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !safeArchivePath(header.Name) {
			return nil, errors.New("unsafe archive path")
		}
		names = append(names, header.Name)
	}
	sort.Strings(names)
	return names, nil
}
