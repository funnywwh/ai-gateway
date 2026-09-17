package tenancy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// Reisolate moves one existing tenant between the two confinement modes —
// per-tenant OS account ("user") and the shared-account bubblewrap mount
// namespace ("bwrap") — without changing its data, ports, key material, or
// public routing.
//
// The migration is deliberate about what it does NOT do: the account left
// behind by a user→bwrap migration is never deleted (deleting an OS identity is
// irreversible and buys nothing once no unit references it), and a
// bwrap→user migration creates the tenant's account fresh because a shared UID
// cannot be handed back. Ownership of the tenant's writable roots always moves
// to the target mode's account, or the worker cannot write after the switch.
func (m *Manager) Reisolate(ctx context.Context, t registry.Tenant, target string) (err error) {
	if target != registry.IsolationUser && target != registry.IsolationBwrap {
		return fmt.Errorf("isolation target must be %q or %q", registry.IsolationUser, registry.IsolationBwrap)
	}
	return m.WithLifecycleLock(func() error {
		current, ok := m.Registry.Get(t.Name)
		if !ok {
			return fmt.Errorf("tenant %q not found", t.Name)
		}
		if err := m.validateTenantPaths(current); err != nil {
			return err
		}
		if current.EffectiveIsolation() == target {
			return fmt.Errorf("tenant %s already runs in %q isolation", t.Name, target)
		}
		return m.reisolateLocked(ctx, current, target)
	})
}

func (m *Manager) reisolateLocked(ctx context.Context, t registry.Tenant, target string) (err error) {
	// The previous unit and the target unit are resolved up front: the registry
	// switches mid-flight, so unit() would answer differently on either side of
	// that line.
	unitName := m.unit(t.Name)
	newUnitName := m.unitForIsolation(t.Name, target)
	oldIsolation := t.EffectiveIsolation()
	status, err := m.Status(ctx, t)
	if err != nil {
		return err
	}
	started := false
	switchedRegistry := false
	ownerMoved := false
	userCreated := false
	defer func() {
		if err == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if started {
			if _, stopErr := m.run(rollbackCtx, "systemctl", "stop", newUnitName); stopErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback stop target worker: %w", stopErr))
			}
		}
		if ownerMoved {
			owner := m.ownerForIsolation(t.Name, oldIsolation)
			if _, chownErr := m.run(rollbackCtx, "chown", "-R", owner+":"+owner, filepath.Join(m.Config.TenantRoot, t.Name), t.Workspace); chownErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback ownership: %w", chownErr))
			}
		}
		if switchedRegistry {
			if putErr := m.Registry.Put(t); putErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback registry: %w", putErr))
			} else if saveErr := m.Registry.Save(); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback registry: %w", saveErr))
			}
		}
		if userCreated {
			if _, delErr := m.run(rollbackCtx, "userdel", m.user(t.Name)); delErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback user (data retained): %w", delErr))
			}
		}
		if status.Active {
			if _, startErr := m.run(rollbackCtx, "systemctl", "start", unitName); startErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback start previous worker: %w", startErr))
			}
		}
	}()

	// Preflight everything the target mode needs before the current worker is
	// stopped: a missing account, an unusable bwrap binary, or a missing unit
	// template must fail while the old worker is still serving. The checks run
	// against the target isolation, not the recorded one.
	targetTenant := t
	targetTenant.Isolation = target
	if target == registry.IsolationBwrap {
		if err = m.SandboxProfileReady(targetTenant); err != nil {
			return err
		}
	} else {
		if _, err = m.run(ctx, "useradd", "--system", "--user-group", "--no-create-home", "--home-dir", t.Workspace, "--shell", "/usr/sbin/nologin", m.user(t.Name)); err != nil {
			return err
		}
		userCreated = true
	}
	if _, err = m.run(ctx, "systemctl", "stop", unitName); err != nil {
		return err
	}
	owner := m.ownerForIsolation(t.Name, target)
	if _, err = m.run(ctx, "chown", "-R", owner+":"+owner, filepath.Join(m.Config.TenantRoot, t.Name), t.Workspace); err != nil {
		return err
	}
	ownerMoved = true
	updated := t
	updated.Isolation = target
	if err = m.Registry.Put(updated); err != nil {
		return err
	}
	switchedRegistry = true
	if err = m.Registry.Save(); err != nil {
		return err
	}
	if _, err = m.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if _, err = m.run(ctx, "systemctl", "start", newUnitName); err != nil {
		return err
	}
	started = true
	if err = m.ProbeWorker(ctx, updated); err != nil {
		return fmt.Errorf("worker readiness probe: %w", err)
	}
	if status.Enabled {
		if _, err = m.run(ctx, "systemctl", "enable", newUnitName); err != nil {
			return err
		}
	}
	// Nothing else to retire: both unit families are static templates expanded
	// by systemd (%i), so neither mode leaves a per-tenant unit file, and the
	// per-tenant account of a user→bwrap migration is deliberately left in
	// place — deleting an OS identity is irreversible and buys nothing once no
	// unit references it.
	return nil
}

// ownerForIsolation returns the account that owns a tenant's writable roots in
// the given isolation mode.
func (m *Manager) ownerForIsolation(name, isolation string) string {
	if isolation == registry.IsolationBwrap {
		return m.Config.Deploy.WorkerUser
	}
	return m.user(name)
}

// unitForIsolation expands the unit template of one isolation mode for a
// tenant, independent of the mode currently recorded in the registry.
func (m *Manager) unitForIsolation(name, isolation string) string {
	template := m.Config.Deploy.WorkerUnit
	if isolation == registry.IsolationBwrap {
		template = m.Config.Deploy.WorkerUnitBwrap
	}
	if template == "" {
		if isolation == registry.IsolationBwrap {
			template = "dsh-worker-bwrap@.service"
		} else {
			template = "dsh-worker@.service"
		}
	}
	return workerUnitNameFor(template, name)
}
