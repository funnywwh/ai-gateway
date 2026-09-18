package tenancy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserMountRootPreparation(t *testing.T) {
	m, _, tenant := backupFixture(t)
	root := filepath.Join(tenant.Workspace, "browser")
	// Disabled leaves ordinary directories and even symlinks untouched.
	outside := t.TempDir()
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareBrowserMountRoot(tenant); err != nil {
		t.Fatal(err)
	}
	if m.sandboxTenant(tenant).BrowserMountRoot != "" {
		t.Fatal("disabled feature reserved browser root")
	}
	m.Config.BrowserWorkspaces.Enabled = true
	if err := m.prepareBrowserMountRoot(tenant); err == nil {
		t.Fatal("accepted symlink root")
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	// Existing tenants may lack the container; every runner profile build,
	// including Restart, must prepare it before rendering the protected bind.
	if _, err := m.SandboxProfile(tenant); err != nil {
		t.Fatal(err)
	}
	if got := m.sandboxTenant(tenant).BrowserMountRoot; got != root {
		t.Fatalf("root %q", got)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("unsafe root %v", info.Mode())
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareBrowserMountRoot(tenant); err == nil {
		t.Fatal("accepted broad permissions")
	}
	m.Config.BrowserWorkspaces.Enabled = false
	if err := m.prepareBrowserMountRoot(tenant); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(root)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("disabled feature changed ordinary directory")
	}
}

func TestCreateBrowserTenantPreparesRootBeforeProfile(t *testing.T) {
	m, _, _ := managerFixture(t)
	m.Config.BrowserWorkspaces.Enabled = true
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"m"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(tenant.Workspace, "browser"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("missing private root: %v", err)
	}
}
