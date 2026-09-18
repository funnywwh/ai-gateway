//go:build linux

package dshgwsup

import "syscall"

// procAttrs gives the child its own process group — so a single negative-pid signal
// reaches it and every tenant worker it started — and asks the kernel to deliver
// SIGTERM when aigw dies, so an ungraceful aigw exit cannot leave a dshgw holding
// tenant ports.
func procAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
}
