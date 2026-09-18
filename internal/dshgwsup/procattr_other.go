//go:build !linux

package dshgwsup

import "syscall"

// procAttrs keeps the supervisor buildable off Linux, where the dshgw child is not
// supported anyway (its isolation is bubblewrap). Only the process group is set.
func procAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
