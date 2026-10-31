//go:build windows

package runner

import (
	"os/exec"
	"strconv"
)

// killOnCancel makes canceling the context of cmd end its whole process tree.
// go test runs the test binary as a child process: killing only the go command
// would leave a hung test running after the caller has given up on it.
func killOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		// Best effort: the Kill below reports a failure to end the process itself.
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}
