package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/nodedep"
)

// The multi-machine node CLI (M77). `node add/update/remove` edit the control plane's node records;
// `node deploy` is the one-click install over ssh; `node rotate-token` changes the shared secret on
// both sides at once.
//
// Everything that describes the target machine (the ssh destination, the deployment directory, the
// runtime paths) lives in the record, so a deploy needs no flags: the flags exist to write the
// record in the first place, and `deploy` repeats exactly what the record says.

// nodeFlags collects the record-editing flags add and update share.
type nodeFlags struct {
	sshHost   *string
	sshPort   *int
	sshUser   *string
	sshKey    *string
	sshKeyIn  *string
	listen    *string
	url       *string
	dir       *string
	stateDir  *string
	pluginDir *string
	template  *string
	bwrapBin  *string
	nodeBin   *string
	binJS     *string
	dshRoot   *string
	portLo    *int
	portHi    *int
	tokenFile *string
	token     *string
	def       *bool
}

func (c *cli) nodeFlagSet(name string) (*flag.FlagSet, *nodeFlags) {
	fs := c.flagSet(name)
	flags := &nodeFlags{}
	flags.sshHost = fs.String("ssh-host", "", "target machine (host or IP)")
	flags.sshPort = fs.Int("ssh-port", 0, "ssh port (default 22)")
	flags.sshUser = fs.String("ssh-user", "", "ssh account on the target")
	flags.sshKey = fs.String("ssh-key", "", "private key on the control plane (default: <state>/node-ssh/<node>/id_ed25519)")
	flags.sshKeyIn = fs.String("ssh-key-in", "", "read a private key from this file and store it for the node")
	flags.listen = fs.String("listen", "", "address the node agent binds on the target (host:port)")
	flags.url = fs.String("url", "", "address the control plane reaches the node at (default http://<ssh-host>:<listen port>)")
	flags.dir = fs.String("deploy-dir", "", "deployment root on the target (default /srv/dshgw-node)")
	flags.stateDir = fs.String("node-state-dir", "", "state directory on the target (default <deploy-dir>/state)")
	flags.pluginDir = fs.String("plugin-dir", "", "plugin directory on the target (default <deploy-dir>/plugins)")
	flags.template = fs.String("template-home", "", "template directory on the target (default <deploy-dir>/template-home)")
	flags.bwrapBin = fs.String("bwrap-bin", "", "bubblewrap path on the target (default /usr/bin/bwrap)")
	flags.nodeBin = fs.String("node-bin", "", "Node runtime path on the target")
	flags.binJS = fs.String("bin-js", "", "dsh launcher path on the target")
	flags.dshRoot = fs.String("current-link", "", "dsh release directory on the target")
	flags.portLo = fs.Int("worker-port-lo", 0, "worker port band low bound on the target")
	flags.portHi = fs.Int("worker-port-hi", 0, "worker port band high bound on the target")
	flags.tokenFile = fs.String("token-file", "", "token file path ON THE TARGET (default <deploy-dir>/<node>.token)")
	flags.token = fs.String("token", "", "shared secret for this node (default: generated at deploy time)")
	flags.def = fs.Bool("default", false, "make this the node new tenants land on")
	return fs, flags
}

// nodeAdd creates a record. It touches nothing on the target: deploying is a separate, explicit
// step, because "registered" and "installed" are different states an operator needs to tell apart.
func (c *cli) nodeAdd(ctx context.Context, args []string) error {
	fs, flags := c.nodeFlagSet("node add")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw node add [OPTIONS] NAME")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	if deps.cfg.NodeMode() {
		return errors.New("this configuration is a worker node: nodes are registered on the control plane")
	}
	spec, err := c.specFromFlags(deps, fs.Arg(0), flags)
	if err != nil {
		return err
	}
	if *flags.sshKeyIn != "" {
		if err := c.storeNodeKey(deps, spec.Name, *flags.sshKeyIn, *flags.sshKey); err != nil {
			return err
		}
	}
	view, err := deps.nodeAdmin.Add(ctx, spec)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "registered node %s (%s as %s); next: dshgw node deploy %s\n",
		view.Name, view.SSHHost, view.SSHUser, view.Name)
	return nil
}

