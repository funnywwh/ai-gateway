package tenancy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCreateChecksTenantUIDAccessBeforePublishAndStart(t *testing.T) {
	m, runner := managerFixture(t)
	seen := 0
	runner.hook = func(_ context.Context, command Command) ([]byte, error, bool) {
		if command.Path != "runuser" {
			return nil, nil, false
		}
		seen++
		if _, exists := m.Registry.Get("alice"); exists {
			t.Fatal("tenant published before its UID can read the artifacts")
		}
		if len(command.Args) != 6 || strings.Join(command.Args[:4], " ") != "-u dsh-alice -- /usr/bin/test" {
			t.Fatalf("access probe is not scoped to tenant UID: %+v", command)
		}
		return nil, nil, true
	}
	if _, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-key", []string{"m"}, CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if seen != 4 {
		t.Fatalf("access probes=%d want 4", seen)
	}
	commands := commandsText(runner.commands)
	if strings.LastIndex(commands, "runuser") > strings.Index(commands, "systemctl start") {
		t.Fatal("worker started before access verification")
	}
	info, err := os.Stat(m.Config.TenantRoot)
	if err != nil || info.Mode().Perm() != 0o711 {
		t.Fatalf("new shared tenant parent=%v error=%v", info, err)
	}
}

func TestCreateAccessDeniedRollsBackBeforeWorkerStart(t *testing.T) {
	m, runner := managerFixture(t)
	runner.fail = "runuser"
	_, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-key", []string{"m"}, CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "shared parent search permissions") {
		t.Fatalf("access failure hidden: %v", err)
	}
	if _, exists := m.Registry.Get("alice"); exists {
		t.Fatal("inaccessible tenant registered")
	}
	commands := commandsText(runner.commands)
	if strings.Contains(commands, "systemctl start") || !strings.Contains(commands, "userdel dsh-alice") {
		t.Fatalf("unsafe start/rollback after access failure: %s", commands)
	}
	for _, root := range []string{m.Config.TenantRoot, m.Config.WorkspaceRoot, m.Config.Deploy.TenantConfigRoot} {
		if _, err := os.Stat(filepath.Join(root, "alice")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inaccessible test tenant data remains: %s (%v)", root, err)
		}
	}
}

func TestCreateExplicitModesSurviveRestrictiveOperatorUmask(t *testing.T) {
	m, _ := managerFixture(t)
	// The production installer provisions shared parents with explicit install
	// modes. This test isolates the newly owned per-tenant directory modes.
	for _, root := range []string{m.Config.TenantRoot, m.Config.WorkspaceRoot, m.Config.Deploy.TenantConfigRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-key", []string{"m"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Dir(tenant.DshHome): 0o700,
		tenant.Workspace:             0o700,
		filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice"): 0o750,
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode mismatch: got %v err=%v want=%o", path, info, err, want)
		}
	}
}
