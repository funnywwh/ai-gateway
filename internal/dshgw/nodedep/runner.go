// Package nodedep deploys a dshgw worker node onto another machine over ssh (M77).
//
// The deploy is one idempotent sequence of phases — preflight, upload, activate, unit, start,
// verify — and every remote effect goes through one Exec seam (the system `ssh` binary, streaming
// the payload as a tar on stdin). That seam is what makes the whole thing testable without a
// second machine, and it is also why nothing here needs sftp, rsync or a second login: one ssh
// connection per phase, no agent forwarding, no passwords.
//
// Two properties are deliberate and load-bearing:
//
//   - the node's state directory is NEVER touched. A deploy replaces binaries, plugins, the
//     generated configuration and the unit file; the tenants, their workspaces and their registry
//     live under <dir>/state and survive every upgrade.
//   - a failed phase rolls back to what was there before, because the alternative is a node that
//     is half one version and half another.
package nodedep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
)

// ExecFunc runs one remote command. stdin carries the payload for the upload phase.
//
// Every remote effect in this package goes through it, so the tests drive the whole sequence
// against a stand-in "target" (a directory tree) and the production implementation is the only
// thing that knows ssh exists.
type ExecFunc func(ctx context.Context, command string, stdin io.Reader) (stdout, stderr []byte, err error)

// Assets are the local files a deploy ships. They come from the control plane's own deployment:
// the running binary (so both ends share a version), the plugin directories and the prepared
// tenant template.
type Assets struct {
	// DshgwBin is the control plane's own executable by default (os.Executable()).
	DshgwBin string
	// PluginDir is deploy.plugin_path's directory (picker-clamp.js plus the tenant plugins).
	PluginDir string
	// TemplateHome is the prepared dsh profile tree shipped to the node. Ignored when the deploy is
	// told to prepare it on the node instead.
	TemplateHome string
	// DshRoot is the dsh release directory; it is validated on the target, never shipped (a Node +
	// dsh installation is a runtime install, like bwrap).
	DshRoot string
}

// Options control one deploy.
type Options struct {
	// AcceptHostKey is the target's host-key fingerprint the operator confirmed. Empty means the
	// deploy stops after preflight and reports the fingerprint to confirm: trusting a key nobody
	// looked at is exactly what makes a first connection over ssh unsafe.
	AcceptHostKey string
	// PrepareTemplateOnNode skips shipping the template and runs the node's own prepare-template.sh
	// instead. It requires Corepack and network access on the node.
	PrepareTemplateOnNode bool
	// WithPackages installs missing OS packages with `sudo -n` (needs passwordless sudo). Off by
	// default: a deploy that silently apt-gets things is not a deploy anybody can predict.
	WithPackages bool
	// AigwBaseURL is what the generated configuration points the node's workers at.
	AigwBaseURL string
	// Runtime installation paths: what to override from the node record (a path that moved).
	BwrapBin    string
	NodeBin     string
	BinJS       string
	CurrentLink string
	// Features carried into the generated node configuration.
	DirectoryPicker   string
	PluginBrowserFS   string
	WorkerLimits      config.WorkerLimits
	SSHWorkspaces     bool
	BrowserWorkspaces bool
	TenantPlugins     config.TenantPlugins
	// HostShares are the host directories this node may bind (machine-local, so the record carries
	// them; the option exists for a deploy that changes them).
	HostShares *config.HostShares
	// Rotation replaces the node's token as part of the deploy (both sides at once).
	RotateToken bool
	// Token is the token to install; empty means "generate one" (or keep the existing one when the
	// target already has a token file and rotation was not asked for).
	Token string
}

// Phase names, in the order they run. They are what the operator sees while waiting and what the
// failure message names.
const (
	PhasePreflight = "preflight"
	PhaseUpload    = "upload"
	PhaseActivate  = "activate"
	PhaseUnit      = "unit"
	PhaseStart     = "start"
	PhaseVerify    = "verify"
)

// PhaseResult is one finished phase.
type PhaseResult struct {
	Name     string        `json:"name"`
	OK       bool          `json:"ok"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration"`
}

// Result is what a deploy did.
type Result struct {
	Phases []PhaseResult `json:"phases"`
	// Version/Revision are the node build that was installed (this control plane's own).
	Version  string `json:"version,omitempty"`
	Revision string `json:"revision,omitempty"`
	// Token is the node's shared secret as installed. It is only ever shown to the caller that has
	// to store it; the CLI masks it in output unless asked.
	Token string `json:"-"`
	// Fingerprint is the target's host key as pinned.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Systemd says whether the node is supervised by a systemd user unit (false means the
	// setsid/nohup fallback, which does not survive a reboot).
	Systemd bool `json:"systemd"`
	// RotatedToken reports that the token changed in this deploy.
	RotatedToken bool `json:"rotated_token,omitempty"`
}

