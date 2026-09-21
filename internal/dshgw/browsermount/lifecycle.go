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
	published := sh.published
	sh.mu.Unlock()
	if !sh.detached && published {
		if !workerStopped {
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
		if err := mounted.Unmount(); err != nil {
			return fmt.Errorf("unmount %s: %w", path, err)
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
	all := s.snapshot(tenant)
	tenants := make(map[string]registry.Tenant)
	for _, sh := range all {
		sh.disconnect()
		tenants[sh.tenant.Name] = sh.tenant
	}
	for _, sh := range all {
		sh.lifecycle.Lock()
		sh.lifecycle.Unlock()
	}
	if s.stopWorker != nil {
		for _, t := range tenants {
			stopCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := s.stopWorker(stopCtx, t)
			cancel()
			if err != nil {
				return fmt.Errorf("stop worker before drop: %w", err)
			}
		}
	}
	var errs []error
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
