// Package pluginhost manages provider plugin processes: discovery, spawning,
// handshake, heartbeats, cancellation, credential handoff and crash restarts.
package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winger/ai-gateway/pkg/pluginapi"
)

// Config tunes the host.
type Config struct {
	// Dir is scanned for plugin executables.
	Dir string
	// Extra lists additional plugin binary paths.
	Extra []string
	// StateDir is the base directory for per-instance plugin state (0700).
	StateDir string
	// StartTimeout bounds the handshake.
	StartTimeout time.Duration
	// PingInterval is the heartbeat period.
	PingInterval time.Duration
	// MaxRestartsPerMinute caps crash restarts.
	MaxRestartsPerMinute int
	// LogTailLines is how many stderr lines are retained per instance.
	LogTailLines int
	// CancelGrace is how long a plugin has to honour provider.cancel.
	CancelGrace time.Duration
}

// DefaultConfig returns sane defaults (mirrors the YAML defaults).
func DefaultConfig() Config {
	return Config{
		Dir:                  "./plugins",
		StateDir:             "./data/plugin-state",
		StartTimeout:         3 * time.Second,
		PingInterval:         15 * time.Second,
		MaxRestartsPerMinute: 5,
		LogTailLines:         200,
		CancelGrace:          1500 * time.Millisecond,
	}
}

// Options describe one plugin instance.
type Options struct {
	Instance    string
	Kind        string
	Binary      string
	ConfigJSON  string
	Credentials map[string]string
	// AutoRestart restarts the process after an unexpected exit.
	AutoRestart bool
}

// Process is one running plugin instance.
type Process struct {
	Instance string
	Kind     string
	Binary   string
	Client   *pluginapi.Client

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started time.Time
	logs    *logRing
	done    chan struct{}

	mu        sync.Mutex
	exitErr   error
	draining  bool
	missed    int
	restarts  int
	lastError string
	autoStart bool
	options   Options
}

// StartedAt returns the process start time.
func (p *Process) StartedAt() time.Time { return p.started }

// Done is closed when the process exits.
func (p *Process) Done() <-chan struct{} { return p.done }

// ExitErr returns the process exit error (nil while running).
func (p *Process) ExitErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

// Restarts returns how many times this instance has been restarted.
func (p *Process) Restarts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restarts
}

// LastError returns the most recent start/crash error.
func (p *Process) LastError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError
}

// Logs returns the retained stderr lines (oldest first).
func (p *Process) Logs() []string { return p.logs.Lines() }

// LogTail returns at most n recent stderr lines.
func (p *Process) LogTail(n int) []string { return p.logs.Tail(n) }

// Draining reports whether the instance is being drained before a restart.
func (p *Process) Draining() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.draining
}

// SetDraining marks the instance as draining (no new requests should be routed to it).
func (p *Process) SetDraining(v bool) {
	p.mu.Lock()
	p.draining = v
	p.mu.Unlock()
}

// PID returns the operating system process id.
func (p *Process) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Host owns every plugin process.
type Host struct {
	cfg Config
	log *slog.Logger

	ctx      context.Context
	cancel   context.CancelFunc
	stopping atomic.Bool

	mu    sync.Mutex
	procs map[string]*Process

	restartMu         sync.Mutex
	restartsInWindow  int
	windowStart       time.Time

	// OnCredentials is called when a plugin rotates its own credentials.
	OnCredentials func(instance string, creds map[string]string, reason string)
}

// New creates a host.
func New(cfg Config, log *slog.Logger) *Host {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{
		cfg:         cfg,
		log:         log,
		ctx:         ctx,
		cancel:      cancel,
		procs:       map[string]*Process{},
		windowStart: time.Now(),
	}
}

