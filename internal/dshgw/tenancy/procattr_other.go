//go:build !linux

package tenancy

import "syscall"

// workerProcAttrs keeps the package buildable off Linux, where the bwrap-based
// worker isolation does not exist. Only the process group is set.
func workerProcAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
