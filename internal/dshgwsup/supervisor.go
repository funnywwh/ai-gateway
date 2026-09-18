package dshgwsup

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
)

// readyMarker is the child's readiness line (cmd/dshgw/serve.go logs
// msg="dshgw listening" through slog's text handler immediately before it accepts
// connections). Matching log text couples aigw to one line of the child's output —
// the same deliberate coupling dshgw already relies on to capture dsh's startup
// URL — and it is the only signal that means "listening", as opposed to "process
// started".
const readyMarker = "dshgw listening"

// Default timeouts. Readiness is fast (the child loads a small registry); shutdown
// gets longer because the child drains tenant connections.
const (
	DefaultReadyTimeout  = 20 * time.Second
	DefaultStopTimeout   = 25 * time.Second
	DefaultMaxRestarts   = 3
	DefaultRestartWindow = 5 * time.Minute

	// outputLines is how much tail output is kept for error messages: enough to
	// explain a refusal, small enough to stay out of the way.
	outputLines = 40
)

// Options configures one supervised child.
type Options struct {
	// Binary is the absolute path of the dshgw executable.
	Binary string
	// ConfigPath is the configuration written for the child.
	ConfigPath string
	Logger     *slog.Logger
	// ReadyTimeout bounds the wait for readyMarker.
	ReadyTimeout time.Duration
	// StopTimeout bounds the wait for a graceful exit before SIGKILL.
	StopTimeout time.Duration
	// MaxRestarts bounds unexpected exits inside RestartWindow.
	MaxRestarts   int
	RestartWindow time.Duration
	// Now is injectable so the restart window is testable without sleeping.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = DefaultReadyTimeout
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = DefaultStopTimeout
	}
	if o.MaxRestarts <= 0 {
		o.MaxRestarts = DefaultMaxRestarts
	}
	if o.RestartWindow <= 0 {
		o.RestartWindow = DefaultRestartWindow
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now() }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Supervisor owns one dshgw child process for the lifetime of aigw.
type Supervisor struct {
	opts Options

	mu    sync.Mutex
	cmd   *exec.Cmd
	pid   int
	ready bool
	// readySignalled records that Start's wait has already been released. It is
	// deliberately separate from ready: a restarted child reports readiness again,
	// and closing the same channel twice would panic. Only Start waits, and only
	// once, so the signal is one-shot while ready stays per-child.
	readySignalled bool
	stopping       bool
	started        bool
	restarts       []time.Time
	lastErr        error
	lines          *lineRing

	done    chan struct{}
	readyCh chan struct{}
}

// New validates the options and returns a supervisor that has not started anything.
func New(opts Options) (*Supervisor, error) {
	opts = opts.withDefaults()
	if _, err := checkExecutable(opts.Binary); err != nil {
		return nil, err
	}
	if err := checkAbsolute("dshgw config", opts.ConfigPath); err != nil {
		return nil, err
	}
	return &Supervisor{opts: opts, lines: newLineRing(outputLines)}, nil
}

// LookupBinary resolves the dshgw executable that lives next to aigw. The sibling
// layout is what makes "one directory, aigw is the main program" true: an
// explicit override exists for unusual layouts, but the default is derived from
// the running executable so a moved installation cannot silently keep using a
// stale dshgw.
func LookupBinary(aigwExecutable, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("dshgw.binary %q must be an absolute path", override)
		}
		return checkExecutable(override)
	}
	if strings.TrimSpace(aigwExecutable) == "" {
		return "", errors.New("cannot locate the running aigw executable; set dshgw.binary explicitly")
	}
	return checkExecutable(filepath.Join(filepath.Dir(aigwExecutable), "dshgw"))
}

func checkExecutable(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("dshgw executable %s is unusable: %w", path, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("dshgw executable %s is not an executable file", path)
	}
	return path, nil
}

// Start launches the child and returns once it reports readiness. A child that
// exits before becoming ready is an error here, not a silent background retry:
// aigw's operator asked for a working DSH surface and must hear that it is not
// there.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("dshgw supervisor already started")
	}
	s.started = true
	s.stopping = false
	s.done = make(chan struct{})
	s.readyCh = make(chan struct{})
	done, readyCh := s.done, s.readyCh
	s.mu.Unlock()

	go s.loop(ctx)

	select {
	case <-readyCh:
		s.opts.Logger.Info("dshgw child ready", "pid", s.PID(), "binary", s.opts.Binary, "config", s.opts.ConfigPath)
		return nil
	case <-done:
		return fmt.Errorf("dshgw exited before becoming ready: %w", s.Err())
	case <-time.After(s.opts.ReadyTimeout):
		// Stop what we started: leaving a half-initialised child behind would hold
		// ports the operator is about to inspect.
		stopCtx, cancel := context.WithTimeout(context.Background(), s.opts.StopTimeout)
		defer cancel()
		_ = s.Stop(stopCtx)
		return fmt.Errorf("dshgw did not report readiness within %s: %w", s.opts.ReadyTimeout, s.Err())
	case <-ctx.Done():
		return ctx.Err()
	}
}

