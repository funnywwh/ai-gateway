package sshworkspace

import (
	"context"
	"errors"
	"path"
	"strings"
)

// Entry is one child of a listed remote directory.
type Entry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Hidden bool   `json:"hidden"`
}

// Listing is one level of a remote directory, shaped like the dsh directory-picker's own
// listing so the plugin can render the same kind of browser.
type Listing struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// Probe answers two questions at once: can this host be reached with the account's key, and
// where is that account's home directory remotely (the default place to start browsing).
func (o Options) Probe(ctx context.Context, run ExecFunc, remote Remote, host string) (string, error) {
	out, err := o.ssh(ctx, run, remote, host, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(out)
	if home == "" || !strings.HasPrefix(home, "/") {
		return "", Errorf(CodeCommandFail, "remote host reported no usable home directory")
	}
	return home, nil
}

// Canonical resolves a remote directory through symlinks. It is the de-duplication key: two
// spellings of one directory must map onto one mount point, or the same remote tree would be
// mounted twice under two workspaces.
func (o Options) Canonical(ctx context.Context, run ExecFunc, remote Remote, host, remotePath string) (string, error) {
	if err := ValidateRemotePath(remotePath); err != nil {
		return "", err
	}
	out, err := o.ssh(ctx, run, remote, host, "cd "+ShellQuote(remotePath)+" || exit 3\npwd -P")
	if err != nil {
		return "", err
	}
	canonical := strings.TrimSpace(out)
	if canonical == "" || !strings.HasPrefix(canonical, "/") {
		return "", Wrap(CodePathMissing, "remote path "+remotePath+" is not a readable directory", nil)
	}
	if err := ValidateRemotePath(canonical); err != nil {
		return "", Wrap(CodeInvalidPath, "remote host reported an unusable canonical path", err)
	}
	return canonical, nil
}

// ListDir lists the child directories of one remote directory.
//
// `ls -1ap` is the portable primitive here (GNU, busybox and BSD all accept it), and the
// trailing slash is what marks a directory. Only directories are returned: this list exists
// to pick a workspace, and the shipped picker behaves the same way. `cd` failing is how a
// missing or unreadable directory is reported, because ls would otherwise print the parent's
// contents when handed a file.
func (o Options) ListDir(ctx context.Context, run ExecFunc, remote Remote, host, remotePath string) (Listing, error) {
	if err := ValidateRemotePath(remotePath); err != nil {
		return Listing{}, err
	}
	max := o.MaxEntries
	if max <= 0 {
		max = 1000
	}
	out, err := o.ssh(ctx, run, remote, host, "cd "+ShellQuote(remotePath)+" || exit 3\nLC_ALL=C ls -1ap")
	if err != nil {
		return Listing{}, err
	}
	listing := Listing{Path: remotePath, Entries: []Entry{}}
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimRight(line, "\r")
		if !strings.HasSuffix(name, "/") {
			continue
		}
		// The marker comes off first: `ls -1ap` prints "./" and "../" with the same trailing
		// slash it puts on every directory, so comparing them before trimming let the two
		// pseudo-entries through as children. Caught by TestIntegrationMountOverLoopback.
		name = strings.TrimSuffix(name, "/")
		if name == "" || name == "." || name == ".." {
			continue
		}
		listing.Entries = append(listing.Entries, Entry{
			Name:   name,
			Path:   path.Join(remotePath, name),
			Hidden: strings.HasPrefix(name, "."),
		})
		if len(listing.Entries) >= max {
			listing.Truncated = true
			break
		}
	}
	return listing, nil
}

// Mkdir creates one directory level on the remote host. It is deliberately non-recursive:
// the caller names a single segment under a directory it just listed, so "create a missing
// subtree" is never what the user asked for and a typo cannot silently build a deep tree.
func (o Options) Mkdir(ctx context.Context, run ExecFunc, remote Remote, host, parent, name string) (string, error) {
	if err := ValidateRemotePath(parent); err != nil {
		return "", err
	}
	if err := ValidateSegment(name); err != nil {
		return "", err
	}
	target := path.Join(parent, name)
	_, err := o.ssh(ctx, run, remote, host, "mkdir -- "+ShellQuote(target))
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) && strings.Contains(strings.ToLower(failure.Message), "file exists") {
			return "", Errorf(CodeMkdirExists, "%s already exists on the remote host", target)
		}
		if errors.As(err, &failure) && failure.Code == CodeCommandFail {
			return "", Wrap(CodeMkdirFailed, "cannot create "+target+" on the remote host: "+failure.Message, failure)
		}
		return "", err
	}
	return target, nil
}
