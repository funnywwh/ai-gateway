package browsermount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// close is the normal (HTTP/expiry) lifecycle: disconnect/exclude, replace the
// binding worker, then unmount. lifecycle serializes activation and all retries.
// No Service/share mutex is held across callbacks. The lifecycle mutex IS held:
// restart callbacks must never call DropTenant (manager.Restart does not).
func (s *Service) close(sh *share) error {
	return s.closeContext(context.Background(), sh, false, false)
}

// closeContext tears one mount down. purge releases a stable mount point for good (the
// operator removed the folder); without it a share mounted with a stable directory key keeps
// its mount point, because that empty directory IS the virtual path DSH's workspace entry
// (and the sessions grouped under it) points at, and the next mount of the same key reuses
// it. A share without a stable key behaves exactly as before: its mount point is per-mount
// scratch and is always removed.
func (s *Service) closeContext(ctx context.Context, sh *share, workerStopped, purge bool) error {
	sh.disconnect()
	sh.lifecycle.Lock()
	defer sh.lifecycle.Unlock()
	if sh.cleaned {
		return nil
	}
	s.mu.Lock()
	stopping := s.stopping
	s.mu.Unlock()
	// Once Quiesce begins, only the AFTER-stop owner may do cleanup. This also
	// prevents late edge HTTP handlers from restarting workers after shutdown.
	if stopping && !workerStopped {
		return errors.New("service stopping; cleanup deferred to shutdown")
	}
	sh.mu.Lock()
	published, final := sh.published, sh.final
	sh.mu.Unlock()
	if !sh.detached && published {
		// A share the caller marked final (logout, tenant drop, shutdown) has no worker left to
		// release a namespace for: restarting one would bring a signed-out account's dsh back to
		// life just to unmount its mount. The forced detach below does not need the namespace
		// gone, so the mount is taken out of the table either way.
		if !workerStopped && !final {
			if s.restart == nil {
				return errors.New("worker teardown unavailable; mount retained")
			}
			rctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := s.restart(rctx, sh.tenant)
			cancel()
			if err != nil {
				return fmt.Errorf("worker restart before close: %w", err)
			}
		}
		sh.detached = true
	}
	return s.cleanupLocked(sh, purge)
}

// gracefulUnmountBudget bounds the graceful unmount attempt (M76).
//
// go-fuse's Server.Unmount runs `fusermount3 -u` and then WAITS FOR ITS SERVE LOOP, and the serve
// loop ends only when the kernel releases the FUSE connection — which is exactly what does not
// happen while something else holds the mount. On a mount that refuses to detach, then, Unmount
// does not fail: it never returns. Measured on the gateway host (2026-09-22): a logout request and
// the whole reaper sat in go-fuse's WaitGroup.Wait with a worker still running and two mounts
// attached, and the person's 退出 never came back. The bound is what turns that into an error the
// forced ladder can act on; the abandoned attempt's goroutine returns by itself once the connection
// is released (the abort in the ladder is what releases it).
const gracefulUnmountBudget = time.Second

// errUnmountSlow reports a graceful unmount that neither succeeded nor failed inside its budget.
var errUnmountSlow = errors.New("unmount did not finish in time")

// unmountGracefully runs Mounted.Unmount under a deadline, because it can block forever.
func unmountGracefully(mounted Mounted, budget time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- mounted.Unmount() }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errUnmountSlow
	}
}

