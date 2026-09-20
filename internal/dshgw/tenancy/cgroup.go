package tenancy

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
	"syscall"
	"time"
)

// scopeCgroupCandidates lists this host's plausible cgroup locations for a scope
// created by this account's user manager.
//
// A transient user scope is a child of the manager service unit, not of the caller:
// systemd puts `systemd-run --user --scope` units under the manager's own delegated
// slice (measured on this host: `user.slice/user-1000.slice/user@1000.service/app
// .slice/dshgw-worker-<tenant>-<id>.scope`, while dshgw itself runs in
// `…/user@1000.service/dsh-web.service`). Deriving the path from /proc/self/cgroup and
// appending the unit name therefore names a cgroup that never exists — so the
// candidates are derived from the manager's cgroup, with the caller's own as a fallback
// for a manager that nests scopes differently, and the user-level slice as the last
// resort.
func scopeCgroupCandidates(unit string) []string {
	if unit == "" {
		return nil
	}
	leaf := unit + ".scope"
	var out []string
	seen := map[string]bool{}
	add := func(base string) {
		if base == "" {
			return
		}
		path := filepath.Join(base, leaf)
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	if mine := currentCgroupPath(); mine != "" {
		// The manager service unit spelled out in the path is the surest base:
		// /proc/self/cgroup reports the path globally, even from the sandbox.
		// app.slice comes first because that is where this host's manager puts user
		// scopes; the bare service unit is the layout other systemd versions use.
		parts := strings.Split(strings.TrimPrefix(mine, cgroupMount), "/")
		for i, part := range parts {
			if strings.HasSuffix(part, ".service") {
				service := filepath.Join(append([]string{cgroupMount}, parts[:i+1]...)...)
				add(filepath.Join(service, "app.slice"))
				add(service)
				break
			}
		}
		add(mine)
	}
	add(filepath.Join(cgroupMount, "user.slice", userSliceName()))
	return out
}

// userSliceName is this account's slice under user.slice ("user-1000.slice").
func userSliceName() string {
	return fmt.Sprintf("user-%d.slice", os.Getuid())
}

// findScopeCgroup locates the cgroup a transient user scope owns, or "" when it does
// not exist — the scope is created asynchronously, and `--collect` removes it as soon
// as the worker exits.
//
// The check is for a real cgroup, not just for a directory that exists: the
// candidates' parent directories always exist, so a bare IsDir would report the
// manager's own slice as the worker's cgroup, and the stop path would then try to
// empty that.
func findScopeCgroup(unit string) string {
	leaf := unit + ".scope"
	for _, path := range scopeCgroupCandidates(unit) {
		if !strings.HasSuffix(path, string(filepath.Separator)+leaf) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, "cgroup.procs")); err != nil {
			continue
		}
		return path
	}
	return ""
}

// scopeCgroupPath returns the first candidate, for diagnostics and for the tests that
// state what they expect. Use findScopeCgroup to learn where a scope actually landed.
func scopeCgroupPath(unit string) string {
	candidates := scopeCgroupCandidates(unit)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// The worker's cgroup, read from the outside.
//
// A worker's limits live in a systemd user scope, and a scope is a cgroup: every
// descendant of the worker — the sandbox, node, and whatever the agent's own shell
// tools started — stays accounted there. The kernel removes that cgroup when its
// last member exits, which is why a worker that merely got SIGTERM normally leaves
// nothing behind.
//
// The case this file exists for is the one that does not exit. A tool call whose
// reader is parked in an uninterruptible FUSE wait survives both the SIGTERM that
// stops its worker and the SIGKILL that follows, and while it lives the scope stays
// loaded and active, holding the tenant's scope name and — before scope names
// became per-incarnation — its next start. Reading the cgroup directly is what lets
// the runner say how many such processes it left behind instead of claiming a clean
// stop, and lets it finish the job once they become killable.

// cgroupMount is where cgroup v2 is mounted on every supported host.
const cgroupMount = "/sys/fs/cgroup"

// currentCgroupPath returns this process's cgroup as a path under the cgroup mount
// ("/sys/fs/cgroup" + the unified hierarchy entry). Reading it is how a scope's
// cgroup path is derived without shelling out to systemctl.
func currentCgroupPath() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// cgroup v2 writes exactly "0::/path"; a v1 line names controllers first.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" || parts[1] != "" {
			continue
		}
		path := parts[2]
		if !strings.HasPrefix(path, "/") {
			continue
		}
		return cgroupMount + filepath.Clean(path)
	}
	return ""
}

// cgroupProcesses lists the PIDs still accounted to one cgroup. It is a direct read
// of cgroup.procs, so it reports the truth about orphaned descendants rather than
// what the runner's own process bookkeeping believes.
func cgroupProcesses(path string) []int {
	if path == "" {
		return nil
	}
	file, err := os.Open(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return nil
	}
	defer file.Close()
	var pids []int
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// signalPIDs sends a signal to each PID, skipping the ones that are already gone.
func signalPIDs(pids []int, signal syscall.Signal) {
	for _, pid := range pids {
		if pid <= 0 || pid == os.Getpid() {
			continue
		}
		_ = syscall.Kill(pid, signal)
	}
}

// terminateCgroup empties one cgroup: SIGTERM (process groups first, so a shell and
// its children go together), then SIGKILL, then a bounded wait for the kernel to
// release what it can. It returns the PIDs that are still alive when the budget runs
// out — a process parked in an uninterruptible wait cannot be signalled away, and
// pretending otherwise is how a stale scope later looks like a mystery.
func terminateCgroup(ctx context.Context, path string, grace, kill time.Duration) []int {
	pids := cgroupProcesses(path)
	if len(pids) == 0 {
		return nil
	}
	// Negative PIDs address process groups: the worker's tools usually run in one.
	groups := map[int]bool{}
	for _, pid := range pids {
		if pid > 0 && pid != os.Getpid() {
			groups[pid] = true
		}
	}
	for pid := range groups {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	signalPIDs(pids, syscall.SIGTERM)
	if waitForEmptyCgroup(ctx, path, grace) {
		return nil
	}
	for pid := range groups {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	signalPIDs(pids, syscall.SIGKILL)
	if waitForEmptyCgroup(ctx, path, kill) {
		return nil
	}
	return cgroupProcesses(path)
}

// waitForEmptyCgroup polls until the cgroup has no members or the budget expires.
func waitForEmptyCgroup(ctx context.Context, path string, budget time.Duration) bool {
	if budget <= 0 {
		return len(cgroupProcesses(path)) == 0
	}
	deadline := time.Now().Add(budget)
	for {
		if len(cgroupProcesses(path)) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return len(cgroupProcesses(path)) == 0
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// stopScope removes one worker's transient scope unit.
//
// Stopping is what systemd offers for "end this unit now": the unit's cgroup is
// emptied and the name is released, so the next worker cannot be refused because a
// predecessor's scope is still loaded. It is best effort — a unit that is already
// gone is not an error, and a failure here is reported by the caller, never fatal.
func stopScope(ctx context.Context, unit string) error {
	if strings.TrimSpace(unit) == "" {
		return nil
	}
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, path, "--user", "stop", unit)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+userRuntimeDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if strings.Contains(text, "not loaded") || strings.Contains(text, "not found") {
			return nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return err
		}
		return errors.New(strings.TrimSpace(err.Error() + ": " + text))
	}
	return nil
}
