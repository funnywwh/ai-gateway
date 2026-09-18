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
func (s *Service) close(sh *share) error { return s.closeContext(context.Background(), sh, false) }
func (s *Service) closeContext(ctx context.Context, sh *share, workerStopped bool) error {
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
	return s.cleanupLocked(sh)
}

// cleanupLocked requires lifecycle and a disconnected share with no namespace
// references. A failed unmount, directory removal or record removal stays
// retryable; only fully successful cleanup creates a tombstone.
func (s *Service) cleanupLocked(sh *share) error {
	if sh.cleaned {
		return nil
	}
	sh.mu.Lock()
	mounted, path := sh.mounted, sh.path
	sh.mu.Unlock()
	if mounted != nil {
		if err := mounted.Unmount(); err != nil {
			return fmt.Errorf("unmount %s: %w", path, err)
		}
		sh.mu.Lock()
		sh.mounted = nil
		sh.mu.Unlock()
	}
	if path != "" {
		if err := removeAbsentOK(path); err != nil {
			return fmt.Errorf("remove mount directory: %w", err)
		}
	}
	if s.recordDir != "" {
		if err := removeAbsentOK(s.recordPath(sh.id)); err != nil {
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
	s.tombstones[sh.token] = tombstone{owner: sh.owner, tenant: sh.tenant.Name, until: now.Add(2 * time.Minute)}
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
		if err := s.closeContext(ctx, sh, true); err != nil {
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
func (s *Service) expire(ctx context.Context) error {
	var errs []error
	for _, sh := range s.snapshot("") {
		sh.mu.Lock()
		expired := sh.closed || time.Since(sh.seen) > lease
		if expired && !sh.closed {
			sh.closed = true
			close(sh.done)
		}
		sh.mu.Unlock()
		if expired {
			if err := s.closeContext(ctx, sh, false); err != nil {
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
