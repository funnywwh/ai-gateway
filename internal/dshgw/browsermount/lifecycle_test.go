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

func TestPartialRestartIsRetriedAndCloseRebuildsWorker(t *testing.T) {
	var calls atomic.Int32
	s := NewWithMount(func(context.Context, registry.Tenant) error {
		if calls.Add(1) == 1 {
			return errors.New("partial restart failure")
		}
		return nil
	}, func(string, fs.Backend) (Mounted, error) { return &fakeMount{}, nil })
	tenant := registry.Tenant{Name: "a", Workspace: t.TempDir()}
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DropTenant(context.Background(), tenant.Name)
	activate := func() error {
		done := make(chan error, 1)
		go func() {
			_, err := s.dispatch(context.Background(), "activate", tenant, "owner", payload{Token: sh.token})
			done <- err
		}()
		select {
		case req := <-sh.queue:
			_, err := s.dispatch(context.Background(), "respond", tenant, "owner", payload{Token: sh.token, ID: req.ID, Result: fs.Response{OK: true, Value: fs.Value{Kind: "directory"}}})
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("activate did not validate browser root")
		}
		return <-done
	}
	if err := activate(); err == nil {
		t.Fatal("restart failure hidden")
	}
	if sh.active || !sh.restartAttempted {
		t.Fatal("partial restart flags", sh.active, sh.restartAttempted)
	}
	if err := activate(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("restart not retried")
	}
	if _, err := s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: sh.token}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("close must remove sandbox binding")
	}
}

func TestDropTenantDoesNotRestart(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	sh.active = true
	sh.restartAttempted = true
	s.restart = func(context.Context, registry.Tenant) error { t.Error("lifecycle drop restarted worker"); return nil }
	if err := s.DropTenant(context.Background(), tenant.Name); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseCannotBeRenewedAfterExpiration(t *testing.T) {
	s, tenant, _ := fixture(t)
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	sh.seen = time.Now().Add(-2 * lease)
	sh.mu.Unlock()
	if _, err := s.dispatch(context.Background(), "heartbeat", tenant, "owner", payload{Token: sh.token}); err == nil {
		t.Fatal("expired capability renewed")
	}
}