// Runner performs deploys.
type Runner struct {
	// Exec runs remote commands. Nil means the production ssh runner (NewSSHExec).
	Exec ExecFunc
	// Probe verifies the node answers after it starts. Nil skips the verify phase (tests, and a
	// caller that verifies itself).
	Probe func(ctx context.Context, name string) error
	// Fingerprint reads the target's host key fingerprint out of its pinned known_hosts file — the
	// file the preflight connection just filled when the key was not pinned yet. Nil uses
	// ssh-keygen against that file.
	Fingerprint func(ctx context.Context, knownHostsFile, host string) (string, error)
	// OnActivated is called with the token once the target has it and the phase is still reversible.
	// The control plane persists it there, so the verification that follows (and every later
	// request) presents the secret the node actually holds instead of the one it had before the
	// deploy.
	OnActivated func(token string) error
	// Version/Revision are injected by cmd/dshgw's build flags.
	Version  string
	Revision string
	// Log receives the human narrative: one line per remote command (with its output on failure)
	// plus the phase transitions. It is the file operators read when a deploy fails at 02:00.
	Log io.Writer
	// Now is injectable for tests.
	Now func() time.Time
	// Timeout bounds one remote command.
	Timeout time.Duration
	// ProbeTimeout is how long to wait for a started node to answer (default 30s).
	ProbeTimeout time.Duration
}