// loop runs one child at a time and decides whether an unexpected exit may be
// retried. It returns only when aigw is stopping or the restart budget is spent.
func (s *Supervisor) loop(ctx context.Context) {
	defer close(s.done)
	for {
		cmd, err := s.spawn()
		if err != nil {
			s.fail(fmt.Errorf("starting dshgw failed: %w", err))
			return
		}
		waitErr := cmd.Wait()
		s.mu.Lock()
		stopping := s.stopping
		s.pid = 0
		s.cmd = nil
		s.mu.Unlock()
		if stopping || ctx.Err() != nil {
			s.opts.Logger.Info("dshgw child stopped", "err", errString(waitErr))
			return
		}
		if !s.allowRestart() {
			s.fail(fmt.Errorf("dshgw exited unexpectedly (%s) and the restart budget (%d in %s) is spent; DSH tenants are unavailable until aigw restarts: %s",
				errString(waitErr), s.opts.MaxRestarts, s.opts.RestartWindow, s.RecentOutput()))
			return
		}
		s.opts.Logger.Warn("dshgw child exited; restarting", "err", errString(waitErr))
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// spawn starts one child with its own process group and death signal: if aigw is
// killed outright (SIGKILL, panic), the kernel still tears the child down instead
// of leaving an orphan holding tenant ports.
func (s *Supervisor) spawn() (*exec.Cmd, error) {
	cmd := exec.Command(s.opts.Binary, "--config", s.opts.ConfigPath, "serve")
	cmd.SysProcAttr = procAttrs()
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	s.mu.Lock()
	s.cmd = cmd
	s.pid = cmd.Process.Pid
	s.ready = false
	s.mu.Unlock()
	go s.pump(pr)
	s.opts.Logger.Info("dshgw child started", "pid", cmd.Process.Pid, "binary", s.opts.Binary)
	return cmd, nil
}

// pump forwards the child's output. Both streams share one pipe so the log keeps
// the child's own ordering, which matters when a startup failure prints a cause
// and then a consequence.
func (s *Supervisor) pump(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 8*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		if line == "" {
			continue
		}
		s.lines.add(line)
		if !s.isReady() && strings.Contains(line, readyMarker) {
			s.markReady()
		}
		s.opts.Logger.Debug("dshgw", "line", line)
	}
}

// Stop terminates the child and waits for the supervisor loop to finish. It is
// safe to call when nothing is running.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	pid := s.pid
	done := s.done
	s.mu.Unlock()

	if pid > 0 {
		// Negative pid targets the process group created by Setpgid, so tenant
		// workers started by the child receive the signal too.
		if err := syscallKill(-pid); err != nil {
			s.opts.Logger.Warn("signalling the dshgw process group failed", "pid", pid, "err", err)
		}
	}
	select {
	case <-done:
		return nil
	case <-time.After(s.opts.StopTimeout):
		s.opts.Logger.Warn("dshgw did not exit in time; killing it", "pid", pid)
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	case <-ctx.Done():
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("dshgw supervisor loop did not finish after the child was killed")
	}
}

// Ready reports whether the current child has reported readiness.
func (s *Supervisor) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// PID returns the current child's process id, or 0 when none is running.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

// Err returns the failure that made DSH unavailable, or nil while it is healthy.
func (s *Supervisor) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// RecentOutput returns the tail of the child's output, for error messages.
func (s *Supervisor) RecentOutput() string { return s.lines.join("\n") }

func (s *Supervisor) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *Supervisor) markReady() {
	s.mu.Lock()
	s.ready = true
	alreadySignalled := s.readySignalled
	s.readySignalled = true
	readyCh := s.readyCh
	s.mu.Unlock()
	if !alreadySignalled && readyCh != nil {
		close(readyCh)
	}
}

// allowRestart records one restart and reports whether it is inside the budget.
func (s *Supervisor) allowRestart() bool {
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.restarts[:0]
	for _, at := range s.restarts {
		if now.Sub(at) < s.opts.RestartWindow {
			kept = append(kept, at)
		}
	}
	s.restarts = append(kept, now)
	return len(s.restarts) <= s.opts.MaxRestarts
}

func (s *Supervisor) fail(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
	s.opts.Logger.Error("dshgw child unavailable", "err", err)
}

func errString(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// lineRing keeps the last N lines of child output.
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
