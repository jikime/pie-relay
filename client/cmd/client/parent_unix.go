//go:build !windows

package main

import "syscall"

// pidAlive reports whether pid is still running. syscall.Kill(pid, 0) probes a
// pid without signaling it: nil = alive, EPERM = alive but not ours to signal,
// ESRCH = gone.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
