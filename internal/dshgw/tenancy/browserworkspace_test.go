package tenancy

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type browserHook struct {
	mounts []string
	drops  int
}

func (h *browserHook) MountsFor(string) []string                { return h.mounts }
func (h *browserHook) DropTenant(context.Context, string) error { h.drops++; return nil }

func TestBrowserWorkspacePatchRefresh(t *testing.T) {
	cfg, tenant := renderFixture(t)
	arts, err := RenderTenantArtifacts(cfg, tenant, "sk-secret", models("m"), TenantOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteArtifacts(arts); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(tenant.DshHome, "storages", "workspace.json")
	before, err := os.ReadFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(tenant.DshHome, "profiles", "web", "cordis.patch.yml")
	for _, enabled := range []bool{false, true, true, false} {
		cfg.BrowserWorkspaces.Enabled = enabled
		if warning, err := EnsureBrowserWorkspaceRow(cfg, tenant); err != nil || warning != "" {
			t.Fatalf("refresh: %s %v", warning, err)
		}
		data, err := os.ReadFile(patch)
		if err != nil {
			t.Fatal(err)
		}
		count := strings.Count(string(data), "id: browser-workspace")
		if enabled && count != 1 || !enabled && count != 0 {
			t.Fatalf("wrong row count %d", count)
		}
		if enabled && !strings.Contains(string(data), "mountSubdir: browser") {
			t.Fatal("missing fixed mount directory")
		}
	}
	after, _ := os.ReadFile(workspace)
	if !bytes.Equal(before, after) {
		t.Fatal("refresh changed user workspaces")
	}
	hook := &browserHook{mounts: []string{filepath.Join(tenant.Workspace, "browser", "local")}}
	m := &Manager{Config: cfg, BrowserWorkspaces: hook}
	if got := m.sandboxTenant(tenant).BrowserMounts; len(got) != 1 || got[0] != hook.mounts[0] {
		t.Fatal("mounts not propagated")
	}
}

func TestBrowserArchiveExcludesRemoteFiles(t *testing.T) {
	root := t.TempDir()
	browser := filepath.Join(root, "browser")
	if err := os.MkdirAll(browser, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(browser, "private"), []byte("remote"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := addArchiveRoot(tw, root, "workspace", browser); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(header.Name, "browser") {
			t.Fatal("archived browser data")
		}
		if header.Name == "workspace/local" {
			found = true
		}
	}
	if !found {
		t.Fatal("local data lost")
	}
}

func TestBrowserLifecycleDropNotRestart(t *testing.T) {
	m, _, tenant := backupFixture(t)
	hook := &browserHook{}
	m.BrowserWorkspaces = hook
	if err := m.Restart(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if hook.drops != 0 {
		t.Fatal("restart dropped browser mounts")
	}
	if err := m.StopWorker(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if hook.drops != 1 {
		t.Fatal("stop did not drop browser mounts")
	}
	if _, err := m.Remove(context.Background(), tenant, true); err != nil {
		t.Fatal(err)
	}
	if hook.drops != 2 {
		t.Fatal("remove did not drop browser mounts")
	}
}

// A detached runtime hook has no usable worker mounts but can report live mounts
// that another process owns. Empty exclusions must not lose ordinary local data.
type browserBackupHook struct {
	browserHook
	excluded []string
	err      error
}

func (h *browserBackupHook) BrowserBackupExclusions(string) ([]string, error) {
	return h.excluded, h.err
}

func TestBrowserBackupScopesAndDisabledLocalData(t *testing.T) {
	for _, whole := range []bool{false, true} {
		for _, mode := range []string{"enabled", "disabled-local", "disabled-live"} {
			t.Run(fmt.Sprintf("whole=%t/%s", whole, mode), func(t *testing.T) {
				m, _, tenant := backupFixture(t)
				browser := filepath.Join(tenant.Workspace, "browser")
				remote := filepath.Join(browser, "mount")
				if err := os.MkdirAll(remote, 0700); err != nil {
					t.Fatal(err)
				}
				for _, file := range []string{filepath.Join(browser, "ordinary"), filepath.Join(remote, "remote")} {
					if err := os.WriteFile(file, []byte("payload"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				hook := &browserBackupHook{}
				m.BrowserWorkspaces = hook
				m.Config.BrowserWorkspaces.Enabled = mode == "enabled"
				if mode == "disabled-live" {
					hook.excluded = []string{remote}
				}
				var snapshot string
				var err error
				if whole {
					snapshot, err = m.Backup(context.Background())
				} else {
					snapshot, err = m.BackupTenant(tenant)
				}
				if err != nil {
					t.Fatal(err)
				}
				files := archiveFiles(t, snapshot)
				ordinary, remoteFound := false, false
				for name := range files {
					if strings.HasSuffix(name, "/browser/ordinary") {
						ordinary = true
					}
					if strings.HasSuffix(name, "/browser/mount/remote") {
						remoteFound = true
					}
				}
				if ordinary != (mode != "enabled") {
					t.Fatalf("ordinary present=%t mode=%s", ordinary, mode)
				}
				if remoteFound != (mode == "disabled-local") {
					t.Fatalf("remote present=%t mode=%s", remoteFound, mode)
				}
			})
		}
	}
}

func TestBrowserBackupExclusionFailureIsFatal(t *testing.T) {
	m, _, tenant := backupFixture(t)
	for _, hook := range []*browserBackupHook{
		{err: fmt.Errorf("mount inventory unavailable")},
		{excluded: []string{tenant.Workspace}},
	} {
		m.BrowserWorkspaces = hook
		if _, err := m.BackupTenant(tenant); err == nil {
			t.Fatal("tenant backup accepted unsafe exclusions")
		}
		if _, err := m.Backup(context.Background()); err == nil {
			t.Fatal("full backup accepted unsafe exclusions")
		}
	}
}
