package tenancy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

// WorkerRunner owns every tenant worker process of one dshgw instance.
//
// In the aigw-supervised shape (M58) there is no systemd unit and no per-tenant
// account: each worker is a direct child of dshgw, started inside the tenant's
// bubblewrap profile and running as aigw's own account. Lifecycle is therefore
// process management — start, stop, restart, observe — and nothing else:
//
//   - a worker dies with dshgw (Pdeathsig), so an abrupt aigw exit cannot leave
//     tenant processes holding ports;
//   - the startup URL dsh prints is read from the child's own output, replacing
//     the journalctl round trip the systemd shape needed;
//   - "should this tenant be running" is explicit state in the registry
//     (registry.Tenant.Suspended), not systemd enablement.
type WorkerRunner struct {
	Config *config.Config
	// Profile builds one tenant's argv. It is a field so tests can run a stand-in
	// program, and so the production path stays the single source of truth for
	// what a sandbox looks like (internal/dshgw/sandbox).
	Profile func(registry.Tenant) ([]string, error)
	// Probe is the readiness check used after a start. The manager already owns
	// the /api 401 contract; the runner only needs to call it.
	Probe func(context.Context, registry.Tenant) error
	// Limits are applied to each worker's own cgroup when this host can provide
	// one; a host that cannot still runs the worker (see CgroupManager).
	Limits WorkerLimits
	// Logger receives worker lifecycle events and forwarded output.
	Logger *slog.Logger
	// StopTimeout bounds the graceful wait before a worker is killed.
	StopTimeout time.Duration

	mu    sync.Mutex
	procs map[string]*workerProc

	// limitsWarned records that the "limits unavailable" warning was already
	// emitted, so a deployment without a user manager warns once instead of on
	// every tenant start.
	limitsWarned bool
}

type workerProc struct {
	tenant  registry.Tenant
	cmd     *exec.Cmd
	started time.Time
	pid     int

	mu       sync.Mutex
	startURL string
	exitErr  error
	// finished marks the process as gone. It is a separate flag from exitErr on
	// purpose: a worker that exits cleanly has a nil error, so testing the error
	// would report every cleanly-exited worker as still running.
	finished  bool
	exited    chan struct{}
	stopping  bool
	outputLog *lineRing
}

func (r *WorkerRunner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *WorkerRunner) stopTimeout() time.Duration {
	if r.StopTimeout > 0 {
		return r.StopTimeout
	}
	return 20 * time.Second
}

