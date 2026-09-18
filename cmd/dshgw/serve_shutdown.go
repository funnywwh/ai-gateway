package main

import (
	"context"
	"fmt"
)

type browserShutdownService interface {
	Quiesce()
	Shutdown(context.Context) error
}

// shutdownBrowserWorkspaces is the ordered ownership handoff. Quiesce wakes
// pending requests and waits out browser restart callbacks WITHOUT holding locks
// needed by worker hooks. The worker callback must terminally fence new starts
// and synchronously release namespaces. Never unmount after an earlier failure.
func shutdownBrowserWorkspaces(ctx context.Context, service browserShutdownService, reaperDone, startupDone <-chan struct{}, drain, stopWorkers func(context.Context) error) error {
	service.Quiesce()
	for _, done := range []<-chan struct{}{reaperDone, startupDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("wait browser lifecycle owners: %w", ctx.Err())
		}
	}
	if err := drain(ctx); err != nil {
		return fmt.Errorf("drain browser ingress: %w", err)
	}
	if err := stopWorkers(ctx); err != nil {
		return fmt.Errorf("stop workers before browser cleanup: %w", err)
	}
	if err := service.Shutdown(ctx); err != nil {
		return fmt.Errorf("browser cleanup: %w", err)
	}
	return nil
}
