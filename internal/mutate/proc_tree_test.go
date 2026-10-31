package mutate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCancelEndsTheWholeProcessTree: go test starts the test binary as a child.
// Canceling the context must end that child too, or a hung mutant keeps
// running after the driver has given up on it.
func TestCancelEndsTheWholeProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Setenv(treeHelperEnv, "parent")
	t.Setenv(treePIDFileEnv, pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runCmd(ctx, t.TempDir(), os.Args[0], "-test.run=^TestTreeHelper$")
		done <- err
	}()

	pid := waitForGrandchild(t, pidFile)
	if !processAlive(pid) {
		t.Fatalf("premise: the grandchild %d is not running before the cancel", pid)
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("runCmd returned no error after a cancel")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runCmd did not return after the cancel")
	}
	requireGrandchildGone(t, pid)
}
