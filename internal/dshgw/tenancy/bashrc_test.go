package tenancy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shell startup file is what makes a terminal inside a tenant sandbox coloured: the
// profile's /etc is a whitelist, so the host's bash startup files are not there and the tenant's
// HOME carries no ~/.bashrc either. `/etc/bash.bashrc` is bound from a per-tenant view, exactly
// like the passwd view next to it.

func TestSandboxProfileBindsTheTenantShellStartupFile(t *testing.T) {
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	argv, err := m.SandboxProfile(tenant)
	if err != nil {
		t.Fatal(err)
	}
	view := tenantBashrcFile(tenant.DshHome)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind "+view+" /etc/bash.bashrc") {
		t.Fatalf("profile does not bind the rendered shell startup file %s:\n%s", view, joined)
	}
	if want := filepath.Join(tenant.DshHome, "sandbox", "bashrc"); view != want {
		t.Fatalf("view lives at %s, want %s", view, want)
	}
	data, err := os.ReadFile(view)
	if err != nil {
		t.Fatal(err)
	}
	// The one thing the file must do: make the programs in the sandbox ask for colour, and
	// give the prompt one. Without these a terminal shows a monochrome `ls` even though it
	// renders colour perfectly.
	for _, want := range []string{"alias ls='ls --color=auto'", "dircolors", "PS1="} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("shell startup file does not contain %q:\n%s", want, data)
		}
	}
	info, err := os.Stat(view)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("view mode = %04o, want 0644: it is /etc/bash.bashrc inside the sandbox", perm)
	}
	if dir, err := os.Stat(filepath.Dir(view)); err != nil {
		t.Fatal(err)
	} else if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Fatalf("sandbox directory mode = %04o, want 0700", perm)
	}
}

func TestSandboxProfileRerendersTheShellStartupFileEveryBuild(t *testing.T) {
	// The tenant owns this directory and can delete or edit the file; a profile build is the
	// moment that has to be undone, because the bind is strict — a missing source keeps the
	// worker from starting, and a stale one would silently keep the terminal colourless.
	m, _, _ := managerFixture(t)
	tenant := fixtureTenant(t, m, "alice", 32100)
	if _, err := m.SandboxProfile(tenant); err != nil {
		t.Fatal(err)
	}
	view := tenantBashrcFile(tenant.DshHome)
	if err := os.WriteFile(view, []byte("# stale\n"), 0o644); err != nil {
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
		t.Fatalf("a stale shell startup file survived a profile build: %q", data)
	}
	if !strings.Contains(string(data), "--color=auto") {
		t.Fatalf("the rebuilt shell startup file lost its colour defaults:\n%s", data)
	}
}
