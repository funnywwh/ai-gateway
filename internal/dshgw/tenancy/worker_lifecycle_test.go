package tenancy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func policyFixture(t *testing.T) (*Manager, *WorkerRunner, registry.Tenant, string) {
	t.Helper()
	fixture, tenant, launches := lifecycleFixture(t)
	tenant.PublicPort = tenant.WorkerPort + 1
	tenant.KeyPrefix = "abcdefghijkl"
	tenant.CreatedAt = time.Now().UTC()
	tenant.Handshake = registry.HandshakePending
	root := t.TempDir()
	reg := registry.New(filepath.Join(root, "tenants.json"), filepath.Join(root, "keys.json"))
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Config: fixture.Config, Registry: reg, Probe: func(context.Context, registry.Tenant) error { return nil }}
	r := m.workers() // Exercise the real default constructor's policy wiring.
	if r.CanStart == nil {
		t.Fatal("manager did not install current-registry policy")
	}
	r.Profile = fixture.Profile
	r.StopTimeout = time.Second
	t.Cleanup(func() {
		if err := m.ShutdownWorkers(context.Background()); err != nil {
			t.Errorf("cleanup manager: %v", err)
		}
	})
	return m, r, tenant, launches
}

func TestWorkerRunnerCanStartChecksQueuedTransitions(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, deleted := range []bool{false, true} {
			t.Run(fmt.Sprintf("restart=%v/deleted=%v", restart, deleted), func(t *testing.T) {
				m, r, tenant, launches := policyFixture(t)
				profile := r.Profile
				profileCalls := 0
				r.Profile = func(t registry.Tenant) ([]string, error) {
					profileCalls++
					return profile(t)
				}
				// Queue a request carrying an enabled snapshot behind the gate,
				// then change durable intent before it may enter start().
				r.lifecycle.Lock()
				var once sync.Once
				unlock := func() { once.Do(r.lifecycle.Unlock) }
				defer unlock()
				entered := make(chan struct{})
				result := make(chan error, 1)
				go func() {
					close(entered)
					if restart {
						result <- r.Restart(context.Background(), tenant)
					} else {
						result <- r.Start(context.Background(), tenant)
					}
				}()
				<-entered
				if deleted {
					m.Registry.Delete(tenant.Name)
				} else {
					current := tenant
					current.Suspended = true
					if err := m.Registry.Put(current); err != nil {
						t.Fatal(err)
					}
				}
				unlock()
				err := awaitLifecycle(t, result)
				want := "suspended"
				if deleted {
					want = "no longer exists"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("queued transition: got %v, want %s", err, want)
				}
				if profileCalls != 0 || len(launchedPIDs(t, launches)) != 0 {
					t.Fatal("disallowed queued transition built a profile or spawned")
				}
			})
		}
	}
}

func TestManagerStartWorkerClearsSuspensionBeforePolicy(t *testing.T) {
	m, r, tenant, launches := policyFixture(t)
	tenant.Suspended = true
	if err := m.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), tenant); err == nil {
		t.Fatal("direct start unexpectedly bypassed suspension")
	}
	if err := m.StartWorker(context.Background(), tenant); err != nil {
		t.Fatalf("explicit enable failed: %v", err)
	}
	current, ok := m.Registry.Get(tenant.Name)
	if !ok || current.Suspended {
		t.Fatal("explicit enable failed to clear durable suspension")
	}
	assertLivePIDs(t, launches, 1)
}

func TestManagerWorkerStartAllowedWithoutRegistry(t *testing.T) {
	m := &Manager{Config: &config.Config{}}
	if err := m.workers().CanStart(registry.Tenant{Name: "standalone", Suspended: true}); err != nil {
		t.Fatalf("nil registry should preserve standalone fixture behavior: %v", err)
	}
}