// cleanupLocked requires lifecycle and a disconnected share with no namespace
// references. A failed unmount, directory removal or record removal stays
// retryable; only fully successful cleanup creates a tombstone.
func (s *Service) cleanupLocked(sh *share, purge bool) error {
	if sh.cleaned {
		return nil
	}
	// Closing the share is what makes it final: serving() goes false, a resume is refused,
	// and every waiter on done is released. It belongs here, in the one place that runs on
	// every teardown path (HTTP close, grace expiry, tenant drop, shutdown), because a
	// disconnect alone no longer ends a mount — it only starts its grace window.
	sh.mu.Lock()
	if !sh.closed {
		sh.closed = true
		close(sh.done)
	}
	mounted, path, persistent := sh.mounted, sh.path, sh.persistent
	sh.mu.Unlock()
	if mounted != nil {
		err := unmountGracefully(mounted, gracefulUnmountBudget)
		if err != nil {
			// The graceful path is go-fuse's Server.Unmount, which runs `fusermount3 -u`. It
			// cannot take a mount something else holds — a sandbox that outlived its worker, or
			// a foreign mount namespace that received a copy by propagation (measured on the
			// deployment host: a snap's private namespace). Retrying that call is what the
			// reaper did for an hour on 2026-09-22, so escalate once to the forced ladder:
			// fusermount's lazy flag, then aborting the FUSE connection, then one last attempt.
			if forceErr := s.forceDetach(path); forceErr != nil {
				return fmt.Errorf("unmount %s: %w (forced detach: %v)", path, err, forceErr)
			}
			if errors.Is(err, errUnmountSlow) {
				slog.Warn("a browser mount did not unmount in time; it was force-detached",
					"mountpoint", path, "budget", gracefulUnmountBudget.String(),
					"detail", "go-fuse's Unmount waits for its serve loop, which ends only when the kernel releases the connection")
			}
		}
		sh.mu.Lock()
		sh.mounted = nil
		sh.mu.Unlock()
	}
	// A stable mount point is the virtual path of a LOCAL directory: unmounting leaves it
	// empty and reusable, and removing it would break the workspace entry that maps to it (a
	// missing path also drops that workspace's sessions from DSH's membership view). Only an
	// explicit purge, or a share that never had a stable key, releases the directory.
	if path != "" && (!persistent || purge) {
		if err := removeAbsentOK(path); err != nil {
			return fmt.Errorf("remove mount directory: %w", err)
		}
	}
	if s.recordDir != "" {
		if err := removeAbsentOK(s.recordPath(sh.tenant.Name, sh.id)); err != nil {
			return fmt.Errorf("remove mount record: %w", err)
		}
	}
	sh.cleaned = true
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.shares, sh.token)
	now := time.Now()
	for token, ts := range s.tombstones {
		if !now.Before(ts.until) {
			delete(s.tombstones, token)
		}
	}
	// Evict the oldest unexpired entry too: TTL alone is not a memory bound.
	for len(s.tombstones) >= 256 {
		var oldest string
		var until time.Time
		for token, ts := range s.tombstones {
			if oldest == "" || ts.until.Before(until) {
				oldest, until = token, ts.until
			}
		}
		delete(s.tombstones, oldest)
	}
	s.tombstones[sh.token] = tombstone{owner: sh.owner, tenant: sh.tenant.Name, until: now.Add(2 * time.Minute), path: path, persistent: persistent}
	return nil
}
func removeAbsentOK(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (s *Service) snapshot(tenant string) []*share {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []*share
	for _, sh := range s.shares {
		if tenant == "" || sh.tenant.Name == tenant {
			all = append(all, sh)
		}
	}
	return all
}

// DropTenant is the manager's AFTER-stop hook. Disconnect first, then wait for
// in-flight activations without holding any other lock. A configured raw Stop
// then releases namespaces created by an activation racing the manager's first
// stop. No locks are held across Stop: never use Manager.StopWorker here, since
// it reenters DropTenant. Subsequent worker starts must compute MountsFor inside
// their serialized start operation, so excluded paths cannot be rebound.
// Without SetStopWorker, the caller must itself guarantee stopped namespaces and
// no in-flight starts (useful for standalone mounts/tests only).
func (s *Service) DropTenant(ctx context.Context, tenant string) error {
	return s.teardownShares(ctx, s.snapshot(tenant), true)
}

// DetachTenant is the logout half of DropTenant (M76): every mount this account owns is
// excluded from worker profiles and force-detached, but no worker is started or stopped here —
// the logout path detaches first and stops the tenant's dsh LAST, which is what makes a dsh
// parked in a FUSE request die quickly instead of burning its whole stop timeout. A share that
// cannot be detached stays retryable and is reported, never restarted into existence.
//
// It is idempotent: a tenant with no mounts detaches nothing and returns nil.
func (s *Service) DetachTenant(ctx context.Context, tenant string) error {
	return s.teardownShares(ctx, s.snapshot(tenant), false)
}

// teardownShares is the one teardown sequence: mark every share final, disconnect them, wait
// out in-flight activations, optionally stop the binding workers, then close each share (which
// unmounts, forcing its way through a mount the graceful path cannot take).
//
// A failure to stop a binding worker no longer skips the cleanup: the forced detach does not
// need that namespace to be gone, and leaving mounts behind because an unrelated step failed is
// exactly the behaviour M76 replaces (the pre-M76 code returned early, so a failed worker stop
// left every mount mounted).
func (s *Service) teardownShares(ctx context.Context, all []*share, stopBindingWorkers bool) error {
	tenants := make(map[string]registry.Tenant)
	for _, sh := range all {
		sh.mu.Lock()
		// final: this share's binding worker is gone for good, so a later reaper pass retries the
		// detach but never restarts a worker to release a namespace for it.
		sh.final = true
		sh.mu.Unlock()
		sh.disconnect()
		tenants[sh.tenant.Name] = sh.tenant
	}
	for _, sh := range all {
		sh.lifecycle.Lock()
		sh.lifecycle.Unlock()
	}
	var errs []error
	if stopBindingWorkers && s.stopWorker != nil {
		for _, t := range tenants {
			stopCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := s.stopWorker(stopCtx, t)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("stop worker before drop: %w", err))
			}
		}
	}
	for _, sh := range all {
		if err := s.closeContext(ctx, sh, true, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Quiesce rejects new mounts, wakes filesystem operations and waits for all
// previously admitted activation/close callbacks. It does NOT stop workers or
// unmount, and must not be called from a restart callback. No service mutex is
// held while waiting: callback -> MountsFor and manager hooks can still run.
// Owner sequence: cancel/join Run; Quiesce; stop/wait workers; Shutdown.
func (s *Service) Quiesce() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	all := s.snapshot("")
	for _, sh := range all {
		sh.disconnect()
	}
	for _, sh := range all {
		sh.lifecycle.Lock()
		sh.lifecycle.Unlock()
	}
}

// Shutdown requires all worker namespaces already stopped, including concurrent
// StartWorkers/admin operations. It never restarts workers. On failure records
// and the state lock remain for retry; success releases the instance lock.
func (s *Service) Shutdown(ctx context.Context) error {
	s.Quiesce()
	if err := s.DropTenant(ctx, ""); err != nil {
		return err
	}
	return s.ReleaseLock()
}

// expire is the reaper. It closes two kinds of share:
//
//   - a share whose browser went away and did not come back within reconnectGrace: the
//     reload path. The window is what lets a page refresh resume the SAME mount instead of
//     paying a worker restart and a new mount point for a 300ms reload;
//   - a share that never disconnected but stopped polling past its lease: a wedged or
//     silently dead browser side.
//
// A share inside its grace window is left alone on purpose: it holds its kernel mount and
// mount point so the returning page can keep the same path, and it is already excluded from
// every worker profile (see serving), so waiting costs nothing but the record.
func (s *Service) expire(ctx context.Context) error {
	var errs []error
	for _, sh := range s.snapshot("") {
		sh.mu.Lock()
		disconnected := !sh.disconnectedAt.IsZero()
		waiting := sh.awaitingResume()
		// A share that lost its page is reaped when its grace window runs out; one that
		// still claims a browser is reaped when that browser stops polling past its lease.
		// Testing the lease for a disconnected share would stretch its lifetime to
		// grace+lease and delay the worker restart by up to a minute.
		expired := sh.closed || (!waiting && (disconnected || time.Since(sh.seen) > lease))
		sh.mu.Unlock()
		if expired {
			if err := s.closeContext(ctx, sh, false, false); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Run only reaps leases. Cancellation stops reaping, NOT cleanup: the owner must
// explicitly stop binding workers and call Shutdown, and handle its error.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.expire(ctx); err != nil {
				slog.Warn("browser mount expiry cleanup failed; will retry", "err", err)
			}
		}
	}
}
