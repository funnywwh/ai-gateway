// Package hostshare binds operator-declared host directories into tenant workspaces.
//
// It is the local counterpart of the ssh workspace (M64): the same idea — an account gets a
// directory it can open as a workspace — without ssh, without sshfs and without FUSE. The
// worker's bubblewrap profile binds each declared directory at
// <workspace>/<subdir>/<name>, so inside the sandbox it is an ordinary directory: reads are
// local, inotify works, and none of the failure modes of a network file system exist.
//
// The local case is also where sshfs is most dangerous, which is why this exists at all. An
// sshfs mount of a directory that CONTAINS the mount point makes the mount contain itself: the
// account's workspace lives under the shared directory, so any recursive reader descends into
// its own copy until every request on the mount's single sftp channel is stuck, and the
// processes waiting on them sit in an uninterruptible (D) state that no signal can break.
// Measured on this host, 2026-09-21 — see docs/dshgw.md §7b. A share cannot do that: binding a
// directory is kernel state the sandbox builds once, and a scan that walks back into the
// workspace simply reads the same files again.
//
// Unlike the ssh half there is no mailbox request: a host directory is not the tenant's to
// choose. The deployment declares the list, the accounts it is for, and whether it is writable.
package hostshare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/sandbox"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// Declaration is one host directory as the deployment declared it.
type Declaration struct {
	Name     string
	Source   string
	ReadOnly bool
	// Tenants are the accounts that may see it. A share with no tenant is never bound.
	Tenants []string
}

// Options is the resolved configuration this service works from.
type Options struct {
	// Subdir is the container segment under each tenant workspace (host_shares.subdir).
	Subdir string
	// Declarations are the shares, in display order.
	Declarations []Declaration
}

// Service resolves declarations into per-tenant bindings and materializes their sandbox paths.
type Service struct {
	options Options
	logger  *slog.Logger
}

// New assembles the service. A deployment that does not configure shares gets no service at
// all, which is what leaves the tenancy hook nil.
func New(options Options, logger *slog.Logger) (*Service, error) {
	if strings.TrimSpace(options.Subdir) == "" {
		return nil, errors.New("hostshare: subdir is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{options: options, logger: logger}, nil
}

// Enabled reports whether the deployment declared any share.
func (s *Service) Enabled() bool { return s != nil && len(s.options.Declarations) > 0 }

// ContainerFor is the gateway-managed container inside one workspace.
func (s *Service) ContainerFor(workspace string) string {
	if s == nil || workspace == "" {
		return ""
	}
	return filepath.Join(workspace, s.options.Subdir)
}

// SharesFor lists the bindings one account's sandbox must carry. The target is computed from
// the account's own workspace, so a share can never name a path outside it.
func (s *Service) SharesFor(tenant, workspace string) []sandbox.HostShare {
	if !s.Enabled() || workspace == "" {
		return nil
	}
	container := s.ContainerFor(workspace)
	var out []sandbox.HostShare
	for _, declaration := range s.options.Declarations {
		if !visibleTo(declaration, tenant) {
			continue
		}
		out = append(out, sandbox.HostShare{
			Name:     declaration.Name,
			Source:   declaration.Source,
			Target:   filepath.Join(container, declaration.Name),
			ReadOnly: declaration.ReadOnly,
		})
	}
	return out
}

// visibleTo reports whether one account may see one share.
func visibleTo(declaration Declaration, tenant string) bool {
	for _, candidate := range declaration.Tenants {
		if candidate == tenant {
			return true
		}
	}
	return false
}

// Ensure creates the container and every target inside the workspace, and writes the account's
// mirror of what it may open. It runs before a worker starts, because the profile binds these
// paths at startup: a target that does not exist yet would be skipped by --bind-try and stay
// invisible until the next start.
//
// A declaration whose host directory disappeared is left out (with a line in the log) rather
// than failing the worker: the operator removed it, and that must not be the reason an account
// cannot start.
func (s *Service) Ensure(tenant, workspace, dshHome string) error {
	if !s.Enabled() || workspace == "" {
		return nil
	}
	container := s.ContainerFor(workspace)
	shares := s.SharesFor(tenant, workspace)
	live := make([]sandbox.HostShare, 0, len(shares))
	for _, share := range shares {
		info, err := os.Stat(share.Source)
		if err != nil {
			s.logger.Warn("a host share is not there; skipping it",
				"tenant", tenant, "share", share.Name, "source", share.Source, "err", err)
			continue
		}
		if !info.IsDir() {
			s.logger.Warn("a host share is not a directory; skipping it",
				"tenant", tenant, "share", share.Name, "source", share.Source)
			continue
		}
		// The container is created only for an account that actually has a share: an account
		// the deployment did not name gets no empty directory in its workspace.
		if err := ensurePrivateDir(container); err != nil {
			return err
		}
		if err := ensurePrivateDir(share.Target); err != nil {
			return err
		}
		live = append(live, share)
	}
	if dshHome == "" {
		return nil
	}
	// The mirror is what the account's own panel reads: the sandbox cannot tell a bound
	// directory from a real one, so the list has to come from the gateway that made it. A
	// failure to write it is logged, never fatal — the binding is already in the profile.
	if err := s.writeMirror(tenant, dshHome, live); err != nil {
		s.logger.Error("writing the account host share mirror failed", "tenant", tenant, "err", err)
	}
	return nil
}

// ensurePrivateDir creates one directory (and its parents) at 0700, the same mode the ssh
// workspace uses for its container.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restricting %s: %w", dir, err)
	}
	return nil
}

// mirror is the document the tenant-side panel reads.
type mirror struct {
	Version int           `json:"version"`
	Tenant  string        `json:"tenant"`
	Subdir  string        `json:"subdir"`
	Shares  []mirrorShare `json:"shares"`
}

// mirrorShare is one entry as the tenant sees it.
//
// The host path is deliberately absent: the account only needs the path inside its own
// workspace, and the mapping from share name to host directory is the operator's configuration,
// not something a neighbor's screen should reveal.
type mirrorShare struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

const mirrorVersion = 1

// writeMirror records the account's shares in its own DSH home.
func (s *Service) writeMirror(tenant, dshHome string, shares []sandbox.HostShare) error {
	document := mirror{Version: mirrorVersion, Tenant: tenant, Subdir: s.options.Subdir, Shares: []mirrorShare{}}
	for _, share := range shares {
		document.Shares = append(document.Shares, mirrorShare{Name: share.Name, Target: share.Target, ReadOnly: share.ReadOnly})
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(filepath.Join(dshHome, "host-shares.json"), append(data, '\n'), 0o600)
}