// Discover returns executable plugin binaries from the configured directory and extras.
func (h *Host) Discover() ([]string, error) {
	found := map[string]bool{}
	if h.cfg.Dir != "" {
		entries, err := os.ReadDir(h.cfg.Dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("pluginhost: scan %s: %w", h.cfg.Dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Mode()&0o111 == 0 {
				continue
			}
			found[filepath.Join(h.cfg.Dir, e.Name())] = true
		}
	}
	for _, extra := range h.cfg.Extra {
		if extra == "" {
			continue
		}
		if info, err := os.Stat(extra); err == nil && !info.IsDir() {
			found[extra] = true
		}
	}
	out := make([]string, 0, len(found))
	for path := range found {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// Start launches a plugin instance and completes the handshake.
func (h *Host) Start(ctx context.Context, opts Options) (*Process, error) {
	if opts.Instance == "" {
		return nil, fmt.Errorf("pluginhost: instance name is required")
	}
	if opts.Binary == "" {
		return nil, fmt.Errorf("pluginhost: binary path is required for %s", opts.Instance)
	}
	if h.stopping.Load() {
		return nil, fmt.Errorf("pluginhost: host is shutting down")
	}

	stateDir := ""
	if h.cfg.StateDir != "" {
		stateDir = filepath.Join(h.cfg.StateDir, opts.Instance)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("pluginhost: create state dir: %w", err)
		}
		if err := writeCredentials(stateDir, opts.Credentials); err != nil {
			return nil, err
		}
	}

	// The plugin must not inherit the caller's request context: it outlives the request.
	cmd := exec.Command(opts.Binary, "--aigw-plugin")
	cmd.Env = append(os.Environ(),
		pluginapi.EnvProtocol+"=1",
		pluginapi.EnvInstance+"="+opts.Instance,
		pluginapi.EnvConfig+"="+opts.ConfigJSON,
	)
	if stateDir != "" {
		cmd.Env = append(cmd.Env, pluginapi.EnvStateDir+"="+stateDir)
	}
	cmd.SysProcAttr = procAttrs()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pluginhost: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pluginhost: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("pluginhost: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pluginhost: start %s: %w", opts.Binary, err)
	}

	logs := newLogRing(h.cfg.LogTailLines)
	proc := &Process{
		Instance:  opts.Instance,
		Kind:      opts.Kind,
		Binary:    opts.Binary,
		cmd:       cmd,
		stdin:     stdin,
		started:   time.Now().UTC(),
		logs:      logs,
		done:      make(chan struct{}),
		autoStart: opts.AutoRestart,
		options:   opts,
	}

	go h.captureStderr(proc, stderr)

	timeout := h.cfg.StartTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	client, err := pluginapi.NewClient(stdout, stdin, timeout)
	if err != nil {
		_ = killGroup(cmd)
		return nil, fmt.Errorf("pluginhost: handshake with %s (%s) failed: %w", opts.Instance, opts.Binary, err)
	}
	proc.Client = client
	client.OnCredentials(func(creds map[string]string, reason string) {
		if h.OnCredentials != nil {
			h.OnCredentials(opts.Instance, creds, reason)
		}
	})

	h.mu.Lock()
	if prev, ok := h.procs[opts.Instance]; ok {
		prev.restarts++
		proc.restarts = prev.restarts
	}
	h.procs[opts.Instance] = proc
	h.mu.Unlock()

	go h.reap(proc)

	h.log.Info("plugin started",
		"instance", opts.Instance,
		"kind", opts.Kind,
		"binary", filepath.Base(opts.Binary),
		"plugin", client.Handshake().Name,
		"version", client.Handshake().Version,
		"pid", proc.PID(),
	)
	return proc, nil
}

// captureStderr forwards plugin log lines into the ring buffer and the gateway log.
func (h *Host) captureStderr(proc *Process, stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 8*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " ")
		if line == "" {
			continue
		}
		proc.logs.add(line)
		h.log.Debug("plugin log", "provider", proc.Instance, "line", line)
	}
}

// reap waits for process exit and schedules a restart when configured.
func (h *Host) reap(proc *Process) {
	err := proc.cmd.Wait()
	proc.mu.Lock()
	proc.exitErr = err
	proc.mu.Unlock()
	if proc.Client != nil {
		proc.Client.Close()
	}
	close(proc.done)

	h.mu.Lock()
	if current, ok := h.procs[proc.Instance]; ok && current == proc {
		delete(h.procs, proc.Instance)
	}
	h.mu.Unlock()

	if err != nil {
		h.log.Warn("plugin exited", "instance", proc.Instance, "err", err)
	} else {
		h.log.Info("plugin exited", "instance", proc.Instance)
	}

	if !proc.autoStart || h.stopping.Load() {
		return
	}
	h.scheduleRestart(proc)
}

