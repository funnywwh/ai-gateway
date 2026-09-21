// Package sshworkspace turns a remote directory reachable over ssh into a directory inside
// one tenant's workspace, so that tenant's dsh can register it as a workspace.
//
// Division of labour (M64):
//
//   - The tenant-side dsh plugin (cmd/dshgw/plugin/ssh-workspace) does everything that only
//     needs ssh — listing remote directories, creating one, probing the host — with the
//     account's own key, running inside the tenant sandbox.
//   - This package does the one step a sandboxed worker cannot do: the sshfs mount. A tenant
//     worker's bubblewrap profile gives it a minimal /dev (no /dev/fuse) and, because the
//     host's root uid is unmapped inside its user namespace, no working setuid fusermount3
//     either — mount(2) is refused no matter which capabilities the profile grants.
//     Measured on the deployment host, see docs/design/m64-ssh-workspace.md §3.
//
// The two halves talk through a file mailbox inside the account's own DSH home
// (<dsh_home>/ssh-requests and <dsh_home>/ssh-replies): each side can only write its own
// directory, so no new listening surface, credential or token is introduced.
//
// Mounts live at <workspace>/<mount_subdir>/<host>/<remote path>. They are visible to that
// account alone for the same reason everything else is: the sandbox binds one tenant's
// workspace and nothing else, and the profile binds each active mount point explicitly.
package sshworkspace

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options is the configuration this package needs. It is deliberately decoupled from
// config.Config (the same choice internal/dshgw/sandbox makes) so the package stays
// trivially testable and cannot drift into depending on the whole configuration surface.
type Options struct {
	// MountSubdir is the single path segment under a tenant workspace that holds mounts.
	MountSubdir string
	// SSHBin and SSHFSBin are absolute paths or bare names resolved through PATH.
	SSHBin   string
	SSHFSBin string
	// IdentitySource is the private key copied into accounts that have no account-level key.
	IdentitySource string
	// IdentityDir holds per-account keys (<dir>/<tenant>) and wins over IdentitySource.
	IdentityDir string
	// SSHConfigDir holds one alias list per account (<dir>/<tenant>), copied to
	// <workspace>/.ssh/config for accounts that have none. It is the ONLY source of a
	// tenant's aliases: there is deliberately no host-wide source, because one shared list
	// is both a leak (every tenant would learn every host the operator knows) and a
	// coupling (one edit would decide for every tenant). Configuration validation refuses
	// a directory inside the deployment account's own ~/.ssh for the same reason.
	SSHConfigDir string
	// Hosts is an optional allow-list. Empty accepts any syntactically valid host spec.
	Hosts []string
	// ConnectTimeout bounds every ssh round-trip the gateway makes.
	ConnectTimeout time.Duration
	// MaxEntries bounds one remote directory listing.
	MaxEntries int
	// SSHFSOptions are passed verbatim to sshfs -o. allow_other is never added: the mount
	// must stay private to the mounting account.
	SSHFSOptions []string
}

// Remote identifies one tenant's writable roots, mirroring the fields the sandbox profile
// and the workspace registry use.
type Remote struct {
	Tenant    string
	Workspace string
	DshHome   string
}

// Error codes are stable strings: the tenant plugin shows them to the user, the
// integration script asserts on them, and the audit line records them.
const (
	CodeHostUnknown  = "ssh/host-unknown"
	CodeUnreachable  = "ssh/unreachable"
	CodeAuthFailed   = "ssh/auth-failed"
	CodeCommandFail  = "ssh/command-failed"
	CodeInvalidPath  = "ssh/invalid-path"
	CodePathMissing  = "ssh/path-not-found"
	CodeMkdirExists  = "ssh/mkdir-exists"
	CodeMkdirFailed  = "ssh/mkdir-failed"
	CodeForbidden    = "mount/forbidden"
	CodeSSHFSMissing = "mount/sshfs-missing"
	CodeMountFailed  = "mount/failed"
	CodeNotMounted   = "mount/not-mounted"
	CodeBusy         = "mount/busy"
	CodeInvalidState = "mount/invalid-state"
)

