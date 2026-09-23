package nodedep

import "strings"

// packagesToInstall lists the OS packages a --with-packages deploy would install, given what the
// preflight found. It is deliberately explicit: this deployment ships no package-format logic
// beyond apt, and a node that needs something else is told so rather than half-installed.
func packagesToInstall(facts map[string]string, opts Options) []string {
	var packages []string
	if facts["BWRAP"] != "yes" {
		packages = append(packages, "bubblewrap")
	}
	if opts.SSHWorkspaces && facts["SSHFS"] != "yes" {
		packages = append(packages, "sshfs")
	}
	if opts.BrowserWorkspaces && facts["FUSE"] != "yes" {
		// /dev/fuse comes with the kernel package; naming it makes the missing piece obvious.
		packages = append(packages, "fuse3")
	}
	return packages
}

// PreflightFacts is the parsed preflight answer, exposed for the CLI's diagnostics.
type PreflightFacts map[string]string

// Get returns one preflight fact.
func (f PreflightFacts) Get(key string) string { return strings.TrimSpace(f[key]) }