// Start launches one tenant worker and returns as soon as the process exists.
// Readiness is the caller's job (Manager.probeWorker polls the worker's /api for
// 401), because a start that fails readiness must still be able to inspect and
// stop the process it created.
func (r *WorkerRunner) Start(ctx context.Context, t registry.Tenant) error {
	if r.Profile == nil {
		return errors.New("worker runner has no profile builder")
	}
	argv, err := r.Profile(t)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return errors.New("worker profile is empty")
	}
	argv, scoped := r.applyLimits(workerUnitName(t.Name), argv)
	r.mu.Lock()
	if r.procs == nil {
		r.procs = map[string]*workerProc{}
	}
	if existing, ok := r.procs[t.Name]; ok {
		if existing.isRunning() {
			r.mu.Unlock()
			return fmt.Errorf("worker for tenant %s is already running (pid %d)", t.Name, existing.pid)
		}
		// The previous process exited: its record is replaced, but only now —
		// keeping it until here is what makes a failed start diagnosable.
		r.logger().Info("replacing an exited tenant worker", "tenant", t.Name, "err", errText(existing.err()))
	}
	proc := &workerProc{tenant: t, exited: make(chan struct{}), outputLog: newLineRing(workerOutputLines)}
	r.procs[t.Name] = proc
	r.mu.Unlock()

	// The worker's cwd is its workspace (the same value the systemd unit passed
	// as WorkingDirectory). Check it here: a missing directory makes exec return
	// a misleading "no such file or directory" about the *program*.
	if info, statErr := os.Stat(t.Workspace); statErr != nil {
		r.forget(t.Name)
		return fmt.Errorf("tenant %s workspace %s is unusable: %w", t.Name, t.Workspace, statErr)
	} else if !info.IsDir() {
		r.forget(t.Name)
		return fmt.Errorf("tenant %s workspace %s is not a directory", t.Name, t.Workspace)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = workerProcAttrs()
	cmd.Dir = t.Workspace
	cmd.Env = workerEnv(r.Config, t)
	if scoped {
		// The wrapper talks to the user manager over this socket; without it
		// systemd-run cannot reach the manager that is supposed to apply the limits.
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+userRuntimeDir())
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		r.forget(t.Name)
		return fmt.Errorf("start worker for %s: %w", t.Name, err)
	}
	proc.cmd = cmd
	proc.pid = cmd.Process.Pid
	proc.started = time.Now().UTC()
	go r.pump(proc, pr)

	go func() {
		waitErr := cmd.Wait()
		_ = pw.Close()
		proc.mu.Lock()
		proc.exitErr = waitErr
		proc.finished = true
		proc.mu.Unlock()
		close(proc.exited)
		// A worker that died before reporting readiness leaves no other trace: its
		// output is the diagnosis, so the tail goes to the log at a level an
		// operator sees without enabling debug.
		r.logger().Info("tenant worker exited", "tenant", t.Name, "pid", proc.pid, "err", errText(waitErr), "output", proc.outputLog.join(" | "))
	}()

	r.logger().Info("tenant worker started", "tenant", t.Name, "pid", proc.pid, "workspace", t.Workspace)
	select {
	case <-proc.exited:
		// The process is gone already: report it here rather than letting the
		// caller discover a dead worker through a readiness timeout.
		return fmt.Errorf("worker for %s exited immediately: %w", t.Name, proc.err())
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

// applyLimits wraps the worker argv in its own systemd user scope when limits are
// configured. A host without a user manager keeps running workers unlimited: a
// missing quota must never turn into a missing tenant, so this warns once and
// returns the original argv.
func (r *WorkerRunner) applyLimits(unit string, argv []string) ([]string, bool) {
	if r.Limits.empty() {
		return argv, false
	}
	if !userManagerAvailable() {
		r.mu.Lock()
		warned := r.limitsWarned
		r.limitsWarned = true
		r.mu.Unlock()
		if !warned {
			r.logger().Warn("per-worker limits configured but unavailable; workers run unlimited", "reason", errNoLimits.Error())
		}
		return argv, false
	}
	wrapped, err := scopeWrapper(unit, r.Limits, argv)
	if err != nil {
		r.logger().Warn("building the worker scope failed; the worker runs unlimited", "err", err)
		return argv, false
	}
	return wrapped, true
}

// Stop terminates one tenant worker: SIGTERM to its process group (so the node
// process and anything it spawned go together), then SIGKILL if it does not exit
// in time. Stopping an already-stopped tenant is not an error.
func (r *WorkerRunner) Stop(ctx context.Context, t registry.Tenant) error {
	r.mu.Lock()
	proc, ok := r.procs[t.Name]
	if ok {
		proc.mu.Lock()
		proc.stopping = true
		proc.mu.Unlock()
	}
	r.mu.Unlock()
	if !ok {
		return nil
	}
	if !proc.isRunning() {
		// The process is already gone (crash or previous stop); only the
		// handshake file may still describe it.
		return r.removeHandshake(t.Name)
	}
	if proc.pid > 0 {
		if err := syscall.Kill(-proc.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			r.logger().Warn("signalling tenant worker failed", "tenant", t.Name, "pid", proc.pid, "err", err)
		}
	}
	select {
	case <-proc.exited:
	case <-time.After(r.stopTimeout()):
		r.logger().Warn("tenant worker did not exit in time; killing it", "tenant", t.Name, "pid", proc.pid)
		if proc.pid > 0 {
			_ = syscall.Kill(-proc.pid, syscall.SIGKILL)
		}
		select {
		case <-proc.exited:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("tenant worker %s survived SIGKILL", t.Name)
		}
	case <-ctx.Done():
		if proc.pid > 0 {
			_ = syscall.Kill(-proc.pid, syscall.SIGKILL)
		}
		return ctx.Err()
	}
	// The handshake file describes a process that no longer exists; leaving it
	// behind would let the proxy redirect a customer to a dead worker.
	return r.removeHandshake(t.Name)
}

// removeHandshake deletes the published startup URL for one tenant.
func (r *WorkerRunner) removeHandshake(name string) error {
	if err := os.Remove(filepath.Join(r.Config.HandshakeDir, name+".url")); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logger().Warn("removing stale handshake failed", "tenant", name, "err", err)
		return err
	}
	return nil
}

