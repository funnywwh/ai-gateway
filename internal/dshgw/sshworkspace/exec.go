package sshworkspace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExecFunc runs one external command and returns its raw streams. Every external effect in
// this package goes through it, so the tests drive the whole open/close flow without ssh,
// sshfs or a real mount.
type ExecFunc func(ctx context.Context, name string, args []string, env []string) (stdout, stderr []byte, err error)

// execDefault is the production ExecFunc.
func execDefault(ctx context.Context, name string, args []string, env []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

// limits bounds one external call so a wedged ssh or a hung FUSE mount cannot pin the
// gateway. ssh itself is bounded by ConnectTimeout; this is the outer stop.
const (
	sshCallBudget    = 30 * time.Second
	sshfsCallBudget  = 45 * time.Second
	unmountBudget    = 20 * time.Second
	commandTailBytes = 400
)

// tail keeps the last bytes of a stream: ssh failures put the reason at the end.
func tail(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) <= commandTailBytes {
		return text
	}
	return "…" + text[len(text)-commandTailBytes:]
}

// classifySSH maps an ssh failure onto a stable code. The mapping is by message because ssh
// reports business failures on stderr with exit status 255 — there is no structured channel.
func classifySSH(stderr []byte, err error) *Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Wrap(CodeUnreachable, "ssh timed out", err)
	}
	text := strings.ToLower(string(stderr))
	switch {
	case strings.Contains(text, "permission denied"),
		strings.Contains(text, "publickey"),
		strings.Contains(text, "host key verification failed"),
		strings.Contains(text, "too many authentication failures"):
		return Wrap(CodeAuthFailed, "ssh authentication failed: "+tail(stderr), err)
	case strings.Contains(text, "could not resolve hostname"):
		return Wrap(CodeHostUnknown, "ssh could not resolve the host: "+tail(stderr), err)
	case strings.Contains(text, "connection refused"),
		strings.Contains(text, "connection timed out"),
		strings.Contains(text, "no route to host"),
		strings.Contains(text, "network is unreachable"),
		strings.Contains(text, "connection closed"):
		return Wrap(CodeUnreachable, "ssh cannot reach the host: "+tail(stderr), err)
	}
	return Wrap(CodeCommandFail, "ssh failed: "+tail(stderr), err)
}

// sshPaths are the per-account files ssh and sshfs are pointed at. They live inside the
// account's own workspace (<workspace>/.ssh), which is also the worker's HOME, so the
// tenant-side plugin reads exactly the same identity the gateway mounts with.
type sshPaths struct {
	key        string
	knownHosts string
	config     string
}

func pathsFor(o Options, remote Remote) sshPaths {
	dir := filepath.Join(remote.Workspace, ".ssh")
	paths := sshPaths{
		key:        filepath.Join(dir, "id_rsa"),
		knownHosts: filepath.Join(dir, "known_hosts"),
		config:     filepath.Join(dir, "config"),
	}
	if !isFile(paths.key) && o.IdentitySource != "" {
		paths.key = o.IdentitySource
	}
	if !isFile(paths.config) {
		paths.config = ""
	}
	if !isFile(paths.knownHosts) {
		paths.knownHosts = ""
	}
	return paths
}

func isFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// sshArgs assembles one ssh invocation. Nothing here is ever interpolated into a shell: the
// script is the single quoted-word-safe string the caller built, and the host is validated
// before it becomes an argument.
func (o Options) sshArgs(remote Remote, host, script string) []string {
	paths := pathsFor(o, remote)
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	if paths.knownHosts != "" {
		args = append(args, "-o", "UserKnownHostsFile="+paths.knownHosts)
	}
	if paths.config != "" {
		args = append(args, "-F", paths.config)
	}
	if paths.key != "" {
		args = append(args, "-i", paths.key)
	}
	if o.ConnectTimeout > 0 {
		args = append(args, "-o", "ConnectTimeout="+strconv.Itoa(int(o.ConnectTimeout.Seconds())))
	}
	return append(args, "--", host, "sh", "-c", script)
}

