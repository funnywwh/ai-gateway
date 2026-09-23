package nodedep

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DeployLogDir is where one deploy log per node lives.
func DeployLogDir(stateDir string) string { return filepath.Join(stateDir, "node-deploy") }

// DeployLogPath is one node's deploy log.
func DeployLogPath(stateDir, node string) string {
	return filepath.Join(DeployLogDir(stateDir), node+".log")
}

// maxDeployLogBytes bounds one deploy log. A deploy writes a line per remote command plus its
// output; a few hundred kilobytes is plenty for the history an operator reads, and an unbounded
// file on a long-lived gateway is a slow leak.
const maxDeployLogBytes = 1 << 20

// MaxDeployLogTail is how much of a deploy log the API serves.
const MaxDeployLogTail = 64 << 10

// OpenDeployLog appends to a node's deploy log, rotating the previous one aside first when it grew
// past the bound. The rotation keeps exactly one generation: what matters is the last deploy.
func OpenDeployLog(stateDir, node string) (io.WriteCloser, string, error) {
	dir := DeployLogDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	path := DeployLogPath(stateDir, node)
	if _, err := os.Stat(path); err == nil {
		if info, err := os.Stat(path); err == nil && info.Size() > maxDeployLogBytes {
			_ = os.Remove(path + ".1")
			if err := os.Rename(path, path+".1"); err != nil {
				return nil, "", err
			}
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, "", err
	}
	fmt.Fprintf(file, "\n===== deploy %s =====\n", TimeNow().Format("2006-01-02T15:04:05Z07:00"))
	return file, path, nil
}

// ReadDeployLogTail returns the end of a node's deploy log, bounded and without partial garbage.
func ReadDeployLogTail(stateDir, node string) (string, error) {
	path := DeployLogPath(stateDir, node)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > MaxDeployLogTail {
		start = info.Size() - MaxDeployLogTail
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxDeployLogTail))
	if err != nil {
		return "", err
	}
	text := string(data)
	if start > 0 {
		// Drop the first (probably partial) line.
		if index := strings.Index(text, "\n"); index >= 0 {
			text = "…\n" + text[index+1:]
		}
	}
	return text, nil
}