// Restart replaces one tenant's worker process.
func (r *WorkerRunner) Restart(ctx context.Context, t registry.Tenant) error {
	if err := r.Stop(ctx, t); err != nil {
		return err
	}
	return r.Start(ctx, t)
}

// WorkerStatus is what the lifecycle commands report about one tenant: a running
// process, a captured startup URL, and when it began.
type WorkerStatus struct {
	Running   bool
	PID       int
	StartURL  string
	StartedAt time.Time
	Detail    string
}

// Status reports the current process state for one tenant.
func (r *WorkerRunner) Status(t registry.Tenant) WorkerStatus {
	r.mu.Lock()
	proc, ok := r.procs[t.Name]
	r.mu.Unlock()
	if !ok {
		return WorkerStatus{Detail: "stopped"}
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	status := WorkerStatus{PID: proc.pid, StartedAt: proc.started, StartURL: proc.startURL, Running: !proc.finished, Detail: "running"}
	if proc.finished {
		status.Detail = "exited: " + errText(proc.exitErr)
	}
	return status
}

// StartAll starts every tenant that should be running. It is what replaces
// systemd's "enabled units come back after a reboot": aigw starting dshgw starts
// the tenants the registry says are not suspended.
func (r *WorkerRunner) StartAll(ctx context.Context, tenants []registry.Tenant) error {
	var failures []error
	for _, t := range tenants {
		if t.Suspended {
			continue
		}
		if err := r.Start(ctx, t); err != nil {
			if strings.Contains(err.Error(), "already running") {
				continue
			}
			failures = append(failures, fmt.Errorf("tenant %s: %w", t.Name, err))
			continue
		}
		probe := r.Probe
		if probe != nil {
			if err := probe(ctx, t); err != nil {
				failures = append(failures, fmt.Errorf("tenant %s readiness: %w", t.Name, err))
			}
		}
	}
	return errors.Join(failures...)
}

// StopAll stops every running worker. Shutdown and the backup snapshot both need
// a quiet moment, and both used to ask systemd for it.
func (r *WorkerRunner) StopAll(ctx context.Context) error {
	r.mu.Lock()
	tenants := make([]registry.Tenant, 0, len(r.procs))
	for _, proc := range r.procs {
		if proc.isRunning() {
			tenants = append(tenants, proc.tenant)
		}
	}
	r.mu.Unlock()
	var failures []error
	for _, t := range tenants {
		if err := r.Stop(ctx, t); err != nil {
			failures = append(failures, fmt.Errorf("tenant %s: %w", t.Name, err))
		}
	}
	return errors.Join(failures...)
}

// Running reports the tenants with a live worker process.
func (r *WorkerRunner) Running() []registry.Tenant {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]registry.Tenant, 0, len(r.procs))
	for _, proc := range r.procs {
		if proc.isRunning() {
			out = append(out, proc.tenant)
		}
	}
	return out
}

