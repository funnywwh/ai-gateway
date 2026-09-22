// Passwd view for a tenant sandbox. This file is pure: it renders bytes and hands them back,
// so the profile stays reviewable and the host file's reader and writer stay in the caller.
package sandbox

import (
	"fmt"
	"strings"
)

// homeField is the index of the home directory in a passwd line
// (name:passwd:uid:gid:gecos:dir:shell).
const homeField = 5

// RenderPasswd returns the passwd view a tenant's sandbox should see: the host file as it is,
// with every entry for workerUser pointing its home directory at home.
//
// Why a view exists at all: inside the sandbox HOME is the tenant workspace (the runner
// exports HOME=<workspace>), while /etc/passwd comes from the host and still names the
// deployment account's own home — a path the profile hides behind an empty tmpfs. Everything
// that resolves a home directory through getpwuid() then disagrees with $HOME, and OpenSSH is
// the visible one: it expands ~/.ssh/config, known_hosts and the default identity from passwd
// rather than from $HOME, so an alias list the gateway wrote to <workspace>/.ssh/config stays
// invisible and `ssh <alias>` degrades into a DNS lookup of the alias itself ("Could not
// resolve hostname ... Temporary failure in name resolution"). Rewriting this one field keeps
// every other lookup — uid to name, other accounts, the login shell — exactly as the host
// has it.
//
// The caller is responsible for reading the host file and for writing the result where the
// profile binds it (Tenant.PasswdFile); a file the tenant can rewrite only changes the
// name → home view inside its own sandbox, never a mount or a privilege.
func RenderPasswd(host []byte, workerUser, home string) ([]byte, error) {
	if strings.TrimSpace(workerUser) == "" {
		return nil, fmt.Errorf("passwd view needs the worker account name")
	}
	if !strings.HasPrefix(home, "/") {
		return nil, fmt.Errorf("passwd view needs an absolute home directory, got %q", home)
	}
	// A colon or a newline in the home field would forge extra fields or lines.
	if strings.ContainsAny(home, ":\n\r") {
		return nil, fmt.Errorf("passwd view: home directory %q has an unusable character", home)
	}
	lines := strings.Split(string(host), "\n")
	found := false
	for i, line := range lines {
		fields := strings.Split(line, ":")
		if len(fields) < homeField+2 || fields[0] != workerUser {
			continue
		}
		fields[homeField] = home
		lines[i] = strings.Join(fields, ":")
		found = true
	}
	if !found {
		return nil, fmt.Errorf("passwd view: account %q is not in the host passwd file", workerUser)
	}
	// Splitting and re-joining preserves every other byte, including the trailing newline
	// (a final empty element) and a file that does not end with one.
	return []byte(strings.Join(lines, "\n")), nil
}
