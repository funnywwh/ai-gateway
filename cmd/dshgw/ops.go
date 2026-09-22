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
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (c *cli) bind(args []string) error {
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

// captureURL prints the startup URL a tenant worker reported. The systemd shape
// needed MAINPID plus a journalctl scan; the runner reads the URL from the worker
// process output and publishes it as the handshake file, so this is now a read of
// that state (useful when the handshake looks stale).
func (c *cli) captureURL(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: dshgw capture-url TENANT")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, args[0])
	if err != nil {
		return err
	}
	url, err := deps.manager.CaptureURL(ctx, tenant)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, url)
	return nil
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

// doctor validates the deployment invariants of the aigw-supervised shape: the
// filesystem permissions that stand behind the mount namespace, the runtime the
// sandbox binds, and the bwrap preconditions the host must supply.
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
	for _, tenant := range deps.reg.List() {
		needBrowserFS = needBrowserFS || tenant.PluginBrowserFS == "on"
	}
	checks := []check{
		{"state-dir-private", func() error { return checkPrivateDirectory(deps.cfg.StateDir) }},
		{"tenant-root-private", func() error { return checkPrivateDirectory(deps.cfg.TenantRoot) }},
		{"workspace-root-private", func() error { return checkPrivateDirectory(deps.cfg.WorkspaceRoot) }},
		{"config-private", func() error { return checkPrivateFile(c.configPath) }},
		{"registry-private", func() error { return checkPrivateFile(deps.cfg.RegistryPath) }},
		{"handshake-dir-private", func() error { return checkPrivateDirectory(deps.cfg.HandshakeDir) }},
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
		{"bwrap-bin", func() error { return executable(deps.cfg.Deploy.BwrapBin) }},
		{"bwrap-apparmor-userns", checkAppArmorUserNSRestriction},
		{"bwrap-sandbox-runtime", func() error { return sandbox.ValidateRuntime(sandboxRuntimeConfig(deps.cfg)) }},
		{"bwrap-worker-account", func() error { return sandbox.ValidateWorkerAccount(deps.cfg.Deploy.WorkerUser) }},
		{"feishu-login", func() error { return checkFeishuLogin(deps.cfg) }},
	}
	// The tenant-side web plugins (M75) ship beside the picker plugin, and only an enabled one is
	// checked: a deployment that switched a plugin off is not broken by not having deployed it.
	for _, plugin := range []struct {
		dir     string
		enabled bool
	}{
		{dir: "web-tty", enabled: deps.cfg.TenantPlugins.WebTTY.Enabled},
		{dir: "workspace-files", enabled: deps.cfg.TenantPlugins.WorkspaceFiles.Enabled},
		{dir: "git-diff", enabled: deps.cfg.TenantPlugins.GitDiff.Enabled},
	} {
		if !plugin.enabled {
			continue
		}
		path := filepath.Join(filepath.Dir(deps.cfg.Deploy.PluginPath), plugin.dir, "index.js")
		checks = append(checks, check{name: plugin.dir + "-plugin", run: func() error { _, err := os.Stat(path); return err }})
	}
	failures := 0
	for _, item := range checks {
		if err := item.run(); err != nil {
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
				return checkPrivateFile(filepath.Join(deps.cfg.HandshakeDir, tenant.Name+".url"))
			}},
			{"tenant-" + tenant.Name + "-credentials", func() error { return checkPrivateFile(filepath.Join(tenant.DshHome, ".credentials.yaml")) }},
			{"tenant-" + tenant.Name + "-settings", func() error { return checkPrivateFile(filepath.Join(tenant.DshHome, "settings.yaml")) }},
			{"tenant-" + tenant.Name + "-sandbox", func() error { return deps.manager.SandboxProfileReady(tenant) }},
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

// checkFeishuLogin reports the two facts an operator cannot see from the outside: whether a
// ticket secret was actually configured, and whether the scheme this gateway advertises agrees
// with the cookie it will issue. Both failures otherwise show up as "login did nothing".
func checkFeishuLogin(cfg *config.Config) error {
	if !cfg.Feishu.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.Feishu.TicketSecret) == "" {
		return errors.New("feishu.ticket_secret is empty: no login ticket could be verified")
	}
	if !cfg.SecureSessionCookie() {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(cfg.Feishu.AigwLoginURL), "http://") {
		return errors.New("the deployment advertises plain http but would issue a Secure session cookie: set public_scheme: http")
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

// checkPrivateDirectory requires a deployment directory to grant nothing to the
// group or other classes. Every worker runs as the account that owns these
// directories, so the strictest mode is also the correct one: the mount namespace
// is the isolation boundary, and these bits are what keeps an unrelated local user
// out of tenant data if that boundary ever fails.
func checkPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if extra := info.Mode().Perm() & 0o077; extra != 0 {
		return fmt.Errorf("%s mode %04o grants group or other access; tenant roots must be 0700", path, info.Mode().Perm())
	}
	return nil
}

// checkPrivateFile is checkPrivateDirectory for one file.
func checkPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if extra := info.Mode().Perm() & 0o077; extra != 0 {
		return fmt.Errorf("%s mode %04o grants group or other access; gateway state must be 0600", path, info.Mode().Perm())
	}
	return nil
}
