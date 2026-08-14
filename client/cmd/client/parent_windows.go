//go:build windows

package main

import "golang.org/x/sys/windows"

// pidAlive reports whether pid is still running on Windows. OpenProcess with
// PROCESS_QUERY_LIMITED_INFORMATION succeeds only for a live pid; a process that
// has exited reports an exit code other than STILL_ACTIVE (259).
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