// Signing out stops a tenant's dsh, so signing in has to start it again — before the browser is
// redirected to it, and without disturbing a worker that is already serving somebody (M69).
func TestEnsureRunningStartsAStoppedTenantAndLeavesARunningOneAlone(t *testing.T) {
	m, r, tenant, launches := policyFixture(t)
	ctx := context.Background()

	started, err := m.EnsureRunning(ctx, tenant)
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if !started {
		t.Fatal("a tenant with no worker was not started")
	}
	if len(r.Running()) != 1 {
		t.Fatalf("running workers = %+v, want the tenant's", r.Running())
	}
	first := launchedPIDs(t, launches)

	started, err = m.EnsureRunning(ctx, tenant)
	if err != nil {
		t.Fatalf("second EnsureRunning: %v", err)
	}
	if started {
		t.Fatal("a running worker was reported as started")
	}
	if second := launchedPIDs(t, launches); len(second) != len(first) {
		t.Fatalf("a running worker was restarted: %v -> %v", first, second)
	}
	// A running worker is left running: the login refresh changes settings.yaml, which dsh
	// hot-reloads, and restarting would cut off whatever turn it is in.
	if len(r.Running()) != 1 {
		t.Fatalf("the worker disappeared: %+v", r.Running())
	}
}

// The operator's suspension outranks a user's login: a tenant the deployment turned off is not
// silently brought back by somebody signing in.
func TestEnsureRunningRefusesASuspendedTenant(t *testing.T) {
	m, r, tenant, launches := policyFixture(t)
	tenant.Suspended = true
	if err := m.Registry.Put(tenant); err != nil {
		t.Fatal(err)
	}
	if err := m.Registry.Save(); err != nil {
		t.Fatal(err)
	}
	started, err := m.EnsureRunning(context.Background(), tenant)
	if err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("a suspended tenant was started: started=%t err=%v", started, err)
	}
	if len(launchedPIDs(t, launches)) != 0 || len(r.Running()) != 0 {
		t.Fatal("a suspended tenant got a worker")
	}
	current, _ := m.Registry.Get(tenant.Name)
	if !current.Suspended {
		t.Fatal("the durable suspension was cleared")
	}
}

