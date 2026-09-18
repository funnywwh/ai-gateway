//go:build linux

package tenancy

import "syscall"

// workerProcAttrs puts each tenant worker in its own process group — so one
// negative-pid signal reaches node and everything it spawned — and asks the
// kernel to SIGTERM it when dshgw dies. Without the death signal, a crashed
// dshgw would leave tenant processes holding their ports until someone noticed.
func workerProcAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}
}
