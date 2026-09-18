package sshworkspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSSHFSResolvedAlias(t *testing.T) {
	env := newTestEnv(t, Options{})
	config := "Host alias\n HostName real.example\n User configured\n Port 2200\n"
	if err := os.WriteFile(filepath.Join(env.remote.Workspace, ".ssh", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ host, destination, port string }{
		{"alias", "configured@real.example:/app", "2200"},
		{"explicit@alias", "explicit@real.example:/app", "2200"},
		{"alias:2222", "configured@real.example:/app", "2222"},
		{"explicit@alias:2222", "explicit@real.example:/app", "2222"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			args := (Options{}).sshfsArgs(env.remote, tc.host, "/app", "/mount")
			if len(args) < 6 {
				t.Fatalf("missing arguments: %v", args)
			}
			if want := []string{"-p", tc.port, tc.destination, "/mount"}; !reflect.DeepEqual(args[2:], want) {
				t.Fatalf("args = %v, want suffix %v", args, want)
			}
			for _, forbidden := range []string{"User=", "HostName=", "Port="} {
				if strings.Contains(args[1], forbidden) {
					t.Errorf("alias leaked into FUSE options: %v", args)
				}
			}
			if !strings.HasPrefix(args[1], "ssh_command=ssh -F /dev/null,") {
				t.Errorf("unsafe ssh command: %v", args)
			}
		})
	}
}
