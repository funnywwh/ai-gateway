package sshworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/audit"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// RestartFunc restarts one tenant worker.
//
// A mount set cannot change under a running sandbox: the bubblewrap profile binds each mount
// point at worker start (and `--bind` does not carry later submounts), so a new mount is only
// visible inside the account's dsh after that worker starts again. The callback is injected
// rather than imported so this package stays independent of the tenancy manager.
type RestartFunc func(ctx context.Context, tenant string) error

// Service performs the mounts tenants ask for and keeps them recorded.
type Service struct {
	options Options
	store   *Store
	exec    ExecFunc
	restart RestartFunc
	audit   audit.Sink
	logger  *slog.Logger
	now     func() time.Time
	// mounted and lookPath are seams: the mount table and the PATH probe are host facts the
	// tests must be able to state, and both are read on every call rather than cached.
	mounted  func(mountpoint string) (string, error)
	lookPath func(file string) (string, error)

	binMu  sync.Mutex
	binErr error
	binOK  bool
}

// New assembles the service. A logger and an audit sink are optional; the store is not.
func New(options Options, store *Store, restart RestartFunc, sink audit.Sink, logger *slog.Logger) (*Service, error) {
	if store == nil {
		return nil, errors.New("sshworkspace: a state store is required")
	}
	if err := ValidateMountSubdir(options.MountSubdir); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		options:  options,
		store:    store,
		exec:     execDefault,
		restart:  restart,
		mounted:  mountedAt,
		lookPath: exec.LookPath,
		audit:    sink,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// Options exposes the resolved options (read-only use).
func (s *Service) Options() Options { return s.options }

// CheckBinaries fails when ssh or sshfs cannot be executed, so a misconfigured deployment
// says so at startup instead of at the first user click.
func (s *Service) CheckBinaries() error {
	s.binMu.Lock()
	defer s.binMu.Unlock()
	if s.binOK || s.binErr != nil {
		return s.binErr
	}
	if _, err := s.lookPath(s.options.sshBin()); err != nil {
		s.binErr = Wrap(CodeUnreachable, "ssh is not executable", err)
		return s.binErr
	}
	if _, err := s.lookPath(s.options.sshfsBin()); err != nil {
		s.binErr = Wrap(CodeSSHFSMissing, "sshfs is not executable: install it (sudo apt install -y sshfs)", err)
		return s.binErr
	}
	s.binOK = true
	return nil
}

// Mounts lists one account's recorded mounts.
func (s *Service) Mounts(tenant string) ([]Mount, error) { return s.store.ForTenant(tenant) }

// All returns every recorded mount.
func (s *Service) All() ([]Mount, error) { return s.store.Load() }

// EnsureIdentity provisions the per-account ssh material under <workspace>/.ssh: the private
// key (0600), an empty known_hosts to accept new host keys into (0600), and, when a source is
// configured, the alias list both halves resolve hosts with (0644).
//
// Nothing here is overwritten once present: an account's key may have been rotated by hand,
// and a provisioning step that silently reverts that would be a security bug, not a
// convenience. A missing identity is reported by the caller that needs it.
func (s *Service) EnsureIdentity(tenant, workspace, dshHome string) error {
	dir := filepath.Join(workspace, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "id_rsa")
	if !isFile(keyPath) {
		source := ""
		if s.options.IdentityDir != "" {
			candidate := filepath.Join(s.options.IdentityDir, tenant)
			if isFile(candidate) {
				source = candidate
			}
		}
		if source == "" {
			source = s.options.IdentitySource
		}
		if source == "" {
			return Errorf(CodeInvalidState, "no ssh identity for %s: set ssh_workspaces.identity_source or ssh_workspaces.identity_dir/<account>", tenant)
		}
		if err := securefile.CheckPermissions(source, 0o600); err != nil {
			return Wrap(CodeInvalidState, "ssh identity "+source+" must be a regular 0600 file", err)
		}
		data, err := securefile.ReadLimitedRegular(source, 64<<10)
		if err != nil {
			return Wrap(CodeInvalidState, "reading ssh identity "+source, err)
		}
		if err := securefile.WriteAtomic(keyPath, data, 0o600); err != nil {
			return err
		}
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	if !isFile(knownHosts) {
		if err := securefile.WriteAtomic(knownHosts, nil, 0o600); err != nil {
			return err
		}
	}
	configPath := filepath.Join(dir, "config")
	if !isFile(configPath) && s.options.SSHConfigSource != "" && isFile(s.options.SSHConfigSource) {
		data, err := securefile.ReadLimitedRegular(s.options.SSHConfigSource, 64<<10)
		if err != nil {
			return err
		}
		if err := securefile.WriteAtomic(configPath, data, 0o644); err != nil {
			return err
		}
	}
	// The account's own view of its mounts. Inside the sandbox a mount point is an ordinary
	// directory (mounting is kernel state the worker cannot see), so this file is what tells
	// the tenant plugin which mirrored paths are real mounts and which are just parent
	// directories of the mirror layout.
	return s.writeMirror(tenant, dshHome)
}

// writeMirror records one account's mounts inside that account's DSH home, where its own dsh
// can read them.
func (s *Service) writeMirror(tenant, dshHome string) error {
	if dshHome == "" {
		return nil
	}
	mounts, err := s.store.ForTenant(tenant)
	if err != nil {
		return err
	}
	document := struct {
		Version int     `json:"version"`
		Tenant  string  `json:"tenant"`
		Mounts  []Mount `json:"mounts"`
	}{Version: stateVersion, Tenant: tenant, Mounts: mounts}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return securefile.WriteAtomic(filepath.Join(dshHome, "ssh-mounts.json"), append(data, '\n'), 0o600)
}

// MountsFor lists the mount points the worker profile must bind for one account. A record
// that cannot be read yields nothing: an unbindable profile is worse than a missing mount.
func (s *Service) MountsFor(tenant string) []string {
	mounts, err := s.store.ForTenant(tenant)
	if err != nil {
		s.logger.Error("reading the ssh mount record failed", "tenant", tenant, "err", err)
		return nil
	}
	return Mountpoints(mounts)
}

// Open mounts one remote directory for an account and returns the mount record.
//
// The returned boolean reports whether the account's worker was restarted to make the mount
// visible; a caller that already holds a worker restart budget of its own can use it to
// explain the interruption to the user.
func (s *Service) Open(ctx context.Context, remote Remote, host, remotePath string) (Mount, bool, error) {
	if err := s.CheckBinaries(); err != nil {
		return Mount{}, false, err
	}
	if err := s.options.permits(host); err != nil {
		return Mount{}, false, err
	}
	if err := ValidateRemotePath(remotePath); err != nil {
		return Mount{}, false, err
	}
	if !filepath.IsAbs(remote.Workspace) || remote.Tenant == "" {
		return Mount{}, false, Errorf(CodeInvalidState, "tenant workspace is not resolved")
	}
	// Canonicalizing doubles as the reachability and existence probe: a host we cannot log
	// into and a directory that is not there both fail here, before anything is mounted.
	canonical, err := s.options.Canonical(ctx, s.exec, remote, host, remotePath)
	if err != nil {
		return Mount{}, false, err
	}
	mountpoint, err := s.options.MountpointFor(remote.Workspace, host, canonical)
	if err != nil {
		return Mount{}, false, err
	}
	if existing, found, err := s.store.Find(mountpoint); err != nil {
		return Mount{}, false, err
	} else if found {
		if fstype, _ := s.mounted(mountpoint); fstype != "" {
			return existing, false, nil
		}
	}
	if fstype, _ := s.mounted(mountpoint); fstype != "" && fstype != "fuse.sshfs" {
		return Mount{}, false, Errorf(CodeBusy, "%s already holds a %s mount", mountpoint, fstype)
	}
	if err := s.EnsureIdentity(remote.Tenant, remote.Workspace, remote.DshHome); err != nil {
		return Mount{}, false, err
	}
	if err := s.prepareMountpoint(remote.Workspace, mountpoint); err != nil {
		return Mount{}, false, err
	}
	if err := s.mount(ctx, remote, host, canonical, mountpoint); err != nil {
		return Mount{}, false, err
	}
	mount := Mount{
		Tenant:          remote.Tenant,
		Host:            host,
		Remote:          remotePath,
		CanonicalRemote: canonical,
		Mountpoint:      mountpoint,
		CreatedAt:       s.now(),
	}
	if _, err := s.store.Add(mount); err != nil {
		// The mount exists but is unrecorded: detach it rather than leave a mount nothing
		// knows about (the next worker start would silently expose it without a record).
		_, _ = s.options.unmount(ctx, s.exec, mountpoint)
		return Mount{}, false, err
	}
	if err := s.writeMirror(remote.Tenant, remote.DshHome); err != nil {
		s.logger.Error("writing the account mount mirror failed", "tenant", remote.Tenant, "err", err)
	}
	restarted := s.restartWorker(ctx, remote.Tenant)
	s.record("ssh-mount-open", remote.Tenant, mountpoint, host+":"+canonical, 200)
	return mount, restarted, nil
}

// Close detaches one of an account's mounts and forgets it.
func (s *Service) Close(ctx context.Context, tenant, dshHome, mountpoint string) (lazy bool, restarted bool, err error) {
	record, found, err := s.store.Find(mountpoint)
	if err != nil {
		return false, false, err
	}
	if !found {
		return false, false, Errorf(CodeNotMounted, "%s is not a mounted ssh workspace", mountpoint)
	}
	if record.Tenant != tenant {
		return false, false, Errorf(CodeForbidden, "%s belongs to another account", mountpoint)
	}
	lazy, err = s.detach(ctx, mountpoint, 3, 300*time.Millisecond)
	if err != nil {
		return lazy, false, err
	}
	if _, err := s.store.Remove(mountpoint); err != nil {
		return lazy, false, err
	}
	if _, statErr := os.Stat(mountpoint); statErr == nil {
		_ = os.Remove(mountpoint) // best effort: only succeeds while empty
	}
	if err := s.writeMirror(tenant, dshHome); err != nil {
		s.logger.Error("writing the account mount mirror failed", "tenant", tenant, "err", err)
	}
	restarted = s.restartWorker(ctx, tenant)
	s.record("ssh-mount-close", tenant, mountpoint, record.Host+":"+record.CanonicalRemote, 200)
	return lazy, restarted, nil
}

// detach unmounts one mount and waits for it to leave the mount table.
//
// The holder is usually the worker's own mount namespace: stopping or restarting a worker
// tears its sandbox down asynchronously, so a mount can stay attached for a moment after the
// process that used it is gone. Purging an account while a mount is still attached fails with
// EBUSY on the mount point — a confusing symptom far from its cause — so this waits, and says
// what is holding it when the wait runs out.
func (s *Service) detach(ctx context.Context, mountpoint string, attempts int, delay time.Duration) (bool, error) {
	lazy := false
	for attempt := 0; attempt < attempts; attempt++ {
		detached, err := s.options.unmount(ctx, s.exec, mountpoint)
		lazy = lazy || detached
		if err == nil {
			if fstype, _ := s.mounted(mountpoint); fstype == "" {
				// The table is not the whole truth: a sandbox that bound this mount keeps an
				// internal reference until its namespace is gone, and until then the mount
				// point cannot be removed (EBUSY) even though it no longer shows up here. The
				// directory is the observable that matches what a purge will actually hit.
				if removeErr := os.Remove(mountpoint); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
					return lazy, nil
				}
			}
		}
		if attempt < attempts-1 {
			select {
			case <-ctx.Done():
				return lazy, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	fstype, _ := s.mounted(mountpoint)
	return lazy, Errorf(CodeBusy, "%s is still mounted (%s): a session may still hold it open", mountpoint, fstype)
}

// DropTenant detaches every mount an account owns. It is called when an account is stopped or
// deleted, where no worker is left to restart.
func (s *Service) DropTenant(ctx context.Context, tenant string) error {
	mounts, err := s.store.ForTenant(tenant)
	if err != nil {
		return err
	}
	var failures []string
	for _, mount := range mounts {
		if _, unmountErr := s.detach(ctx, mount.Mountpoint, 20, 500*time.Millisecond); unmountErr != nil {
			failures = append(failures, unmountErr.Error())
			continue
		}
		if _, err := s.store.Remove(mount.Mountpoint); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		s.record("ssh-mount-drop", tenant, mount.Mountpoint, mount.Host+":"+mount.CanonicalRemote, 200)
	}
	if len(failures) > 0 {
		return Errorf(CodeMountFailed, "unmounting %s: %s", tenant, strings.Join(failures, "; "))
	}
	return nil
}

// Reconcile re-mounts every recorded mount that vanished, for the accounts it is given.
//
// Mounts do not survive a gateway restart, and they disappear with a crashed sshfs, so the
// record file is the source of truth. Failures are logged, never fatal: one unreachable
// remote host must not stop the gateway or the other accounts.
func (s *Service) Reconcile(ctx context.Context, remotes []Remote) {
	if err := s.CheckBinaries(); err != nil {
		s.logger.Warn("ssh workspaces disabled for this start", "err", err)
		return
	}
	mounts, err := s.store.Load()
	if err != nil {
		s.logger.Error("ssh mount record is unusable", "err", err)
		return
	}
	byTenant := map[string]Remote{}
	for _, remote := range remotes {
		byTenant[remote.Tenant] = remote
	}
	for _, mount := range mounts {
		remote, ok := byTenant[mount.Tenant]
		if !ok {
			continue
		}
		if fstype, _ := mountedAt(mount.Mountpoint); fstype != "" {
			continue
		}
		if err := s.prepareMountpoint(remote.Workspace, mount.Mountpoint); err != nil {
			s.logger.Error("ssh remount failed", "tenant", mount.Tenant, "mountpoint", mount.Mountpoint, "err", err)
			continue
		}
		if err := s.mount(ctx, remote, mount.Host, mount.CanonicalRemote, mount.Mountpoint); err != nil {
			s.logger.Error("ssh remount failed", "tenant", mount.Tenant, "mountpoint", mount.Mountpoint, "err", err)
			continue
		}
		s.logger.Info("ssh workspace remounted", "tenant", mount.Tenant, "mountpoint", mount.Mountpoint)
		s.record("ssh-mount-remount", mount.Tenant, mount.Mountpoint, mount.Host+":"+mount.CanonicalRemote, 200)
	}
	// The mirror is refreshed for every account, mounted or not: it is what the account's
	// plugin reads to tell a mount from a parent directory.
	for _, remote := range remotes {
		if err := s.writeMirror(remote.Tenant, remote.DshHome); err != nil {
			s.logger.Error("writing the account mount mirror failed", "tenant", remote.Tenant, "err", err)
		}
	}
}

// PollOnce services the mailbox of every running account and returns how many requests it
// handled. It is meant to be called on a timer; a failure for one account never stops the
// others.
func (s *Service) PollOnce(ctx context.Context, remotes []Remote) int {
	if err := s.CheckBinaries(); err != nil {
		return 0
	}
	handled := 0
	for _, remote := range remotes {
		requests, problems := ReadRequests(remote.DshHome)
		for _, problem := range problems {
			s.logger.Warn("ssh request skipped", "tenant", remote.Tenant, "err", problem)
		}
		for _, request := range requests {
			reply := s.HandleRequest(ctx, remote, request)
			if err := WriteReply(remote.DshHome, reply); err != nil {
				s.logger.Error("writing ssh reply failed", "tenant", remote.Tenant, "id", request.ID, "err", err)
			}
			if err := RemoveRequest(remote.DshHome, request.ID); err != nil {
				s.logger.Error("removing ssh request failed", "tenant", remote.Tenant, "id", request.ID, "err", err)
			}
			handled++
		}
	}
	return handled
}

// HandleRequest performs one mailbox request and shapes the outcome as a reply. It never
// returns an error: the reply is the channel a tenant can read.
func (s *Service) HandleRequest(ctx context.Context, remote Remote, request Request) Reply {
	reply := Reply{ID: request.ID, At: s.now(), Host: request.Host}
	switch request.Op {
	case RequestOpen:
		mount, restarted, err := s.Open(ctx, remote, request.Host, request.Remote)
		if err != nil {
			reply.Code = CodeOf(err)
			if reply.Code == "" {
				reply.Code = CodeMountFailed
			}
			reply.Error = err.Error()
			s.record("ssh-mount-open", remote.Tenant, "", request.Host+":"+request.Remote, 500)
			return reply
		}
		reply.OK = true
		reply.Mountpoint = mount.Mountpoint
		reply.Remote = mount.CanonicalRemote
		reply.Restarted = restarted
		return reply
	case RequestClose:
		mountpoint := request.Mountpoint
		if mountpoint == "" && request.Host != "" && request.Remote != "" {
			if canonical, err := s.options.Canonical(ctx, s.exec, remote, request.Host, request.Remote); err == nil {
				if candidate, err := s.options.MountpointFor(remote.Workspace, request.Host, canonical); err == nil {
					mountpoint = candidate
				}
			}
		}
		lazy, restarted, err := s.Close(ctx, remote.Tenant, remote.DshHome, mountpoint)
		if err != nil {
			reply.Code = CodeOf(err)
			if reply.Code == "" {
				reply.Code = CodeMountFailed
			}
			reply.Error = err.Error()
			s.record("ssh-mount-close", remote.Tenant, mountpoint, request.Host+":"+request.Remote, 500)
			return reply
		}
		reply.OK = true
		reply.Mountpoint = mountpoint
		reply.Lazy = lazy
		reply.Restarted = restarted
		return reply
	default:
		reply.Code = CodeInvalidPath
		reply.Error = fmt.Sprintf("unsupported operation %q", request.Op)
		return reply
	}
}

// prepareMountpoint creates the mount point and its two parents at 0700.
func (s *Service) prepareMountpoint(workspace, mountpoint string) error {
	subdir := filepath.Join(workspace, s.options.MountSubdir)
	hostDir := filepath.Dir(mountpoint)
	for _, dir := range []string{subdir, hostDir, mountpoint} {
		if !Within(workspace, dir) {
			return Errorf(CodeForbidden, "%s would leave the account workspace", dir)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Wrap(CodeMountFailed, "creating "+dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return Wrap(CodeMountFailed, "restricting "+dir, err)
		}
	}
	return nil
}

// mount runs sshfs and verifies that something is actually mounted afterwards; a mount that
// did not take is detached again instead of being recorded as working.
func (s *Service) mount(ctx context.Context, remote Remote, host, remotePath, mountpoint string) error {
	budget := s.options.ConnectTimeout
	if budget <= 0 || budget > sshfsCallBudget {
		budget = sshfsCallBudget
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	_, stderr, err := s.exec(callCtx, s.options.sshfsBin(), s.options.sshfsArgs(remote, host, remotePath, mountpoint), nil)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Wrap(CodeSSHFSMissing, "sshfs is not installed", err)
		}
		return Wrap(CodeMountFailed, fmt.Sprintf("sshfs %s:%s -> %s failed: %s", host, remotePath, mountpoint, tail(stderr)), err)
	}
	fstype, err := s.mounted(mountpoint)
	if err != nil {
		return Wrap(CodeMountFailed, "cannot read the mount table", err)
	}
	if !strings.HasPrefix(fstype, "fuse") {
		_, _ = s.options.unmount(context.WithoutCancel(ctx), s.exec, mountpoint)
		return Errorf(CodeMountFailed, "sshfs reported success but %s is not mounted", mountpoint)
	}
	return nil
}

// restartWorker restarts one account's worker and reports whether it did. A restart failure
// is logged and reported as "not restarted": the mount exists and the next worker start will
// pick it up, so this is a visibility problem, not a lost mount.
func (s *Service) restartWorker(ctx context.Context, tenant string) bool {
	if s.restart == nil {
		return false
	}
	if err := s.restart(ctx, tenant); err != nil {
		s.logger.Error("restarting the tenant worker failed; the mount will be visible after its next start", "tenant", tenant, "err", err)
		return false
	}
	return true
}

// record writes one audit event. Auditing never fails an operation: it is a diagnostic
// channel, and refusing a completed mount because a log line could not be written would be
// worse than the missing line.
func (s *Service) record(kind, tenant, path, reason string, status int) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Write(audit.Event{Kind: kind, Tenant: tenant, Path: path, Reason: reason, Status: status}); err != nil {
		s.logger.Error("audit write failed", "kind", kind, "tenant", tenant, "err", err)
	}
}

// Mountpoints is the convenience the worker profile needs: the paths to bind for one account.
func Mountpoints(mounts []Mount) []string {
	out := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		out = append(out, mount.Mountpoint)
	}
	return out
}