// StartURL returns the startup URL a worker printed, once it has.
func (r *WorkerRunner) StartURL(name string) (string, bool) {
	r.mu.Lock()
	proc, ok := r.procs[name]
	r.mu.Unlock()
	if !ok {
		return "", false
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	return proc.startURL, proc.startURL != ""
}

// Output returns the tail of one worker's output, for error messages.
func (r *WorkerRunner) Output(name string) string {
	r.mu.Lock()
	proc, ok := r.procs[name]
	r.mu.Unlock()
	if !ok {
		return ""
	}
	return proc.outputLog.join("\n")
}

func (r *WorkerRunner) forget(name string) {
	r.mu.Lock()
	delete(r.procs, name)
	r.mu.Unlock()
}

// pump forwards worker output and captures the startup URL dsh prints. That URL
// carries the tenant's session token, so it is written to the handshake file the
// proxy serves from and never logged.
func (r *WorkerRunner) pump(proc *workerProc, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 8*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		if line == "" {
			continue
		}
		proc.outputLog.add(redactStartupURL(line))
		if match := startupURLRE.FindStringSubmatch(line); match != nil {
			port := match[2]
			if port != fmt.Sprint(proc.tenant.WorkerPort) {
				r.logger().Warn("worker startup URL port does not match the registry", "tenant", proc.tenant.Name, "url_port", port, "registry_port", proc.tenant.WorkerPort)
				continue
			}
			r.recordStartURL(proc, match[1])
		}
		r.logger().Debug("tenant worker", "tenant", proc.tenant.Name, "line", redactStartupURL(line))
	}
}

func (r *WorkerRunner) recordStartURL(proc *workerProc, url string) {
	proc.mu.Lock()
	if proc.startURL != "" {
		proc.mu.Unlock()
		return
	}
	proc.startURL = url
	proc.mu.Unlock()
	target := filepath.Join(r.Config.HandshakeDir, proc.tenant.Name+".url")
	if err := securefile.WriteAtomic(target, []byte(url+"\n"), 0o600); err != nil {
		r.logger().Error("writing the handshake file failed", "tenant", proc.tenant.Name, "err", err)
		return
	}
	r.logger().Info("tenant worker ready", "tenant", proc.tenant.Name, "port", proc.tenant.WorkerPort)
}

func (p *workerProc) isRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.finished
}

func (p *workerProc) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exitErr == nil {
		return errors.New("exit status unknown")
	}
	return p.exitErr
}

// workerEnv is the environment the old systemd units supplied through
// EnvironmentFile. With no unit there is no file: dshgw hands the values to the
// child directly, so the worker cannot be pointed at another tenant's state by
// editing a shared environment file.
// dshAnchor is the package.json plugin resolution is anchored to.
//
// It must name a path that exists *inside the sandbox*, and the sandbox binds the
// release's resolved directory (…/releases/r1), not the `current` symlink that
// usually points at it. Using the configured path here made `dsh` fail with
// "Cannot find module …" the moment a deployment kept the documented
// `/opt/dsh/current` layout, because the anchor resolved through a symlink the
// sandbox never had. Resolving it keeps the anchor and the mount set consistent.
func dshAnchor(cfg *config.Config) string {
	release := cfg.Dsh.CurrentLink
	if resolved, err := filepath.EvalSymlinks(release); err == nil {
		release = resolved
	}
	return filepath.Join(release, "package.json")
}

func workerEnv(cfg *config.Config, t registry.Tenant) []string {
	env := []string{
		"DSH_PORT=" + fmt.Sprint(t.WorkerPort),
		"DSH_HOME=" + t.DshHome,
		"HOME=" + t.Workspace,
		"DSHGW_DSH_ANCHOR=" + dshAnchor(cfg),
		"PATH=" + filepath.Dir(cfg.Dsh.NodeBin) + ":/usr/local/bin:/usr/bin:/bin",
	}
	// Keep the operator's own PATH additions (for example a locally installed
	// tool the tenant is expected to use) without letting the worker inherit
	// variables that describe the gateway itself.
	for _, key := range []string{"LANG", "LC_ALL", "TZ"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// redactStartupURL removes the session token from a worker log line: the token is
// a bearer credential for that tenant, and worker output is forwarded into the
// gateway log.
func redactStartupURL(line string) string {
	if idx := strings.Index(line, "/?token="); idx >= 0 {
		end := idx + len("/?token=")
		rest := line[end:]
		stop := strings.IndexAny(rest, " \t)")
		if stop < 0 {
			stop = len(rest)
		}
		return line[:end] + "[REDACTED]" + rest[stop:]
	}
	return line
}

func errText(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

const workerOutputLines = 60

// lineRing keeps the last N lines of a worker's output.
type lineRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLineRing(max int) *lineRing { return &lineRing{max: max} }

func (r *lineRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *lineRing) join(sep string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, sep)
}
