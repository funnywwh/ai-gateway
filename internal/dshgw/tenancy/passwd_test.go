package tenancy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The passwd view is what makes getpwuid agree with HOME inside a tenant sandbox: the runner
// exports HOME=<workspace>, and OpenSSH resolves ~/~/.ssh/config from passwd, not from HOME.

func TestSandboxProfileBindsTheTenantPasswdView(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	argv, err := m.SandboxProfile(tenant)
	if err != nil {
		t.Fatal(err)
	}
	view := tenantPasswdFile(tenant.DshHome)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind "+view+" /etc/passwd") {
		t.Fatalf("profile does not bind the rendered view %s:\n%s", view, joined)
	}
	if want := filepath.Join(tenant.DshHome, "sandbox", "passwd"); view != want {
		t.Fatalf("view lives at %s, want %s", view, want)
	}
	data, err := os.ReadFile(view)
	if err != nil {
		t.Fatal(err)
	}
	if want := "dshgw:x:1001:1001::" + tenant.Workspace + ":/bin/sh"; !strings.Contains(string(data), want) {
		t.Fatalf("view does not name this tenant's workspace:\n%s", data)
	}
	if strings.Contains(string(data), "/home/dshgw") {
		t.Fatalf("the host's own home survived the view:\n%s", data)
	}
	info, err := os.Stat(view)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("view mode = %04o, want 0644: it is /etc/passwd inside the sandbox", perm)
	}
	if dir, err := os.Stat(filepath.Dir(view)); err != nil {
		t.Fatal(err)
	} else if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Fatalf("sandbox directory mode = %04o, want 0700", perm)
	}
}

func TestSandboxProfileRefusesAViewWithoutTheWorkerAccount(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	m.HostPasswd = func() ([]byte, error) { return []byte("root:x:0:0:root:/root:/bin/sh\n"), nil }
	if _, err := m.SandboxProfile(tenant); err == nil || !strings.Contains(err.Error(), m.Config.Deploy.WorkerUser) {
		t.Fatalf("a view without the worker account was accepted: %v", err)
	}
}

func TestSandboxProfileRefusesAViewWithoutAWorkerAccount(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	m.Config.Deploy.WorkerUser = ""
	if _, err := m.SandboxProfile(tenant); err == nil || !strings.Contains(err.Error(), "worker_user") {
		t.Fatalf("a profile without a worker account was rendered: %v", err)
	}
}

func TestSandboxProfileRerendersThePasswdViewEveryBuild(t *testing.T) {
	// The workspace path is fixed in the registry, but the host's own entry is not: re-rendering
	// on every build is what keeps the view in step after an operator re-homes the account.
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	if _, err := m.SandboxProfile(tenant); err != nil {
		t.Fatal(err)
	}
	view := tenantPasswdFile(tenant.DshHome)
	if err := os.WriteFile(view, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SandboxProfile(tenant); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "stale") {
		t.Fatalf("a stale view survived a profile build: %q", data)
	}
}
