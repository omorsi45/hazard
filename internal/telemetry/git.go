package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Git runs git in the repository at Dir.
type Git struct{ Dir string }

// scrubbedGitEnv lists variables that redirect git to another repository,
// work tree or index. A surrounding git hook exports them, and they must not
// send these commands to a different repository than Dir.
var scrubbedGitEnv = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_COMMON_DIR": true,
	"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_PREFIX": true,
}

func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !scrubbedGitEnv[strings.ToUpper(name)] {
			env = append(env, kv)
		}
	}
	return env
}

// run runs git and returns its stdout and exit status. A nonzero status is not
// an error; the error is for a git that could not run or a canceled context.
func (g Git) run(ctx context.Context, args ...string) (stdout []byte, exit int, err error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = g.Dir
	cmd.Env = gitEnv()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, ctxErr
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return out.Bytes(), 0, nil
	case errors.As(runErr, &exitErr):
		return out.Bytes(), exitErr.ExitCode(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), runErr, strings.TrimSpace(errOut.String()))
	}
	return nil, 0, fmt.Errorf("run git: %w", runErr)
}

// Resolve returns the full SHA of the commit ref names.
func (g Git) Resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	out, _, err := g.run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%q does not name a commit: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAncestor reports whether a is an ancestor of b, or b itself. A commit that
// does not exist is an error, never "not an ancestor".
func (g Git) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if _, err := g.Resolve(ctx, a); err != nil {
		return false, err
	}
	if _, err := g.Resolve(ctx, b); err != nil {
		return false, err
	}
	_, exit, err := g.run(ctx, "merge-base", "--is-ancestor", a, b)
	switch {
	case err == nil:
		return true, nil
	case exit == 1:
		return false, nil
	}
	return false, err
}

// Range returns oldest followed by every commit of oldest..newest along the
// ancestry path, parents first, and the parent of oldest ("" for a root).
func (g Git) Range(ctx context.Context, oldest, newest string) (commits []string, parent string, err error) {
	if ok, err := g.IsAncestor(ctx, oldest, newest); err != nil {
		return nil, "", err
	} else if !ok {
		return nil, "", fmt.Errorf("%s is not an ancestor of %s", oldest, newest)
	}
	out, _, err := g.run(ctx, "rev-list", "--reverse", "--topo-order", "--ancestry-path", oldest+".."+newest)
	if err != nil {
		return nil, "", err
	}
	commits = append([]string{oldest}, strings.Fields(string(out))...)
	// A root commit has no parent: rev-parse then prints nothing and exits 1.
	out, exit, err := g.run(ctx, "rev-parse", "--verify", "--quiet", oldest+"^")
	switch {
	case err == nil:
		parent = strings.TrimSpace(string(out))
	case exit != 1:
		return nil, "", err
	}
	return commits, parent, nil
}
