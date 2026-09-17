package tenancy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
)

// enableBwrapMode switches a fixture manager to the bwrap isolation mode,
// including the injected account/runtime checks that stand in for a real host.
// It is applied after a user-mode create to exercise migration into bwrap.
func enableBwrapMode(t *testing.T, m *Manager) {
	t.Helper()
	m.WorkerAccount = func(name string) error {
		// A stub must fail like the production check does — by returning an
		// error — so a missing worker_user surfaces as a lifecycle failure
		// instead of a test panic.
		if name == "" {
			return errors.New("worker_user must name the shared worker account")
		}
		if name != "dshgw" {
			return fmt.Errorf("unexpected worker account %q", name)
		}
		return nil
	}
	m.RuntimeCheck = sandbox.ValidateBindings
	m.Config.Deploy.Isolation = config.IsolationBwrap
	m.Config.Deploy.WorkerUser = "dshgw"
	m.Config.Deploy.BwrapBin = "/usr/bin/bwrap"
	m.Config.Deploy.DshgwBinary = "/opt/dshgw/bin/dshgw"
	m.Config.Deploy.WorkerUnitBwrap = "dsh-worker-bwrap@.service"
}

// bwrapFixture is managerFixture plus the deployment facts the bwrap isolation
// mode needs: the shared worker account and the injected account/runtime checks
// that stand in for a real host.
func bwrapFixture(t *testing.T) (*Manager, *fakeRunner) {
	t.Helper()
	m, runner := managerFixture(t)
	enableBwrapMode(t, m)
	return m, runner
}

func TestCreateInBwrapModeCreatesNoOSTenantUser(t *testing.T) {
	m, r := bwrapFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tenant.EffectiveIsolation() != registry.IsolationBwrap {
		t.Fatalf("tenant recorded isolation %q", tenant.Isolation)
	}
	joined := commandsText(r.commands)
	for _, forbidden := range []string{"useradd", "userdel"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("bwrap mode ran %s:\n%s", forbidden, joined)
		}
	}
	if strings.Contains(joined, "runuser") {
		t.Fatalf("bwrap mode probed access with runuser:\n%s", joined)
	}
	// The writable roots belong to the one shared worker account.
	if !strings.Contains(joined, "chown -R dshgw:dshgw") {
		t.Fatalf("shared worker account did not take ownership:\n%s", joined)
	}
	// The unit is the bwrap template instance and starts the sandbox launcher.
	if !strings.Contains(joined, "systemctl start dsh-worker-bwrap@alice.service") {
		t.Fatalf("wrong worker unit started:\n%s", joined)
	}
}

func TestBwrapWorkerSpecMatchesSharedAccountLauncher(t *testing.T) {
	m, _ := bwrapFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := m.WorkerSpecFor(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if spec.User != "dshgw" || spec.Group != m.Config.Deploy.GatewayGroup {
		t.Fatalf("unexpected worker identity: %+v", spec)
	}
	if !strings.Contains(spec.Exec, "sandbox-exec alice") || !strings.Contains(spec.Exec, m.Config.Deploy.DshgwBinary) || !strings.Contains(spec.Exec, m.Config.Deploy.ConfigPath) {
		t.Fatalf("ExecStart does not launch the sandbox launcher: %q", spec.Exec)
	}
	if spec.UnitTemplate != "dsh-worker-bwrap@.service" {
		t.Fatalf("unexpected unit template: %+v", spec)
	}
}

func TestUserModeWorkerUnitKeepsPerTenantIdentity(t *testing.T) {
	m, _ := managerFixture(t)
	tenant := registry.Tenant{Name: "alice", Isolation: "", WorkerPort: 32100, Workspace: "/srv/dsh/alice", DshHome: "/var/lib/dshgw/tenants/alice/.dsh"}
	spec, err := m.WorkerSpecFor(tenant)
	if err != nil {
		t.Fatal(err)
	}
	if spec.User != "dsh-alice" || spec.Group != "dsh-alice" {
		t.Fatalf("user mode lost the per-tenant identity: %+v", spec)
	}
	if strings.Contains(spec.Exec, "sandbox-exec") {
		t.Fatalf("user mode started the sandbox launcher: %q", spec.Exec)
	}
	if !strings.Contains(spec.Exec, "--port ${DSH_PORT}") {
		t.Fatalf("user mode ExecStart drifted: %q", spec.Exec)
	}
}

func TestBwrapProfileHidesOtherTenants(t *testing.T) {
	m, _ := bwrapFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	m, _ := managerFixture(t)
	tenant := registry.Tenant{Name: "alice", Isolation: registry.IsolationUser}
	if _, err := m.SandboxProfile(tenant); err == nil {
		t.Fatal("user-mode tenant produced a bwrap profile")
	}
}

func TestBwrapModeRequiresSharedAccount(t *testing.T) {
	m, _ := bwrapFixture(t)
	m.Config.Deploy.WorkerUser = ""
	_, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "worker_user") {
		t.Fatalf("missing shared account not reported: %v", err)
	}
	if _, exists := m.Registry.Get("alice"); exists {
		t.Fatal("tenant published despite an unusable worker account")
	}
}

