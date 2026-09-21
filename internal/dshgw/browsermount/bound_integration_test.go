package browsermount

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	fs "github.com/winger/ai-gateway/internal/dshgw/browserworkspace"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

func requireBrowserWorkspaceSandbox(t *testing.T) {
	t.Helper()
	if os.Getenv("BROWSERWORKSPACE_SANDBOX_TEST") != "1" {
		t.Skip("set BROWSERWORKSPACE_SANDBOX_TEST=1")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatal(err)
	}
}

// finishRealMount is test-only, after every namespace owner has exited. It also
// joins the FUSE server after externally performed crash cleanup, rather than
// issuing another Unmount that would only report an already-detached path.
func finishRealMount(t *testing.T, path string, server *fuse.Server) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		mounted, err := mountInfoPath(path)
		if err == nil && mounted {
			err = server.Unmount()
		}
		if err == nil {
			server.Wait()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Error("FUSE cleanup:", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("FUSE server cleanup exceeded deadline")
	}
}

func TestRealBoundClose(t *testing.T) {
	requireBrowserWorkspaceSandbox(t)
	workspace := t.TempDir()
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "hello"), []byte("bound-close"), 0600); err != nil {
		t.Fatal(err)
	}
	var server *fuse.Server
	var worker *exec.Cmd
	var stdin io.WriteCloser
	var stderr bytes.Buffer
	done := make(chan struct{})
	var waitErr error
	var closeInput sync.Once
	workerCtx, cancelWorker := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelWorker()
	stopWorker := func(ctx context.Context) error {
		if worker == nil {
			return nil
		}
		closeInput.Do(func() { _ = stdin.Close() })
		select {
		case <-done:
			return waitErr
		case <-ctx.Done():
			cancelWorker()
			select {
			case <-done:
				return errors.Join(ctx.Err(), waitErr)
			case <-time.After(2 * time.Second):
				return errors.New("worker wait exceeded cleanup deadline")
			}
		}
	}
	var callbacks atomic.Int32
	var service *Service
	service = NewWithMount(func(ctx context.Context, t registry.Tenant) error {
		callbacks.Add(1)
		if len(service.MountsFor(t.Name)) != 0 {
			return errors.New("closing mount still in worker profile")
		}
		select {
		case <-done:
			return errors.New("worker exited before close callback; test did not hold live namespace")
		default:
		}
		return stopWorker(ctx)
	}, func(path string, _ fs.Backend) (Mounted, error) {
		var err error
		server, err = fs.MountFS(path, fs.BackendFunc(func(_ context.Context, r fs.Request) (fs.Response, error) { return localOperation(local, r), nil }))
		return server, err
	})
	tenant := registry.Tenant{Name: "bound-close", Workspace: workspace}
	sh, err := service.open(tenant, "owner", "docs", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := stopWorker(ctx); err != nil {
			t.Error("worker cleanup:", err)
		}
		finishRealMount(t, sh.path, server)
	}()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	worker = exec.CommandContext(workerCtx, bwrap, "--die-with-parent", "--unshare-pid",
		"--ro-bind", "/usr", "/usr", "--ro-bind", "/usr/lib", "/lib", "--ro-bind", "/usr/lib64", "/lib64", "--ro-bind", "/bin", "/bin",
		"--dev", "/dev", "--proc", "/proc", "--bind", sh.path, sh.path, "--", "/bin/sh", "-c",
		`cd "$1" || exit 11; test "$(cat hello)" = bound-close || exit 12; printf 'READY\n'; IFS= read -r line || :; exit 0`, "sh", sh.path)
	worker.Stderr = &stderr
	stdout, err := worker.StdoutPipe()
	if err != nil {
		worker = nil
		t.Fatal(err)
	}
	stdin, err = worker.StdinPipe()
	if err != nil {
		worker = nil
		t.Fatal(err)
	}
	if err = worker.Start(); err != nil {
		worker = nil
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "READY\n" {
			err = fmt.Errorf("unexpected readiness %q", line)
		}
		ready <- err
	}()
	// Wait starts after READY's pipe read completes, per os/exec.StdoutPipe's
	// contract. After this point the sole Wait result is reusable by both paths.
	select {
	case err = <-ready:
	case <-workerCtx.Done():
		err = workerCtx.Err()
		cancelWorker()
	}
	go func() { waitErr = worker.Wait(); close(done) }()
	if err != nil {
		cancelWorker()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = stopWorker(ctx)
		select {
		case <-done:
			t.Fatalf("worker readiness: %v; stderr=%q", err, stderr.String())
		default:
			t.Fatal(err)
		}
	}
	// A correct read must remain blocked until the callback closes stdin. This
	// catches accidental shell typos/early exits that otherwise yield false PASS.
	select {
	case <-done:
		t.Fatalf("worker did not keep namespace alive: %v; stderr=%q", waitErr, stderr.String())
	case <-time.After(100 * time.Millisecond):
	}
	sh.mu.Lock()
	sh.restartAttempted = false
	sh.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := service.dispatch(ctx, "close", tenant, "owner", payload{Token: sh.token})
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		cancelWorker()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		_ = stopWorker(cleanupCtx)
		select {
		case <-result:
		case <-cleanupCtx.Done():
			t.Error("close goroutine did not finish after forced worker exit")
		}
		t.Fatal("close exceeded deadline")
	}
	if callbacks.Load() != 1 {
		t.Fatal("close did not stop bound worker exactly once")
	}
	select {
	case <-done:
		if waitErr != nil {
			t.Fatalf("worker exit: %v; stderr=%q", waitErr, stderr.String())
		}
	default:
		t.Fatal("unmounted before worker wait")
	}
	if mounted, err := mountInfoPath(sh.path); err != nil || mounted {
		t.Fatal("host mount survived close", mounted, err)
	}
}

