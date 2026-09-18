package sshworkspace

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHostSpec(t *testing.T) {
	valid := []string{"gpt001", "aipc", "user@host", "user@10.0.0.1", "host.example.com", "h-1_2"}
	for _, spec := range valid {
		if err := ValidateHostSpec(spec); err != nil {
			t.Errorf("ValidateHostSpec(%q) = %v, want nil", spec, err)
		}
	}
	invalid := []string{"", "-oProxyCommand=x", "-host", "host name", "host\nname", "host/path", "../host", strings.Repeat("a", 300)}
	for _, spec := range invalid {
		if err := ValidateHostSpec(spec); err == nil {
			t.Errorf("ValidateHostSpec(%q) = nil, want an error", spec)
		}
	}
}

func TestValidateRemotePath(t *testing.T) {
	valid := []string{"/", "/home/winger", "/srv/app-1/子目录", "/a/b/..hidden"}
	for _, path := range valid {
		if err := ValidateRemotePath(path); err != nil {
			t.Errorf("ValidateRemotePath(%q) = %v, want nil", path, err)
		}
	}
	invalid := []string{"", "relative/path", "/a/../b", "/a//b", "/a/", "/a\nb", "/a\x00b"}
	for _, path := range invalid {
		if err := ValidateRemotePath(path); err == nil {
			t.Errorf("ValidateRemotePath(%q) = nil, want an error", path)
		}
	}
}

func TestValidateSegment(t *testing.T) {
	for _, name := range []string{"work", "a-b_c.d", "目录"} {
		if err := ValidateSegment(name); err != nil {
			t.Errorf("ValidateSegment(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "a\nb", strings.Repeat("x", 256)} {
		if err := ValidateSegment(name); err == nil {
			t.Errorf("ValidateSegment(%q) = nil, want an error", name)
		}
	}
}

func TestValidateMountSubdirRejectsHidden(t *testing.T) {
	if err := ValidateMountSubdir(".ssh"); err == nil {
		t.Fatal("ValidateMountSubdir(.ssh) = nil, want an error: a hidden container would be invisible in the picker")
	}
	if err := ValidateMountSubdir("ssh"); err != nil {
		t.Fatalf("ValidateMountSubdir(ssh) = %v, want nil", err)
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		root, candidate string
		want            bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/b/../c", false},
		{"", "/a", false},
		{"/a", "", false},
	}
	for _, tc := range cases {
		if got := Within(tc.root, tc.candidate); got != tc.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tc.root, tc.candidate, got, tc.want)
		}
	}
}

