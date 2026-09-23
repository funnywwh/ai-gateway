package nodedep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
)

// localExec runs one command on the control plane itself (ssh-keygen for fingerprints). It is a
// variable so tests can drive the fingerprint path without the real binary.
var localExec = func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return []byte(out.String()), []byte(errBuf.String()), err
}

// SSHDir is where a node's deployment identity lives on the control plane.
func SSHDir(stateDir, node string) string { return filepath.Join(stateDir, "node-ssh", node) }

// KnownHostsPath is the pinned known_hosts for one node. It is per node on purpose: one file per
// deployment target means a pinned host key cannot be confused with another machine's.
func KnownHostsPath(stateDir, node string) string {
	return filepath.Join(SSHDir(stateDir, node), "known_hosts")
}

// KeyPathFor is where a pasted private key is stored.
func KeyPathFor(stateDir, node string) string {
	return filepath.Join(SSHDir(stateDir, node), "id_ed25519")
}

// NewSSHExec runs commands on the target with the system ssh binary.
//
// The options are the ones that make a deploy predictable: no user ssh configuration at all
// (`-F /dev/null`), no agent and no password prompts (`-o BatchMode=yes -o IdentitiesOnly=yes`),
// our own known_hosts (never the operator's), and a host-key policy that is either "accept-new"
// before the operator has confirmed the key or "yes" afterwards — never "no".
func NewSSHExec(target nodestore.Node) ExecFunc {
	return func(ctx context.Context, command string, stdin io.Reader) ([]byte, []byte, error) {
		args := SSHArgs(target, command)
		cmd := exec.CommandContext(ctx, "ssh", args...)
		if stdin != nil {
			cmd.Stdin = stdin
		} else {
			cmd.Stdin = strings.NewReader("")
		}
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return []byte(stdout.String()), []byte(stderr.String()), err
	}
}

// SSHArgs builds the argv for one remote command. It is exported for the acceptance script and for
// the tests that assert the security-relevant options are present.
func SSHArgs(target nodestore.Node, command string) []string {
	knownHosts := strings.TrimSpace(target.SSH.KnownHostsFile)
	if knownHosts == "" {
		knownHosts = os.DevNull
	}
	policy := "accept-new"
	if strings.TrimSpace(target.SSH.HostKeyFingerprint) != "" {
		policy = "yes"
	}
	port := target.SSH.Port
	if port <= 0 {
		port = 22
	}
	args := []string{
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=" + policy,
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		"-p", fmt.Sprint(port),
	}
	if key := strings.TrimSpace(target.SSH.KeyFile); key != "" {
		args = append(args, "-i", key)
	}
	args = append(args, target.SSH.User+"@"+target.SSH.Host)
	// The remote side runs a login shell with the command as its script: the scripts this package
	// generates are multi-line, and `sh -c` keeps them working on hosts whose default shell is not
	// POSIX.
	args = append(args, "sh -c "+shellQuote(command))
	return args
}

// HostKeyFingerprint reads the SHA256 fingerprint of a host key from a known_hosts file.
//
// It runs `ssh-keygen -lf` locally: the key itself arrived over the first connection (which is why
// the first deploy's policy is accept-new), and this is what the operator is asked to confirm.
func HostKeyFingerprint(ctx context.Context, knownHostsFile, host string) (string, error) {
	if _, err := os.Stat(knownHostsFile); err != nil {
		return "", fmt.Errorf("no known_hosts yet for %s: %w", host, err)
	}
	stdout, stderr, err := localExec(ctx, "ssh-keygen", "-lf", knownHostsFile, "-E", "sha256", "-F", host)
	if err != nil {
		// -F matches on the exact host name; a machine reached by address is stored that way, so
		// fall back to reading the whole file's fingerprints and taking the first.
		stdout, stderr, err = localExec(ctx, "ssh-keygen", "-lf", knownHostsFile, "-E", "sha256")
		if err != nil {
			return "", fmt.Errorf("read the host key fingerprint: %w: %s", err, strings.TrimSpace(string(stderr)))
		}
	}
	// The two output shapes differ: `-lf -F host` prints "<host> <type> SHA256:…" while `-lf file`
	// prints "<bits> SHA256:… <host> (<type>)". Scanning every field for the fingerprint handles
	// both, and a comment line ("# Host … found: line N") has no field that looks like one.
	for _, line := range strings.Split(string(stdout), "\n") {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "SHA256:") {
				return field, nil
			}
		}
	}
	return "", errors.New("the known_hosts file holds no SHA256 fingerprint")
}

// StoreFingerprintHint returns the fingerprint a node record already pins, if any.
func StoreFingerprintHint(target nodestore.Node) string {
	return strings.TrimSpace(target.SSH.HostKeyFingerprint)
}

// PrepareSSHDir creates the per-node ssh directory with private permissions before anything is
// written into it (a private key and a pinned known_hosts live here).
func PrepareSSHDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// DefaultSSHTimeout is the per-command bound a deploy uses when the caller sets none.
const DefaultSSHTimeout = 10 * time.Minute