func (r *Runner) log() *slog.Logger { return slog.Default() }

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) exec(ctx context.Context, name string, target nodestore.Node, command string, stdin io.Reader) ([]byte, []byte, error) {
	exec := r.Exec
	if exec == nil {
		exec = NewSSHExec(target)
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if r.Log != nil {
		fmt.Fprintf(r.Log, "%s [%s] $ %s\n", r.now().Format(time.RFC3339), name, oneLine(command))
	}
	stdout, stderr, err := exec(runCtx, command, stdin)
	if r.Log != nil {
		for _, stream := range []struct {
			label string
			data  []byte
		}{{"out", stdout}, {"err", stderr}} {
			if len(stream.data) == 0 {
				continue
			}
			for _, line := range strings.Split(strings.TrimRight(string(stream.data), "\n"), "\n") {
				fmt.Fprintf(r.Log, "%s [%s] %s: %s\n", r.now().Format(time.RFC3339), name, stream.label, line)
			}
		}
	}
	return stdout, stderr, err
}

// Deploy runs the whole sequence against one node.
//
// A failure at any phase after the target has been touched triggers a rollback to the previous
// binaries, plugins, configuration and unit — the caller gets the failure AND a node that is back
// on the version it was running before.
func (r *Runner) Deploy(ctx context.Context, target nodestore.Node, assets Assets, opts Options) (result Result, err error) {
	if err := r.prepareSSH(target); err != nil {
		return result, err
	}
	layout := newLayout(target)
	result.Version, result.Revision = r.Version, r.Revision
	phase := func(name string, run func() (string, error)) error {
		started := r.now()
		detail, phaseErr := run()
		result.Phases = append(result.Phases, PhaseResult{Name: name, OK: phaseErr == nil, Detail: detail, Duration: r.now().Sub(started)})
		if phaseErr != nil {
			return fmt.Errorf("%s: %w", name, phaseErr)
		}
		return nil
	}

	// ---- preflight -------------------------------------------------------------------------
	var facts map[string]string
	if err := phase(PhasePreflight, func() (string, error) {
		output, stderr, execErr := r.exec(ctx, PhasePreflight, target, preflightScript(layout, target, opts), nil)
		if execErr != nil {
			return "", execErrFrom(output, stderr, execErr)
		}
		facts = parseFacts(output)
		if missing := missingPreconditions(facts, opts); len(missing) > 0 {
			return strings.Join(missing, "; "), fmt.Errorf("this machine is missing %s", strings.Join(missing, ", "))
		}
		return facts["UNAME"] + " " + facts["USER"], nil
	}); err != nil {
		return result, err
	}
	// The fingerprint comes from our own known_hosts, not from the target: the key is what the
	// connection just established, and this is the value the operator is asked to confirm.
	fingerprintFn := r.Fingerprint
	if fingerprintFn == nil {
		fingerprintFn = HostKeyFingerprint
	}
	fingerprint, fingerprintErr := fingerprintFn(ctx, target.SSH.KnownHostsFile, target.SSH.Host)
	if fingerprintErr != nil {
		return result, fmt.Errorf("preflight: cannot read the target's host key from %s: %w", target.SSH.KnownHostsFile, fingerprintErr)
	}
	result.Fingerprint = strings.TrimSpace(fingerprint)
	if opts.AcceptHostKey == "" && strings.TrimSpace(target.SSH.HostKeyFingerprint) == "" {
		// The fingerprint gate: nothing has been sent yet, and the operator confirms once. From then
		// on the key is pinned in the record and every later connection is strict.
		return result, &FingerprintRequired{Host: target.SSH.Host, Fingerprint: result.Fingerprint}
	}
	if pinned := strings.TrimSpace(target.SSH.HostKeyFingerprint); pinned != "" {
		// A deploy against a node whose fingerprint was pinned must not silently accept a
		// different key: StrictHostKeyChecking would fail the command, but naming it here turns
		// "ssh exited 255" into "this is not the machine you deployed to last time".
		if opts.AcceptHostKey != "" && opts.AcceptHostKey != pinned {
			return result, fmt.Errorf("preflight: the operator confirmed %s, but %s was pinned as %s", opts.AcceptHostKey, target.Name, pinned)
		}
	} else if opts.AcceptHostKey != "" && opts.AcceptHostKey != result.Fingerprint {
		return result, fmt.Errorf("preflight: the operator confirmed %s, but %s answered with %s", opts.AcceptHostKey, target.SSH.Host, result.Fingerprint)
	}

	// ---- generate what the node will run ----------------------------------------------------
	token := strings.TrimSpace(opts.Token)
	keepToken := false
	switch {
	case token != "":
		// The caller supplied one (a rotation coordinated with the control plane's record).
	case opts.RotateToken:
		generated, genErr := GenerateToken()
		if genErr != nil {
			return result, genErr
		}
		token = generated
		result.RotatedToken = true
	case facts["HAS_TOKEN"] == "yes" && sameSecret(target.Token, facts["TOKEN_SHA256"]):
		// The target already holds the secret this control plane has: keep it. Rotating here would
		// invalidate the record for no reason.
		keepToken = true
	default:
		// Anything else means the two sides do not agree (a fresh node, a target from an interrupted
		// deploy, a record whose secret was lost). The deploy owns the token, so it installs a new
		// one on both sides rather than leaving a node nobody can talk to.
		generated, genErr := GenerateToken()
		if genErr != nil {
			return result, genErr
		}
		token = generated
		result.RotatedToken = true
	}
	result.Token = token

	generated, err := GenerateNodeConfig(target, opts, token, keepToken)
	if err != nil {
		return result, err
	}
	unit := GenerateUnit(target, layout)

	// ---- upload ----------------------------------------------------------------------------
	requiresTemplate := !opts.PrepareTemplateOnNode
	if err := phase(PhaseUpload, func() (string, error) {
		payload, cleanup, buildErr := buildPayload(assets, generated, unit, requiresTemplate)
		if buildErr != nil {
			return "", buildErr
		}
		defer cleanup()
		if token != "" && !keepToken {
			if err := writePayloadToken(payload, token); err != nil {
				return "", err
			}
		}
		if err := writePayloadBuild(payload, r.Version, r.Revision); err != nil {
			return "", err
		}
		if opts.WithPackages {
			output, stderr, execErr := r.exec(ctx, "packages", target, RemoteInstallScript(packagesToInstall(facts, opts)), nil)
			if execErr != nil {
				return "", execErrFrom(output, stderr, execErr)
			}
		}
		if err := layout.ensureRemoteDirs(ctx, r, target); err != nil {
			return "", err
		}
		size, sizeErr := payloadSize(payload)
		if sizeErr != nil {
			return "", sizeErr
		}
		tarStream, tarErr := tarReader(payload)
		if tarErr != nil {
			return "", tarErr
		}
		output, stderr, execErr := r.exec(ctx, PhaseUpload, target, uploadScript(layout), tarStream)
		if execErr != nil {
			return "", execErrFrom(output, stderr, execErr)
		}
		return fmt.Sprintf("%s of payload", humanBytes(size)), nil
	}); err != nil {
		return result, err
	}

	// ---- activate --------------------------------------------------------------------------
	if err := phase(PhaseActivate, func() (string, error) {
		output, stderr, execErr := r.exec(ctx, PhaseActivate, target, activateScript(layout, requiresTemplate, token, keepToken), nil)
		if execErr != nil {
			return "", execErrFrom(output, stderr, execErr)
		}
		if !keepToken && token != "" && r.OnActivated != nil {
			if err := r.OnActivated(token); err != nil {
				return "", fmt.Errorf("record the node's new token: %w", err)
			}
		}
		return "binaries, plugins, configuration in place", nil
	}); err != nil {
		// The old tree is still in .prev; put it back.
		rollbackErr := r.Rollback(ctx, target)
		return result, errors.Join(err, rollbackErr)
	}

	// ---- unit ------------------------------------------------------------------------------
	systemd := facts["USER_MANAGER"] == "yes"
	if err := phase(PhaseUnit, func() (string, error) {
		output, stderr, execErr := r.exec(ctx, PhaseUnit, target, unitScript(layout, systemd), nil)
		if execErr != nil {
			return "", execErrFrom(output, stderr, execErr)
		}
		return map[bool]string{true: "systemd user unit installed", false: "no user manager: setsid/nohup fallback"}[systemd], nil
	}); err != nil {
		return result, errors.Join(err, r.Rollback(ctx, target))
	}
	result.Systemd = systemd

	// ---- start (and verify) ----------------------------------------------------------------
	startErr := phase(PhaseStart, func() (string, error) {
		output, stderr, execErr := r.exec(ctx, PhaseStart, target, startScript(layout, systemd, target), nil)
		if execErr != nil {
			return "", execErrFrom(output, stderr, execErr)
		}
		detail := strings.TrimSpace(string(output))
		if r.Probe == nil {
			return detail, nil
		}
		probeErr := r.awaitProbe(ctx, target)
		if probeErr == nil {
			return detail, nil
		}
		if !systemd {
			return "", probeErr
		}
		// A systemd user manager that accepted the unit but never ran it is a real deployment shape:
		// a container, an ssh session without lingering, a host whose user bus answers
		// `show-environment` but cannot start units. Rather than fail the deploy, start the node
		// detached — and say which one happened, because "it is running" and "it comes back after a
		// reboot" are different promises.
		fallbackOut, fallbackErrOut, fallbackErr := r.exec(ctx, PhaseStart, target, startScript(layout, false, target), nil)
		if fallbackErr != nil {
			return "", errors.Join(probeErr, execErrFrom(fallbackOut, fallbackErrOut, fallbackErr))
		}
		systemd = false
		if retryErr := r.awaitProbe(ctx, target); retryErr != nil {
			return "", retryErr
		}
		return "started detached (the systemd user manager accepted the unit but did not run it)", nil
	})
	result.Systemd = systemd
	if startErr != nil {
		return result, errors.Join(startErr, r.Rollback(ctx, target))
	}
	return result, nil
}

// sameSecret compares this control plane's recorded secret with the hash the target reported. An
// empty value on either side means "cannot tell", which the caller turns into a rotation.
func sameSecret(recorded, reportedHash string) bool {
	recorded, reportedHash = strings.TrimSpace(recorded), strings.TrimSpace(reportedHash)
	if recorded == "" || reportedHash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(recorded))
	return hex.EncodeToString(sum[:]) == strings.ToLower(reportedHash)
}