func TestBwrapModeRejectsWorldReadableTenantRoots(t *testing.T) {
	m, _ := bwrapFixture(t)
	// In shared-account mode the mount view is the isolation boundary and the
	// host permission bits are the second line of defence, so the preflight
	// must reject a tenant leaf that an unrelated local process could walk
	// through — exactly what a lax umask (0775 instead of 0700) produces.
	tenantHome := filepath.Join(m.Config.TenantRoot, "alice", ".dsh")
	workspace := filepath.Join(m.Config.WorkspaceRoot, "alice")
	configDir := filepath.Join(m.Config.Deploy.TenantConfigRoot, "alice")
	for _, dir := range []string{tenantHome, workspace, configDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tenant := registry.Tenant{
		Name: "alice", Isolation: registry.IsolationBwrap, WorkerPort: 32100,
		Workspace: workspace, DshHome: tenantHome,
	}
	err := m.SandboxProfileReady(tenant)
	if err == nil || !strings.Contains(err.Error(), "other-user access") {
		t.Fatalf("world-readable tenant roots accepted: %v", err)
	}
}

func TestBwrapCreateRollsBackWithoutUserdel(t *testing.T) {
	m, r := bwrapFixture(t)
	r.fail = "systemctl start"
	_, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err == nil {
		t.Fatal("expected failure")
	}
	joined := commandsText(r.commands)
	if strings.Contains(joined, "userdel") {
		t.Fatalf("rollback tried to delete a shared account:\n%s", joined)
	}
	if _, exists := m.Registry.Get("alice"); exists {
		t.Fatal("registry not rolled back")
	}
}

func TestBwrapRemoveSkipsUserdel(t *testing.T) {
	m, r := bwrapFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Remove(context.Background(), tenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == "" {
		t.Fatal("remove produced no snapshot")
	}
	joined := commandsText(r.commands)
	if strings.Contains(joined, "userdel") {
		t.Fatalf("bwrap removal deleted an OS account:\n%s", joined)
	}
	// Purge removed the data directories the shared account owned.
	for _, path := range []string{filepath.Dir(tenant.DshHome), tenant.Workspace} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("purged data remains at %s", path)
		}
	}
}

func TestReisolateUserToBwrapMovesOwnershipAndUnit(t *testing.T) {
	m, r := managerFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	enableBwrapMode(t, m)
	if err := m.Reisolate(context.Background(), tenant, registry.IsolationBwrap); err != nil {
		t.Fatal(err)
	}
	joined := commandsText(r.commands)
	for _, want := range []string{
		"systemctl stop dsh-worker@alice.service",
		"systemctl start dsh-worker-bwrap@alice.service",
		"systemctl enable dsh-worker-bwrap@alice.service",
		"chown -R dshgw:dshgw",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "userdel") {
		t.Errorf("migration deleted an OS account:\n%s", joined)
	}
	current, ok := m.Registry.Get("alice")
	if !ok || current.EffectiveIsolation() != registry.IsolationBwrap {
		t.Fatalf("registry did not record bwrap isolation: %+v", current)
	}
}

func TestReisolateBwrapToUserRestoresPerTenantIdentity(t *testing.T) {
	m, r := bwrapFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reisolate(context.Background(), tenant, registry.IsolationUser); err != nil {
		t.Fatal(err)
	}
	joined := commandsText(r.commands)
	for _, want := range []string{
		"useradd --system --user-group --no-create-home",
		"systemctl stop dsh-worker-bwrap@alice.service",
		"systemctl start dsh-worker@alice.service",
		"chown -R dsh-alice:dsh-alice",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	current, ok := m.Registry.Get("alice")
	if !ok || current.EffectiveIsolation() != registry.IsolationUser {
		t.Fatalf("registry did not record user isolation: %+v", current)
	}
}

func TestReisolateRollsBackRegistryOwnershipAndWorker(t *testing.T) {
	m, r := bwrapFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r.fail = "systemctl start"
	if err := m.Reisolate(context.Background(), tenant, registry.IsolationUser); err == nil {
		t.Fatal("expected the forced start failure to fail the migration")
	}
	current, ok := m.Registry.Get("alice")
	if !ok || current.EffectiveIsolation() != registry.IsolationBwrap {
		t.Fatalf("registry not restored: %+v", current)
	}
	joined := commandsText(r.commands)
	// The freshly created account is rolled back, ownership returns to the
	// shared worker account, and the previous worker is started again.
	for _, want := range []string{"userdel dsh-alice", "chown -R dshgw:dshgw", "systemctl start dsh-worker-bwrap@alice.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
}

func TestReisolateRejectsSameModeAndUnknownTarget(t *testing.T) {
	m, _ := bwrapFixture(t)
	tenant, err := m.Create(context.Background(), "alice", "sk-aaaaaaaaa-rest", []string{"model"}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reisolate(context.Background(), tenant, registry.IsolationBwrap); err == nil {
		t.Fatal("same-mode migration accepted")
	}
	if err := m.Reisolate(context.Background(), tenant, "jail"); err == nil {
		t.Fatal("unknown target accepted")
	}
}