// ssh runs one remote shell script.
func (o Options) ssh(ctx context.Context, run ExecFunc, remote Remote, host, script string) (string, error) {
	if err := o.permits(host); err != nil {
		return "", err
	}
	budget := o.ConnectTimeout
	if budget <= 0 || budget > sshCallBudget {
		budget = sshCallBudget
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	stdout, stderr, err := run(callCtx, o.sshBin(), o.sshArgs(remote, host, script), nil)
	if err != nil {
		return "", classifySSH(stderr, err)
	}
	return string(stdout), nil
}

func (o Options) sshBin() string {
	if strings.TrimSpace(o.SSHBin) != "" {
		return o.SSHBin
	}
	return "ssh"
}

func (o Options) sshfsBin() string {
	if strings.TrimSpace(o.SSHFSBin) != "" {
		return o.SSHFSBin
	}
	return "sshfs"
}

// sshfsArgs assembles the mount command. allow_other is never added, and an operator cannot
// introduce it through configuration: the mount must stay usable by the mounting account
// alone (which is exactly the tenant worker's uid).
func (o Options) sshfsArgs(remote Remote, host, remotePath, mountpoint string) []string {
	paths := pathsFor(o, remote)
	options := []string{}
	for _, opt := range o.SSHFSOptions {
		trimmed := strings.TrimSpace(opt)
		// allow_other/allow_root would let every account on the host reach this mount —
		// and every tenant worker shares one uid, so that is exactly the isolation this
		// feature must not break. Configuration cannot introduce them.
		if trimmed == "" || strings.Contains(trimmed, "allow_other") || strings.Contains(trimmed, "allow_root") {
			continue
		}
		options = append(options, trimmed)
	}
	if paths.key != "" {
		options = append(options, "IdentityFile="+paths.key)
	}
	if paths.knownHosts != "" {
		options = append(options, "UserKnownHostsFile="+paths.knownHosts)
	}
	if paths.config != "" {
		options = append(options, "ssh_command=ssh -F "+paths.config)
	}
	options = append(options, "StrictHostKeyChecking=accept-new")
	if o.ConnectTimeout > 0 {
		options = append(options, "ConnectTimeout="+strconv.Itoa(int(o.ConnectTimeout.Seconds())))
	}
	args := []string{"-o", strings.Join(options, ",")}
	return append(args, host+":"+remotePath, mountpoint)
}

// mountedAt reports the filesystem type mounted exactly at mountpoint, or "" when nothing
// is. /proc/self/mounts is read directly instead of shelling out to findmnt: it is always
// present, and it is the same table findmnt would format.
func mountedAt(mountpoint string) (string, error) {
	file, err := os.Open("/proc/self/mounts")
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if decodeMountField(fields[1]) == mountpoint {
			return fields[2], nil
		}
	}
	return "", scanner.Err()
}

// decodeMountField undoes the kernel's octal escaping (\040 for a space, \011, \012, \134).
func decodeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if value, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(value))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}

// unmount detaches one FUSE mount, falling back to a lazy detach when the filesystem is
// busy (a session inside the account may still hold it open).
func (o Options) unmount(ctx context.Context, run ExecFunc, mountpoint string) (lazy bool, err error) {
	callCtx, cancel := context.WithTimeout(ctx, unmountBudget)
	defer cancel()
	_, stderr, runErr := run(callCtx, "fusermount3", []string{"-u", mountpoint}, nil)
	if runErr == nil {
		return false, nil
	}
	_, lazyStderr, lazyErr := run(callCtx, "fusermount3", []string{"-z", mountpoint}, nil)
	if lazyErr == nil {
		return true, nil
	}
	return false, Wrap(CodeMountFailed, fmt.Sprintf("unmount %s failed: %s / %s", mountpoint, tail(stderr), tail(lazyStderr)), runErr)
}
