package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/session"
)

func TestSharedRootTraversal(t *testing.T) {
	root := t.TempDir()
	for _, mode := range []os.FileMode{0o700, 0o750, 0o770, 0o777} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := checkSharedTraversal(root); err == nil {
			t.Fatalf("unsafe/unsearchable shared root %04o accepted", mode)
		}
	}
	for _, mode := range []os.FileMode{0o711, 0o751, 0o755} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := checkSharedTraversal(root); err != nil {
			t.Fatalf("shared root %04o refused: %v", mode, err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := checkSharedTraversal(link); err == nil {
		t.Fatal("symlink shared root accepted")
	}
}

func deploymentFile(t *testing.T, relative string) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(here)))
	data, err := os.ReadFile(filepath.Join(root, "deploy", "dshgw", relative))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInstallerGivesWorkersSearchWithoutGatewayMembership(t *testing.T) {
	text := deploymentFile(t, "install.sh")
	for _, want := range []string{
		"install -d -o root -g dshgw -m 0751 /var/lib/dshgw\n",
		"install -d -o root -g root -m 0711 /var/lib/dshgw/tenants\n",
		"install -d -o dshgw -g dshgw -m 0700 /var/lib/dshgw/gateway\n",
		"install -d -o root -g dshgw -m 0750 /var/lib/dshgw/handshake /etc/dshgw /etc/dshgw/tenants\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("installer missing shared/private permission contract: %s", want)
		}
	}
}

func TestServerAndLifecycleUseConfiguredSessionCapacity(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(cfg, []byte("state_dir: "+filepath.Join(root, "state")+"\nmax_sessions: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &cli{configPath: cfg}
	deps, err := app.loadRuntime(true) // shared by serve and lifecycle commands
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.manager.Sessions.Issue("alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.manager.Sessions.Issue("bob", time.Hour); !errors.Is(err, session.ErrCapacity) {
		t.Fatalf("configured session limit ignored: %v", err)
	}
}

func TestGatewayNamespaceDoesNotPinAtomicStateFiles(t *testing.T) {
	unit := deploymentFile(t, "dshgw.service")
	var readOnly, readWrite []string
	for _, line := range strings.Split(unit, "\n") {
		if rest, ok := strings.CutPrefix(line, "ReadOnlyPaths="); ok {
			readOnly = append(readOnly, strings.Fields(rest)...)
		}
		if rest, ok := strings.CutPrefix(line, "ReadWritePaths="); ok {
			readWrite = append(readWrite, strings.Fields(rest)...)
		}
	}
	if strings.Join(readOnly, " ") != "/etc/dshgw /var/lib/dshgw" {
		t.Fatalf("state must be mounted by directory, not pinned per file: %v", readOnly)
	}
	if len(readWrite) != 1 || readWrite[0] != "/var/lib/dshgw/gateway" {
		t.Fatalf("only gateway mutable state should be writable: %v", readWrite)
	}
	if !strings.Contains(unit, "ProtectSystem=strict") {
		t.Fatal("filesystem protection was removed instead of fixing mount granularity")
	}
}
