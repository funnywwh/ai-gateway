package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/dshgwsup"
)

// dshgwChild is everything main needs to supervise the child: where the binary
// is, which configuration file describes it, and the supervisor itself.
type dshgwChild struct {
	binary     string
	configPath string
	config     dshgwsup.ChildConfig
	// adminSocket is where THIS parent reaches the child's provisioning channel. It is
	// derived here rather than left to configuration because the child gets the same value
	// in its generated file: an operator who never sets dshgw.admin_socket would otherwise
	// have a console whose 启用/停用 DSH buttons (and the automatic enable a Feishu binding
	// performs) dial an empty address.
	adminSocket string
}

// buildDshgwChild resolves, generates and validates the child's configuration.
//
// It runs before aigw starts serving: an operator who enabled the DSH surface
// must learn that it cannot start from the startup path, with the offending
// setting named, instead of from a log line minutes later.
func buildDshgwChild(cfg *config.Config, aigwExecutable string) (*dshgwChild, error) {
	binary, err := dshgwsup.LookupBinary(aigwExecutable, cfg.Dshgw.Binary)
	if err != nil {
		return nil, err
	}
	// The state directory defaults to <database directory>/dshgw — with the shipped
	// defaults that is ./data/dshgw, inside the single data root (M63). It is resolved
	// to an absolute path here because the child's own configuration requires clean
	// absolute paths: a relative value used to be rejected outright, which made the
	// supervised shape unusable with a relative database path (the default!).
	stateDir := cfg.Dshgw.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(filepath.Dir(cfg.Database.Path), "dshgw")
	}
	stateDir, err = resolveChildPath("dshgw.state_dir", stateDir)
	if err != nil {
		return nil, err
	}
	tenantRoot, err := resolveChildPath("dshgw.tenant_root", firstNonEmpty(cfg.Dshgw.TenantRoot, filepath.Join(stateDir, "tenants")))
	if err != nil {
		return nil, err
	}
	workspaceRoot, err := resolveChildPath("dshgw.workspace_root", firstNonEmpty(cfg.Dshgw.WorkspaceRoot, filepath.Join(stateDir, "workspaces")))
	if err != nil {
		return nil, err
	}
	// The template is a prepared profile inside the data root: it is built by
	// deploy/dshgw/prepare-template.sh (which reads DSHGW_TEMPLATE_HOME), not shipped at
	// an installation path. Leaving it empty let the child fall back to its own
	// /opt/dshgw default, which is how a path from the deleted root install survived
	// into the supervised shape.
	templateHome, err := resolveChildPath("dshgw.template_home", firstNonEmpty(cfg.Dshgw.TemplateHome, os.Getenv("DSHGW_TEMPLATE_HOME"), filepath.Join(stateDir, "template-home")))
	if err != nil {
		return nil, err
	}
	// The clamp directory picker is a plugin the worker imports by absolute file URL, and
	// it is the child's default, so an empty path is a startup error rather than a tenant
	// session with a picker pointing nowhere. The plugin ships with this repository
	// (cmd/dshgw/plugin/picker-clamp.js) and is named explicitly by the deployment.
	pluginPath := strings.TrimSpace(cfg.Dshgw.PluginPath)
	if pluginPath == "" {
		return nil, errors.New("dshgw.plugin_path is required: the child's default directory picker is clamp and needs the plugin (this repository ships it at ./cmd/dshgw/plugin/picker-clamp.js)")
	}
	if pluginPath, err = resolveChildPath("dshgw.plugin_path", pluginPath); err != nil {
		return nil, err
	}
	adminSocket := cfg.Dshgw.AdminSocket
	if adminSocket == "" {
		adminSocket = filepath.Join(stateDir, "admin.sock")
	}
	child := dshgwsup.ChildConfig{
		PublicHost: cfg.Dshgw.PublicHost,
		// Single-domain path mode: unset keeps the port-based origins. Trimmed here
		// so "https://host/" and "https://host" mean the same deployment.
		NoStoreAPIs:      cfg.Dshgw.NoStoreAPIs,
		PublicBaseURL:    strings.TrimRight(strings.TrimSpace(cfg.Dshgw.PublicBaseURL), "/"),
		TenantPathPrefix: strings.TrimSpace(cfg.Dshgw.TenantPathPrefix),
		PortalPathPrefix: strings.TrimSpace(cfg.Dshgw.PortalPathPrefix),
		// The scheme browsers use: passed through so a plain-HTTP deployment does not
		// get Secure cookies the browser throws away.
		PublicScheme: strings.TrimSpace(cfg.Dshgw.PublicScheme),
		PortalPort:   cfg.Dshgw.PortalPort,
		TenantPortLo: cfg.Dshgw.TenantPortLo,
		TenantPortHi: cfg.Dshgw.TenantPortHi,
		WorkerPortLo: cfg.Dshgw.WorkerPortLo,
		WorkerPortHi: cfg.Dshgw.WorkerPortHi,
		Listen:       cfg.Dshgw.Listen,
		// The child reaches aigw over loopback: it runs on the same host, and the
		// tenant data plane never needs to leave it.
		AigwBaseURL:     dshgwAigwBaseURL(cfg),
		AdminSocket:     adminSocket,
		PluginBrowserFS: strings.TrimSpace(cfg.Dshgw.PluginBrowserFS),
		StateDir:        stateDir,
		// Both roots are overridable so a migration can point the new shape at the
		// directories the old deployment already filled, and so workspaces can live
		// on the volume that has the space. Unset keeps the state_dir-derived defaults.
		TenantRoot:    tenantRoot,
		WorkspaceRoot: workspaceRoot,
		Dsh: dshgwsup.DshRuntime{
			NodeBin:     dshPath(cfg.Dshgw.NodeBin, "DSHGW_NODE"),
			BinJS:       dshBinJS(cfg),
			CurrentLink: dshPath(cfg.Dshgw.CurrentLink, "DSHGW_DSH_ROOT"),
		},
		WorkerLimits: dshgwsup.WorkerLimits{
			MemoryHighBytes: cfg.Dshgw.WorkerMemoryHighBytes,
			MemoryMaxBytes:  cfg.Dshgw.WorkerMemoryMaxBytes,
			TasksMax:        cfg.Dshgw.WorkerTasksMax,
			CPUQuotaPercent: cfg.Dshgw.WorkerCPUQuotaPercent,
		},
		Deploy: dshgwsup.Deploy{
			PublicListen: strings.TrimSpace(cfg.Dshgw.PublicListen),
			// Every worker runs as aigw's own account: that is what "no service
			// account, no root" means, and it keeps the child's file access inside
			// the account that already owns the state directory.
			WorkerUser:       currentAccount(),
			PluginPath:       pluginPath,
			TenantConfigRoot: filepath.Join(stateDir, "tenant-config"),
			TemplateHome:     templateHome,
			ConfigPath:       filepath.Join(stateDir, "config.yaml"),
			BackupDir:        filepath.Join(stateDir, "backups"),
		},
	}
	// The child runs ssh, so its own configuration carries the feature; this side only has
	// to place the paths in the deployment's terms. A relative key path is resolved here,
	// where the deployment root is known, instead of inside a generated file.
	if cfg.Dshgw.BrowserWorkspaces.Enabled {
		child.BrowserWorkspaces = &dshgwsup.BrowserWorkspaces{Enabled: true}
	}
	if cfg.Dshgw.SSHWorkspaces.Enabled {
		ssh := &dshgwsup.SSHWorkspaces{
			Enabled:            true,
			MountSubdir:        strings.TrimSpace(cfg.Dshgw.SSHWorkspaces.MountSubdir),
			Hosts:              cfg.Dshgw.SSHWorkspaces.Hosts,
			ConnectTimeout:     strings.TrimSpace(cfg.Dshgw.SSHWorkspaces.ConnectTimeout),
			PollInterval:       strings.TrimSpace(cfg.Dshgw.SSHWorkspaces.PollInterval),
			MaxEntries:         cfg.Dshgw.SSHWorkspaces.MaxEntries,
			SSHFSOptions:       cfg.Dshgw.SSHWorkspaces.SSHFSOptions,
			DisableAutoRemount: cfg.Dshgw.SSHWorkspaces.DisableAutoRemount,
		}
		for _, path := range []struct {
			label  string
			source string
			target *string
		}{
			{"dshgw.ssh_workspaces.identity_source", cfg.Dshgw.SSHWorkspaces.IdentitySource, &ssh.IdentitySource},
			{"dshgw.ssh_workspaces.identity_dir", cfg.Dshgw.SSHWorkspaces.IdentityDir, &ssh.IdentityDir},
			{"dshgw.ssh_workspaces.ssh_config_source", cfg.Dshgw.SSHWorkspaces.SSHConfigSource, &ssh.SSHConfigSource},
			{"dshgw.ssh_workspaces.ssh_bin", cfg.Dshgw.SSHWorkspaces.SSHBin, &ssh.SSHBin},
			{"dshgw.ssh_workspaces.sshfs_bin", cfg.Dshgw.SSHWorkspaces.SSHFSBin, &ssh.SSHFSBin},
		} {
			trimmed := strings.TrimSpace(path.source)
			if trimmed == "" {
				continue
			}
			resolved, resolveErr := resolveChildPath(path.label, trimmed)
			if resolveErr != nil {
				return nil, resolveErr
			}
			*path.target = resolved
		}
		child.SSHWorkspaces = ssh
	}
	if cert, key := strings.TrimSpace(cfg.Dshgw.TLSCertificate), strings.TrimSpace(cfg.Dshgw.TLSCertificateKey); cert != "" || key != "" {
		if cert == "" || key == "" {
			return nil, errors.New("dshgw.tls_certificate and dshgw.tls_certificate_key must be set together")
		}
		child.TLS = &dshgwsup.TLSConfig{Certificate: cert, CertificateKey: key}
	}
	// The child serves the portal, so it is the side that redeems a Feishu login; it needs the
	// URL to send people to and the key that proves a ticket came from this aigw. Both are
	// derived here, which is why enabling Feishu needs no matching edit in the child's file.
	if cfg.Feishu.Enabled && cfg.Feishu.DSHLogin {
		secret, err := feishuTicketSecret(cfg)
		if err != nil {
			return nil, err
		}
		child.Feishu = &dshgwsup.Feishu{
			Enabled:      true,
			AigwLoginURL: cfg.FeishuLoginURL(),
			TicketSecret: string(secret),
		}
	}
	if err := child.Validate(); err != nil {
		return nil, fmt.Errorf("dshgw child configuration: %w", err)
	}
	return &dshgwChild{binary: binary, configPath: child.Deploy.ConfigPath, config: child, adminSocket: adminSocket}, nil
}