// awaitProbe waits for the node to answer, so a start that takes a moment is not reported as a
// failure. A node that never answers is a failed deploy, and the caller rolls back.
func (r *Runner) awaitProbe(ctx context.Context, target nodestore.Node) error {
	if r.Probe == nil {
		return nil
	}
	deadline := r.now().Add(r.probeWindow())
	var last error
	for {
		last = r.Probe(ctx, target.Name)
		if last == nil {
			return nil
		}
		if r.now().After(deadline) || ctx.Err() != nil {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// probeWindow is how long a started node may take before the deploy calls it failed.
func (r *Runner) probeWindow() time.Duration {
	if r.ProbeTimeout > 0 {
		return r.ProbeTimeout
	}
	return 30 * time.Second
}

// Rollback restores the previous binaries, plugins, configuration and unit and starts them again.
//
// It is best-effort by design: if the rollback itself fails, the caller must hear about that too —
// a node left half-upgraded is exactly the state an operator has to be told about.
func (r *Runner) Rollback(ctx context.Context, target nodestore.Node) error {
	layout := newLayout(target)
	if r.Log != nil {
		fmt.Fprintf(r.Log, "%s [rollback] restoring the previous deployment\n", r.now().Format(time.RFC3339))
	}
	output, stderr, err := r.exec(ctx, "rollback", target, rollbackScript(layout), nil)
	if err != nil {
		return fmt.Errorf("rollback after a failed deploy: %w", execErrFrom(output, stderr, err))
	}
	return nil
}

// prepareSSH makes sure the connection can even be attempted: ssh appends to a known_hosts file
// but does not create the directory it lives in, and a missing private key is worth saying before
// ssh reports "permission denied" three phases later.
func (r *Runner) prepareSSH(target nodestore.Node) error {
	if key := strings.TrimSpace(target.SSH.KeyFile); key != "" {
		if _, err := os.Stat(key); err != nil {
			return fmt.Errorf("node %s: ssh private key %s is not readable: %w", target.Name, key, err)
		}
	}
	knownHosts := strings.TrimSpace(target.SSH.KnownHostsFile)
	if knownHosts == "" {
		return nil
	}
	if _, err := PrepareSSHDir(filepath.Dir(knownHosts)); err != nil {
		return fmt.Errorf("node %s: prepare %s: %w", target.Name, filepath.Dir(knownHosts), err)
	}
	if _, err := os.Stat(knownHosts); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
			return fmt.Errorf("node %s: create %s: %w", target.Name, knownHosts, err)
		}
	}
	return nil
}

// Stop stops the node on its machine: the systemd user unit when it has one, the pidfile otherwise.
func (r *Runner) Stop(ctx context.Context, target nodestore.Node, systemd bool) error {
	if err := r.prepareSSH(target); err != nil {
		return err
	}
	layout := newLayout(target)
	output, stderr, err := r.exec(ctx, "stop", target, stopScript(layout, systemd), nil)
	if err != nil {
		return fmt.Errorf("stop node %s: %w", target.Name, execErrFrom(output, stderr, err))
	}
	return nil
}

// Purge stops the node and deletes everything a deploy created there — including its state
// directory, which holds every tenant's workspace. It is only reachable from an explicit
// `node remove --purge --yes`.
func (r *Runner) Purge(ctx context.Context, target nodestore.Node) error {
	if err := r.prepareSSH(target); err != nil {
		return err
	}
	layout := newLayout(target)
	if err := r.Stop(ctx, target, true); err != nil {
		if r.Log != nil {
			fmt.Fprintf(r.Log, "stop before purge failed (continuing): %v\n", err)
		}
	}
	output, stderr, err := r.exec(ctx, "purge", target, purgeScript(layout), nil)
	if err != nil {
		return fmt.Errorf("purge node %s: %w", target.Name, execErrFrom(output, stderr, err))
	}
	return nil
}

// FingerprintRequired is returned by a first deploy so the operator can confirm the target's host
// key before anything is sent to it.
type FingerprintRequired struct {
	Host        string
	Fingerprint string
}

func (e *FingerprintRequired) Error() string {
	return fmt.Sprintf("%s has host key %s; confirm it with --accept-host-key %s (or `dshgw node deploy` again after verifying it out of band)", e.Host, e.Fingerprint, e.Fingerprint)
}

// execErrFrom folds a remote command's streams into the error, so a failure names what the remote
// said instead of just "exit status 1".
func execErrFrom(stdout, stderr []byte, err error) error {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		text = strings.TrimSpace(string(stdout))
	}
	if text == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, truncateOneLine(text, 400))
}

