//go:build !windows

package runner

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killOnCancel starts cmd in a process group of its own and makes canceling
// its context kill the whole group. go test runs the test binary as a child
// process: killing only the go command would leave a hung test running, and
// holding the output pipes open, after the caller has given up on it.
func killOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// The group ID is the ID of the process that led it.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
