package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
	"path/filepath"
	"time"
)

func (c *cli) tenant(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("tenant requires create, list, rotate-key, restart, or remove")
	}
	switch args[0] {
	case "create":
		return c.tenantCreate(ctx, args[1:])
	case "list":
		return c.tenantList(ctx, args[1:])
	case "rotate-key":
		return c.tenantRotate(ctx, args[1:])
	case "restart":
		return c.tenantRestart(ctx, args[1:])
	case "remove":
		return c.tenantRemove(ctx, args[1:])
	default:
		return fmt.Errorf("unknown tenant command %q", args[0])
	}
}
func (c *cli) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	return fs
}
func (c *cli) tenantCreate(ctx context.Context, args []string) error {
	fs := c.flagSet("tenant create")
	keyFile := fs.String("key-file", "", "read API key from a mode-0600 absolute file")
	allowEmpty := fs.Bool("allow-empty-models", false, "create pending tenant when model list is empty")
	picker := fs.String("directory-picker", "", "clamp or browse")
	browser := fs.String("browser-fs", "", "on or off")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw tenant create [OPTIONS] NAME")
	}
	key, err := readKey(c.stdin, *keyFile)
	if err != nil {
		return err
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	models, err := validateKey(ctx, deps.validator, key)
	if err != nil {
		return err
	}
	ports, err := listeningPorts(ctx)
	if err != nil {
		return err
	}
	deps.manager.Taken = func(port int) bool { return ports[port] }
	tenant, err := deps.manager.Create(ctx, fs.Arg(0), key, models, tenancy.CreateOptions{AllowEmptyModels: *allowEmpty, DirectoryPicker: *picker, PluginBrowserFS: *browser})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "created tenant %s: https://%s:%d/ (worker 127.0.0.1:%d)\n", tenant.Name, deps.cfg.PublicHost, tenant.PublicPort, tenant.WorkerPort)
	return nil
}
func (c *cli) tenantList(ctx context.Context, args []string) error {
	fs := c.flagSet("tenant list")
	asJSON := fs.Bool("json", false, "emit JSON")
	noStatus := fs.Bool("no-status", false, "do not query systemd")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: dshgw tenant list [--json] [--no-status]")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	type row struct {
		Name       string `json:"name"`
		PublicPort int    `json:"public_port"`
		WorkerPort int    `json:"worker_port"`
		KeyPrefix  string `json:"key_prefix"`
		// Running is the worker *process* state; Suspended is the operator's
		// durable intent. Both matter: a tenant can be running while suspended
		// (until the next start) and stopped without being suspended (a crash).
		Running          bool     `json:"running"`
		Suspended        bool     `json:"suspended"`
		PID              int      `json:"pid"`
		Handshake        string   `json:"handshake"`
		DirectoryPicker  string   `json:"directory_picker"`
		BrowserFS        string   `json:"browser_fs"`
		ModelsPending    bool     `json:"models_pending"`
		LastLogin        string   `json:"last_login,omitempty"`
		UID              int      `json:"uid"`
		User             string   `json:"user"`
		Isolation        string   `json:"isolation"`
		DshHome          string   `json:"dsh_home"`
		Workspace        string   `json:"workspace"`
		CreatedAt        string   `json:"created_at"`
		PreviousPrefixes []string `json:"previous_prefixes"`
		HandshakePath    string   `json:"handshake_path"`
		GatewayKeyPath   string   `json:"gateway_key_path"`
		AigwBaseURL      string   `json:"aigw_base_url"`
		KeyRevalidate    string   `json:"key_revalidate"`
		PortalURL        string   `json:"portal_url"`
	}
	rows := []row{}
	for _, tenant := range deps.reg.List() {
		item := row{
			Name: tenant.Name, PublicPort: tenant.PublicPort, WorkerPort: tenant.WorkerPort, KeyPrefix: tenant.KeyPrefix,
			Suspended: tenant.Suspended, Handshake: string(tenant.Handshake), DirectoryPicker: tenant.DirectoryPicker,
			BrowserFS: tenant.PluginBrowserFS, ModelsPending: tenant.ModelsPending, UID: tenant.UID,
			User: deps.cfg.Deploy.WorkerUser, Isolation: tenant.EffectiveIsolation(),
			DshHome: tenant.DshHome, Workspace: tenant.Workspace, CreatedAt: tenant.CreatedAt.UTC().Format(time.RFC3339Nano),
			PreviousPrefixes: append([]string{}, tenant.PreviousPrefixes...),
			HandshakePath:    filepath.Join(deps.cfg.HandshakeDir, tenant.Name+".url"),
			GatewayKeyPath:   filepath.Join(deps.cfg.Deploy.TenantConfigRoot, tenant.Name, "gateway.key"),
			AigwBaseURL:      deps.cfg.AigwBaseURL, KeyRevalidate: deps.cfg.KeyRevalidate,
			PortalURL: deps.cfg.WithTrailingSlash(deps.cfg.OriginForPort(deps.cfg.PortalPort)),
		}
		if last, lastErr := (&activity.Store{Path: deps.cfg.ActivityPath}).LastLogin(tenant.Name); lastErr == nil && !last.IsZero() {
			item.LastLogin = last.Format(time.RFC3339)
		}
		if !*noStatus {
			status, _ := deps.manager.Status(ctx, tenant)
			item.Running, item.PID = status.Running, status.PID
		}
		rows = append(rows, item)
	}
	if *asJSON {
		data, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintln(c.stdout, string(data))
		return nil
	}
	fmt.Fprintln(c.stdout, "TENANT\tPUBLIC\tWORKER\tPREFIX\tRUNNING\tSUSPENDED\tPID\tHANDSHAKE\tPICKER\tBROWSER_FS\tMODELS_PENDING\tISOLATION\tLAST_LOGIN")
	for _, item := range rows {
		fmt.Fprintf(c.stdout, "%s\t%d\t%d\t%s\t%t\t%t\t%d\t%s\t%s\t%s\t%t\t%s\t%s\n", item.Name, item.PublicPort, item.WorkerPort, item.KeyPrefix, item.Running, item.Suspended, item.PID, item.Handshake, item.DirectoryPicker, item.BrowserFS, item.ModelsPending, item.Isolation, item.LastLogin)
	}
	return nil
}