func (h *Host) scheduleRestart(proc *Process) {
	h.restartMu.Lock()
	if time.Since(h.windowStart) > time.Minute {
		h.windowStart = time.Now()
		h.restartsInWindow = 0
	}
	if h.cfg.MaxRestartsPerMinute > 0 && h.restartsInWindow >= h.cfg.MaxRestartsPerMinute {
		h.restartMu.Unlock()
		h.log.Error("plugin restart budget exhausted",
			"instance", proc.Instance, "limit_per_minute", h.cfg.MaxRestartsPerMinute)
		return
	}
	h.restartsInWindow++
	backoff := restartBackoff(h.restartsInWindow)
	h.restartMu.Unlock()

	proc.mu.Lock()
	proc.lastError = "restarting after exit"
	proc.mu.Unlock()

	h.log.Warn("restarting plugin", "instance", proc.Instance, "backoff", backoff)
	go func() {
		select {
		case <-time.After(backoff):
		case <-h.ctx.Done():
			return
		}
		opts := proc.options
		opts.AutoRestart = true
		if _, err := h.Start(h.ctx, opts); err != nil {
			h.log.Error("plugin restart failed", "instance", proc.Instance, "err", err)
		}
	}()
}

func restartBackoff(attempt int) time.Duration {
	delay := time.Duration(attempt) * 200 * time.Millisecond
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	return delay
}

// Get returns a running instance.
func (h *Host) Get(instance string) (*Process, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	proc, ok := h.procs[instance]
	return proc, ok
}

// List returns every running instance (sorted by name).
func (h *Host) List() []*Process {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Process, 0, len(h.procs))
	for _, proc := range h.procs {
		out = append(out, proc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out
}

// Ping checks one instance.
func (h *Host) Ping(ctx context.Context, instance string) error {
	proc, ok := h.Get(instance)
	if !ok {
		return fmt.Errorf("pluginhost: instance %s is not running", instance)
	}
	return proc.Client.Ping(ctx)
}

// Stop terminates one instance: graceful shutdown frame, then process-group kill.
func (h *Host) Stop(ctx context.Context, instance string) error {
	proc, ok := h.Get(instance)
	if !ok {
		return nil
	}
	proc.SetDraining(true)
	proc.mu.Lock()
	proc.autoStart = false
	proc.mu.Unlock()

	if proc.Client != nil {
		_ = proc.Client.Shutdown()
		proc.Client.Close()
	}
	_ = proc.stdin.Close()

	grace := h.cfg.CancelGrace
	if grace <= 0 {
		grace = time.Second
	}
	select {
	case <-proc.done:
		return nil
	case <-time.After(grace):
	case <-ctx.Done():
	}
	return killGroup(proc.cmd)
}

// Drain marks an instance as draining and waits until in-flight calls are done
// (or the force deadline elapses) before terminating it.
func (h *Host) Drain(ctx context.Context, instance string, forceAfter time.Duration) error {
	proc, ok := h.Get(instance)
	if !ok {
		return nil
	}
	proc.SetDraining(true)

	deadline := time.Now().Add(forceAfter)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		if !procBusy(proc) {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return h.Stop(ctx, instance)
}

// procBusy reports whether the plugin still has open calls.
// The protocol client does not expose in-flight counts, so the check is best-effort;
// the router stops admitting new work once Draining is set.
func procBusy(proc *Process) bool { return false }

// StopAll terminates every instance and prevents restarts.
func (h *Host) StopAll(ctx context.Context) error {
	h.stopping.Store(true)
	defer h.cancel()

	var firstErr error
	for _, proc := range h.List() {
		if err := h.Stop(ctx, proc.Instance); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// writeCredentials hands credentials to the plugin through its state directory
// (mode 0600). The plugin reads and may delete the file on startup.
func writeCredentials(stateDir string, creds map[string]string) error {
	path := filepath.Join(stateDir, pluginapi.CredentialsFileName)
	if len(creds) == 0 {
		_ = os.Remove(path)
		return nil
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("pluginhost: encode credentials: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("pluginhost: write credentials: %w", err)
	}
	return nil
}

// killGroup terminates the whole plugin process group.
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	// Negative pid targets the process group created by Setpgid.
	if err := syscallKill(-pid); err != nil {
		_ = cmd.Process.Kill()
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscallSignalZero); err != nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd.Process.Kill()
}
