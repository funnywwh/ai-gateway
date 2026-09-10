package pluginhost

import "syscall"

// syscallKill sends SIGTERM to a process group (negative pid).
func syscallKill(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// syscallSignalZero probes process liveness without delivering a signal.
var syscallSignalZero = syscall.Signal(0)
