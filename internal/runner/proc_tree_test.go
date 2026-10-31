package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCancelEndsTheWholeProcessTree: go test starts the test binary as a child.
// Canceling the context must end that child too, or a hung test keeps running
// (and keeps its pipes open) after hazard has given up on it.
func TestCancelEndsTheWholeProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	inv := Invocation{
		Argv: []string{os.Args[0], "-test.run=^TestTreeHelper$"},
		Dir:  t.TempDir(),
		Env:  []string{treeHelperEnv + "=parent", treePIDFileEnv + "=" + pidFile},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		code, err := runProcess(ctx, inv, io.Discard, io.Discard)
		done <- outcome{code, err}
	}()

	pid := waitForGrandchild(t, pidFile)
	if !processAlive(pid) {
		t.Fatalf("premise: the grandchild %d is not running before the cancel", pid)
	}
	cancel()

	select {
	case got := <-done:
		if got.err == nil || got.code == 0 {
			t.Errorf("runProcess = %d, %v after a cancel, want an error and a nonzero code", got.code, got.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runProcess did not return after the cancel")
	}
	requireGrandchildGone(t, pid)
}
