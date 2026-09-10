//go:build linux

package pluginhost

import "syscall"

// procAttrs isolates the plugin process in its own group and asks the kernel to
// deliver SIGTERM when the gateway dies, so orphaned plugins cannot pile up.
func procAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
}
