package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/contract"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
	"github.com/winger/ai-gateway/internal/dshgw/tenancy"
)

type Options struct {
	Config     *config.Config
	Registry   *registry.Registry
	Manager    *tenancy.Manager
	Candidate  string
	PickerTest string
	Checks     []contract.Check
	RunPicker  func(context.Context, string) error
}
type Result struct {
	Version   string
	Previous  string
	Current   string
	Restarted []string
}

// Apply upgrades the selected release and restarts workers that were active
// before the switch. The caller must hold Manager.WithLifecycleLock for the
// entire operation; Apply deliberately does not acquire a nested lifecycle
// lock. This keeps the upgrade atomic with the caller's registry snapshot and
// prevents lock re-entrancy deadlocks.
func Apply(ctx context.Context, opt Options) (result Result, err error) {
	if opt.Config == nil || opt.Registry == nil || opt.Manager == nil {
		return result, errors.New("upgrade dependencies are incomplete")
	}
	candidate, err := canonicalPath(opt.Candidate)
	if err != nil {
		return result, err
	}
	root, err := canonicalPath(opt.Config.Dsh.ReleasesRoot)
	if err != nil {
		return result, err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || filepath.Dir(rel) != "." {
		return result, errors.New("candidate must be one release directory below dsh.releases_root")
	}
	version, err := releaseVersion(candidate)
	if err != nil {
		return result, err
	}
	candidateRuntime := opt.Config.Dsh
	candidateRuntime.BinJS = filepath.Join(candidate, "lib", "bin.js")
	checks := opt.Checks
	if checks == nil {
		checks = contract.DshChecks(candidateRuntime)
	}
	report := contract.Run(ctx, checks)
	if !report.OK {
		return result, errors.New("candidate failed required dsh contracts")
	}
	runPicker := opt.RunPicker
	if runPicker == nil {
		runPicker = func(ctx context.Context, candidate string) error {
			return pickerContract(ctx, opt.Config.Dsh.NodeBin, opt.PickerTest, opt.Config.Deploy.PluginPath, candidate)
		}
	}
	if err := runPicker(ctx, candidate); err != nil {
		return result, err
	}

	current, err := filepath.Abs(opt.Config.Dsh.CurrentLink)
	if err != nil {
		return result, err
	}
	info, err := os.Lstat(current)
	if err != nil {
		return result, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return result, errors.New("dsh.current_link must be a symbolic link")
	}
	oldTarget, err := os.Readlink(current)
	if err != nil {
		return result, err
	}
	if !filepath.IsAbs(oldTarget) {
		oldTarget = filepath.Join(filepath.Dir(current), oldTarget)
	}
	oldTarget, err = canonicalPath(oldTarget)
	if err != nil {
		return result, err
	}
	if oldTarget == candidate {
		return Result{Version: version, Previous: oldTarget, Current: candidate}, nil
	}

	// Capture the pre-upgrade lifecycle state before changing the link. A
	// status error is fail-closed: switching without knowing whether a worker
	// was active could stop an intentionally running worker or start one that
	// was intentionally stopped.
	active := make([]registry.Tenant, 0)
	for _, tenant := range opt.Registry.List() {
		status, statusErr := opt.Manager.Status(ctx, tenant)
		if statusErr != nil || strings.TrimSpace(status.Detail) == "" {
			if statusErr == nil {
				statusErr = errors.New("worker status detail is unavailable")
			}
			return result, fmt.Errorf("status tenant %s before upgrade: %w", tenant.Name, statusErr)
		}
		if status.Active {
			active = append(active, tenant)
		}
	}

	if err := switchLink(current, candidate); err != nil {
		return result, err
	}
	switched := true
	attempted := make([]registry.Tenant, 0, len(active))
	restarted := make([]string, 0, len(active))
	defer func() {
		if !switched {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Workers must never be restarted while the candidate link is still
		// selected. If restoration fails, preserve that error and leave workers
		// untouched rather than running them against the wrong release.
		restoreErr := switchLink(current, oldTarget)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore dsh.current_link: %w", restoreErr))
			return
		}
		err = errors.Join(err, rollbackWorkers(rollbackCtx, opt.Manager, attempted))
	}()
	for _, tenant := range active {
		// Record the attempt before Restart so a failed candidate restart is
		// still retried against the restored link during rollback.
		attempted = append(attempted, tenant)
		if restartErr := opt.Manager.Restart(ctx, tenant); restartErr != nil {
			err = fmt.Errorf("restart tenant %s with candidate: %w", tenant.Name, restartErr)
			return result, err
		}
		if probeErr := opt.Manager.ProbeWorker(ctx, tenant); probeErr != nil {
			err = fmt.Errorf("probe tenant %s with candidate: %w", tenant.Name, probeErr)
			return result, err
		}
		restarted = append(restarted, tenant.Name)
	}
	switched = false
	result = Result{Version: version, Previous: oldTarget, Current: candidate, Restarted: restarted}
	return result, nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func releaseVersion(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return "", err
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	if doc.Version == "" {
		return "", errors.New("candidate package.json has no version")
	}
	if _, err := os.Stat(filepath.Join(root, "lib", "bin.js")); err != nil {
		return "", err
	}
	return doc.Version, nil
}

func switchLink(link, target string) error {
	absoluteLink, err := filepath.Abs(link)
	if err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(absoluteLink), "."+filepath.Base(absoluteLink)+".next-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	tempLink := filepath.Join(tempDir, filepath.Base(absoluteLink))
	if err := os.Symlink(target, tempLink); err != nil {
		return err
	}
	if err := os.Rename(tempLink, absoluteLink); err != nil {
		return err
	}
	return nil
}

func rollbackWorkers(ctx context.Context, manager *tenancy.Manager, tenants []registry.Tenant) (rollbackErr error) {
	for _, tenant := range tenants {
		restartErr := manager.Restart(ctx, tenant)
		if restartErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback restart tenant %s: %w", tenant.Name, restartErr))
		}
		if probeErr := manager.ProbeWorker(ctx, tenant); probeErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("rollback probe tenant %s: %w", tenant.Name, probeErr))
		}
	}
	return rollbackErr
}

func pickerContract(ctx context.Context, node, test, plugin, candidate string) error {
	if test == "" {
		return errors.New("picker contract test path is required")
	}
	cmd := exec.CommandContext(ctx, node, test)
	cmd.Env = append(os.Environ(), "DSHGW_DSH_ROOT="+candidate, "DSHGW_PICKER_PLUGIN="+plugin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("picker contract failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
