package sshworkspace

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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

// defaultMaxConns is how many sftp connections one mount gets when the deployment configures
// none. sshfs' own default is 1, which makes every session of an account queue behind the
// slowest reader on the mount (see sshfsArgs).
const defaultMaxConns = 4

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
	alias      []string
}

// privatePath checks every component before following any tenant-controlled path.
// A symlink (including a dangling one), hard-linked key, or exposed key fails closed.
func privatePath(workspace, name string) (bool, error) {
	if !filepath.IsAbs(workspace) || !Within(workspace, name) {
		return false, fmt.Errorf("identity outside workspace")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(name), string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("symlink in ssh path %s", current)
		}
		if current != name && !info.IsDir() {
			return false, fmt.Errorf("not a directory: %s", current)
		}
		if current != name && Within(filepath.Join(workspace, ".ssh"), current) && info.Mode().Perm()&0o077 != 0 {
			return false, fmt.Errorf("ssh directory must be private: %s", current)
		}
		if current == name {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
				return false, fmt.Errorf("identity must be a private regular file: %s", name)
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
				return false, fmt.Errorf("hard-linked identity: %s", name)
			}
		}
	}
	return true, nil
}

func pathsFor(o Options, remote Remote, host string) (sshPaths, error) {
	dir := filepath.Join(remote.Workspace, ".ssh")
	paths := sshPaths{knownHosts: filepath.Join(dir, "known_hosts"), config: filepath.Join(dir, "config")}
	digest := sha256.Sum256([]byte(host))
	for _, candidate := range []string{filepath.Join(dir, "host_keys", fmt.Sprintf("%x", digest), "id_rsa"), filepath.Join(dir, "id_rsa")} {
		exists, err := privatePath(remote.Workspace, candidate)
		if err != nil {
			return paths, Wrap(CodeAuthFailed, "unsafe ssh identity", err)
		}
		if exists {
			paths.key = candidate
			break
		}
	}
	if paths.key == "" {
		return paths, Errorf(CodeAuthFailed, "no ssh identity for %q; upload a host or default key", host)
	}
	// Keep known_hosts account-local even before ssh creates the file. Never fall
	// back to the gateway operator's HOME when account metadata is absent.
	for _, name := range []string{paths.config, paths.knownHosts} {
		info, err := os.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return paths, Wrap(CodeAuthFailed, "reading ssh metadata", err)
		}
		if !info.Mode().IsRegular() {
			return paths, Errorf(CodeAuthFailed, "unsafe ssh metadata path %s", name)
		}
	}
	var err error
	paths.alias, err = aliasOptions(paths, host)
	return paths, err
}

func isFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// aliasOptions deliberately ignores IdentityFile, Include, ProxyCommand and agents.
// -F /dev/null also excludes the gateway process's global and personal defaults.
var aliasValueRE = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)

func aliasOptions(paths sshPaths, host string) ([]string, error) {
	target, port, _ := SplitHostSpec(host)
	alias := target[strings.LastIndex(target, "@")+1:]
	explicitUser := strings.Contains(target, "@")
	data, err := securefile.ReadLimitedRegular(paths.config, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, Wrap(CodeHostUnknown, "reading account ssh config", err)
	}
	for _, h := range ParseSSHConfig(data) {
		if h.Name != alias {
			continue
		}
		var options []string
		if h.HostName != "" {
			if !aliasValueRE.MatchString(h.HostName) {
				return nil, Errorf(CodeHostUnknown, "invalid configured HostName")
			}
			options = append(options, "HostName="+h.HostName)
		}
		if !explicitUser && h.User != "" {
			if !aliasValueRE.MatchString(h.User) {
				return nil, Errorf(CodeHostUnknown, "invalid configured User")
			}
			options = append(options, "User="+h.User)
		}
		if port == 0 && h.invalidPort {
			return nil, Errorf(CodeHostUnknown, "invalid configured Port")
		}
		if port == 0 && h.Port > 0 {
			options = append(options, "Port="+strconv.Itoa(h.Port))
		}
		return options, nil
	}
	return nil, nil
}

