package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/contract"
	"github.com/winger/ai-gateway/internal/dshgw/proxy"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (c *cli) bind(args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if len(args) != 2 {
		return errors.New("usage: dshgw bind PREFIX TENANT")
	}
	prefix, err := cleanPrefix(args[0])
	if err != nil {
		return err
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if _, err := tenantByName(deps.reg, args[1]); err != nil {
		return err
	}
	if err := deps.manager.BindPrefix(args[1], prefix); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "bound prefix %s to tenant %s\n", prefix, args[1])
	return nil
}
func (c *cli) loginURL(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: dshgw login-url [PREFIX]")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(c.stdout, deps.cfg.WithTrailingSlash(deps.cfg.OriginForPort(deps.cfg.PortalPort)))
		return nil
	}
	prefix, err := cleanPrefix(args[0])
	if err != nil {
		return err
	}
	tenant, ok := deps.reg.ByPrefix(prefix)
	if !ok {
		return errors.New("prefix is not bound")
	}
	fmt.Fprintln(c.stdout, deps.cfg.WithTrailingSlash(deps.cfg.TenantOrigin(tenant.Name)))
	return nil
}
func (c *cli) syncModels(ctx context.Context, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: dshgw sync-models TENANT")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, args[0])
	if err != nil {
		return err
	}
	key, err := (proxy.FileKeySource{Root: deps.cfg.Deploy.TenantConfigRoot}).Key(tenant.Name)
	if err != nil {
		return err
	}
	models, err := validateKey(ctx, deps.validator, key)
	if err != nil {
		return err
	}
	if err := deps.manager.SyncModels(tenant, models); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "synced %d models for tenant %s\n", len(models), tenant.Name)
	return nil
}
func (c *cli) revalidate(ctx context.Context, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if len(args) > 1 {
		return errors.New("usage: dshgw revalidate [TENANT]")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenants := deps.reg.List()
	if len(args) == 1 {
		tenant, err := tenantByName(deps.reg, args[0])
		if err != nil {
			return err
		}
		tenants = []registry.Tenant{tenant}
	}
	source := proxy.FileKeySource{Root: deps.cfg.Deploy.TenantConfigRoot}
	failures := 0
	for _, tenant := range tenants {
		key, keyErr := source.Key(tenant.Name)
		if keyErr == nil {
			_, keyErr = validateKey(ctx, deps.validator, key)
		}
		if keyErr != nil {
			failures++
			fmt.Fprintf(c.stdout, "%s\tFAIL\t%s\n", tenant.Name, redactError(keyErr))
		} else {
			fmt.Fprintf(c.stdout, "%s\tOK\n", tenant.Name)
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d tenant key(s) failed revalidation", failures)
	}
	return nil
}
func (c *cli) captureURL(ctx context.Context, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: dshgw capture-url TENANT [MAINPID]")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, args[0])
	if err != nil {
		return err
	}
	return deps.manager.CaptureURL(ctx, tenant, func() string {
		if len(args) == 2 {
			return args[1]
		}
		return ""
	}())
}
func (c *cli) contract(ctx context.Context, args []string) error {
	fs := c.flagSet("contract")
	asJSON := fs.Bool("json", false, "emit JSON report")
	keyFile := fs.String("key-file", "", "optionally validate this aigw key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mode := "all"
	if fs.NArg() == 1 {
		mode = fs.Arg(0)
	} else if fs.NArg() > 1 {
		return errors.New("usage: dshgw contract [--json] [--key-file PATH] [all|dsh|aigw]")
	}
	if mode != "all" && mode != "dsh" && mode != "aigw" {
		return fmt.Errorf("unknown contract mode %q", mode)
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	var checks []contract.Check
	if mode == "all" || mode == "dsh" {
		checks = append(checks, contract.DshChecks(deps.cfg.Dsh)...)
	}
	key := ""
	if *keyFile != "" {
		key, err = readKey(strings.NewReader(""), *keyFile)
		if err != nil {
			return err
		}
	}
	if mode == "all" || mode == "aigw" {
		checks = append(checks, contract.AigwChecks(deps.cfg.AigwBaseURL, key)...)
	}
	report := contract.Run(ctx, checks)
	if *asJSON {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
	} else {
		for _, result := range report.Results {
			status := "PASS"
			if result.Skipped {
				status = "SKIP"
			} else if !result.OK {
				status = "FAIL"
			}
			fmt.Fprintf(c.stdout, "%-4s %-40s %s", status, result.Name, result.Duration.Round(time.Millisecond))
			if result.Error != "" {
				fmt.Fprintf(c.stdout, ": %s", result.Error)
			}
			fmt.Fprintln(c.stdout)
		}
	}
	if !report.OK {
		return errors.New("one or more required contract checks failed")
	}
	return nil
}
func (c *cli) backup(ctx context.Context, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if len(args) != 0 {
		return errors.New("usage: dshgw backup")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	path, err := deps.manager.Backup(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, path)
	return nil
}
func (c *cli) renderNginx(ctx context.Context, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	fs := c.flagSet("render-nginx")
	reload := fs.Bool("reload", false, "reload nginx after successful validation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: dshgw render-nginx [--reload]")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if err := deps.manager.ReconcileNginx(ctx, *reload); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "rendered %d tenant nginx server(s)\n", len(deps.reg.List()))
	return nil
}
func (c *cli) doctor(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dshgw doctor")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	type check struct {
		name string
		run  func() error
	}
	needBrowserFS := deps.cfg.PluginBrowserFS == "on"
	bwrapActive := deps.cfg.Deploy.Isolation == config.IsolationBwrap
	for _, tenant := range deps.reg.List() {
		needBrowserFS = needBrowserFS || tenant.PluginBrowserFS == "on"
		bwrapActive = bwrapActive || tenant.EffectiveIsolation() == registry.IsolationBwrap
	}
	checks := []check{
		{"shared-state-traversal", func() error { return checkSharedTraversal(deps.cfg.StateDir) }},
		{"tenant-root-traversal", func() error { return checkSharedTraversal(deps.cfg.TenantRoot) }},
		{"workspace-root-traversal", func() error { return checkSharedTraversal(deps.cfg.WorkspaceRoot) }},
		{"config-mode", func() error { return securefile.CheckPermissions(c.configPath, 0o640) }},
		{"config-owner", func() error { return securefile.CheckOwnership(c.configPath, "root", deps.cfg.Deploy.GatewayGroup) }},
		{"registry-mode", func() error { return securefile.CheckPermissions(deps.cfg.RegistryPath, 0o600) }},
		{"registry-owner", func() error {
			return securefile.CheckOwnership(deps.cfg.RegistryPath, deps.cfg.Deploy.GatewayUser, deps.cfg.Deploy.GatewayGroup)
		}},
		{"key-map-mode", func() error { return securefile.CheckPermissions(deps.cfg.KeyMapPath, 0o640) }},
		{"key-map-owner", func() error {
			return securefile.CheckOwnership(deps.cfg.KeyMapPath, "root", deps.cfg.Deploy.GatewayGroup)
		}},
		{"handshake-dir-mode", func() error { return checkDirectoryPermissions(deps.cfg.HandshakeDir, 0o750) }},
		{"handshake-dir-owner", func() error {
			return securefile.CheckOwnership(deps.cfg.HandshakeDir, "root", deps.cfg.Deploy.GatewayGroup)
		}},
		{"dsh-node", func() error { return executable(deps.cfg.Dsh.NodeBin) }},
		{"dsh-bin-js", func() error { _, err := os.Stat(deps.cfg.Dsh.BinJS); return err }},
		{"dsh-current-symlink", func() error {
			info, err := os.Lstat(deps.cfg.Dsh.CurrentLink)
			if err == nil && info.Mode()&os.ModeSymlink == 0 {
				return errors.New("current_link is not a symlink")
			}
			return err
		}},
		{"dsh-template", func() error { return tenancy.ValidateTemplate(deps.cfg.Deploy.TemplateHome, needBrowserFS) }},
		{"picker-plugin", func() error { _, err := os.Stat(deps.cfg.Deploy.PluginPath); return err }},
		{"tls-certificate", func() error { _, err := os.Stat(deps.cfg.TLS.Certificate); return err }},
		{"tls-key", func() error { _, err := os.Stat(deps.cfg.TLS.CertificateKey); return err }},
		{"worker-unit", func() error {
			_, err := os.Stat(filepath.Join(deps.cfg.Deploy.SystemdDir, deps.cfg.Deploy.WorkerUnit))
			return err
		}},
		{"gateway-unit", func() error {
			_, err := os.Stat(filepath.Join(deps.cfg.Deploy.SystemdDir, deps.cfg.Deploy.GatewayUnit))
			return err
		}},
		{"worker-slice", func() error {
			_, err := os.Stat(filepath.Join(deps.cfg.Deploy.SystemdDir, deps.cfg.Deploy.WorkerSlice))
			return err
		}},
		{"nginx-include", func() error { return securefile.CheckPermissions(deps.cfg.Deploy.NginxIncludePath, 0o644) }},
		{"nginx-config", func() error {
			_, err := tenancy.ExecRunner{}.Run(ctx, tenancy.Command{Path: deps.cfg.Deploy.NginxBinary, Args: []string{"-t"}})
			return err
		}},
	}
	if bwrapActive {
		// The bwrap isolation mode's preconditions. They are only checked when
		// the mode is actually in use, so a per-tenant-account deployment is
		// never blocked by requirements it does not have.
		checks = append(checks,
			check{"bwrap-bin", func() error { return executable(deps.cfg.Deploy.BwrapBin) }},
			check{"bwrap-apparmor-userns", checkAppArmorUserNSRestriction},
			check{"bwrap-sandbox-runtime", func() error { return sandbox.ValidateRuntime(sandboxRuntimeConfig(deps.cfg)) }},
			check{"bwrap-worker-account", func() error { return sandbox.ValidateWorkerAccount(deps.cfg.Deploy.WorkerUser) }},
			check{"worker-unit-bwrap", func() error { return checkBwrapWorkerUnit(deps.cfg) }},
		)
	}
	failures := 0
	for _, item := range checks {
		err := item.run()
		if err != nil {
			failures++
			fmt.Fprintf(c.stdout, "FAIL\t%s\t%s\n", item.name, err)
		} else {
			fmt.Fprintf(c.stdout, "OK\t%s\n", item.name)
		}
	}
	for _, tenant := range deps.reg.List() {
		source := proxy.FileKeySource{Root: deps.cfg.Deploy.TenantConfigRoot}
		tenantChecks := []check{
			{"tenant-" + tenant.Name + "-key", func() error { _, err := source.Key(tenant.Name); return err }},
			{"tenant-" + tenant.Name + "-handshake", func() error {
				return securefile.CheckPermissions(filepath.Join(deps.cfg.HandshakeDir, tenant.Name+".url"), 0o640)
			}},
			{"tenant-" + tenant.Name + "-credentials", func() error {
				return securefile.CheckPermissions(filepath.Join(tenant.DshHome, ".credentials.yaml"), 0o600)
			}},
			{"tenant-" + tenant.Name + "-settings", func() error {
				return securefile.CheckPermissions(filepath.Join(tenant.DshHome, "settings.yaml"), 0o600)
			}},
		}
		if tenant.EffectiveIsolation() == registry.IsolationBwrap {
			// Proves the tenant's own profile still builds and that its roots
			// grant nothing to an unrelated user: in shared-account mode those
			// permission bits are the second line of defence behind the mounts.
			tenantChecks = append(tenantChecks, check{"tenant-" + tenant.Name + "-sandbox", func() error {
				return deps.manager.SandboxProfileReady(tenant)
			}})
		}
		for _, item := range tenantChecks {
			if err := item.run(); err != nil {
				failures++
				fmt.Fprintf(c.stdout, "FAIL\t%s\t%s\n", item.name, err)
			} else {
				fmt.Fprintf(c.stdout, "OK\t%s\n", item.name)
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("doctor found %d failure(s)", failures)
	}
	return nil
}
func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

func checkDirectoryPermissions(path string, allowed os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if extra := info.Mode().Perm() &^ allowed.Perm(); extra != 0 {
		return fmt.Errorf("%s mode %04o is broader than %04o", path, info.Mode().Perm(), allowed.Perm())
	}
	return nil
}

// Shared roots must allow an unrelated worker UID to search them, without
// allowing group/other writes. Private tenant leaves and secret files remain
// protected independently; this check never requires gateway group membership.
func checkSharedTraversal(path string) error {
	if err := checkDirectoryPermissions(path, 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("%s lacks search permission for independent tenant UIDs; shared state needs 0751 and tenant root 0711", path)
	}
	return nil
}
