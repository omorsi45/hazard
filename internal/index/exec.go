package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runResult is the outcome of one external command.
type runResult struct {
	Stdout, Stderr []byte
	Command        Command
}

// runCommand runs argv in dir with extra environment entries. A nonzero exit is
// reported in Command.Exit with a nil error; an error means the command could
// not run to completion (not found, or the context ended).
func runCommand(ctx context.Context, dir string, extraEnv []string, argv ...string) (runResult, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	res := runResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Command: Command{Argv: append([]string(nil), argv...), Dir: dir, Seconds: time.Since(start).Seconds()}}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.Command.Exit = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("run %s: %w", strings.Join(argv, " "), err)
	}
	return res, nil
}
