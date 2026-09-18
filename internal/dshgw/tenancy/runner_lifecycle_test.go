package tenancy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// Stand-in processes only: no node, sandbox, mounts, network or real workers.
// The append-only PID log lets tests detect orphaned processes, not merely trust
// the runner's map (which was exactly what overlapping starts could corrupt).
func lifecycleFixture(t *testing.T) (*WorkerRunner, registry.Tenant, string) {
	t.Helper()
	r, _, root := runnerFixture(t, "#!/bin/sh\necho $$ >> launches\nexec sleep 300\n")
	tenant := testTenant(t, root, "lifecycle", 32155)
	r.StopTimeout = time.Second
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return r, tenant, filepath.Join(tenant.Workspace, "launches")
}

func launchedPIDs(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, text := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(text)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func assertLivePIDs(t *testing.T, path string, want int) {
	t.Helper()
	live := 0
	for _, pid := range launchedPIDs(t, path) {
		err := syscall.Kill(pid, 0)
		if err == nil {
			live++
		} else if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("check pid %d: %v", pid, err)
		}
	}
	if live != want {
		t.Fatalf("live fixture processes = %d, want %d", live, want)
	}
}

func awaitLifecycle(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("lifecycle operation deadlocked")
		return nil
	}
}

func TestWorkerRunnerLifecycleConcurrentStartsAndRestarts(t *testing.T) {
	r, tenant, launches := lifecycleFixture(t)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 12)
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			results <- r.Start(ctx, tenant)
		}()
	}
	close(start)
	success := 0
	for i := 0; i < cap(results); i++ {
		err := awaitLifecycle(t, results)
		if err == nil {
			success++
		} else if !strings.Contains(err.Error(), "already running") {
			t.Fatal(err)
		}
	}
	if success != 1 || len(launchedPIDs(t, launches)) != 1 {
		t.Fatalf("successful starts = %d, launches = %v", success, launchedPIDs(t, launches))
	}
	assertLivePIDs(t, launches, 1)

	// All concurrent restarts must succeed, not interleave Stop/Start halves.
	for i := 0; i < cap(results); i++ {
		go func() { results <- r.Restart(ctx, tenant) }()
	}
	for i := 0; i < cap(results); i++ {
		if err := awaitLifecycle(t, results); err != nil {
			t.Fatalf("concurrent restart: %v", err)
		}
	}
	assertLivePIDs(t, launches, 1)
	if len(launchedPIDs(t, launches)) != 13 {
		t.Fatalf("expected each restart to launch one replacement: %v", launchedPIDs(t, launches))
	}
}

func TestWorkerRunnerLifecycleStopWaitsForProfile(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("all=%v", all), func(t *testing.T) {
			r, tenant, launches := lifecycleFixture(t)
			profile := r.Profile
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			r.Profile = func(t registry.Tenant) ([]string, error) {
				close(entered)
				<-release
				return profile(t)
			}
			started, stopped := make(chan error, 1), make(chan error, 1)
			go func() { started <- r.Start(context.Background(), tenant) }()
			<-entered
			go func() {
				if all {
					stopped <- r.StopAll(context.Background())
				} else {
					stopped <- r.Stop(context.Background(), tenant)
				}
			}()
			select {
			case err := <-stopped:
				t.Fatalf("stop bypassed in-flight profile: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			unblock()
			if err := awaitLifecycle(t, started); err != nil {
				t.Fatal(err)
			}
			if err := awaitLifecycle(t, stopped); err != nil {
				t.Fatal(err)
			}
			assertLivePIDs(t, launches, 0)
			// StopAll, unlike Shutdown, must remain reusable for backups.
			r.Profile = profile
			if err := r.Start(context.Background(), tenant); err != nil {
				t.Fatal(err)
			}
			assertLivePIDs(t, launches, 1)
		})
	}
}

