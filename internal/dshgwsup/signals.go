package dshgwsup

import "syscall"

// syscallKill sends SIGTERM to a process group (negative pid).
func syscallKill(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
