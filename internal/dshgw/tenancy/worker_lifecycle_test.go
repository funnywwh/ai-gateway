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
