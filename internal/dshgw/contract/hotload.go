package contract

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/config"
)

//go:embed credentials_hotload.mjs
var credentialsHotloadScript []byte

// DshChecks keeps the original seven compatibility checks and adds a real,
// no-cost model/credential-hotload roundtrip in the same disposable worker.
func DshChecks(runtime config.DshRuntime) []Check {
	return append(baseDshChecks(runtime), Check{
		Name: "credentials-live-hotload", Required: true, Timeout: 90 * time.Second,
		Run: func(ctx context.Context) error { return checkCredentialsHotload(ctx, runtime) },
	})
}

type boundedOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	const limit = 1 << 20
	n := len(data)
	if remaining := limit - b.Len(); len(data) > remaining {
		b.truncated = true
		data = data[:remaining]
	}
	_, _ = b.Buffer.Write(data)
	return n, nil // Drain excess output without retaining it.
}

func checkCredentialsHotload(ctx context.Context, runtime config.DshRuntime) error {
	file, err := os.CreateTemp("", "dshgw-hotload-*.mjs")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(credentialsHotloadScript); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, runtime.NodeBin, path, runtime.NodeBin, runtime.BinJS)
	// SIGKILL would strand the script's detached disposable worker. Its own
	// 60-second deadline normally wins; on caller cancellation, allow its
	// SIGTERM cleanup to settle before the executor's final kill deadline.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("disposable credential hotload contract failed; run its standalone probe for sanitized diagnostics")
	}
	if stdout.truncated || stderr.truncated {
		return errors.New("credential hotload contract output exceeded 1 MiB")
	}
	return validateHotloadReport(stdout.Bytes())
}

func validateHotloadReport(data []byte) error {
	var report struct {
		Passed               bool   `json:"passed"`
		DshVersion           string `json:"dshVersion"`
		FakeRequests         int    `json:"fakeRequests"`
		SameSession          bool   `json:"sameSession"`
		WorkerPidUnchanged   bool   `json:"workerPidUnchanged"`
		StartupAnnouncements int    `json:"startupAnnouncements"`
		CredentialReload     bool   `json:"credentialReloadEvent"`
		FirstKeyObserved     bool   `json:"firstKeyObserved"`
		RotatedKeyObserved   bool   `json:"rotatedKeyObserved"`
		Cleaned              bool   `json:"cleaned"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return errors.New("invalid credential hotload contract report")
	}
	if !report.Passed || report.DshVersion == "" || report.FakeRequests != 2 || !report.SameSession ||
		!report.WorkerPidUnchanged || report.StartupAnnouncements != 1 || !report.CredentialReload ||
		!report.FirstKeyObserved || !report.RotatedKeyObserved || !report.Cleaned {
		return errors.New("credential hotload contract did not attest both keys, reload, unchanged worker and cleanup")
	}
	return nil
}
