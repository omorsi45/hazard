package runner

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	treeHelperEnv  = "HAZARD_TEST_TREE_HELPER"
	treePIDFileEnv = "HAZARD_TEST_TREE_PIDFILE"
)

// TestTreeHelper is the process the tree test runs, not a test of its own: it
// does nothing unless treeHelperEnv names a role. The parent plays go test, which
// starts the test binary as a child; the grandchild plays that test binary: it
// records its process ID and sleeps.
func TestTreeHelper(t *testing.T) {
	switch os.Getenv(treeHelperEnv) {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestTreeHelper$")
		child.Env = append(os.Environ(), treeHelperEnv+"=grandchild")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
	case "grandchild":
		if err := os.WriteFile(os.Getenv(treePIDFileEnv), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
	}
}

// waitForGrandchild returns the process ID the grandchild wrote, once it did.
func waitForGrandchild(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the grandchild never wrote its process ID")
	return 0
}

// requireGrandchildGone fails the test unless the process is gone soon, and
// makes sure a survivor does not outlive the test.
func requireGrandchildGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Errorf("the grandchild %d is still running after the command was canceled: only its parent was ended", pid)
		if p, err := os.FindProcess(pid); err == nil {
			if err := p.Kill(); err != nil {
				t.Logf("kill the leftover grandchild: %v", err)
			}
		}
	}
}
