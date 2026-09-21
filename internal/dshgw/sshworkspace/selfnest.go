package sshworkspace

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The mount that contains its own mount point.
//
// sshfs maps one remote directory onto one local mount point, and that mount point lives
// inside the account's workspace (<workspace>/ssh/<host>/<remote path>). When the remote
// directory is itself an ancestor of the mount point, the mounted tree contains the mount
// point, so the mount contains itself: /home/winger/work/ai_gateway mounted from this very
// host lands at .../workspaces/dsh-tenant/ssh/rag-server/home/winger/work/ai_gateway, which is
// inside /home/winger/work/ai_gateway.
//
// Nothing is broken at the moment of mounting. The damage starts with the first recursive
// reader: `grep -r`, `find` and the workspace indexer descend into the mounted copy, meet the
// same directory one level deeper, and descend again, each step a round trip on the mount's
// sftp channel. Measured on this host, 2026-09-21: a `grep -rn … .` issued from a workspace
// that held such a mount left the kernel with 8 unanswered requests on one FUSE connection and
// three processes in an uninterruptible (D) state — `grep` itself, whose open directory
// handles already named the second nesting level, and every later probe of the mount point.
// D-state waits ignore signals, so those processes could not be killed, and because every
// session of the account shares the one mount, they all froze at once.
//
// So the refusal has to come before the mount. It also has to know whether the ssh target is
// this host: on another machine the same path string names a different directory, and mounting
// that is ordinary and must keep working. Two proofs of "this host" are accepted, cheapest
// first — the target names one of this host's own addresses, or it reports this host's machine
// id — and the address check alone settles the loopback and alias-to-self cases.

// hostAddresses lists the names that mean "this host" when an ssh target spells them out. It is
// a variable because it is a host fact the tests must be able to state.
var hostAddresses = localHostAddresses

// machineIDPath is where a Linux host names itself. A host without it (or with an unreadable
// one) reports no machine id, which leaves the address comparison as the only proof.
var machineIDPath = "/etc/machine-id"

// localHostAddresses collects every name this host answers to: its hostname (short and long
// form), the loopback names, and each address of each interface. Comparing against all of them
// is what makes an alias whose HostName is this host's own LAN address — the rag-server case —
// recognizable without a single ssh round trip.
func localHostAddresses() map[string]bool {
	names := map[string]bool{
		"localhost": true, "localhost.localdomain": true,
		"127.0.0.1": true, "::1": true, "0.0.0.0": true, "::": true,
	}
	if host, err := os.Hostname(); err == nil {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			names[host] = true
			if short, _, found := strings.Cut(host, "."); found {
				names[short] = true
			}
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return names
	}
	for _, addr := range addrs {
		names[strings.Trim(addr.String(), "[]")] = true
		if network, ok := addr.(*net.IPNet); ok {
			names[network.IP.String()] = true
		}
	}
	return names
}

// localMachineID reads this host's machine id, or "" when it has none.
func localMachineID() string {
	data, err := os.ReadFile(machineIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// refuseSelfNestedMount rejects a mount that would contain its own mount point. It is called
// before every sshfs invocation, from the one place that runs sshfs, so a user click and a
// gateway restart (Reconcile re-mounting a recorded mount) are both covered.
func (s *Service) refuseSelfNestedMount(ctx context.Context, remote Remote, host, remotePath, mountpoint string) error {
	if remotePath == "" || mountpoint == "" || !filepath.IsAbs(remotePath) || !filepath.IsAbs(mountpoint) {
		return nil
	}
	// The local tree answers the containment question first, because it is free: a remote
	// directory that has no counterpart at that path here cannot be one of the mount point's
	// ancestors, and that is the ordinary case for every mount of another machine.
	if !ancestorOfMountpoint(mountpoint, remotePath) {
		return nil
	}
	// Same path, and there is no telling whether it is the same machine without asking. Only
	// when it is this host does the mount contain itself.
	if !s.isThisHost(ctx, remote, host) {
		return nil
	}
	s.logger.Warn("refusing an ssh workspace mount that would contain its own mount point",
		"tenant", remote.Tenant, "host", host, "remote", remotePath, "mountpoint", mountpoint)
	s.record("ssh-mount-refused", remote.Tenant, mountpoint, host+":"+remotePath, 403)
	return Errorf(CodeForbidden,
		"refusing to mount %s on %s: that directory contains the mount point, so the mount would contain itself. "+
			"Every recursive scan through it (grep -r, find, an indexer) descends into its own copy for as long as the "+
			"path length allows, and the requests pile up on the mount's single sftp channel until the whole mount hangs "+
			"in an uninterruptible state — for every session of this account at once. "+
			"Mount a directory that does not contain the workspace (any other directory on that host is fine).",
		remotePath, mountpoint)
}

// isThisHost reports whether the ssh target is the gateway host itself, which is what makes the
// remote directory a local directory and the containment question answerable here.
func (s *Service) isThisHost(ctx context.Context, remote Remote, host string) bool {
	// Resolving through the account's alias is what turns rag-server into 192.168.190.86. A
	// missing identity or an unreadable alias list is not this function's business: the mount
	// that follows will fail on its own terms.
	target, _, err := s.options.sshTarget(remote, host)
	if err != nil {
		return false
	}
	name := target
	if at := strings.LastIndex(name, "@"); at >= 0 {
		name = name[at+1:]
	}
	name = strings.Trim(strings.ToLower(strings.TrimSpace(name)), "[]")
	if hostAddresses()[name] {
		return true
	}
	// An alias can name this host in a way its own addresses do not spell out (a DNS name, a
	// second interface, a hostname only the remote resolver knows). The machine id settles it,
	// at the price of one ssh round trip — paid only for a path that already looks like an
	// ancestor, never for an ordinary mount.
	remoteID, err := s.options.MachineID(ctx, s.exec, remote, host)
	if err != nil || remoteID == "" {
		return false
	}
	localID := localMachineID()
	return localID != "" && remoteID == localID
}

// ancestorOfMountpoint reports whether target names the mount point or one of its ancestors.
//
// It compares device and inode instead of path strings, because two spellings of one directory
// are one directory: on this host /data/home/winger/work and /home/winger/work are two mount
// points of the same ext4 file system, so a mount of the first is a mount of an ancestor of a
// workspace under the second, and a string prefix test would have missed it. A symlinked
// ancestor is caught for the same reason, since stat resolves the link.
//
// The mount point itself is never stat'ed: this runs before a mount is made, but a stale FUSE
// mount can be sitting on that path, and a stat on a wedged one is the very uninterruptible
// wait this file exists to prevent. Walking up from its parent reaches every ancestor without
// touching it.
func ancestorOfMountpoint(mountpoint, target string) bool {
	mountpoint = filepath.Clean(mountpoint)
	target = filepath.Clean(target)
	if mountpoint == target {
		return true
	}
	// A source inside the mount point cannot contain it. Skipping it early also keeps the stat
	// below off a path that may be served by the very mount being replaced.
	if Within(mountpoint, target) {
		return false
	}
	info, err := os.Stat(target)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	for dir := filepath.Dir(mountpoint); ; {
		if info, err := os.Stat(dir); err == nil {
			if current, ok := info.Sys().(*syscall.Stat_t); ok && current.Dev == stat.Dev && current.Ino == stat.Ino {
				return true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
