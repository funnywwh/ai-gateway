package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

// PrepareLogin is the login moment's lifecycle hook (M69), called once per successful sign-in
// by the portal — the key form and the Feishu ticket form alike.
//
// What it does, in order:
//
//  1. a tenant that has no key at all is configured from the key this login proved valid
//     (AdoptKey; a tenant that already has one keeps it — login never rotates);
//  2. the platform slice of the tenant's dsh configuration is re-applied from aigw, using the
//     tenant's OWN worker key: dsh calls aigw with that key, so its grants decide the model
//     list, and `SyncModels` merges it into the file rather than replacing the file, leaving
//     every tenant-owned key (their own providers, theme, permission preset) untouched;
//  3. the platform credential reference is restored if the tenant's page removed it;
//  4. the account's ssh workspace mounts are re-mounted if it has records and they are detached
//     (M76) — signing out detaches them, so signing in has to put them back BEFORE the worker
//     starts: a worker's profile binds the mount points that exist when it starts;
//  5. the worker is brought up if it is not running — signing out stops it, so a sign-in has to
//     start it again before the browser is redirected to it.
//
// Errors are returned rather than swallowed so the caller can log them; the proxy's policy is
// that a failure here never blocks an already-authenticated login.
func (o managerOps) PrepareLogin(ctx context.Context, name, submittedKey string) error {
	t, ok := o.m.Registry.Get(name)
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	if submittedKey != "" {
		adopted, err := o.AdoptKey(ctx, name, submittedKey)
		if err != nil {
			return fmt.Errorf("adopting the login key: %w", err)
		}
		if adopted {
			o.log().Info("login key adopted for the tenant", "tenant", name)
		}
	}
	key, err := (proxy.FileKeySource{Root: o.cfg.Deploy.TenantConfigRoot}).Key(name)
	if err != nil {
		return fmt.Errorf("reading the tenant's stored key: %w", err)
	}
	models, err := o.validator.ValidateKey(ctx, key)
	if err != nil {
		return fmt.Errorf("aigw model list for %s: %w", name, err)
	}
	provisioned, err := o.m.EnsureProvisioned(ctx, t, key, models)
	if err != nil {
		return fmt.Errorf("provisioning %s at login: %w", name, err)
	}
	if provisioned {
		o.log().Info("tenant provisioned at login", "tenant", name, "models", len(models))
	}
	if err := o.m.SyncModels(t, models); err != nil {
		return fmt.Errorf("applying the model list for %s: %w", name, err)
	}
	restored, err := tenancy.EnsureCredentialRef(filepath.Join(t.DshHome, ".credentials.yaml"), tenancy.AIGWAPIKeyRef, key)
	if err != nil {
		return fmt.Errorf("restoring the platform credential for %s: %w", name, err)
	}
	// The mounts come back BEFORE the worker starts (M76): a worker's profile binds the mount
	// points that exist when it starts, so restoring afterwards would leave the account looking
	// at an empty mount point until its next restart. Signing out detached them; signing in puts
	// them back without another click. A failure is reported, never fatal — an unreachable remote
	// must not cost the person their login — so the caller logs and audits it.
	mountsRestored, mountsDeferred := 0, false
	if o.m.SSHWorkspaces != nil {
		before := len(o.m.SSHWorkspaces.AttachedMounts(name))
		running := false
		if state, statusErr := o.m.Status(ctx, t); statusErr == nil {
			running = state.Running
		}
		restoreCtx, cancel := context.WithTimeout(ctx, sshMountRestoreBudget)
		err := o.m.SSHWorkspaces.Restore(restoreCtx, t.Name, t.Workspace, t.DshHome)
		cancel()
		if err != nil {
			o.auditLogin(name, "login_mount_restore_failed", err.Error())
			o.log().Warn("restoring the account's ssh workspace mounts at login failed", "tenant", name, "err", err)
		} else if after := len(o.m.SSHWorkspaces.AttachedMounts(name)); after > before {
			mountsRestored = after - before
			if running {
				// A worker that is already running keeps the profile it started with, and
				// restarting it here would kill whatever turn the person is in the middle of
				// (M69 D4). The mount is visible from its next start on, or immediately if the
				// account reconnects the folder in the SSH workspace panel.
				mountsDeferred = true
				o.auditLogin(name, "ssh_mount_restore_deferred", "worker already running")
				o.log().Warn("the restored ssh workspace mounts are not visible in the running worker's sandbox yet",
					"tenant", name, "mounts", mountsRestored, "detail", "they appear at its next start, or reconnect the folder in the panel")
			}
		}
	}
	started, err := o.m.EnsureRunning(ctx, t)
	if err != nil {
		return fmt.Errorf("starting %s for its login: %w", name, err)
	}
	o.log().Info("tenant prepared for login",
		"tenant", name, "models", len(models), "credential_restored", restored,
		"mounts_restored", mountsRestored, "mounts_deferred", mountsDeferred, "worker_started", started)
	return nil
}

// sshMountRestoreBudget bounds the login-time remount of an account's ssh workspaces. It is part
// of the login budget, so it must not be able to hold the person at the portal: a remote host that
// does not answer costs one mount, not the login.
const sshMountRestoreBudget = 15 * time.Second

// auditLogin records a login-time mount outcome on the same audit stream as the rest of the
// lifecycle, so "the mount did not come back" is answerable afterwards.
func (o managerOps) auditLogin(tenant, kind, detail string) {
	if o.auditor == nil {
		return
	}
	_ = o.auditor.Write(audit.Event{Kind: kind, Tenant: tenant, Reason: detail, Status: http.StatusFound})
}

// StopSignedOut tears a tenant's dsh down because its last session signed out (M69, sequenced by
// M76): its mounts are force-detached FIRST and its dsh is force-stopped LAST.
//
// The decision that nobody is left in the tenant belongs to the proxy, which owns the session
// store; this is only the "how", without recording an operator suspension. It is idempotent — a
// tenant whose worker is not running and has no mounts tears down as a no-op — and it reports
// what was left behind instead of pretending the teardown was complete.
func (o managerOps) StopSignedOut(ctx context.Context, name string) (proxy.LogoutResult, error) {
	t, ok := o.m.Registry.Get(name)
	if !ok {
		return proxy.LogoutResult{}, fmt.Errorf("tenant %q not found", name)
	}
	result, err := o.m.StopForLogout(ctx, t)
	mapped := proxy.LogoutResult{
		MountsDetached: result.MountsDetached,
		MountsLeftover: result.MountsLeftover,
		WorkerStopped:  result.WorkerStopped,
	}
	if err != nil {
		return mapped, fmt.Errorf("stopping %s: %w", name, err)
	}
	return mapped, nil
}