// Error is one business failure with a stable code.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an Error with a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a code and message to an underlying failure.
func Wrap(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// CodeOf reports the stable code of an error, or "" when it carries none.
func CodeOf(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// hostSpecRE is the one shape a host spec may take. It is deliberately narrow: the spec is
// handed to ssh as an argument, so a leading "-" (an option), a path separator (a mount
// escape) or whitespace (an argument split) must be impossible.
var hostSpecRE = regexp.MustCompile(`^[A-Za-z0-9._@][A-Za-z0-9._@:-]{0,254}$`)

// ValidateHostSpec rejects anything that must never reach an ssh command line.
func ValidateHostSpec(spec string) error {
	if !hostSpecRE.MatchString(spec) {
		return Errorf(CodeHostUnknown, "invalid host %q: expected an ssh alias or user@host", spec)
	}
	if strings.HasPrefix(spec, "-") {
		return Errorf(CodeHostUnknown, "invalid host %q: must not start with \"-\"", spec)
	}
	return nil
}

// SplitHostSpec separates an optional ":port" suffix from a host spec.
//
// It exists because ssh takes the port as a flag ("ssh -p 2222 host"), not as part of the
// destination: a person who types `gpt001:2222` means the same thing but ssh would look up a
// host literally named "gpt001:2222". IPv6 literals are not supported yet. The port stays in
// the recorded spec, so the mount point keeps naming what was asked for.
func SplitHostSpec(spec string) (target string, port int, err error) {
	if err := ValidateHostSpec(spec); err != nil {
		return "", 0, err
	}
	if strings.Count(spec, ":") > 1 {
		return "", 0, Errorf(CodeHostUnknown, "host %q contains multiple colons; IPv6 is not supported", spec)
	}
	target = spec
	index := strings.LastIndex(spec, ":")
	if index < 0 {
		return target, 0, nil
	}
	rawPort := spec[index+1:]
	if rawPort == "" {
		return "", 0, Errorf(CodeHostUnknown, "host %q ends with \":\" but names no port", spec)
	}
	parsed, convErr := strconv.Atoi(rawPort)
	if convErr != nil {
		return "", 0, Errorf(CodeHostUnknown, "host %q ends with a non-numeric port %q", spec, rawPort)
	}
	if parsed < 1 || parsed > 65535 {
		return "", 0, Errorf(CodeHostUnknown, "host %q names port %d outside 1..65535", spec, parsed)
	}
	head := spec[:index]
	if head == "" {
		return "", 0, Errorf(CodeHostUnknown, "host %q names no destination before its port", spec)
	}
	return head, parsed, nil
}

// Host is one usable target parsed from an ssh config file.
type Host struct {
	Name        string
	HostName    string
	User        string
	Port        int
	invalidPort bool
}

// ParseSSHConfig extracts the concrete aliases of one ssh config document.
//
// Wildcard and negated patterns (Host *, Host !bad) are skipped rather than expanded: this
// list exists to offer a choice, not to reimplement ssh's matching, and a name a person
// cannot type is not a choice. The first block that names an alias wins, matching ssh.
func ParseSSHConfig(data []byte) []Host {
	var hosts []Host
	seen := map[string]bool{}
	current := -1
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		split := strings.IndexAny(line, " \t=")
		if split < 0 {
			continue
		}
		key, value := line[:split], strings.TrimLeft(line[split:], " \t=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(stripInlineComment(value))
		if key == "host" {
			current = -1
			for _, pattern := range strings.Fields(value) {
				if strings.ContainsAny(pattern, "*?!") || pattern == "" {
					continue
				}
				if seen[pattern] {
					continue
				}
				seen[pattern] = true
				hosts = append(hosts, Host{Name: pattern})
				current = len(hosts) - 1
				break
			}
			continue
		}
		if current < 0 {
			continue
		}
		switch key {
		case "hostname":
			hosts[current].HostName = value
		case "user":
			hosts[current].User = value
		case "port":
			port, err := strconv.Atoi(value)
			hosts[current].invalidPort = err != nil || strings.Trim(value, "0123456789") != "" || port < 1 || port > 65535
			hosts[current].Port = port
		}
	}
	return hosts
}

// stripInlineComment drops a trailing "# comment" the way ssh does, but only when the "#"
// starts a word: a value that legitimately contains one (a path, a user name) is left alone.
func stripInlineComment(value string) string {
	if idx := strings.Index(value, " #"); idx >= 0 {
		return value[:idx]
	}
	if idx := strings.Index(value, "\t#"); idx >= 0 {
		return value[:idx]
	}
	return value
}

// ValidateRemotePath accepts only an absolute, control-character-free path with no parent
// traversal. Everything that reaches a remote shell goes through this first.
func ValidateRemotePath(p string) error {
	if p == "" {
		return Errorf(CodeInvalidPath, "remote path is empty")
	}
	if len(p) > 4096 {
		return Errorf(CodeInvalidPath, "remote path is longer than 4096 bytes")
	}
	if !strings.HasPrefix(p, "/") {
		return Errorf(CodeInvalidPath, "remote path %q is not absolute", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return Errorf(CodeInvalidPath, "remote path contains a control character")
		}
	}
	for _, element := range strings.Split(p, "/") {
		if element == ".." {
			return Errorf(CodeInvalidPath, "remote path %q must not contain \"..\"", p)
		}
	}
	if cleaned := path.Clean(p); cleaned != p {
		return Errorf(CodeInvalidPath, "remote path %q is not in normal form (want %q)", p, cleaned)
	}
	return nil
}

// ValidateSegment accepts one path segment a caller proposes to create.
func ValidateSegment(name string) error {
	if name == "" || len(name) > 255 {
		return Errorf(CodeInvalidPath, "name must be 1..255 bytes")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return Errorf(CodeInvalidPath, "%q is not a single path segment", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return Errorf(CodeInvalidPath, "name contains a control character")
		}
	}
	return nil
}

// ValidateMountSubdir accepts the one configured path segment that holds mounts.
func ValidateMountSubdir(subdir string) error {
	if err := ValidateSegment(subdir); err != nil {
		return err
	}
	if strings.HasPrefix(subdir, ".") {
		return Errorf(CodeInvalidState, "mount_subdir %q must not be hidden", subdir)
	}
	return nil
}

// Within reports whether candidate is root itself or sits inside it. Both paths must be
// absolute and cleaned; the comparison is lexical, exactly like the sandbox profile's, so a
// registry record can never widen what gets bound or mounted.
func Within(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// MountpointFor maps one remote directory onto the account's own workspace. The mapping is
// deterministic (the same remote directory always yields the same mount point) and total
// (it can never escape the workspace).
func (o Options) MountpointFor(workspace, host, remote string) (string, error) {
	if err := ValidateMountSubdir(o.MountSubdir); err != nil {
		return "", err
	}
	if err := ValidateHostSpec(host); err != nil {
		return "", err
	}
	if err := ValidateRemotePath(remote); err != nil {
		return "", err
	}
	if !filepath.IsAbs(workspace) {
		return "", Errorf(CodeInvalidState, "workspace %q is not absolute", workspace)
	}
	mountpoint := filepath.Join(workspace, o.MountSubdir, host, filepath.FromSlash(strings.TrimPrefix(remote, "/")))
	if !Within(workspace, mountpoint) || mountpoint == workspace {
		return "", Errorf(CodeForbidden, "mount point %q would leave the account workspace %q", mountpoint, workspace)
	}
	return mountpoint, nil
}

// ShellQuote renders s as exactly one POSIX shell word. Remote commands are assembled as
// `sh -c <script>` strings, so every interpolation point must go through this.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// AllowList reports whether a host spec is permitted by the configured list.
func (o Options) AllowList() []string {
	out := make([]string, 0, len(o.Hosts))
	for _, host := range o.Hosts {
		if strings.TrimSpace(host) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(host))
	}
	sort.Strings(out)
	return out
}

// permits reports whether spec is acceptable: syntactically valid, and either the allow-list
// is empty or the spec is on it. The list is a guard rail, not a boundary — the key an
// account holds is what really decides where it may go (see docs/dshgw.md).
func (o Options) permits(spec string) error {
	if _, _, err := SplitHostSpec(spec); err != nil {
		return err
	}
	if len(o.Hosts) == 0 {
		return nil
	}
	for _, allowed := range o.Hosts {
		if strings.TrimSpace(allowed) == spec {
			return nil
		}
	}
	return Errorf(CodeHostUnknown, "host %q is not in ssh_workspaces.hosts", spec)
}