func (c *cli) tenantRotate(ctx context.Context, args []string) error {
	fs := c.flagSet("tenant rotate-key")
	keyFile := fs.String("key-file", "", "read new API key from file")
	keepOld := fs.Bool("keep-old-prefix", false, "retain old public prefix as login alias")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw tenant rotate-key [OPTIONS] NAME")
	}
	key, err := readKey(c.stdin, *keyFile)
	if err != nil {
		return err
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, fs.Arg(0))
	if err != nil {
		return err
	}
	models, err := validateKey(ctx, deps.validator, key)
	if err != nil {
		return err
	}
	if err := deps.manager.RotateKey(ctx, tenant, key, models, *keepOld); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "rotated tenant %s key (%d models)\n", tenant.Name, len(models))
	return nil
}
func (c *cli) tenantRestart(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: dshgw tenant restart NAME")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, args[0])
	if err != nil {
		return err
	}
	if err := deps.manager.Restart(ctx, tenant); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "restarted tenant %s\n", tenant.Name)
	return nil
}
func tenantRemovalError(snapshot string, err error) error {
	if err == nil || snapshot == "" {
		return err
	}
	return fmt.Errorf("%w (snapshot: %s)", err, snapshot)
}

func (c *cli) tenantRemove(ctx context.Context, args []string) error {
	fs := c.flagSet("tenant remove")
	purge := fs.Bool("purge", false, "delete state and workspace after snapshot")
	yes := fs.Bool("yes", false, "confirm irreversible purge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw tenant remove [--purge --yes] NAME")
	}
	if *purge && !*yes {
		return errors.New("--purge requires --yes after reviewing the automatic snapshot destination")
	}
	deps, err := c.loadRuntime(true)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, fs.Arg(0))
	if err != nil {
		return err
	}
	snapshot, err := deps.manager.Remove(ctx, tenant, *purge)
	if err != nil {
		return tenantRemovalError(snapshot, err)
	}
	action := "retained"
	if *purge {
		action = "purged"
	}
	fmt.Fprintf(c.stdout, "removed tenant %s (%s data); snapshot: %s\n", tenant.Name, action, snapshot)
	return nil
}