// sshArgs assembles one ssh invocation. Nothing here is ever interpolated into a shell: the
// script is the single quoted-word-safe string the caller built, and the host is validated
// before it becomes an argument.
func (o Options) sshArgs(remote Remote, host, script string) []string {
	paths, err := pathsFor(o, remote, host)
	if err != nil {
		return nil
	}
	args := []string{"-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	for _, opt := range paths.alias {
		args = append(args, "-o", opt)
	}
	if paths.knownHosts != "" {
		args = append(args, "-o", "UserKnownHostsFile="+paths.knownHosts)
	}
	if paths.key != "" {
		args = append(args, "-i", paths.key)
	}
	if o.ConnectTimeout > 0 {
		args = append(args, "-o", "ConnectTimeout="+strconv.Itoa(int(o.ConnectTimeout.Seconds())))
	}
	// Only the identities this command names are offered, so an operator's agent cannot decide
	// which key authenticates a tenant's mount.
	args = append(args, "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes")
	// ssh joins the command arguments with spaces and hands the result to the remote user's
	// shell, which parses it again. The script therefore has to arrive already quoted as one
	// word: passing it raw made the remote `sh -c printf %s "$HOME"` (i.e. $0 = "%s", no
	// arguments) and every call failed with printf's usage message. Caught by
	// TestIntegrationMountOverLoopback, not by the fakes, which never re-parse.
	target, port, err := SplitHostSpec(host)
	if err != nil {
		return nil
	}
	if port > 0 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	return append(args, "--", target, "sh", "-c", ShellQuote(script))
}

