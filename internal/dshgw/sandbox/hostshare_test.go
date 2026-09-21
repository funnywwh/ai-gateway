package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// hostShareTenant wires one account with the given shares: the container and every target are
// created the way the gateway creates them before a worker starts.
func hostShareTenant(t *testing.T, f profileFixture, shares ...HostShare) Tenant {
	t.Helper()
	tenant := f.alice
	tenant.HostShareRoot = filepath.Join(tenant.Workspace, "host")
	tenant.HostShares = shares
	for _, dir := range []string{tenant.HostShareRoot} {
		mustMkdir(t, dir, 0o700)
	}
	for _, share := range shares {
		mustMkdir(t, share.Target, 0o700)
	}
	return tenant
}

func TestProfileBindsHostSharesInsideTheContainer(t *testing.T) {
	f := newProfileFixture(t)
	source := filepath.Join(f.root, "shared-docs")
	mustMkdir(t, source, 0o700)
	// The writable share's source has to exist for the binding to be emitted at all.
	repoSource := filepath.Join(f.root, "shared-repo")
	mustMkdir(t, repoSource, 0o700)
	container := filepath.Join(f.alice.Workspace, "host")
	tenant := hostShareTenant(t, f,
		HostShare{Name: "docs", Source: source, Target: filepath.Join(container, "docs"), ReadOnly: true},
		HostShare{Name: "repo", Source: repoSource, Target: filepath.Join(container, "repo")},
	)

	argv, err := Profile(f.rt, tenant)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := parseMounts(t, argv)
	var readOnly, writable, containerMount *mount
	for i := range mounts {
		switch {
		case mounts[i].flag == "--ro-bind" && mounts[i].dst == tenant.HostShareRoot:
			containerMount = &mounts[i]
		case mounts[i].flag == "--ro-bind-try" && mounts[i].dst == filepath.Join(tenant.HostShareRoot, "docs"):
			readOnly = &mounts[i]
		case mounts[i].flag == "--bind-try" && mounts[i].dst == filepath.Join(tenant.HostShareRoot, "repo"):
			writable = &mounts[i]
		}
	}
	if containerMount == nil {
		t.Fatalf("the host share container is not bound read-only first: %v", argv)
	}
	if readOnly == nil || readOnly.src != source {
		t.Fatalf("the read-only share is not bound from its host directory: %v", argv)
	}
	if writable == nil {
		t.Fatalf("the writable share is not bound: %v", argv)
	}
	// The container binds before its children: a later --bind over a read-only container keeps
	// its own writability (the same rule as the browser mount root).
	if indexOfDst(mounts, tenant.HostShareRoot) > indexOfDst(mounts, filepath.Join(tenant.HostShareRoot, "docs")) {
		t.Error("the container is bound after its children")
	}
}

// indexOfDst is the position of one destination in the parsed mount list, or a large number.
func indexOfDst(mounts []mount, dst string) int {
	for i := range mounts {
		if mounts[i].dst == dst {
			return i
		}
	}
	return 1 << 30
}

func TestProfileRejectsUnsafeHostShares(t *testing.T) {
	f := newProfileFixture(t)
	source := filepath.Join(f.root, "shared")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(f.alice.Workspace, "host")
	if err := os.MkdirAll(container, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		tenant func(Tenant) Tenant
	}{
		{"a share without a protected container", func(tenant Tenant) Tenant {
			tenant.HostShareRoot = ""
			tenant.HostShares = []HostShare{{Name: "docs", Source: source, Target: filepath.Join(container, "docs")}}
			return tenant
		}},
		{"a container outside the workspace", func(tenant Tenant) Tenant {
			tenant.HostShareRoot = filepath.Join(f.root, "elsewhere")
			tenant.HostShares = []HostShare{{Name: "docs", Source: source, Target: filepath.Join(tenant.HostShareRoot, "docs")}}
			return tenant
		}},
		{"a target outside the container", func(tenant Tenant) Tenant {
			tenant.HostShareRoot = container
			tenant.HostShares = []HostShare{{Name: "docs", Source: source, Target: filepath.Join(f.alice.Workspace, "docs")}}
			return tenant
		}},
		{"the container itself as a target", func(tenant Tenant) Tenant {
			tenant.HostShareRoot = container
			tenant.HostShares = []HostShare{{Name: "docs", Source: source, Target: container}}
			return tenant
		}},
		{"a relative source", func(tenant Tenant) Tenant {
			tenant.HostShareRoot = container
			tenant.HostShares = []HostShare{{Name: "docs", Source: "./shared", Target: filepath.Join(container, "docs")}}
			return tenant
		}},
		{"the file system root as a source", func(tenant Tenant) Tenant {
			tenant.HostShareRoot = container
			tenant.HostShares = []HostShare{{Name: "docs", Source: "/", Target: filepath.Join(container, "docs")}}
			return tenant
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Profile(f.rt, tc.tenant(f.alice)); err == nil {
				t.Fatal("the profile was accepted, want a refusal")
			}
		})
	}

	// A share whose host directory is gone is skipped, not fatal: the account must still start.
	tenant := f.alice
	tenant.HostShareRoot = container
	tenant.HostShares = []HostShare{{Name: "gone", Source: filepath.Join(f.root, "vanished"), Target: filepath.Join(container, "gone")}}
	argv, err := Profile(f.rt, tenant)
	if err != nil {
		t.Fatalf("a missing share was fatal: %v", err)
	}
	if indexOfDst(mustParseMounts(t, argv), filepath.Join(container, "gone")) != 1<<30 {
		t.Error("a missing share was bound anyway")
	}
}

