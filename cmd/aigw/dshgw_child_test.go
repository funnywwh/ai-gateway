package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/winger/ai-gateway/internal/config"
)

// childFixture is a configuration that enables the supervised child, with every
// path pointing into a temporary directory and the dsh runtime supplied by the
// environment (which is how the repository's own scripts describe a staged dsh).
func childFixture(t *testing.T) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	node := filepath.Join(root, "dsh/node/bin/node")
	release := filepath.Join(root, "dsh/releases/r1")
	for _, dir := range []string{filepath.Dir(node), filepath.Join(release, "lib"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{node, filepath.Join(release, "lib", "bin.js")} {
		if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	aigwBinary := filepath.Join(root, "bin/aigw")
	dshgwBinary := filepath.Join(root, "bin/dshgw")
	for _, file := range []string{aigwBinary, dshgwBinary} {
		if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DSHGW_NODE", node)
	t.Setenv("DSHGW_DSH_ROOT", release)
	t.Setenv("DSHGW_TEMPLATE_HOME", filepath.Join(root, "template-home"))
	cfg := &config.Config{
		Server:   config.Server{Listen: ":8088"},
		Database: config.Database{Path: filepath.Join(root, "data/aigw.db")},
		Dshgw: config.Dshgw{
			Enabled: true, PublicHost: "localhost", Listen: "127.0.0.1:31699",
			PortalPort: 31000, TenantPortLo: 31001, TenantPortHi: 31299,
			WorkerPortLo: 31300, WorkerPortHi: 31599,
		},
	}
	return cfg, aigwBinary
}

func TestBuildDshgwChildDerivesTheWholeSurface(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(filepath.Dir(cfg.Database.Path), "dshgw")
	if child.binary != filepath.Join(filepath.Dir(aigwBinary), "dshgw") {
		t.Fatalf("binary = %q, want the sibling of aigw", child.binary)
	}
	if child.configPath != filepath.Join(stateDir, "config.yaml") {
		t.Fatalf("config path = %q", child.configPath)
	}
	generated := child.config
	checks := map[string]string{
		"state dir":      generated.StateDir,
		"workspace root": generated.WorkspaceRoot,
		"tenant config":  generated.Deploy.TenantConfigRoot,
		"admin socket":   generated.AdminSocket,
		"child config":   generated.Deploy.ConfigPath,
		"backup dir":     generated.Deploy.BackupDir,
		"aigw base url":  generated.AigwBaseURL,
		"node bin":       generated.Dsh.NodeBin,
		"current link":   generated.Dsh.CurrentLink,
	}
	wants := map[string]string{
		"state dir":      stateDir,
		"workspace root": filepath.Join(stateDir, "workspaces"),
		"tenant config":  filepath.Join(stateDir, "tenant-config"),
		"admin socket":   filepath.Join(stateDir, "admin.sock"),
		"child config":   filepath.Join(stateDir, "config.yaml"),
		"backup dir":     filepath.Join(stateDir, "backups"),
		"aigw base url":  "http://127.0.0.1:8088",
		"node bin":       os.Getenv("DSHGW_NODE"),
		"current link":   os.Getenv("DSHGW_DSH_ROOT"),
	}
	for label, want := range wants {
		if checks[label] != want {
			t.Errorf("%s = %q, want %q", label, checks[label], want)
		}
	}
	// Every tenant worker runs as aigw's own account: no service account, no root.
	if generated.Deploy.WorkerUser == "" || generated.Deploy.WorkerUser == "root" {
		t.Fatalf("worker account = %q; workers must run as the invoking account", generated.Deploy.WorkerUser)
	}
}

func TestBuildDshgwChildPrefersExplicitOverridesAndBasePath(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	cfg.Server.BasePath = "/aigw/"
	cfg.Server.Listen = "127.0.0.1:9099"
	cfg.Dshgw.AigwBaseURL = "http://aigw.internal:8088"
	cfg.Dshgw.StateDir = filepath.Join(t.TempDir(), "custom-state")
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.AigwBaseURL != "http://aigw.internal:8088" {
		t.Fatalf("base url = %q, want the explicit override", child.config.AigwBaseURL)
	}
	if child.config.StateDir != cfg.Dshgw.StateDir {
		t.Fatalf("state dir = %q, want the configured one", child.config.StateDir)
	}

	// Without an override, aigw's own listener is used, including its mount prefix:
	// the child calls aigw's own API, not the console.
	cfg.Dshgw.AigwBaseURL = ""
	child, err = buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	if child.config.AigwBaseURL != "http://127.0.0.1:9099/aigw" {
		t.Fatalf("base url = %q, want the loopback listener with the mount prefix", child.config.AigwBaseURL)
	}
}

// Without a dsh runtime there is nothing for the child to run tenants from; the
// failure must name what is missing instead of surfacing as a restart loop.
func TestBuildDshgwChildRequiresTheDSHRuntime(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	t.Setenv("DSHGW_NODE", "")
	cfg.Dshgw.NodeBin = ""
	_, err := buildDshgwChild(cfg, aigwBinary)
	if err == nil {
		t.Fatal("missing node binary accepted")
	}
	if !strings.Contains(err.Error(), "node_bin") {
		t.Fatalf("error does not name the missing setting: %v", err)
	}
	// A missing sibling binary is refused too: enabled means "aigw starts dshgw".
	if err := os.Remove(filepath.Join(filepath.Dir(aigwBinary), "dshgw")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildDshgwChild(cfg, aigwBinary); err == nil {
		t.Fatal("missing dshgw binary accepted")
	}
}

func TestPrepareWritesTheChildConfigOnce(t *testing.T) {
	cfg, aigwBinary := childFixture(t)
	child, err := buildDshgwChild(cfg, aigwBinary)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := child.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first prepare reported no change")
	}
	info, err := os.Stat(child.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("child config mode = %04o, want 0600", perm)
	}
	changed, err = child.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical configuration rewritten")
	}
}
