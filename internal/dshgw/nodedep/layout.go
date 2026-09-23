package nodedep

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/nodestore"
)

// GenerateToken mints a node's shared secret: 32 random bytes, base64url, no padding, no
// whitespace. It is the same shape the configuration documents and the node's token file expects.
func GenerateToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate a node token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// layout is every path the deploy touches on the target, derived from the node record so the
// generated configuration, the unit file and the shell scripts all agree.
type layout struct {
	dir          string
	binDir       string
	pluginDir    string
	templateHome string
	stateDir     string
	configPath   string
	tokenPath    string
	unitPath     string
	incoming     string
	pidFile      string
	logFile      string
	prevSuffix   string
}

func newLayout(target nodestore.Node) layout {
	dir := strings.TrimRight(target.Deploy.Dir, "/")
	if dir == "" {
		dir = "/srv/dshgw-node"
	}
	stateDir := target.Deploy.StateDir
	if stateDir == "" {
		stateDir = path.Join(dir, "state")
	}
	pluginPath := target.Deploy.PluginPath
	if pluginPath == "" {
		pluginPath = path.Join(dir, "plugins", "picker-clamp.js")
	}
	templateHome := target.Deploy.TemplateHome
	if templateHome == "" {
		templateHome = path.Join(dir, "template-home")
	}
	tokenPath := strings.TrimSpace(target.Deploy.TokenPath)
	if tokenPath == "" {
		tokenPath = path.Join(dir, target.Name+".token")
	}
	return layout{
		dir:          dir,
		binDir:       path.Join(dir, "bin"),
		pluginDir:    path.Dir(pluginPath),
		templateHome: templateHome,
		stateDir:     stateDir,
		configPath:   path.Join(dir, "dshgw-node.yaml"),
		tokenPath:    tokenPath,
		unitPath:     path.Join(dir, "dshgw-node.service"),
		incoming:     path.Join(dir, ".incoming"),
		pidFile:      path.Join(dir, "node.pid"),
		logFile:      path.Join(dir, "dshgw-node.log"),
		prevSuffix:   ".prev",
	}
}

// ensureRemoteDirs creates the tree a deploy writes into. The state directory is created but never
// modified afterwards: it is the node's data.
func (l layout) ensureRemoteDirs(ctx context.Context, runner *Runner, target nodestore.Node) error {
	mkdir := func(dir string, mode string) error {
		command := "mkdir -p " + shellQuote(dir) + " && chmod " + mode + " " + shellQuote(dir)
		stdout, stderr, err := runner.exec(ctx, "mkdir", target, command, nil)
		if err != nil {
			return execErrFrom(stdout, stderr, err)
		}
		return nil
	}
	for _, item := range []struct {
		dir  string
		mode string
	}{
		{l.dir, "0750"},
		{l.binDir, "0755"},
		{l.pluginDir, "0755"},
		{l.templateHome, "0755"},
		{l.incoming, "0700"},
		{l.stateDir, "0700"},
	} {
		if err := mkdir(item.dir, item.mode); err != nil {
			return err
		}
	}
	return nil
}