// nodeUpdate changes a record. Only the flags that were given change anything, so an operator can
// fix one path without re-typing the rest.
func (c *cli) nodeUpdate(ctx context.Context, args []string) error {
	fs, flags := c.nodeFlagSet("node update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw node update [OPTIONS] NAME")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	spec, err := c.specFromFlags(deps, fs.Arg(0), flags)
	if err != nil {
		return err
	}
	if *flags.sshKeyIn != "" {
		if err := c.storeNodeKey(deps, spec.Name, *flags.sshKeyIn, *flags.sshKey); err != nil {
			return err
		}
	}
	if _, err := deps.nodeAdmin.Update(ctx, spec); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "updated node %s\n", spec.Name)
	return nil
}

// nodeRemove forgets a record. Without --purge it changes nothing on the target: an operator who
// wants the machine to stop serving asks for the purge explicitly.
func (c *cli) nodeRemove(ctx context.Context, args []string) error {
	fs := c.flagSet("node remove")
	purge := fs.Bool("purge", false, "also stop the node on the target and delete its state there (irreversible)")
	yes := fs.Bool("yes", false, "confirm the purge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw node remove [--purge --yes] NAME")
	}
	if *purge && !*yes {
		return errors.New("--purge requires --yes: it deletes the node's tenants and workspaces on that machine")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	if err := deps.nodeAdmin.Remove(ctx, name, *purge); err != nil {
		return err
	}
	if *purge {
		fmt.Fprintf(c.stdout, "stopped and purged node %s\n", name)
		return nil
	}
	fmt.Fprintf(c.stdout, "removed node %s (nothing changed on the machine)\n", name)
	return nil
}

// nodeDeploy is the one-click install: preflight, upload, activate, unit, start, verify — with a
// rollback when a later phase fails after the target has been touched.
func (c *cli) nodeDeploy(ctx context.Context, args []string) error {
	fs := c.flagSet("node deploy")
	accept := fs.String("accept-host-key", "", "confirm the target's host key fingerprint (see the first run)")
	onNode := fs.Bool("prepare-template-on-node", false, "let the node prepare the tenant template itself (needs Corepack and network)")
	withPackages := fs.Bool("with-packages", false, "install missing OS packages with sudo -n (off by default)")
	rotate := fs.Bool("rotate-token", false, "also replace the node's shared secret")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: dshgw node deploy [OPTIONS] NAME")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	if _, err := deps.nodeAdmin.recordFor(name); err != nil {
		return fmt.Errorf("node %q is not registered; add it first with `dshgw node add`", name)
	}
	// The deploy runs as a job (the admin channel needs that shape so the console can watch it);
	// the CLI waits for it and prints the phases as they land, which is what an operator at a
	// terminal wants to see.
	if !*asJSON {
		fmt.Fprintf(c.stdout, "deploying %s (log %s)\n", name, nodedep.DeployLogPath(deps.cfg.StateDir, name))
	}
	if _, err := deps.nodeAdmin.StartDeploy(ctx, name, DeployOptions{
		AcceptHostKey: *accept, PrepareTemplateOnNode: *onNode, WithPackages: *withPackages, RotateToken: *rotate,
	}); err != nil {
		return err
	}
	status := c.waitDeploy(ctx, deps, name, !*asJSON)
	if status.Error != "" {
		hint := ""
		if status.Fingerprint != "" && strings.Contains(status.Error, "host key") {
			hint = "\nconfirm the fingerprint with: dshgw node deploy --accept-host-key " + status.Fingerprint + " " + name
		}
		return fmt.Errorf("%s\nsee %s%s", status.Error, status.LogPath, hint)
	}
	if *asJSON {
		encoded, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, string(encoded))
		return nil
	}
	fmt.Fprintf(c.stdout, "deployed %s: %s revision %s, %s\n", name, status.Version, shortRevision(status.Revision),
		map[bool]string{true: "supervised by a systemd user unit", false: "running detached (no systemd user manager: it will not survive a reboot)"}[status.Systemd])
	fmt.Fprintf(c.stdout, "token %s; log %s\n",
		map[bool]string{true: "rotated", false: "unchanged (the node already had the control plane's secret)"}[status.Rotated], status.LogPath)
	return nil
}

// nodeRotateToken deploys again with a fresh shared secret, updating both sides. It takes the same
// flags as a deploy (minus the token options), because a rotation *is* a deploy.
func (c *cli) nodeRotateToken(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: dshgw node rotate-token [OPTIONS] NAME")
	}
	return c.nodeDeploy(ctx, append([]string{"--rotate-token"}, args...))
}

