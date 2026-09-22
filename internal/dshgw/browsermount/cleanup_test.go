package browsermount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

type checkedMount struct{ unmount func() error }

func (m checkedMount) Unmount() error { return m.unmount() }

func awaitError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle deadlocked")
		return nil
	}
}

func TestAllClosePathsReleaseNamespaceBeforeUnmount(t *testing.T) {
	for _, mode := range []string{"http", "expiry", "shutdown", "drop"} {
		t.Run(mode, func(t *testing.T) {
			var bound atomic.Bool
			bound.Store(true)
			var unmounts, restarts atomic.Int32
			var s *Service
			tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
			s = NewWithMount(func(ctx context.Context, _ registry.Tenant) error {
				if len(s.MountsFor(tenant.Name)) != 0 {
					t.Error("closing mount still eligible for binding")
				}
				restarts.Add(1)
				bound.Store(false)
				return nil
			}, func(string, fs.Backend) (Mounted, error) {
				return checkedMount{func() error {
					if bound.Load() {
						return errors.New("namespace still bound")
					}
					unmounts.Add(1)
					return nil
				}}, nil
			})
			sh, err := s.open(tenant, "owner", "dir", true)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately never activate: startup or another activation can already bind it.
			pending := make(chan error, 1)
			go func() { _, err := sh.Call(context.Background(), fs.Request{Op: "stat"}); pending <- err }()
			<-sh.queue
			switch mode {
			case "http":
				_, err = s.dispatch(context.Background(), "close", tenant, "owner", payload{Token: sh.token})
			case "expiry":
				sh.mu.Lock()
				sh.seen = time.Now().Add(-2 * lease)
				sh.mu.Unlock()
				err = s.expire(context.Background())
			case "shutdown":
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { s.Run(ctx); done <- nil }()
				cancel()
				awaitError(t, done)
				if unmounts.Load() != 0 {
					t.Fatal("Run cancellation unmounted before workers stopped")
				}
				s.Quiesce()
				bound.Store(false)
				err = s.Shutdown(context.Background())
			case "drop":
				bound.Store(false)
				err = s.DropTenant(context.Background(), tenant.Name)
			}
			if err != nil {
				t.Fatal(err)
			}
			if awaitError(t, pending) == nil {
				t.Fatal("pending request not disconnected")
			}
			if unmounts.Load() != 1 {
				t.Fatal("wrong unmount count", unmounts.Load())
			}
			want := int32(1)
			if mode == "shutdown" || mode == "drop" {
				want = 0
			}
			if restarts.Load() != want {
				t.Fatal("wrong restart count", restarts.Load())
			}
		})
	}
}

