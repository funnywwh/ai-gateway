package browsermount

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func TestDropWaitsForRacingActivationThenStopsAgain(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var bound atomic.Bool
	var stops atomic.Int32
	var s *Service
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	s = NewWithMount(func(context.Context, registry.Tenant) error {
		close(entered)
		<-release
		// Simulate a namespace spawned after the manager's first Stop completed.
		bound.Store(true)
		return nil
	}, func(string, fs.Backend) (Mounted, error) {
		return checkedMount{func() error {
			if bound.Load() {
				return errors.New("unmounted live namespace")
			}
			return nil
		}}, nil
	})
	s.SetStopWorker(func(context.Context, registry.Tenant) error {
		if len(s.MountsFor(tenant.Name)) != 0 {
			t.Error("stop sees bindable closing path")
		}
		// The raw worker callback can query the service without a lock inversion.
		stops.Add(1)
		bound.Store(false)
		return nil
	})
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	activation := make(chan error, 1)
	go func() {
		_, err := s.dispatch(context.Background(), "activate", tenant, "owner", payload{Token: sh.token})
		activation <- err
	}()
	req := <-sh.queue
	if _, err := s.dispatch(context.Background(), "respond", tenant, "owner", payload{Token: sh.token, ID: req.ID, Result: fs.Response{OK: true, Value: fs.Value{Kind: "directory"}}}); err != nil {
		t.Fatal(err)
	}
	<-entered
	dropping := make(chan error, 1)
	go func() { dropping <- s.DropTenant(context.Background(), tenant.Name) }()
	select {
	case err := <-dropping:
		t.Fatal("drop bypassed active restart", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	_ = awaitError(t, activation)
	if err := awaitError(t, dropping); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 {
		t.Fatal("raw stop not invoked")
	}
}

// M76 reversed what a failed worker stop means here. Before it, a stop that failed returned
// early, so every mount stayed mounted because an unrelated step had failed — and the caller
// learned only that the stop failed. Now the failure is still reported (a worker that would not
// die is an operator's business), but the mounts are detached anyway: the forced detach does not
// need that namespace to be gone, and a signed-out account must not keep a kernel mount behind.
func TestDropReportsAStopFailureAndStillDetaches(t *testing.T) {
	s, tenant, m := fixture(t)
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.SetStopWorker(func(context.Context, registry.Tenant) error {
		if calls.Add(1) == 1 {
			return errors.New("worker still running")
		}
		return nil
	})
	err = s.DropTenant(context.Background(), tenant.Name)
	if err == nil {
		t.Fatal("stop error hidden")
	}
	if !strings.Contains(err.Error(), "stop worker before drop") {
		t.Fatalf("the error does not name the failed step: %v", err)
	}
	m.mu.Lock()
	unmounted := m.closed
	m.mu.Unlock()
	if !unmounted {
		t.Fatal("mount kept because an unrelated step failed")
	}
	if _, ok := s.shares[sh.token]; ok {
		t.Fatal("fully detached share still tracked")
	}
	if err := s.DropTenant(context.Background(), tenant.Name); err != nil {
		t.Fatal(err)
	}
}

// A share the logout path detached is final: its binding worker is gone for good, so a later
// expiry pass retries the detach but must never restart a worker to release a namespace for it
// (that would bring a signed-out account's dsh back to life).
func TestFinalShareIsNeverRestartedByExpiry(t *testing.T) {
	var restarts atomic.Int32
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	s := NewWithMount(func(context.Context, registry.Tenant) error { restarts.Add(1); return nil }, func(string, fs.Backend) (Mounted, error) {
		return checkedMount{func() error { return errors.New("Device or resource busy") }}, nil
	})
	s.SetForceDetach(func(string) error { return errors.New("still busy") })
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DetachTenant(context.Background(), tenant.Name); err == nil {
		t.Fatal("a mount that could not be detached was reported as detached")
	}
	if restarts.Load() != 0 {
		t.Fatalf("the logout teardown restarted the worker %d times", restarts.Load())
	}
	// The reaper finds the leftover share and tries again; it must not restart a worker for it.
	sh.mu.Lock()
	sh.seen = time.Now().Add(-2 * lease)
	sh.mu.Unlock()
	if err := s.expire(context.Background()); err == nil {
		t.Fatal("a mount that could not be detached was reported as cleaned up")
	}
	if restarts.Load() != 0 {
		t.Fatalf("an expiry pass restarted a signed-out tenant's worker %d times", restarts.Load())
	}
}
