package tenancy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// WorkerLimits are the per-worker resources, replacing the systemd unit's
// MemoryHigh/MemoryMax/CPUQuota/TasksMax. Zero means "no limit".
type WorkerLimits struct {
	MemoryHighBytes int64
	MemoryMaxBytes  int64
	TasksMax        int
	CPUQuotaPercent int
}

func (l WorkerLimits) empty() bool {
	return l.MemoryHighBytes <= 0 && l.MemoryMaxBytes <= 0 && l.TasksMax <= 0 && l.CPUQuotaPercent <= 0
}

// workerUnitName is the transient user scope that carries one worker's limits.
func workerUnitName(tenant string) string { return "dshgw-worker-" + tenant }

// scopeWrapper returns the argv that runs one worker inside its own systemd user
// scope, or nil when limits do not need one.
//
// Why a scope and not a cgroup of our own: cgroup v2 refuses to hand controllers
// down from a cgroup that has processes in it ("no internal processes"), and a
// systemd service's own cgroup always holds the service's main process — measured
// on this host, `cgroup.subtree_control` inside aigw-local.service cannot be
// written at all. A user scope has no such problem: the user manager owns the
// delegated tree, creates the scope, and sets its limits, all without root
// (verified: `systemd-run --user --scope -p MemoryMax=...` yields a scope whose
// memory.max is set and which disappears when the process exits).
//
// The command stays a descendant of dshgw (a scope wraps an existing process tree
// rather than re-parenting it into the user manager), which is what keeps the
// "worker is a child of the gateway" property the rest of the design relies on.
func scopeWrapper(unit string, limits WorkerLimits, argv []string) ([]string, error) {
	if limits.empty() {
		return nil, nil
	}
	path, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, fmt.Errorf("systemd-run is unavailable: %w", err)
	}
	args := []string{"--user", "--scope", "--quiet", "--collect", "--unit=" + unit}
	for _, property := range []struct {
		name  string
		value string
	}{
		{"MemoryHigh", bytesProperty(limits.MemoryHighBytes)},
		{"MemoryMax", bytesProperty(limits.MemoryMaxBytes)},
		{"TasksMax", intProperty(limits.TasksMax)},
		{"CPUQuota", percentProperty(limits.CPUQuotaPercent)},
	} {
		if property.value == "" {
			continue
		}
		args = append(args, "-p", property.name+"="+property.value)
	}
	args = append(args, "--")
	return append([]string{path}, append(args, argv...)...), nil
}

// userRuntimeDir is where this account's systemd manager lives. It is derived from
// the uid rather than trusted from the environment: a process started outside a
// login session (a test harness, a cron job, an ssh command without a session)
// often has XDG_RUNTIME_DIR unset even though the manager is running perfectly
// well, and concluding "no limits possible" from that would silently drop them.
func userRuntimeDir() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); dir != "" {
		return dir
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
}

// userManagerAvailable reports whether limits can be applied at all: they need the
// user's systemd manager, which is what owns the delegated cgroup tree.
func userManagerAvailable() bool {
	if _, err := os.Stat(filepath.Join(userRuntimeDir(), "systemd", "private")); err != nil {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

func bytesProperty(value int64) string {
	if value <= 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func intProperty(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value)
}

func percentProperty(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value) + "%"
}

// errNoLimits is returned when limits were configured but cannot be applied; the
// caller warns and runs the worker unlimited rather than failing the tenant.
var errNoLimits = errors.New("per-worker limits need a systemd user manager (systemd-run --user)")
