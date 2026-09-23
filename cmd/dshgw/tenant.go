package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/activity"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

func (c *cli) tenant(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("tenant requires create, list, start, stop, restart, rotate-key, remove, or set-node")
	}
	switch args[0] {
	case "create":
		return c.tenantCreate(ctx, args[1:])
	case "list":
		return c.tenantList(ctx, args[1:])
	case "start":
		return c.tenantStartStop(ctx, args[1:], true)
	case "stop":
		return c.tenantStartStop(ctx, args[1:], false)
	case "rotate-key":
		return c.tenantRotate(ctx, args[1:])
	case "restart":
		return c.tenantRestart(ctx, args[1:])
	case "remove":
		return c.tenantRemove(ctx, args[1:])
	case "set-node":
		return c.tenantSetNode(ctx, args[1:])
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
	node := fs.String("node", "", "worker node to place the tenant on (default: the deployment's default node)")
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
	placement := strings.TrimSpace(*node)
	if placement == "" {
		placement = deps.cfg.DefaultNodeName()
	}
	tenant, err := deps.manager.Create(ctx, fs.Arg(0), key, models, tenancy.CreateOptions{
		AllowEmptyModels: *allowEmpty, DirectoryPicker: *picker, PluginBrowserFS: *browser, Node: placement,
	})
	if err != nil {
		return err
	}
	where := "this machine"
	if !deps.cfg.IsLocalNode(tenant.Node) {
		where = "node " + tenant.Node
	}
	fmt.Fprintf(c.stdout, "created tenant %s: https://%s:%d/ (worker 127.0.0.1:%d on %s)\n", tenant.Name, deps.cfg.PublicHost, tenant.PublicPort, tenant.WorkerPort, where)
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
		KeyPrefix  string `json:"key_prefix"` //nolint:revive // column alignment below
		// Node is where this tenant's worker runs (M77); "local" is this machine.
		Node string `json:"node"`
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
		remote := !deps.cfg.IsLocalNode(tenant.Node)
		item := row{
			Name: tenant.Name, PublicPort: tenant.PublicPort, WorkerPort: tenant.WorkerPort, KeyPrefix: tenant.KeyPrefix,
			Node:      tenant.Node,
			Suspended: tenant.Suspended, Handshake: string(tenant.Handshake), DirectoryPicker: tenant.DirectoryPicker,
			BrowserFS: tenant.PluginBrowserFS, ModelsPending: tenant.ModelsPending, UID: tenant.UID,
			User: deps.cfg.Deploy.WorkerUser, Isolation: tenant.EffectiveIsolation(),
			DshHome: tenant.DshHome, Workspace: tenant.Workspace, CreatedAt: tenant.CreatedAt.UTC().Format(time.RFC3339Nano),
			PreviousPrefixes: append([]string{}, tenant.PreviousPrefixes...),
			HandshakePath:    handshakePathFor(deps.cfg, tenant.Name, remote),
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
	fmt.Fprintln(c.stdout, "TENANT\tNODE\tPUBLIC\tWORKER\tPREFIX\tRUNNING\tSUSPENDED\tPID\tHANDSHAKE\tPICKER\tBROWSER_FS\tMODELS_PENDING\tISOLATION\tLAST_LOGIN")
	for _, item := range rows {
		fmt.Fprintf(c.stdout, "%s\t%s\t%d\t%d\t%s\t%t\t%t\t%d\t%s\t%s\t%s\t%t\t%s\t%s\n", item.Name, nodeLabel(deps.cfg, item.Node), item.PublicPort, item.WorkerPort, item.KeyPrefix, item.Running, item.Suspended, item.PID, item.Handshake, item.DirectoryPicker, item.BrowserFS, item.ModelsPending, item.Isolation, item.LastLogin)
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

// nodeLabel renders a tenant's placement for humans: the empty value and the literal "local"
// both mean this machine.
func nodeLabel(cfg *config.Config, node string) string {
	if cfg.IsLocalNode(node) {
		return "local"
	}
	return node
}

// handshakePathFor names the handshake file for a tenant. A remote tenant's handshake file lives
// on its node, so claiming the control plane's path would send an operator to a file that is not
// there; the empty string says "not on this machine" instead.
func handshakePathFor(cfg *config.Config, name string, remote bool) string {
	if remote {
		return ""
	}
	return filepath.Join(cfg.HandshakeDir, name+".url")
}

// tenantStartStop drives one tenant's worker up or down (M77). It exists as its own verb because
// the multi-machine surface needs the pair locally for every placement: on this machine it is the
// supervisor call, on a node it is one control request.
func (c *cli) tenantStartStop(ctx context.Context, args []string, start bool) error {
	verb := "stop"
	if start {
		verb = "start"
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: dshgw tenant %s NAME", verb)
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, args[0])
	if err != nil {
		return err
	}
	if start {
		err = deps.manager.StartWorker(ctx, tenant)
	} else {
		err = deps.manager.StopWorker(ctx, tenant)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "%s tenant %s (%s)\n", verb+"ped", tenant.Name, nodeLabel(deps.cfg, tenant.Node))
	return nil
}

// tenantSetNode records a new placement for a stopped tenant whose data is already on the target
// machine (M77).
//
// The guards are the whole point of this command: moving a tenant is a stop, a copy and a
// registration, in that order, and each of the three can be skipped by mistake. So the command
// refuses a running tenant (its worker would keep serving from the old machine), a node this
// deployment does not define, an unreachable node, a target whose data is not there yet, and a
// move to where it already is. The copy itself is the operator's job (`rsync`), because only they
// know which network and which ownership the machines need — and because a botched copy is not
// something this command could undo.
func (c *cli) tenantSetNode(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: dshgw tenant set-node NAME NODE")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	tenant, err := tenantByName(deps.reg, args[0])
	if err != nil {
		return err
	}
	target := strings.TrimSpace(args[1])
	if !deps.cfg.IsLocalNode(target) && target == tenant.Node {
		return fmt.Errorf("tenant %s is already placed on node %s", tenant.Name, target)
	}
	if deps.cfg.IsLocalNode(target) && deps.cfg.IsLocalNode(tenant.Node) {
		return fmt.Errorf("tenant %s already runs on the control plane's own machine", tenant.Name)
	}
	if state, statusErr := deps.manager.Status(ctx, tenant); statusErr != nil {
		return statusErr
	} else if state.Running {
		return fmt.Errorf("tenant %s is running: stop it first (`dshgw tenant stop %s`), copy its data, then register the new placement", tenant.Name, tenant.Name)
	}
	if config.ValidNodeName(target) || target == config.LocalNodeName {
		// Only a node this deployment defines can be reached; setting an undefined one would move
		// the tenant into a state where every one of its requests fails.
		if !deps.cfg.IsLocalNode(target) {
			if _, ok := deps.nodeViewNamed(target); !ok {
				return fmt.Errorf("node %q is not defined by this deployment", target)
			}
		}
	}
	if err := deps.manager.AdoptOnNode(ctx, tenant, target); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "tenant %s is now placed on %s; start it with `dshgw tenant start %s`\n", tenant.Name, nodeLabel(deps.cfg, target), tenant.Name)
	return nil
}