// preflightScript answers everything the deploy needs to know before it sends anything: the
// machine's architecture and user, whether the runtime bits exist, whether the deployment
// directory is writable, and whether a token and a user manager are already there.
//
// It installs nothing, even with --with-packages: that decision belongs to a phase whose failure
// the operator can see in the log.
func preflightScript(l layout, target nodestore.Node, opts Options) string {
	bwrap, nodeBin, binJS, dshRoot := runtimePaths(target, opts)
	checks := []string{
		"set -u",
		`echo "UNAME=$(uname -s -m 2>/dev/null)"`,
		`echo "USER=$(id -un 2>/dev/null)"`,
		`echo "HOME=$(printf %s "$HOME")"`,
		executableCheck("BWRAP", bwrap),
		executableCheck("NODE_BIN", nodeBin),
		fileCheck("DSH_BIN_JS", binJS),
		dirCheck("DSH_ROOT", dshRoot),
		`command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1 && echo "USER_MANAGER=yes" || echo "USER_MANAGER=no"`,
		`loginctl show-user "$(id -un)" 2>/dev/null | grep -q '^Linger=yes' && echo "LINGER=yes" || echo "LINGER=no"`,
		`sudo -n true >/dev/null 2>&1 && echo "SUDO=yes" || echo "SUDO=no"`,
		`command -v sshfs >/dev/null 2>&1 && echo "SSHFS=yes" || echo "SSHFS=no"`,
		`test -e /dev/fuse && echo "FUSE=yes" || echo "FUSE=no"`,
		`command -v corepack >/dev/null 2>&1 && echo "COREPACK=yes" || echo "COREPACK=no"`,
		`( mkdir -p ` + shellQuote(l.dir) + ` 2>/dev/null && test -w ` + shellQuote(l.dir) + ` ) && echo "DIR_WRITABLE=yes" || echo "DIR_WRITABLE=no"`,
		`test -f ` + shellQuote(l.tokenPath) + ` && echo "HAS_TOKEN=yes" || echo "HAS_TOKEN=no"`,
		// The token's hash, never the token: enough to decide whether this control plane and the
		// target already agree, without a second copy of the secret crossing the channel.
		// sha256 of the *trimmed* secret: the file ends in a newline, and the control plane compares
		// against the value it would present, not against the file's bytes.
		`test -f ` + shellQuote(l.tokenPath) + ` && echo "TOKEN_SHA256=$(tr -d '[:space:]' < ` + shellQuote(l.tokenPath) + ` 2>/dev/null | sha256sum | cut -d' ' -f1)" || echo "TOKEN_SHA256="`,
		`test -f ` + shellQuote(l.configPath) + ` && echo "HAS_CONFIG=yes" || echo "HAS_CONFIG=no"`,
		`df -Pk ` + shellQuote(l.dir) + ` 2>/dev/null | awk 'NR==2 {print "DISK_KB_FREE=" $4}'`,
		`echo "RUNNING=$(pgrep -f ` + shellQuote(path.Join(l.binDir, "dshgw")) + ` | head -1 | tr -d '\n')"`,
	}
	return strings.Join(checks, "\n")
}

