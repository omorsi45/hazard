package change

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// scrubbedGitEnv lists variables that redirect git to another repository,
// work tree or index. A surrounding git hook exports them; Extract must read
// the repository named by its repoDir argument instead.
var scrubbedGitEnv = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_PREFIX":                       true,
}

// gitEnv returns the environment for a git child process, followed by extra.
// GIT_OPTIONAL_LOCKS=0 stops `git status` from rewriting the index as a side
// effect.
func gitEnv(extra []string) []string {
	environ := os.Environ()
	env := make([]string, 0, len(environ)+1+len(extra))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if scrubbedGitEnv[strings.ToUpper(name)] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_OPTIONAL_LOCKS=0")
	return append(env, extra...)
}

// runGit runs git with an explicit working directory and argv array and returns
// its stdout. A failure carries the full argv and the tail of stderr; a
// canceled context is reported as the context error.
func runGit(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	return runGitEnv(ctx, dir, nil, stdin, args...)
}

// runGitEnv is runGit with extra environment entries (KEY=value).
func runGitEnv(ctx context.Context, dir string, extraEnv []string, stdin []byte, args ...string) ([]byte, error) {
	argv := append([]string{"-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	cmd.Env = gitEnv(extraEnv)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("git %s: %w", strings.Join(argv, " "), ctxErr)
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(argv, " "), err, stderrTail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func stderrTail(s string) string {
	const max = 512
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = "..." + s[len(s)-max:]
	}
	return s
}