func TestFailedCloseRetainsRecordAndChecksOwnerOnRetry(t *testing.T) {
	var calls, unmounts atomic.Int32
	s := NewWithState(func(context.Context, registry.Tenant) error {
		if calls.Add(1) == 1 {
			return errors.New("worker stop failed")
		}
		return nil
	}, func(string, fs.Backend) (Mounted, error) {
		return checkedMount{func() error { unmounts.Add(1); return nil }}, nil
	}, t.TempDir())
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	closeAs := func(owner string) error {
		_, err := s.dispatch(context.Background(), "close", tenant, owner, payload{Token: sh.token})
		return err
	}
	if closeAs("owner") == nil {
		t.Fatal("teardown error hidden")
	}
	if unmounts.Load() != 0 {
		t.Fatal("unmounted despite live namespace")
	}
	if _, err := os.Stat(s.recordPath(sh.tenant.Name, sh.id)); err != nil {
		t.Fatal("lost record", err)
	}
	if len(s.MountsFor(tenant.Name)) != 0 {
		t.Fatal("failed close re-exposed mount")
	}
	if closeAs("attacker") == nil {
		t.Fatal("closed retry owner bypass")
	}
	if calls.Load() != 1 {
		t.Fatal("attacker triggered callback")
	}
	// Force record removal failure without depending on running as root.
	if err := os.Remove(s.recordPath(sh.tenant.Name, sh.id)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.recordPath(sh.tenant.Name, sh.id), 0700); err != nil {
		t.Fatal(err)
	}
	obstruction := filepath.Join(s.recordPath(sh.tenant.Name, sh.id), "keep")
	if err := os.WriteFile(obstruction, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if closeAs("owner") == nil {
		t.Fatal("record removal failure hidden")
	}
	if _, ok := s.shares[sh.token]; !ok {
		t.Fatal("failed cleanup forgotten")
	}
	if _, ok := s.tombstones[sh.token]; ok {
		t.Fatal("false successful tombstone")
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	if err := closeAs("owner"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || unmounts.Load() != 1 {
		t.Fatal("retry repeated completed stages", calls.Load(), unmounts.Load())
	}
	if err := closeAs("owner"); err != nil {
		t.Fatal("owner tombstone retry", err)
	}
	if closeAs("attacker") == nil {
		t.Fatal("tombstone owner bypass")
	}
}

func TestTombstonesHaveHardCap(t *testing.T) {
	s, tenant, _ := fixture(t)
	for i := 0; i < 300; i++ {
		sh, err := s.open(tenant, "owner", "dir", true)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.close(sh); err != nil {
			t.Fatal(err)
		}
		if len(s.tombstones) > 256 {
			t.Fatal("tombstones unbounded", len(s.tombstones))
		}
	}
	if len(s.tombstones) != 256 {
		t.Fatal("unexpected cap", len(s.tombstones))
	}
}

func TestConcurrentCloseAndQuiesceDoNotRestartAfterStop(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var restarts, unmounts atomic.Int32
	var s *Service
	s = NewWithMount(func(context.Context, registry.Tenant) error {
		restarts.Add(1)
		s.MountsFor("alice") // callback reentrance must not deadlock on service.mu
		once.Do(func() { close(entered) })
		<-release
		return nil
	}, func(string, fs.Backend) (Mounted, error) {
		return checkedMount{func() error { unmounts.Add(1); return nil }}, nil
	})
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	closing := make(chan error, 1)
	go func() { closing <- s.close(sh) }()
	<-entered
	quiet := make(chan error, 1)
	go func() { s.Quiesce(); quiet <- nil }()
	select {
	case <-quiet:
		t.Fatal("Quiesce returned while callback active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := awaitError(t, closing); err != nil {
		t.Fatal(err)
	}
	awaitError(t, quiet)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.close(sh) }()
	}
	wg.Wait()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarts.Load() != 1 || unmounts.Load() != 1 {
		t.Fatal("duplicate lifecycle work", restarts.Load(), unmounts.Load())
	}
	if _, err := s.open(tenant, "owner", "late", true); err == nil {
		t.Fatal("open admitted during shutdown")
	}
}

func TestUnmountFailureRetriesWithoutRestart(t *testing.T) {
	var unmounts, restarts, forced atomic.Int32
	s := NewWithMount(func(context.Context, registry.Tenant) error { restarts.Add(1); return nil }, func(string, fs.Backend) (Mounted, error) {
		return checkedMount{func() error {
			if unmounts.Add(1) == 1 {
				return errors.New("busy")
			}
			return nil
		}}, nil
	})
	// The graceful unmount fails AND the forced ladder cannot take the mount either (M76): the
	// share stays retryable and the worker is not restarted for it.
	s.SetForceDetach(func(string) error { forced.Add(1); return errors.New("still busy") })
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	if s.close(sh) == nil {
		t.Fatal("unmount error hidden")
	}
	if _, ok := s.tombstones[sh.token]; ok {
		t.Fatal("false tombstone")
	}
	if forced.Load() != 1 {
		t.Fatalf("the forced detach ran %d times, want one escalation", forced.Load())
	}
	if err := s.close(sh); err != nil {
		t.Fatal(err)
	}
	if unmounts.Load() != 2 || restarts.Load() != 1 {
		t.Fatal("incorrect retry stages")
	}
	// The retry that succeeded must not escalate again.
	if forced.Load() != 1 {
		t.Fatalf("the forced detach ran %d times, want no escalation once the unmount worked", forced.Load())
	}
}

// The shape the deployment host produced (2026-09-22): the graceful unmount answers EBUSY
// because another mount namespace holds the mount, and the forced ladder is what takes it out
// of the table. Before M76 the same call was retried every five seconds, forever.
func TestUnmountFailureEscalatesToTheForcedDetach(t *testing.T) {
	var forced atomic.Int32
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	s := NewWithMount(func(context.Context, registry.Tenant) error { return nil }, func(string, fs.Backend) (Mounted, error) {
		return checkedMount{func() error { return errors.New("Device or resource busy") }}, nil
	})
	var unmounted atomic.Bool
	s.SetForceDetach(func(path string) error {
		forced.Add(1)
		unmounted.Store(true)
		return nil
	})
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.close(sh); err != nil {
		t.Fatalf("the forced detach did not finish the cleanup: %v", err)
	}
	if forced.Load() != 1 || !unmounted.Load() {
		t.Fatalf("forced detach ran %d times (unmounted=%v)", forced.Load(), unmounted.Load())
	}
	if _, ok := s.shares[sh.token]; ok {
		t.Fatal("a detached share is still tracked")
	}
	if _, err := os.Stat(s.recordPath(tenant.Name, sh.id)); !os.IsNotExist(err) {
		t.Fatalf("the record survived a forced detach: %v", err)
	}
}

// go-fuse's Unmount can block forever (it waits for its serve loop, which ends only when the
// kernel releases the connection). That is what wedged the logout request AND the reaper on
// 2026-09-22, and it is why the graceful attempt is bounded and the forced ladder runs anyway.
func TestABlockingUnmountIsBoundedAndForced(t *testing.T) {
	var forced atomic.Int32
	blocked := make(chan struct{})
	tenant := registry.Tenant{Name: "alice", Workspace: t.TempDir()}
	s := NewWithMount(func(context.Context, registry.Tenant) error { return nil }, func(string, fs.Backend) (Mounted, error) {
		return checkedMount{func() error {
			<-blocked // never returns while the test runs
			return nil
		}}, nil
	})
	s.SetForceDetach(func(string) error {
		forced.Add(1)
		close(blocked) // the abort is what releases the parked unmount
		return nil
	})
	sh, err := s.open(tenant, "owner", "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- s.close(sh) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the forced detach did not finish the cleanup: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the close hung on a blocking unmount instead of forcing its way out")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the bounded attempt took %s", elapsed)
	}
	if forced.Load() != 1 {
		t.Fatalf("forced detach ran %d times, want one", forced.Load())
	}
	if _, ok := s.shares[sh.token]; ok {
		t.Fatal("a detached share is still tracked")
	}
}
