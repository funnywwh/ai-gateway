//go:build !linux

package pluginhost

import "syscall"

// procAttrs isolates the plugin process in its own group on non-Linux platforms.
func procAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
