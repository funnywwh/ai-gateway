package tenancy

import (
	"context"
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
	if err := m.StopForLogout(ctx, tenant); err != nil {
		t.Fatalf("StopForLogout: %v", err)
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
	if err := m.StopForLogout(ctx, tenant); err != nil {
		t.Fatalf("second StopForLogout: %v", err)
	}
	// And the tenant can sign in again.
	started, err := m.EnsureRunning(ctx, tenant)
	if err != nil || !started {
		t.Fatalf("signing back in: started=%t err=%v", started, err)
	}
	assertLivePIDs(t, launches, 1)
}