func mustParseMounts(t *testing.T, argv []string) []mount {
	t.Helper()
	mounts, _, _ := parseMounts(t, argv)
	return mounts
}

// The staging run: a real bubblewrap, a real host directory, and a probe that reports what the
// sandbox can actually see and write. This is the claim that matters — a share is a plain bind,
// so the tenant reads the host directory at its sandbox path, writes only where the deployment
// allowed it, and cannot reach the host path itself.
func TestStagingHostShareIsVisibleReadOnlyOrWritable(t *testing.T) {
	bwrap := bwrapForStaging(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "srv/alice")
	dshHome := filepath.Join(root, "state/tenants/alice/.dsh")
	mustMkdir(t, filepath.Join(root, "dsh/node/bin"), 0o755)
	mustMkdir(t, filepath.Join(root, "dsh/releases/v1/lib"), 0o755)
	mustWrite(t, filepath.Join(root, "dsh/releases/v1/lib/bin.js"), "// dsh launcher\n", 0o644)
	mustSymlink(t, filepath.Join(root, "dsh/releases/v1"), filepath.Join(root, "dsh/current"))
	mustMkdir(t, workspace, 0o700)
	mustMkdir(t, dshHome, 0o700)

	docs := filepath.Join(root, "shared/docs")
	repo := filepath.Join(root, "shared/repo")
	mustMkdir(t, docs, 0o700)
	mustMkdir(t, repo, 0o700)
	mustWrite(t, filepath.Join(docs, "readme.txt"), "shared docs\n", 0o644)
	mustWrite(t, filepath.Join(repo, "existing.txt"), "shared repo\n", 0o644)

	container := filepath.Join(workspace, "host")
	for _, dir := range []string{filepath.Join(container, "docs"), filepath.Join(container, "repo")} {
		mustMkdir(t, dir, 0o700)
	}
	probe := filepath.Join(root, "dsh/node/bin/node")
	mustWrite(t, probe, fmt.Sprintf(`#!/bin/sh
report() { printf '%%s=%%s\n' "$1" "$2"; }
if [ -r %[1]q/readme.txt ]; then report docs-visible "$(cat %[1]q/readme.txt)"; else report docs-visible NO; fi
if [ -r %[2]q/existing.txt ]; then report repo-visible "$(cat %[2]q/existing.txt)"; else report repo-visible NO; fi
if touch %[1]q/new.txt 2>/dev/null; then report docs-write ok; else report docs-write refused; fi
if touch %[2]q/new.txt 2>/dev/null; then report repo-write ok; else report repo-write refused; fi
if [ -e %[3]q ]; then report host-path VISIBLE; else report host-path MISSING; fi
if [ -e %[4]q ]; then report other-share VISIBLE; else report other-share MISSING; fi
report container-entries "$(ls -A %[5]q 2>/dev/null | tr '\n' ',')"
printf 'done=1\n'
`, filepath.Join(container, "docs"), filepath.Join(container, "repo"), docs, filepath.Join(container, "absent"), container), 0o755)

	tenant := Tenant{
		Name:          "alice",
		Workspace:     workspace,
		DshHome:       dshHome,
		WorkerPort:    32100,
		HostShareRoot: container,
		HostShares: []HostShare{
			{Name: "docs", Source: docs, Target: filepath.Join(container, "docs"), ReadOnly: true},
			{Name: "repo", Source: repo, Target: filepath.Join(container, "repo")},
		},
	}
	rt := Runtime{
		BwrapBin:         bwrap,
		NodeBin:          probe,
		BinJS:            filepath.Join(root, "dsh/current/lib/bin.js"),
		CurrentLink:      filepath.Join(root, "dsh/current"),
		TenantRoot:       filepath.Join(root, "state/tenants"),
		WorkspaceRoot:    filepath.Join(root, "srv"),
		TenantConfigRoot: filepath.Join(root, "etc/tenants"),
	}
	argv, err := Profile(rt, tenant)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sandbox run failed: %v\nstderr: %s", err, stderr.String())
	}
	facts := parseStagingReport(t, stdout.String())
	for key, want := range map[string]string{
		"docs-visible":      "shared docs",
		"repo-visible":      "shared repo",
		"docs-write":        "refused",
		"repo-write":        "ok",
		"host-path":         "MISSING",
		"other-share":       "MISSING",
		"container-entries": "docs,repo,",
		"done":              "1",
	} {
		if facts[key] != want {
			t.Errorf("%s = %q, want %q", key, facts[key], want)
		}
	}
	// The write landed on the host directory, not in a copy.
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); err != nil {
		t.Errorf("the writable share did not write through to the host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(docs, "new.txt")); err == nil {
		t.Error("the read-only share was written to")
	}
}