// waitDeploy follows a deploy job to completion, optionally narrating the phases.
func (c *cli) waitDeploy(ctx context.Context, deps *runtimeDeps, name string, narrate bool) DeployStatus {
	seen := map[string]bool{}
	for {
		status := deps.nodeAdmin.DeployStatus(name)
		if narrate {
			for _, phase := range status.Phases {
				if seen[phase.Name] {
					continue
				}
				seen[phase.Name] = true
				fmt.Fprintf(c.stdout, "  %-9s %s (%dms)\n", phase.Name, map[bool]string{true: "ok", false: "FAILED"}[phase.OK], phase.Millis)
			}
		}
		if !status.Running {
			return status
		}
		if ctx.Err() != nil {
			status.Error = "interrupted: the deploy was abandoned in this process (the node keeps whatever the last finished phase installed)"
			return status
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// specFromFlags turns the CLI flags into a registration spec.
func (c *cli) specFromFlags(deps *runtimeDeps, name string, flags *nodeFlags) (NodeSpec, error) {
	spec := NodeSpec{Name: name}
	if *flags.sshHost != "" {
		spec.SSHHost = strings.TrimSpace(*flags.sshHost)
	}
	if *flags.sshPort != 0 {
		spec.SSHPort = *flags.sshPort
	}
	if *flags.sshUser != "" {
		spec.SSHUser = strings.TrimSpace(*flags.sshUser)
	}
	if *flags.sshKey != "" {
		spec.SSHKeyFile = strings.TrimSpace(*flags.sshKey)
	}
	if *flags.listen != "" {
		spec.Listen = strings.TrimSpace(*flags.listen)
	}
	if *flags.url != "" {
		spec.URL = strings.TrimSpace(*flags.url)
	}
	if *flags.dir != "" {
		spec.DeployDir = strings.TrimSpace(*flags.dir)
	}
	if *flags.stateDir != "" {
		spec.NodeStateDir = strings.TrimSpace(*flags.stateDir)
	}
	if *flags.pluginDir != "" {
		spec.PluginDir = strings.TrimSpace(*flags.pluginDir)
	}
	if *flags.template != "" {
		spec.TemplateHome = strings.TrimSpace(*flags.template)
	}
	if *flags.bwrapBin != "" {
		spec.BwrapBin = strings.TrimSpace(*flags.bwrapBin)
	}
	if *flags.nodeBin != "" {
		spec.NodeBin = strings.TrimSpace(*flags.nodeBin)
	}
	if *flags.binJS != "" {
		spec.BinJS = strings.TrimSpace(*flags.binJS)
	}
	if *flags.dshRoot != "" {
		spec.CurrentLink = strings.TrimSpace(*flags.dshRoot)
	}
	if *flags.portLo != 0 {
		spec.WorkerPortLo = *flags.portLo
	}
	if *flags.portHi != 0 {
		spec.WorkerPortHi = *flags.portHi
	}
	if *flags.tokenFile != "" {
		spec.TokenPath = strings.TrimSpace(*flags.tokenFile)
	}
	if *flags.token != "" {
		spec.Token = strings.TrimSpace(*flags.token)
	}
	spec.Default = *flags.def
	return spec, nil
}

// storeNodeKey stores an uploaded private key where the node's record points.
func (c *cli) storeNodeKey(deps *runtimeDeps, name, source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read --ssh-key-in: %w", err)
	}
	path, err := deps.nodeAdmin.StoreSSHKey(name, data, target)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "stored the node's private key at %s\n", path)
	return nil
}

// failedPhase names the phase a failure came from, so the console can show where a deploy stopped.
func failedPhase(result nodedep.Result) string {
	if len(result.Phases) == 0 {
		return nodedep.PhasePreflight
	}
	return result.Phases[len(result.Phases)-1].Name
}

func shortRevision(revision string) string {
	if len(revision) > 7 {
		return revision[:7]
	}
	return revision
}

func tokenDisposition(result nodedep.Result) string {
	if result.RotatedToken {
		return "rotated (the control plane's record was updated with it)"
	}
	if result.Token != "" {
		return "installed"
	}
	return "unchanged (the target already had one)"
}