func TestRealCrashCleanup(t *testing.T) {
	requireBrowserWorkspaceSandbox(t)
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	local := filepath.Join(base, "local")
	for _, p := range []string{workspace, local} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	tenant := registry.Tenant{Name: "crash", UID: 1001, PublicPort: 32101, WorkerPort: 32102, Workspace: workspace, DshHome: filepath.Join(base, "home"), CreatedAt: time.Now(), KeyPrefix: "crash-prefix", Handshake: registry.HandshakePending}
	reg := registry.New(filepath.Join(base, "registry.json"), filepath.Join(base, "registry.keys.map"))
	if err := reg.Put(tenant); err != nil {
		t.Fatal(err)
	}
	var server *fuse.Server
	old := NewWithState(nil, func(path string, _ fs.Backend) (Mounted, error) {
		var err error
		server, err = fs.MountFS(path, fs.BackendFunc(func(_ context.Context, r fs.Request) (fs.Response, error) { return localOperation(local, r), nil }))
		return server, err
	}, base)
	if err := old.AcquireLock(); err != nil {
		t.Fatal(err)
	}
	defer old.ReleaseLock()
	sh, err := old.open(tenant, "owner", "crash", true)
	if err != nil {
		t.Fatal(err)
	}
	defer finishRealMount(t, sh.path, server)
	if mounted, err := mountInfoPath(sh.path); err != nil || !mounted {
		t.Fatal("crash fixture not actually mounted", mounted, err)
	}
	// Simulate loss of the service's in-memory ownership, preserving kernel mount
	// and registry/record. No worker namespace exists in this crash fixture.
	if err := old.ReleaseLock(); err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef0123456789abcdef0123456789abcdef"
	malicious := mountRecord{ID: id, Tenant: tenant.Name, Workspace: workspace, Path: filepath.Join(workspace, "browser", "other"), State: "ready"}
	b, err := json.Marshal(malicious)
	if err != nil {
		t.Fatal(err)
	}
	filename := old.recordPath(tenant.Name, id)
	if err := os.WriteFile(filename, b, 0600); err != nil {
		t.Fatal(err)
	}
	current := NewWithState(nil, nil, base)
	current.SetRegistry(reg)
	if err := current.AcquireLock(); err != nil {
		t.Fatal(err)
	}
	defer current.ReleaseLock()
	result := make(chan error, 1)
	go func() { result <- current.CleanupStale() }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("malicious record accepted")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stale cleanup exceeded deadline")
	}
	if _, err := os.Stat(current.recordPath(sh.tenant.Name, sh.id)); !os.IsNotExist(err) {
		t.Fatal("valid stale record remains", err)
	}
	if _, err := os.Lstat(filename); err != nil {
		t.Fatal("malicious record deleted", err)
	}
	if mounted, err := mountInfoPath(sh.path); err != nil || mounted {
		t.Fatal("stale FUSE mount remains", mounted, err)
	}
	if _, err := os.Stat(sh.path); !os.IsNotExist(err) {
		t.Fatal("stale mountpoint remains", err)
	}
}