// runtimePaths resolves the runtime installation paths a node's configuration will name. The node
// record is authoritative (it describes that machine); the options can override it, which is how a
// deploy picks up a path that moved.
func runtimePaths(target nodestore.Node, opts Options) (bwrap, nodeBin, binJS, currentLink string) {
	pick := func(values ...string) string {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	bwrap = pick(opts.BwrapBin, target.Deploy.BwrapBin, "/usr/bin/bwrap")
	nodeBin = pick(opts.NodeBin, target.Deploy.NodeBin)
	binJS = pick(opts.BinJS, target.Deploy.BinJS)
	currentLink = pick(opts.CurrentLink, target.Deploy.CurrentLink)
	if binJS == "" && currentLink != "" {
		binJS = path.Join(currentLink, "lib", "bin.js")
	}
	return bwrap, nodeBin, binJS, currentLink
}

// executableCheck reports whether a path is an executable file.
func executableCheck(key, path string) string {
	if strings.TrimSpace(path) == "" {
		return `echo "` + key + `=no"`
	}
	return `test -x ` + shellQuote(path) + ` && echo "` + key + `=yes" || echo "` + key + `=no"`
}

// fileCheck reports whether a path is a readable file.
func fileCheck(key, path string) string {
	if strings.TrimSpace(path) == "" {
		return `echo "` + key + `=no"`
	}
	return `test -r ` + shellQuote(path) + ` && echo "` + key + `=yes" || echo "` + key + `=no"`
}

// dirCheck reports whether a path is a directory
func dirCheck(key, path string) string {
	if strings.TrimSpace(path) == "" {
		return `echo "` + key + `=no"`
	}
	return `test -d ` + shellQuote(path) + ` && echo "` + key + `=yes" || echo "` + key + `=no"`
}

// uploadScript receives the payload as a tar stream and unpacks it into <dir>/.incoming, which
// activate then swaps in. Nothing the node is currently running is touched by this phase.
func uploadScript(l layout) string {
	return strings.Join([]string{
		"set -eu",
		"rm -rf " + shellQuote(l.incoming),
		"mkdir -p " + shellQuote(l.incoming),
		"tar -xf - -C " + shellQuote(l.incoming),
		"chmod 0755 " + shellQuote(path.Join(l.incoming, "bin", "dshgw")) + " 2>/dev/null || true",
		"chmod 0600 " + shellQuote(path.Join(l.incoming, "token")) + " 2>/dev/null || true",
		"echo upload-ok",
	}, "\n")
}

// activateScript swaps the uploaded tree in, keeping the previous one as .prev for the rollback.
// The state directory is not in the list and is never mentioned: a deploy must not be able to
// touch a node's tenants.
func activateScript(l layout, withTemplate bool, token string, keepToken bool) string {
	swaps := []string{
		swapLines(path.Join(l.incoming, "bin"), l.binDir, l.prevSuffix),
		swapLines(path.Join(l.incoming, "plugins"), l.pluginDir, l.prevSuffix),
		swapLines(path.Join(l.incoming, "dshgw-node.yaml"), l.configPath, l.prevSuffix),
		swapLines(path.Join(l.incoming, "dshgw-node.service"), l.unitPath, l.prevSuffix),
	}
	if withTemplate {
		swaps = append(swaps, swapLines(path.Join(l.incoming, "template-home"), l.templateHome, l.prevSuffix))
	}
	lines := []string{"set -eu", "umask 077"}
	lines = append(lines, swaps...)
	if !keepToken {
		lines = append(lines,
			"install -m 0600 "+shellQuote(path.Join(l.incoming, "token"))+" "+shellQuote(l.tokenPath))
	}
	lines = append(lines, "rm -rf "+shellQuote(l.incoming), "echo activate-ok")
	return strings.Join(lines, "\n")
}

// swapLines replaces one target with one uploaded item, keeping the previous copy.
func swapLines(newPath, target, prevSuffix string) string {
	return strings.Join([]string{
		"rm -rf " + shellQuote(target+prevSuffix),
		"[ -e " + shellQuote(target) + " ] && mv " + shellQuote(target) + " " + shellQuote(target+prevSuffix) + " || true",
		"mv " + shellQuote(newPath) + " " + shellQuote(target),
	}, "\n")
}

// rollbackScript puts every .prev back and starts the node again. It is written to work even when
// only some of the swaps happened: each step is conditional.
func rollbackScript(l layout) string {
	targets := []string{l.binDir, l.pluginDir, l.configPath, l.unitPath, l.templateHome}
	lines := []string{"set -u"}
	for _, target := range targets {
		lines = append(lines,
			"if [ -e "+shellQuote(target+l.prevSuffix)+" ]; then rm -rf "+shellQuote(target)+"; mv "+shellQuote(target+l.prevSuffix)+" "+shellQuote(target)+"; fi")
	}
	// Restart with whatever is now in place; the fallback covers a target with no user manager.
	lines = append(lines,
		"if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then",
		"  systemctl --user daemon-reload >/dev/null 2>&1 || true",
		"  systemctl --user restart dshgw-node >/dev/null 2>&1 || true",
		"else",
		"  if [ -f "+shellQuote(l.pidFile)+" ]; then kill \"$(cat "+shellQuote(l.pidFile)+")\" 2>/dev/null || true; sleep 1; fi",
		"  if [ -x "+shellQuote(path.Join(l.binDir, "dshgw"))+" ]; then",
		"    setsid nohup "+shellQuote(path.Join(l.binDir, "dshgw"))+" --config "+shellQuote(l.configPath)+" node serve >> "+shellQuote(l.logFile)+" 2>&1 < /dev/null &",
		"    echo $! > "+shellQuote(l.pidFile),
		"  fi",
		"fi",
		"echo rollback-ok")
	return strings.Join(lines, "\n")
}

// unitScript installs and (re)starts the node: a systemd user unit when the machine has a user
// manager, and otherwise a detached process with a pidfile — with the consequence stated in the
// output, because "it is running" and "it comes back after a reboot" are different promises.
func unitScript(l layout, systemd bool) string {
	if systemd {
		return strings.Join([]string{
			"set -eu",
			"mkdir -p ~/.config/systemd/user",
			"install -m 0644 " + shellQuote(l.unitPath) + " ~/.config/systemd/user/dshgw-node.service",
			"systemctl --user daemon-reload",
			"systemctl --user enable dshgw-node >/dev/null",
			"loginctl show-user \"$(id -un)\" 2>/dev/null | grep -q '^Linger=yes' || loginctl enable-linger \"$(id -un)\" >/dev/null 2>&1 || true",
			"echo unit-ok",
		}, "\n")
	}
	return strings.Join([]string{
		"set -eu",
		"mkdir -p ~/.config/systemd/user",
		"install -m 0644 " + shellQuote(l.unitPath) + " ~/.config/systemd/user/dshgw-node.service",
		"echo no-user-manager-detected",
		"echo unit-ok",
	}, "\n")
}

// startScript (re)starts the node and reports how it was started.
func startScript(l layout, systemd bool, target nodestore.Node) string {
	if systemd {
		return strings.Join([]string{
			"set -eu",
			"systemctl --user restart dshgw-node",
			"systemctl --user is-active dshgw-node",
		}, "\n")
	}
	return strings.Join([]string{
		"set -eu",
		"if [ -f " + shellQuote(l.pidFile) + " ]; then kill \"$(cat " + shellQuote(l.pidFile) + ")\" 2>/dev/null || true; sleep 1; fi",
		"setsid nohup " + shellQuote(path.Join(l.binDir, "dshgw")) + " --config " + shellQuote(l.configPath) + " node serve >> " + shellQuote(l.logFile) + " 2>&1 < /dev/null &",
		"echo $! > " + shellQuote(l.pidFile),
		"sleep 1",
		"cat " + shellQuote(l.pidFile),
	}, "\n")
}

// stopScript stops the node: the unit when there is one, the pidfile otherwise. `node remove`
// uses it; the state directory is left alone unless the caller asked for a purge.
func stopScript(l layout, systemd bool) string {
	lines := []string{"set -u"}
	if systemd {
		lines = append(lines, "systemctl --user stop dshgw-node >/dev/null 2>&1 || true")
	}
	lines = append(lines,
		"if [ -f "+shellQuote(l.pidFile)+" ]; then kill \"$(cat "+shellQuote(l.pidFile)+")\" 2>/dev/null || true; rm -f "+shellQuote(l.pidFile)+"; fi",
		"echo stop-ok")
	return strings.Join(lines, "\n")
}

// purgeScript removes everything a deploy created, including the state directory. It is only ever
// run for an explicit removal with purge, and it refuses to guess: the paths are the ones the
// record names.
func purgeScript(l layout) string {
	// The .prev copies are part of what a deploy created (an upgrade leaves the previous binaries
	// behind for its rollback), so a purge removes them too: "removed" must mean removed.
	var lines []string
	lines = append(lines, "set -u", "rm -rf "+shellQuote(l.stateDir))
	for _, target := range []string{l.binDir, l.pluginDir, l.templateHome, l.configPath, l.unitPath} {
		lines = append(lines, "rm -rf "+shellQuote(target)+" "+shellQuote(target+l.prevSuffix))
	}
	lines = append(lines,
		"rm -f "+shellQuote(l.tokenPath)+" "+shellQuote(l.logFile)+" "+shellQuote(l.pidFile),
		"rm -f ~/.config/systemd/user/dshgw-node.service",
		"rmdir "+shellQuote(l.dir)+" 2>/dev/null || true",
		"echo purge-ok")
	return strings.Join(lines, "\n")
}

// shellQuote wraps a value for a POSIX shell. Paths here come from the node's record (an operator
// wrote them) and are never built from untrusted input, but quoting them is still the difference
// between "a path with a space" and "a command nobody meant to run".
func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// payloadSize totals a payload directory for the log line.
func payloadSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// humanBytes renders a byte count for humans.
func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

// copyFile is used while building the payload.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies a directory tree, preserving the executable bit (plugin files are loaded by dsh,
// which needs them readable; the picker host half is a file URL, not an executable).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Symlinks in a plugin directory point at the repository or the release; shipping the
			// link would ship a path that does not exist on the node. Follow it instead.
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			return copyFile(resolved, target, 0o644)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

// sortedKeys is a small helper for deterministic log output in tests.
func sortedKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
