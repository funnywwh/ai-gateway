package nodedep

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Payload layout inside the tar the target receives. The names are fixed so the activate script can
// address them; the token travels as `token` and is installed to the path the record names.
const (
	payloadBinDir       = "bin"
	payloadPluginDir    = "plugins"
	payloadTemplateDir  = "template-home"
	payloadConfigFile   = "dshgw-node.yaml"
	payloadUnitFile     = "dshgw-node.service"
	payloadTokenFile    = "token"
	payloadVersionFile  = "VERSION"
	payloadRevisionFile = "REVISION"
)

// buildPayload assembles the upload in a temporary directory and returns it with a cleanup.
//
// What is shipped, and why:
//
//   - `bin/dshgw` — the control plane's own executable, so both ends speak one protocol version;
//   - `plugins/` — the picker and the tenant plugins, which a node renders into every profile;
//   - `template-home/` — the prepared dsh profile, unless the deploy was told to prepare it on the
//     node (that path needs Corepack and network access there);
//   - `dshgw-node.yaml`, `dshgw-node.service`, `token` — generated for this node;
//   - VERSION/REVISION — what this deploy installed, so an operator can read it without a log.
//
// It deliberately ships no Node runtime and no dsh release: those are machine-local installations
// the preflight verifies, not artifacts to copy around.
func buildPayload(assets Assets, nodeConfig, unit []byte, withTemplate bool) (string, func(), error) {
	dir, err := os.MkdirTemp("", "dshgw-node-payload-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", func() {}, err
	}
	if strings.TrimSpace(assets.DshgwBin) == "" {
		return fail(fmt.Errorf("no dshgw binary to ship: name the control plane's own binary"))
	}
	if err := copyFile(assets.DshgwBin, filepath.Join(dir, payloadBinDir, "dshgw"), 0o755); err != nil {
		return fail(fmt.Errorf("stage the dshgw binary: %w", err))
	}
	if strings.TrimSpace(assets.PluginDir) == "" {
		return fail(fmt.Errorf("no plugin directory to ship (deploy.plugin_path's directory)"))
	}
	if err := copyTree(assets.PluginDir, filepath.Join(dir, payloadPluginDir)); err != nil {
		return fail(fmt.Errorf("stage the plugin directory: %w", err))
	}
	if withTemplate {
		if strings.TrimSpace(assets.TemplateHome) == "" {
			return fail(fmt.Errorf("no prepared template to ship; use --prepare-template-on-node on a node with Corepack and network access"))
		}
		if err := copyTree(assets.TemplateHome, filepath.Join(dir, payloadTemplateDir)); err != nil {
			return fail(fmt.Errorf("stage the prepared template: %w", err))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, payloadConfigFile), nodeConfig, 0o600); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, payloadUnitFile), unit, 0o644); err != nil {
		return fail(err)
	}
	return dir, cleanup, nil
}

// writePayloadToken adds the node's token to a built payload. It is a separate step because the
// token's value is decided after preflight (the target may already have one worth keeping).
func writePayloadToken(dir, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, payloadTokenFile), []byte(token+"\n"), 0o600)
}

// writePayloadBuild writes the build identity files.
func writePayloadBuild(dir, version, revision string) error {
	if err := os.WriteFile(filepath.Join(dir, payloadVersionFile), []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, payloadRevisionFile), []byte(revision+"\n"), 0o644)
}

// tarReader streams a payload directory as a gzipped tar. The remote side unpacks it straight into
// the incoming directory, so no second transfer tool (scp, sftp, rsync) has to exist on either end.
func tarReader(dir string) (io.Reader, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	err := filepath.Walk(dir, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, current)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		name := path.Join("./", filepath.ToSlash(rel))
		switch {
		case info.IsDir():
			return writer.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			return writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777})
		default:
			header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: int64(info.Mode().Perm()), Size: info.Size()}
			if err := writer.WriteHeader(header); err != nil {
				return err
			}
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			if _, err := io.Copy(writer, file); err != nil {
				file.Close()
				return err
			}
			return file.Close()
		}
	})
	if err != nil {
		writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return &buffer, nil
}

// RemoteInstallScript renders the optional package install (--with-packages). It runs only when the
// operator asked for it and the preflight found passwordless sudo; the command it runs is printed
// in the deploy log either way, so a deploy never installs something silently.
func RemoteInstallScript(packages []string) string {
	if len(packages) == 0 {
		return "true"
	}
	quoted := make([]string, 0, len(packages))
	for _, pkg := range packages {
		quoted = append(quoted, shellQuote(pkg))
	}
	return strings.Join([]string{
		"set -eu",
		"command -v apt-get >/dev/null 2>&1 || { echo 'this node has no apt-get; install the packages by hand'; exit 1; }",
		"sudo -n apt-get install -y " + strings.Join(quoted, " "),
		"echo packages-ok",
	}, "\n")
}