// firstNonEmpty returns the first non-empty value, for defaults that depend on
// another setting.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// dshgwAigwBaseURL prefers an explicit override and otherwise points the child at
// aigw's own loopback listener.
func dshgwAigwBaseURL(cfg *config.Config) string {
	if override := strings.TrimSpace(cfg.Dshgw.AigwBaseURL); override != "" {
		return override
	}
	return localAigwBaseURL(cfg)
}

// localAigwBaseURL is the URL the child uses to validate tenant keys. It always
// points at aigw's own loopback listener: the address in server.listen may be
// ":8088" or "127.0.0.1:8088", and neither is directly usable as a URL.
func localAigwBaseURL(cfg *config.Config) string {
	listen := cfg.Server.Listen
	host, port, ok := strings.Cut(listen, ":")
	if !ok || port == "" {
		return "http://127.0.0.1"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if base := strings.TrimSpace(cfg.Server.BasePath); base != "" {
		return "http://" + host + ":" + port + "/" + strings.Trim(base, "/")
	}
	return "http://" + host + ":" + port
}

// dshBinJS is dsh's public launcher. It is derived from the release directory the
// child is pointed at, so an operator only has to state where dsh lives once.
func dshBinJS(cfg *config.Config) string {
	if explicit := strings.TrimSpace(cfg.Dshgw.BinJS); explicit != "" {
		return explicit
	}
	root := dshPath(cfg.Dshgw.CurrentLink, "DSHGW_DSH_ROOT")
	if root == "" {
		return ""
	}
	return filepath.Join(root, "lib", "bin.js")
}

// dshPath resolves one dsh runtime path: explicit configuration wins, otherwise
// the environment variables the repository's own scripts use.
func dshPath(configured, env string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return strings.TrimSpace(os.Getenv(env))
}

// resolveChildPath turns one path meant for the generated child configuration into a
// clean absolute path.
//
// The child validates its own paths as absolute, so aigw is the side that resolves the
// deployment's relative defaults (./data/...): the data root is expressed once, in
// aigw's configuration, and the child receives the answer. Resolution is against the
// process working directory, which is the deployment root in every documented entry
// point (the user unit's WorkingDirectory, scripts/local-run.sh's cd).
func resolveChildPath(label, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if !filepath.IsAbs(path) {
		// A relative path may point deeper into the deployment, never back out of it: the
		// deployment's data root is expressed relative to itself, and silently cleaning
		// ".." would let ./data/../elsewhere land outside the root the docs describe.
		for _, elem := range strings.Split(filepath.ToSlash(path), "/") {
			if elem == ".." {
				return "", fmt.Errorf("%s %q must not contain \"..\"; use an absolute path to leave the deployment root", label, path)
			}
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("%s %q is not a usable relative path: %w", label, path, err)
		}
		path = abs
	}
	if filepath.Clean(path) != path {
		return "", fmt.Errorf("%s %q must be a clean path", label, path)
	}
	return path, nil
}

// currentAccount is the account every tenant worker runs as. It is resolved here
// rather than configured so it cannot silently disagree with the process that
// actually spawns the workers.
func currentAccount() string {
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		return user
	}
	if user := strings.TrimSpace(os.Getenv("LOGNAME")); user != "" {
		return user
	}
	return fmt.Sprintf("%d", os.Geteuid())
}

// prepare writes the child's configuration and returns whether it changed.
func (c *dshgwChild) prepare() (bool, error) {
	if c == nil {
		return false, errors.New("no dshgw child configured")
	}
	return c.config.WriteIfChanged(c.configPath)
}
