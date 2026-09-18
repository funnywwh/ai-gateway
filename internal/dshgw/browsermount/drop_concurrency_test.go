package browsermount

import (
	"context"
	"errors"
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

func TestDropStopFailureRetainsRetryableMount(t *testing.T) {
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
	if err := s.DropTenant(context.Background(), tenant.Name); err == nil {
		t.Fatal("stop error hidden")
	}
	m.mu.Lock()
	unmounted := m.closed
	m.mu.Unlock()
	if unmounted {
		t.Fatal("unmounted after failed stop")
	}
	if _, ok := s.shares[sh.token]; !ok {
		t.Fatal("failed drop forgotten")
	}
	if err := s.DropTenant(context.Background(), tenant.Name); err != nil {
		t.Fatal(err)
	}
}
