//go:build windows

package mutate

import "syscall"

// stillActive is the exit code GetExitCodeProcess reports for a running process.
const stillActive = 259

// processAlive reports whether the process exists and has not exited.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