// StopForLogout stops the worker but must never record an operator suspension: that field is
// the console's on/off intent, and a person signing out is not an operator action.
func TestStopForLogoutStopsWithoutSuspending(t *testing.T) {
	m, r, tenant, launches := policyFixture(t)
	ctx := context.Background()
	if _, err := m.EnsureRunning(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	result, err := m.StopForLogout(ctx, tenant)
	if err != nil {
		t.Fatalf("StopForLogout: %v", err)
	}
	if !result.WorkerStopped {
		t.Fatal("StopForLogout did not verify the worker is gone")
	}
	if len(r.Running()) != 0 {
		t.Fatalf("the worker survived the logout: %+v", r.Running())
	}
	assertLivePIDs(t, launches, 0)
	current, ok := m.Registry.Get(tenant.Name)
	if !ok {
		t.Fatal("the tenant disappeared from the registry")
	}
	if current.Suspended {
		t.Fatal("a logout recorded an operator suspension")
	}
	// Idempotent: a tenant whose worker is already stopped logs out without error.
	if _, err := m.StopForLogout(ctx, tenant); err != nil {
		t.Fatalf("second StopForLogout: %v", err)
	}
	// And the tenant can sign in again.
	started, err := m.EnsureRunning(ctx, tenant)
	if err != nil || !started {
		t.Fatalf("signing back in: started=%t err=%v", started, err)
	}
	assertLivePIDs(t, launches, 1)
}

// M76's order, which is the whole point of the milestone: the mounts are force-detached FIRST and
// the dsh is force-stopped LAST. The observable that pins it is the worker's own state during the
// detach — the mounts must go while the dsh it was running for is still alive — plus the worker
// being gone by the time the teardown returns.
func TestStopForLogoutDetachesMountsBeforeStoppingTheWorker(t *testing.T) {
	var order []string
	m, r, tenant, _ := policyFixture(t)
	watching := &orderedBrowserHook{order: &order, attached: []string{filepath.Join(tenant.Workspace, "browser", "local")}}
	ssh := &orderedSSHHook{order: &order, attached: []string{filepath.Join(tenant.Workspace, "ssh", "aipc", "home")}}
	watching.duringDetach = func() {
		if len(r.Running()) == 0 {
			t.Error("the mounts were detached after the worker was already stopped")
		}
	}
	m.BrowserWorkspaces = watching
	m.SSHWorkspaces = ssh
	if _, err := m.EnsureRunning(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if len(r.Running()) != 1 {
		t.Fatal("the fixture has no running worker to log out of")
	}
	order = nil

	result, err := m.StopForLogout(context.Background(), tenant)
	if err != nil {
		t.Fatalf("StopForLogout: %v", err)
	}
	if len(order) != 2 || order[0] != "browser-detach" || order[1] != "ssh-detach" {
		t.Fatalf("order = %v, want both mount services detached", order)
	}
	if len(r.Running()) != 0 {
		t.Fatalf("the worker survived the logout: %+v", r.Running())
	}
	// Two mounts were attached before and none after: the browser hook reports one, the ssh hook
	// one, and both are gone once their detach ran.
	if result.MountsDetached != 2 {
		t.Fatalf("MountsDetached = %d, want 2", result.MountsDetached)
	}
	if len(result.MountsLeftover) != 0 {
		t.Fatalf("MountsLeftover = %v, want none", result.MountsLeftover)
	}
	if !result.WorkerStopped {
		t.Fatal("the worker stop was not verified")
	}
}

// A mount that could not be detached is reported, and the dsh is still force-stopped: leaving a
// dsh running because an unrelated mount failed is the behaviour M76 removes.
func TestStopForLogoutReportsLeftoversAndStillStops(t *testing.T) {
	var order []string
	m, r, tenant, _ := policyFixture(t)
	leftover := filepath.Join(tenant.Workspace, "browser", "stuck")
	browser := &orderedBrowserHook{order: &order, attached: []string{leftover}, detachErr: errors.New("still mounted")}
	m.BrowserWorkspaces = browser
	if _, err := m.EnsureRunning(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	result, err := m.StopForLogout(context.Background(), tenant)
	if err == nil {
		t.Fatal("a mount that stayed attached was reported as a clean teardown")
	}
	if !strings.Contains(err.Error(), "still mounted") {
		t.Fatalf("the error does not carry the failure: %v", err)
	}
	if len(result.MountsLeftover) != 1 || result.MountsLeftover[0] != leftover {
		t.Fatalf("leftover = %v, want %s", result.MountsLeftover, leftover)
	}
	if !result.WorkerStopped {
		t.Fatal("the dsh stayed running because a mount could not be detached")
	}
	if len(r.Running()) != 0 {
		t.Fatalf("the worker survived the logout: %+v", r.Running())
	}
}

// orderedBrowserHook records the order of the lifecycle calls and can hold a mount back.
type orderedBrowserHook struct {
	order *[]string
	// attached is what this account has mounted now; a failed detach leaves it in place, which is
	// what the teardown reports as a leftover.
	attached  []string
	detachErr error
	// duringDetach runs inside the detach, where a test can observe the worker's state.
	duringDetach func()
}

func (h *orderedBrowserHook) MountsFor(string) []string { return nil }
func (h *orderedBrowserHook) DropTenant(context.Context, string) error {
	*h.order = append(*h.order, "browser-drop")
	return nil
}
func (h *orderedBrowserHook) AttachedMounts(string) []string { return h.attached }
func (h *orderedBrowserHook) DetachTenant(context.Context, string) error {
	*h.order = append(*h.order, "browser-detach")
	if h.duringDetach != nil {
		h.duringDetach()
	}
	if h.detachErr == nil {
		h.attached = nil
	}
	return h.detachErr
}

type orderedSSHHook struct {
	order    *[]string
	attached []string
}

func (h *orderedSSHHook) MountsFor(string) []string                             { return nil }
func (h *orderedSSHHook) EnsureIdentity(string, string, string) error           { return nil }
func (h *orderedSSHHook) DropTenant(context.Context, string) error              { return nil }
func (h *orderedSSHHook) AttachedMounts(string) []string                        { return h.attached }
func (h *orderedSSHHook) Restore(context.Context, string, string, string) error { return nil }
func (h *orderedSSHHook) DetachTenant(context.Context, string) error {
	*h.order = append(*h.order, "ssh-detach")
	h.attached = nil
	return nil
}
