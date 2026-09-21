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
	"strconv"
	"strings"
	"sync"
	"syscall"
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
// key (0600), an empty known_hosts to accept new host keys into (0600), and the account's own
// alias list (0644), seeded from <ssh_config_dir>/<account> when that account has none yet.
//
// The alias list is the account's from the moment it exists: the tenant plugin adds and
// removes entries in it (「我的主机」) and may also edit it by hand. Nothing here is
// overwritten once present — an account's key may have been rotated by hand, and a
// provisioning step that silently reverted the alias list would delete hosts the account
// added. To re-seed one account: replace <ssh_config_dir>/<account>, delete
// <workspace>/.ssh/config, then restart that account's worker.
//
// A missing identity is reported by the caller that needs it.
func (s *Service) EnsureIdentity(tenant, workspace, dshHome string) error {
	dir := filepath.Join(workspace, ".ssh")
	if _, err := privatePath(workspace, filepath.Join(dir, "id_rsa")); err != nil {
		return Wrap(CodeInvalidState, "unsafe ssh identity path", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "id_rsa")
	_, managedErr := os.Lstat(filepath.Join(dir, "identity-managed"))
	if managedErr != nil && !os.IsNotExist(managedErr) {
		return managedErr
	}
	if !isFile(keyPath) && os.IsNotExist(managedErr) {
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
		if source != "" {
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
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	if !isFile(knownHosts) {
		if err := securefile.WriteAtomic(knownHosts, nil, 0o600); err != nil {
			return err
		}
	}
	configPath := filepath.Join(dir, "config")
	if !isFile(configPath) {
		seed, err := s.seedConfig(tenant)
		if err != nil {
			return err
		}
		// The account gets the alias list, not the operator's file: only Host/HostName/User/
		// Port are carried over. An IdentityFile line would name a key that does not exist in
		// this account's HOME (its identity is the single key above), which ssh reports as a
		// warning on every call and would silently pick the wrong key for a host.
		if err := securefile.WriteAtomic(configPath, []byte(aliasConfig(seed)), 0o644); err != nil {
			return err
		}
	}
	// The account's own view of its mounts. Inside the sandbox a mount point is an ordinary
	// directory (mounting is kernel state the worker cannot see), so this file is what tells
	// the tenant plugin which mirrored paths are real mounts and which are just parent
	// directories of the mirror layout.
	return s.writeMirror(tenant, dshHome)
}

// seedConfig reads one account's alias seed from <ssh_config_dir>/<account>.
//
// A missing seed is not an error: the account simply starts with no aliases and can add its
// own hosts (or type user@host directly). An *unusable* seed is an error, and it is never
// skipped silently: the file is the operator's statement about which hosts this account may
// reach, so quietly starting the account with a different list than the configured one would
// be the wrong failure. The source is reached through securefile, so neither the file nor any
// ancestor directory may be a symlink, and the size is bounded.
func (s *Service) seedConfig(tenant string) ([]byte, error) {
	if s.options.SSHConfigDir == "" {
		return nil, nil
	}
	source := filepath.Join(s.options.SSHConfigDir, tenant)
	if tenant == "" || !Within(s.options.SSHConfigDir, source) || source == s.options.SSHConfigDir {
		return nil, Errorf(CodeInvalidState, "ssh config seed for %q would leave %s", tenant, s.options.SSHConfigDir)
	}
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, Wrap(CodeInvalidState, "reading the ssh config seed "+source, err)
	}
	if !info.Mode().IsRegular() {
		return nil, Errorf(CodeInvalidState, "ssh config seed %s must be a regular file", source)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return nil, Errorf(CodeInvalidState, "ssh config seed %s must not be hard linked", source)
	}
	// World-writable is the line, not group-writable: every tenant worker already runs as the
	// deployment account's own uid and group here (permissions are not what separates
	// accounts — path binding is), while the documented 0644 seed and a plain `cp` under the
	// usual umask both have to keep working. A file anyone on the host may rewrite must not
	// decide an account's aliases.
	if info.Mode().Perm()&0o002 != 0 {
		return nil, Errorf(CodeInvalidState, "ssh config seed %s mode %04o is world writable", source, info.Mode().Perm())
	}
	data, err := securefile.ReadLimitedRegular(source, 64<<10)
	if err != nil {
		return nil, Wrap(CodeInvalidState, "reading the ssh config seed "+source, err)
	}
	return data, nil
}

// aliasConfig renders the minimal ssh config a tenant may hold: the aliases an operator
// configured, with nothing that could disagree with the account's own identity.
func aliasConfig(source []byte) string {
	var out strings.Builder
	out.WriteString("# Generated by dshgw: host aliases only. This account's identity is its own\n")
	out.WriteString("# ~/.ssh/id_rsa; per-host keys and other local settings are not carried over.\n")
	for _, host := range ParseSSHConfig(source) {
		out.WriteString("\nHost " + host.Name + "\n")
		if host.HostName != "" {
			out.WriteString("  HostName " + host.HostName + "\n")
		}
		if host.User != "" {
			out.WriteString("  User " + host.User + "\n")
		}
		if host.Port > 0 {
			out.WriteString("  Port " + strconv.Itoa(host.Port) + "\n")
		}
	}
	return out.String()
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
//
// A recorded mount whose sshfs daemon is gone is left out as well. Killing the daemon does
// not detach the mount, so its entry stays in the mount table and bubblewrap refuses such a
// source outright ("Can't get type of source …: Transport endpoint is not connected") — which
// used to take the whole worker down with it, leaving the tenant unreachable with no way back
// short of an operator. Skipping it starts the worker; the account gets the mount back
// through Open, which replaces the dead entry.
func (s *Service) MountsFor(tenant string) []string {
	mounts, err := s.store.ForTenant(tenant)
	if err != nil {
		s.logger.Error("reading the ssh mount record failed", "tenant", tenant, "err", err)
		return nil
	}
	live := make([]Mount, 0, len(mounts))
	for _, mount := range mounts {
		if fstype, _ := s.mounted(mount.Mountpoint); fstype != "" && sshfsDaemonFor(mount.Mountpoint) == 0 {
			s.logger.Warn("an ssh workspace mount has no daemon; leaving it out of the worker's sandbox",
				"tenant", tenant, "mountpoint", mount.Mountpoint,
				"detail", "opening it again replaces the dead entry")
			continue
		}
		live = append(live, mount)
	}
	return Mountpoints(live)
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
		// A recorded mount whose FUSE connection is gone is not a mount: its sshfs daemon
		// died, every read on it fails with ENOTCONN, and a new mount cannot be made over
		// the stale entry. Detaching the corpse is what makes a retry possible — without it
		// the account is stuck with a workspace that can never come back.
		if fstype, _ := s.mounted(mountpoint); fstype != "" && !s.flushStaleMount(ctx, mountpoint) {
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

// flushStaleMount detaches a mount whose FUSE connection is gone, and reports whether it
// did.
//
// This is the state a killed or crashed sshfs daemon leaves behind, and it is worse than
// "not mounted": the entry stays in the mount table, reads on it fail with ENOTCONN, and
// `sshfs` refuses to mount over it ("failed to access mountpoint … Transport endpoint is
// not connected"). The record and the entry therefore have to go together before a retry
// can succeed — and once the daemon is gone, the detach is a plain local unmount that
// cannot block on an unreachable host.
func (s *Service) flushStaleMount(ctx context.Context, mountpoint string) bool {
	if connectionLive(s.mounted, mountpoint) {
		return false
	}
	s.logger.Warn("an ssh workspace mount lost its daemon; detaching the stale entry so it can be mounted again",
		"mountpoint", mountpoint)
	if _, err := s.options.unmount(ctx, s.exec, mountpoint); err != nil {
		s.logger.Error("detaching a stale ssh workspace mount failed", "mountpoint", mountpoint, "err", err)
		return false
	}
	if fstype, _ := s.mounted(mountpoint); fstype != "" {
		// Lazily detached: the table still lists it for whoever holds it open, but the
		// account's own record and mount point are free for a new mount.
		s.logger.Warn("a stale ssh workspace mount was detached lazily", "mountpoint", mountpoint)
	}
	if _, err := s.store.Remove(mountpoint); err != nil {
		s.logger.Error("forgetting a stale ssh workspace mount failed", "mountpoint", mountpoint, "err", err)
		return false
	}
	return true
}

// detach unmounts one mount and waits for it to leave the mount table.
//
// The holder is usually the worker's own mount namespace: stopping or restarting a worker
// tears its sandbox down asynchronously, so a mount can stay attached for a moment after the
// process that used it is gone. Purging an account while a mount is still attached fails with
// EBUSY on the mount point — a confusing symptom far from its cause — so this waits, and says
// what is holding it when the wait runs out.
//
// A mount that survives every attempt is treated as wedged rather than busy, and the wedge is
// broken here: see wedgeFuse. A mount left in that state is what keeps a reader inside the
// tenant's sandbox in an uninterruptible wait, holds the worker's scope open behind it, and
// turns the next worker start into "worker authentication unavailable".
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
	if wedged, err := s.breakWedge(ctx, mountpoint); err != nil {
		return lazy, err
	} else if wedged {
		// The connection is aborted and every waiter on it has been released; one more
		// unmount is what turns that into a detached mount.
		detached, retryErr := s.options.unmount(ctx, s.exec, mountpoint)
		lazy = lazy || detached
		if retryErr == nil {
			if fstype, _ := s.mounted(mountpoint); fstype == "" {
				if removeErr := os.Remove(mountpoint); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
					return lazy, nil
				}
			}
		}
	}
	fstype, _ := s.mounted(mountpoint)
	return lazy, Errorf(CodeBusy, "%s is still mounted (%s): a session may still hold it open", mountpoint, fstype)
}

// breakWedge ends a mount that outlived every unmount attempt, and reports whether it
// did anything.
//
// It first stops the sshfs daemon serving the mount: the daemon's file descriptor is
// what keeps the FUSE connection alive, so killing it fails every pending request — the
// readers parked inside the tenant's sandbox included — and lets the kernel drop the
// connection. Only when no daemon can be found does it abort the connection through
// sysfs, which reaches the same waiters when the daemon itself is the unkillable part.
func (s *Service) breakWedge(ctx context.Context, mountpoint string) (bool, error) {
	if fstype, _ := s.mounted(mountpoint); !strings.HasPrefix(fstype, "fuse") {
		return false, nil
	}
	broken := false
	if killSSHFSDaemon(mountpoint) {
		s.logger.Warn("an ssh workspace daemon outlived its unmount and was stopped; readers waiting on it are released",
			"mountpoint", mountpoint)
		broken = true
		select {
		case <-ctx.Done():
			return broken, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err := fuseAbort(mountpoint); err != nil {
		if broken {
			// The daemon is already gone, which is what releases the waiters; the
			// connection entry failing to abort is worth saying, not worth failing on.
			s.logger.Warn("aborting the ssh workspace FUSE connection failed", "mountpoint", mountpoint, "err", err)
		} else {
			return broken, err
		}
	} else {
		broken = true
	}
	if broken {
		s.logger.Warn("an ssh workspace mount did not detach and its FUSE connection was aborted",
			"mountpoint", mountpoint, "detail", "pending requests fail now instead of waiting for an unreachable host")
	}
	return broken, nil
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
	if err := s.options.permits(host); err != nil {
		return err
	}
	if _, err := pathsFor(s.options, remote, host); err != nil {
		return err
	}
	// The one shape sshfs cannot survive: a source directory that contains its own mount
	// point. Refused here rather than in Open so that Reconcile — which re-mounts what the
	// record lists at every gateway start — is covered by the same rule.
	if err := s.refuseSelfNestedMount(ctx, remote, host, remotePath, mountpoint); err != nil {
		return err
	}
	budget := s.options.ConnectTimeout
	if budget <= 0 || budget > sshfsCallBudget {
		budget = sshfsCallBudget
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	args := s.options.sshfsArgs(remote, host, remotePath, mountpoint)
	if len(args) == 0 {
		return Errorf(CodeAuthFailed, "ssh identity became unavailable")
	}
	_, stderr, err := s.exec(callCtx, s.options.sshfsBin(), args, nil)
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
