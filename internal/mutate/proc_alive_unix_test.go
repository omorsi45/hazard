//go:build !windows

package mutate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// processAlive reports whether the process exists and is not a zombie, which
// init may not reap in a container.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return !errors.Is(err, syscall.ESRCH)
	}
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := bytes.LastIndexByte(data, ')'); i >= 0 && i+2 < len(data) && data[i+2] == 'Z' {
			return false
		}
	}
	return true
}