// ssh runs one remote shell script.
func (o Options) ssh(ctx context.Context, run ExecFunc, remote Remote, host, script string) (string, error) {
	if err := o.permits(host); err != nil {
		return "", err
	}
	if _, err := pathsFor(o, remote, host); err != nil {
		return "", err
	}
	budget := o.ConnectTimeout
	if budget <= 0 || budget > sshCallBudget {
		budget = sshCallBudget
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	args := o.sshArgs(remote, host, script)
	if len(args) == 0 {
		return "", Errorf(CodeAuthFailed, "ssh identity became unavailable")
	}
	stdout, stderr, err := run(callCtx, o.sshBin(), args, nil)
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

// sshTarget resolves the account's alias chain into the destination ssh (and sshfs) is handed,
// plus the port that has to travel as its own flag.
//
// sshfs does not forward User as an SSH option (FUSE rejects it). Resolve the same validated
// alias values into its destination and dedicated port flag instead; never interpolate tenant
// values into ssh_command.
func (o Options) sshTarget(remote Remote, host string) (string, int, error) {
	paths, err := pathsFor(o, remote, host)
	if err != nil {
		return "", 0, err
	}
	target, port, err := SplitHostSpec(host)
	if err != nil {
		return "", 0, err
	}
	user, hostname := "", target
	if at := strings.LastIndex(target, "@"); at >= 0 {
		user, hostname = target[:at], target[at+1:]
	}
	for _, opt := range paths.alias {
		name, value, _ := strings.Cut(opt, "=")
		switch name {
		case "HostName":
			hostname = value
		case "User":
			user = value
		case "Port":
			port, _ = strconv.Atoi(value) // aliasOptions already validated the port.
		}
	}
	target = hostname
	if user != "" {
		target = user + "@" + hostname
	}
	return target, port, nil
}

// sshfsArgs assembles the mount command. allow_other is never added, and an operator cannot
// introduce it through configuration: the mount must stay usable by the mounting account
// alone (which is exactly the tenant worker's uid).
func (o Options) sshfsArgs(remote Remote, host, remotePath, mountpoint string) []string {
	paths, err := pathsFor(o, remote, host)
	if err != nil {
		return nil
	}
	options := []string{"ssh_command=ssh -F /dev/null"}
	target, port, err := o.sshTarget(remote, host)
	if err != nil {
		return nil
	}
	connections := false
	for _, opt := range o.SSHFSOptions {
		trimmed := strings.TrimSpace(opt)
		// allow_other/allow_root would let every account on the host reach this mount —
		// and every tenant worker shares one uid, so that is exactly the isolation this
		// feature must not break. Configuration cannot introduce them.
		lower := strings.ToLower(trimmed)
		if trimmed == "" || strings.ContainsAny(trimmed, ",\n\r") || strings.Contains(lower, "allow_other") || strings.Contains(lower, "allow_root") || strings.HasPrefix(lower, "identity") || strings.HasPrefix(lower, "identities") || strings.HasPrefix(lower, "ssh_command") || strings.HasPrefix(lower, "password") || strings.HasPrefix(lower, "batchmode") || strings.HasPrefix(lower, "preferredauthentications") {
			continue
		}
		if strings.HasPrefix(lower, "max_conns") {
			connections = true
		}
		options = append(options, trimmed)
	}
	// sshfs defaults to ONE connection per mount, and one mount serves every session of an
	// account. On a single channel a slow traversal — a recursive scan, a large checkout —
	// queues every other session's reads behind it, which is what "the workspace got stuck
	// when several sessions worked at once" looks like from the outside. A few connections
	// keep one busy session from starving the rest; a deployment that knows better can still
	// set max_conns itself, and the value is never taken from anywhere but configuration.
	if !connections {
		options = append(options, "max_conns="+strconv.Itoa(defaultMaxConns))
	}
	if paths.key != "" {
		options = append(options, "IdentityFile="+paths.key)
	}
	if paths.knownHosts != "" {
		options = append(options, "UserKnownHostsFile="+paths.knownHosts)
	}
	options = append(options, "StrictHostKeyChecking=accept-new", "BatchMode=yes", "IdentityAgent=none", "IdentitiesOnly=yes")
	if o.ConnectTimeout > 0 {
		options = append(options, "ConnectTimeout="+strconv.Itoa(int(o.ConnectTimeout.Seconds())))
	}
	args := []string{"-o", strings.Join(options, ",")}
	if port > 0 {
		// sshfs has its own -p flag for the ssh port; -o port= would be handed to ssh instead.
		args = append(args, "-p", strconv.Itoa(port))
	}
	return append(args, target+":"+remotePath, mountpoint)
}

// serviceMounted is the reader the FUSE connection probe uses. It is a variable for the
// same reason the service keeps a per-instance one: a test that states the mount table must
// be able to state it for the probe too.
var serviceMounted = mountedAt

// mountedAt reports the filesystem type mounted exactly at mountpoint, or "" when nothing
// is. /proc/self/mounts is read directly instead of shelling out to findmnt: it is always
// present, and it is the same table findmnt would format.
func mountedAt(mountpoint string) (string, error) {
	fstype := ""
	_ = eachMount(func(fields []string) bool {
		if len(fields) < 3 || decodeMountField(fields[1]) != mountpoint {
			return true
		}
		fstype = fields[2]
		return false
	})
	return fstype, nil
}

// eachMount walks one field-split line of /proc/self/mounts at a time. Returning false
// from visit stops the walk. A table that cannot be read is not an error worth
// propagating: the callers all treat "no entry" and "no table" the same way, and the
// table is present on every host this runs on.
func eachMount(visit func(fields []string) bool) error {
	file, err := os.Open("/proc/self/mounts")
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if !visit(fields) {
			return nil
		}
	}
	return scanner.Err()
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
	return o.detachOnce(ctx, run, mountpoint, false)
}

// detachOnce is one unmount attempt. force adds fusermount's lazy flag, which detaches a
// mount that is still in use: the kernel keeps the old superblock for whoever holds it and
// drops the entry from the mount table, which is what an account whose sandbox still has a
// mount point bound needs.
func (o Options) detachOnce(ctx context.Context, run ExecFunc, mountpoint string, force bool) (lazy bool, err error) {
	callCtx, cancel := context.WithTimeout(ctx, unmountBudget)
	defer cancel()
	args := []string{"-u", mountpoint}
	if force {
		args = []string{"-u", "-z", mountpoint}
	}
	_, stderr, runErr := run(callCtx, "fusermount3", args, nil)
	if runErr == nil {
		return force, nil
	}
	if force {
		return false, Wrap(CodeMountFailed, fmt.Sprintf("unmount %s failed: %s", mountpoint, tail(stderr)), runErr)
	}
	// The lazy retry needs -u: `fusermount3 -z` alone is refused ("can only be used with
	// -u"), so the two-flag form is what actually detaches a busy mount.
	_, lazyStderr, lazyErr := run(callCtx, "fusermount3", []string{"-u", "-z", mountpoint}, nil)
	if lazyErr == nil {
		return true, nil
	}
	return false, Wrap(CodeMountFailed, fmt.Sprintf("unmount %s failed: %s / %s", mountpoint, tail(stderr), tail(lazyStderr)), runErr)
}
