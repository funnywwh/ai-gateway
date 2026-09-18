package tenancy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
)

// These tests cover the bwrap profile contract and the provisioning preflight
// that guards it. The systemd/per-tenant-account shapes they used to cover are
// gone (M58): a worker is a child process of dshgw running as one unprivileged
// account, so what remains to pin is the sandbox itself and the refusal paths.

func TestBwrapProfileHidesOtherTenants(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	argv, err := m.SandboxProfile(tenant)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--tmpfs /srv", "--tmpfs /home", "--tmpfs /etc/dshgw", tenant.Workspace, m.Config.TenantRoot} {
		if !strings.Contains(joined, want) {
			t.Fatalf("profile missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--ro-bind / /") {
		t.Fatalf("profile exposes the host root:\n%s", joined)
	}
	if !strings.Contains(joined, m.Config.Dsh.NodeBin) {
		t.Fatalf("profile does not run the configured node:\n%s", joined)
	}
}

func TestSandboxProfileRejectsNonBwrapTenant(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := registry.Tenant{Name: "alice", Isolation: "something-else"}
	if _, err := m.SandboxProfile(tenant); err == nil {
		t.Fatal("a tenant that is not in bwrap isolation produced a profile")
	}
}

func TestSandboxProfileReadyRequiresTheUnprivilegedAccount(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	m.Config.Deploy.WorkerUser = ""
	if err := m.SandboxProfileReady(tenant); err == nil || !strings.Contains(err.Error(), "worker_user") {
		t.Fatalf("missing worker account accepted: %v", err)
	}
}

func TestSandboxProfileReadyRejectsWorldReadableTenantRoots(t *testing.T) {
	m, _, _ := managerFixture(t)
	// The mount namespace is the isolation boundary and these permission bits are
	// the second line of defence, so a leaf any local user could walk through is
	// refused before any worker starts.
	tenant := fixtureTenant(t, m, "alice", 32100)
	if err := os.Chmod(tenant.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	err := m.SandboxProfileReady(tenant)
	if err == nil || !strings.Contains(err.Error(), "other-user access") {
		t.Fatalf("world-readable tenant root accepted: %v", err)
	}
}

func TestCreateStartsWorkerWithoutAnyPerTenantAccount(t *testing.T) {
	m, runner, _ := managerFixture(t)
	created, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Isolation != registry.IsolationBwrap {
		t.Fatalf("tenant recorded isolation %q", created.Isolation)
	}
	if created.UID != os.Geteuid() {
		t.Fatalf("tenant uid = %d, want the supervisor's own uid %d: there is no per-tenant identity", created.UID, os.Geteuid())
	}
	if status := runner.Status(created); !status.Running {
		t.Fatalf("worker is not running after create: %+v", status)
	}
	for _, dir := range []string{filepath.Dir(created.DshHome), created.Workspace} {
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode = %04o, want 0700", dir, perm)
		}
	}
}

func TestCreateRollsBackWorkerAndDataWhenReadinessFails(t *testing.T) {
	m, runner, _ := managerFixture(t)
	m.Probe = func(context.Context, registry.Tenant) error { return errors.New("never ready") }
	_, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err == nil {
		t.Fatal("create succeeded despite a failing readiness probe")
	}
	if _, exists := m.Registry.Get("alice"); exists {
		t.Fatal("failed create left the tenant registered")
	}
	if len(runner.Running()) != 0 {
		t.Fatalf("failed create left a worker process behind: %+v", runner.Running())
	}
	for _, path := range []string{filepath.Join(m.Config.TenantRoot, "alice"), filepath.Join(m.Config.WorkspaceRoot, "alice")} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed create left %s behind: %v", path, statErr)
		}
	}
}

func TestValidateRuntimeRequiresLoaderDirectories(t *testing.T) {
	_, _, _ = managerFixture(t)
	rt := sandbox.Runtime{BwrapBin: "/usr/bin/bwrap", NodeBin: "/opt/dsh/node/bin/node", BinJS: "/opt/dsh/current/lib/bin.js"}
	if err := sandbox.ValidateRuntime(rt); err != nil {
		t.Fatalf("host loader layout rejected: %v", err)
	}
	rt.BwrapBin = "bwrap"
	if err := sandbox.ValidateRuntime(rt); err == nil {
		t.Fatal("relative bwrap_bin accepted")
	}
}