func TestMountpointForIsDeterministicAndContained(t *testing.T) {
	options := Options{MountSubdir: "ssh"}
	workspace := "/state/workspaces/dsh-colin"
	mountpoint, err := options.MountpointFor(workspace, "gpt001", "/opt/aigw")
	if err != nil {
		t.Fatalf("MountpointFor: %v", err)
	}
	want := filepath.Join(workspace, "ssh", "gpt001", "opt", "aigw")
	if mountpoint != want {
		t.Fatalf("MountpointFor = %q, want %q", mountpoint, want)
	}
	again, err := options.MountpointFor(workspace, "gpt001", "/opt/aigw")
	if err != nil || again != mountpoint {
		t.Fatalf("MountpointFor is not deterministic: %q / %v", again, err)
	}
	// A host spec can never contribute a path separator, a traversal cannot survive
	// validation, and the result is still checked against the workspace.
	for _, tc := range []struct{ host, remote string }{
		{"../escape", "/opt"},
		{"gpt001", "/../../etc"},
		{"gpt001", "relative"},
	} {
		if _, err := options.MountpointFor(workspace, tc.host, tc.remote); err == nil {
			t.Errorf("MountpointFor(%q, %q) = nil error, want refusal", tc.host, tc.remote)
		}
	}
	if _, err := options.MountpointFor("relative/workspace", "gpt001", "/opt"); err == nil {
		t.Error("MountpointFor accepted a relative workspace")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"plain":       "'plain'",
		"/a b":        "'/a b'",
		"it's":        `'it'\''s'`,
		"$HOME`id`":   "'$HOME`id`'",
		"a\nb":        "'a\nb'",
		"quote\"only": `'quote"only'`,
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSSHConfig(t *testing.T) {
	document := []byte(`
# leading comment
Host *
  User nobody

Host gpt001
    HostName gpt001.iotalking.top
    Port 2222
    User root
    # a nested comment

Host aipc !skipme
    HostName 192.168.140.252

Host gpt001
    HostName ignored-second-block

Host sz-test
    HostName 47.106.94.177   # trailing comment
`)
	hosts := ParseSSHConfig(document)
	if len(hosts) != 3 {
		t.Fatalf("parsed %d hosts, want 3: %+v", len(hosts), hosts)
	}
	if hosts[0].Name != "gpt001" || hosts[0].HostName != "gpt001.iotalking.top" || hosts[0].Port != 2222 || hosts[0].User != "root" {
		t.Errorf("first host = %+v", hosts[0])
	}
	if hosts[1].Name != "aipc" || hosts[1].HostName != "192.168.140.252" {
		t.Errorf("second host = %+v", hosts[1])
	}
	if hosts[2].HostName != "47.106.94.177" {
		t.Errorf("inline comment was not stripped: %+v", hosts[2])
	}
}

func TestPermitsUsesAllowList(t *testing.T) {
	open := Options{}
	if err := open.permits("anything"); err != nil {
		t.Fatalf("an empty allow-list must accept a valid spec, got %v", err)
	}
	closed := Options{Hosts: []string{"gpt001", "aipc"}}
	if err := closed.permits("gpt001"); err != nil {
		t.Fatalf("allow-listed host refused: %v", err)
	}
	if err := closed.permits("elsewhere"); CodeOf(err) != CodeHostUnknown {
		t.Fatalf("off-list host code = %q, want %q", CodeOf(err), CodeHostUnknown)
	}
	if err := closed.permits("-oProxyCommand=x"); CodeOf(err) != CodeHostUnknown {
		t.Fatalf("option-injection code = %q, want %q", CodeOf(err), CodeHostUnknown)
	}
}

func TestDecodeMountField(t *testing.T) {
	cases := map[string]string{
		`/plain/path`:           "/plain/path",
		`/with\040space`:        "/with space",
		`/with\011tab`:          "/with\ttab",
		`/trailing\134slash`:    `/trailing\slash`,
		`/incomplete\04`:        `/incomplete\04`,
		`/mixed\040and\134\134`: "/mixed and\\\\",
	}
	for in, want := range cases {
		if got := decodeMountField(in); got != want {
			t.Errorf("decodeMountField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSHFSArgsNeverAllowOther(t *testing.T) {
	options := Options{
		MountSubdir:    "ssh",
		SSHFSBin:       "sshfs",
		ConnectTimeout: 0,
		// An operator must not be able to widen the mount to other users, and an account
		// must never be able to read another account's remote tree through a shared mount.
		SSHFSOptions: []string{"reconnect", "allow_other", "allow_root"},
	}
	remote := newTestEnv(t, Options{}).remote
	args := options.sshfsArgs(remote, "gpt001", "/opt/app", "/state/workspaces/dsh-a/ssh/gpt001/opt/app")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "allow_other") || strings.Contains(joined, "allow_root") {
		t.Fatalf("sshfs arguments leak a cross-user option: %q", joined)
	}
	if !strings.Contains(joined, "reconnect") {
		t.Fatalf("configured options were dropped: %q", joined)
	}
	if args[len(args)-1] != "/state/workspaces/dsh-a/ssh/gpt001/opt/app" {
		t.Fatalf("mount point is not the last argument: %q", joined)
	}
	if args[len(args)-2] != "gpt001:/opt/app" {
		t.Fatalf("source is not the second to last argument: %q", joined)
	}
}

func TestSSHArgsAreHardened(t *testing.T) {
	options := Options{MountSubdir: "ssh", ConnectTimeout: 7_000_000_000}
	remote := newTestEnv(t, Options{}).remote
	args := options.sshArgs(remote, "gpt001", "true")
	joined := strings.Join(args, " ")
	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=accept-new", "ConnectTimeout=7", `-- gpt001 sh -c 'true'`} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh arguments %q lack %q", joined, want)
		}
	}
	// The script is handed to ssh as one already-quoted word: ssh joins the arguments with
	// spaces and the remote shell parses them again, so a raw script would be re-split.
	if last := args[len(args)-1]; last != ShellQuote("true") || last != "'true'" {
		t.Errorf("the remote script is not quoted: %q", last)
	}
	// A configured operator source must never participate in runtime selection.
	options.IdentitySource = "/etc/dshgw/ssh/id_rsa"
	if joined := strings.Join(options.sshArgs(remote, "gpt001", "true"), " "); strings.Contains(joined, options.IdentitySource) {
		t.Errorf("ssh arguments leak the configured source: %q", joined)
	}
}
