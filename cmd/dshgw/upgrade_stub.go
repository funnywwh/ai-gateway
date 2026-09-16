package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/winger/ai-gateway/internal/dshgw/upgrade"
)

func (c *cli) upgradeDSH(ctx context.Context, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: dshgw upgrade-dsh CANDIDATE_DIR")
	}
	if !filepath.IsAbs(args[0]) {
		return errors.New("candidate release directory must be absolute")
	}
	deps, err := c.loadRuntime(false)
	if err != nil {
		return err
	}
	pickerTest := filepath.Join(filepath.Dir(deps.cfg.Deploy.PluginPath), "picker-clamp.test.mjs")
	snapshot, err := deps.manager.Backup(ctx)
	if err != nil {
		return fmt.Errorf("pre-upgrade backup: %w", err)
	}
	var result upgrade.Result
	err = deps.manager.WithLifecycleLock(func() error {
		var applyErr error
		result, applyErr = upgrade.Apply(ctx, upgrade.Options{
			Config:     deps.cfg,
			Registry:   deps.reg,
			Manager:    deps.manager,
			Candidate:  args[0],
			PickerTest: pickerTest,
		})
		return applyErr
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "upgraded dsh to %s (%s); restarted %d tenant(s); backup: %s\n", result.Version, result.Current, len(result.Restarted), snapshot)
	return nil
}
