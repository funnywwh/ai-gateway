package main

import (
	"context"
	"fmt"
	"path/filepath"

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
//  4. the worker is brought up if it is not running — signing out stops it, so a sign-in has to
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
	started, err := o.m.EnsureRunning(ctx, t)
	if err != nil {
		return fmt.Errorf("starting %s for its login: %w", name, err)
	}
	o.log().Info("tenant prepared for login",
		"tenant", name, "models", len(models), "credential_restored", restored, "worker_started", started)
	return nil
}

// StopSignedOut stops a tenant's dsh because its last session signed out (M69).
//
// The decision that nobody is left in the tenant belongs to the proxy, which owns the session
// store; this is only the "how": stop the process without recording an operator suspension. It
// is idempotent — a tenant whose worker is not running stops as a no-op.
func (o managerOps) StopSignedOut(ctx context.Context, name string) error {
	t, ok := o.m.Registry.Get(name)
	if !ok {
		return fmt.Errorf("tenant %q not found", name)
	}
	if err := o.m.StopForLogout(ctx, t); err != nil {
		return fmt.Errorf("stopping %s: %w", name, err)
	}
	return nil
}
