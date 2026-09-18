package tenancy

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// prepareBrowserMountRoot runs before launching a worker, never from a tenant
// request. The namespace then binds this gateway-owned container read-only so
// tenant commands cannot replace paths while the gateway creates FUSE mounts.
func (m *Manager) prepareBrowserMountRoot(t registry.Tenant) error {
	if !m.Config.BrowserWorkspaces.Enabled {
		return nil
	}
	if err := m.validateTenantPaths(t); err != nil {
		return err
	}
	workspace, err := filepath.EvalSymlinks(t.Workspace)
	if err != nil {
		return err
	}
	if workspace != t.Workspace {
		return fmt.Errorf("browser workspace root contains symlinks: %s", t.Workspace)
	}
	root := filepath.Join(workspace, "browser")
	if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create browser mount root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("browser mount root must be a private real directory: %s", root)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if canonical != root {
		return fmt.Errorf("browser mount root contains symlinks: %s", root)
	}
	return nil
}
