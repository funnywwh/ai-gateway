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
	stateDir := cfg.Dshgw.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(filepath.Dir(cfg.Database.Path), "dshgw")
	}
	if !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("dshgw.state_dir %q must be an absolute path", stateDir)
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
		PortalPort:       cfg.Dshgw.PortalPort,
		TenantPortLo:     cfg.Dshgw.TenantPortLo,
		TenantPortHi:     cfg.Dshgw.TenantPortHi,
		WorkerPortLo:     cfg.Dshgw.WorkerPortLo,
		WorkerPortHi:     cfg.Dshgw.WorkerPortHi,
		Listen:           cfg.Dshgw.Listen,
		// The child reaches aigw over loopback: it runs on the same host, and the
		// tenant data plane never needs to leave it.
		AigwBaseURL:     dshgwAigwBaseURL(cfg),
		AdminSocket:     adminSocket,
		PluginBrowserFS: strings.TrimSpace(cfg.Dshgw.PluginBrowserFS),
		StateDir:        stateDir,
		// Both roots are overridable so a migration can point the new shape at the
		// directories the old deployment already filled, and so workspaces can live
		// on the volume that has the space. Unset keeps the state_dir-derived defaults.
		TenantRoot:    firstNonEmpty(cfg.Dshgw.TenantRoot, filepath.Join(stateDir, "tenants")),
		WorkspaceRoot: firstNonEmpty(cfg.Dshgw.WorkspaceRoot, filepath.Join(stateDir, "workspaces")),
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
			PluginPath:       strings.TrimSpace(cfg.Dshgw.PluginPath),
			TenantConfigRoot: filepath.Join(stateDir, "tenant-config"),
			TemplateHome:     dshPath(cfg.Dshgw.TemplateHome, "DSHGW_TEMPLATE_HOME"),
			ConfigPath:       filepath.Join(stateDir, "config.yaml"),
			BackupDir:        filepath.Join(stateDir, "backups"),
		},
	}
	if cert, key := strings.TrimSpace(cfg.Dshgw.TLSCertificate), strings.TrimSpace(cfg.Dshgw.TLSCertificateKey); cert != "" || key != "" {
		if cert == "" || key == "" {
			return nil, errors.New("dshgw.tls_certificate and dshgw.tls_certificate_key must be set together")
		}
		child.TLS = &dshgwsup.TLSConfig{Certificate: cert, CertificateKey: key}
	}
	if err := child.Validate(); err != nil {
		return nil, fmt.Errorf("dshgw child configuration: %w", err)
	}
	return &dshgwChild{binary: binary, configPath: child.Deploy.ConfigPath, config: child}, nil
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