// parseFacts reads the KEY=VALUE lines a preflight prints.
func parseFacts(output []byte) map[string]string {
	facts := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		facts[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return facts
}

// missingPreconditions turns the preflight facts into the list of things that must be fixed.
func missingPreconditions(facts map[string]string, opts Options) []string {
	var missing []string
	for _, item := range []struct {
		key   string
		label string
	}{
		{"BWRAP", "bubblewrap at deploy.bwrap_bin"},
		{"NODE_BIN", "the Node runtime"},
		{"DSH_BIN_JS", "the dsh launcher"},
		{"DSH_ROOT", "the dsh release"},
		{"DIR_WRITABLE", "a writable deployment directory"},
	} {
		if facts[item.key] != "yes" {
			missing = append(missing, fmt.Sprintf("%s (%s=%s)", item.label, item.key, facts[item.key]))
		}
	}
	if opts.PrepareTemplateOnNode && facts["COREPACK"] != "yes" {
		missing = append(missing, "Corepack (needed to prepare the template on the node)")
	}
	if opts.WithPackages && facts["SUDO"] != "yes" {
		missing = append(missing, "passwordless sudo (--with-packages was requested)")
	}
	sort.Strings(missing)
	return missing
}

func oneLine(value string) string {
	return truncateOneLine(strings.Join(strings.Fields(value), " "), 300)
}

func truncateOneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// dirFiles lists the files a payload ships, for the log and for tests.
func dirFiles(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel != "." {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