func TestWorkerRunnerLifecycleCanceledStartNeverSpawns(t *testing.T) {
	r, tenant, launches := lifecycleFixture(t)
	profile := r.Profile
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int
	r.Profile = func(t registry.Tenant) ([]string, error) {
		calls++
		return profile(t)
	}
	if err := r.Start(ctx, tenant); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("pre-canceled start: err=%v profile calls=%d", err, calls)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	r.Profile = func(t registry.Tenant) ([]string, error) {
		cancel() // Cancellation while building the profile must also prevent exec.
		return profile(t)
	}
	if err := r.Start(ctx, tenant); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled during profile: %v", err)
	}
	if len(launchedPIDs(t, launches)) != 0 || len(r.Running()) != 0 {
		t.Fatal("canceled start launched a process")
	}
}

func TestWorkerRunnerLifecycleShutdownWaitsAndIsTerminal(t *testing.T) {
	r, tenant, launches := lifecycleFixture(t)
	profile := r.Profile
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	r.Profile = func(t registry.Tenant) ([]string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return profile(t)
	}
	started, shutdown := make(chan error, 1), make(chan error, 1)
	go func() { started <- r.Start(context.Background(), tenant) }()
	<-entered
	go func() { shutdown <- r.Shutdown(context.Background()) }()
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown bypassed in-flight start: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	unblock()
	if err := awaitLifecycle(t, started); err != nil {
		t.Fatal(err)
	}
	if err := awaitLifecycle(t, shutdown); err != nil {
		t.Fatal(err)
	}
	assertLivePIDs(t, launches, 0)
	results := make(chan error, 32)
	for i := 0; i < cap(results); i++ {
		go func(restart bool) {
			if restart {
				results <- r.Restart(context.Background(), tenant)
			} else {
				results <- r.Start(context.Background(), tenant)
			}
		}(i%2 == 0)
	}
	for i := 0; i < cap(results); i++ {
		if err := awaitLifecycle(t, results); !errors.Is(err, ErrWorkerRunnerShutdown) {
			t.Fatalf("terminal start/restart: %v", err)
		}
	}
	if calls.Load() != 1 || len(launchedPIDs(t, launches)) != 1 {
		t.Fatal("profile or process launched after terminal shutdown")
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRunnerLifecycleMixedTransitionsAndShutdown(t *testing.T) {
	r, tenant, launches := lifecycleFixture(t)
	ctx := context.Background()
	if err := r.Start(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	begin := make(chan struct{})
	results := make(chan error, 40)
	for i := 0; i < cap(results); i++ {
		go func(op int) {
			<-begin
			switch op % 5 {
			case 0:
				results <- r.Start(ctx, tenant)
			case 1:
				results <- r.Stop(ctx, tenant)
			case 2:
				results <- r.Restart(ctx, tenant)
			case 3:
				results <- r.StopAll(ctx)
			case 4:
				results <- r.Shutdown(ctx)
			}
		}(i)
	}
	close(begin)
	for i := 0; i < cap(results); i++ {
		err := awaitLifecycle(t, results)
		if err != nil && !errors.Is(err, ErrWorkerRunnerShutdown) && !strings.Contains(err.Error(), "already running") {
			t.Fatal(err)
		}
	}
	assertLivePIDs(t, launches, 0)
	if len(r.Running()) != 0 {
		t.Fatal("worker running after shutdown")
	}
}

func TestManagerWorkersConcurrentInitialization(t *testing.T) {
	for _, injected := range []bool{false, true} {
		t.Run(fmt.Sprintf("injected=%v", injected), func(t *testing.T) {
			m := &Manager{Config: &config.Config{}}
			var want *WorkerRunner
			if injected {
				want = &WorkerRunner{}
				m.Workers = want
			}
			begin := make(chan struct{})
			results := make(chan *WorkerRunner, 64)
			for i := 0; i < cap(results); i++ {
				go func() {
					<-begin
					results <- m.workers()
				}()
			}
			close(begin)
			for i := 0; i < cap(results); i++ {
				got := <-results
				if want == nil {
					want = got
				}
				if got != want {
					t.Fatal("concurrent getters returned different runners")
				}
			}
		})
	}
}
