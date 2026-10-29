package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/omorsi45/hazard/internal/change"
)

// scrubbedGitEnv lists variables that redirect git to another repository,
// work tree or index. A surrounding git hook exports them; the engine must
// read the repository named by Options.RepoDir.
var scrubbedGitEnv = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_PREFIX":                       true,
}

func gitEnv() []string {
	environ := os.Environ()
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if scrubbedGitEnv[strings.ToUpper(name)] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0")
}

// gitResult is the outcome of one git process that ran to completion.
type gitResult struct {
	stdout, stderr []byte
	exit           int
}

// runGit runs git in dir with an argv array. A nonzero exit status is
// reported in the result with a nil error; an error means git could not run
// or the context ended.
func runGit(ctx context.Context, dir string, stdin []byte, args ...string) (gitResult, error) {
	argv := append([]string{"-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := gitResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.exit = exit.ExitCode()
	default:
		return res, fmt.Errorf("git %s: %w", strings.Join(argv, " "), err)
	}
	return res, nil
}

// checkRefs verifies that every explicit ref resolves to a commit.
func checkRefs(ctx context.Context, opts Options) error {
	for _, r := range []struct{ name, ref string }{{"base", opts.Base}, {"head", opts.Head}} {
		if r.ref == "" {
			continue
		}
		if strings.HasPrefix(r.ref, "-") {
			return fmt.Errorf("%w: %s %q must not start with '-'", ErrInvalidRef, r.name, r.ref)
		}
		res, err := runGit(ctx, opts.RepoDir, nil, "rev-parse", "--verify", "--quiet", r.ref+"^{commit}")
		if err != nil {
			return err
		}
		switch res.exit {
		case 0:
		case 1:
			return fmt.Errorf("%w: %s %q does not resolve to a commit", ErrInvalidRef, r.name, r.ref)
		default:
			return fmt.Errorf("git rev-parse %q failed with exit status %d: %s", r.ref, res.exit, strings.TrimSpace(string(res.stderr)))
		}
	}
	return nil
}

// isOID reports whether s is a full hexadecimal object name.
func isOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// coverageAge returns the number of commits from covHead to base, or -1 when
// covHead is not an ancestor of base, is unknown, or either name is not an
// object name. -1 is the staleness signal: it never narrows a selection.
func coverageAge(ctx context.Context, root, covHead, base string) (int, error) {
	if !isOID(covHead) || !isOID(base) {
		return -1, nil
	}
	res, err := runGit(ctx, root, nil, "merge-base", "--is-ancestor", covHead, base)
	if err != nil {
		return -1, err
	}
	if res.exit != 0 {
		return -1, nil
	}
	res, err = runGit(ctx, root, nil, "rev-list", "--count", covHead+".."+base, "--")
	if err != nil {
		return -1, err
	}
	if res.exit != 0 {
		return -1, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(res.stdout)))
	if err != nil || n < 0 {
		return -1, nil
	}
	return n, nil
}

// baseDigests returns the sha256 of the base content of every changed file
// that exists at base, keyed by its base path (the old path of a rename).
// A file with no digest (added, missing at base, not a blob, or a path git
// cannot be asked about) is simply absent, which makes coverage unknown for
// it and widens the selection. A single git process serves all files.
func baseDigests(ctx context.Context, root, base string, files []change.ChangedFile) (map[string]string, error) {
	digests := map[string]string{}
	var paths []string
	var req bytes.Buffer
	for _, f := range files {
		if f.Status == "A" {
			continue
		}
		p := f.Path
		if f.OldPath != "" {
			p = f.OldPath
		}
		if strings.ContainsAny(p, "\r\n") {
			continue
		}
		paths = append(paths, p)
		req.WriteString(base + ":" + p + "\n")
	}
	if len(paths) == 0 {
		return digests, nil
	}
	res, err := runGit(ctx, root, req.Bytes(), "cat-file", "--batch")
	if err != nil {
		return digests, err
	}
	if res.exit != 0 {
		return digests, fmt.Errorf("git cat-file --batch failed with exit status %d: %s", res.exit, strings.TrimSpace(string(res.stderr)))
	}
	out := bufio.NewReader(bytes.NewReader(res.stdout))
	for _, p := range paths {
		header, err := out.ReadString('\n')
		if err != nil {
			return digests, fmt.Errorf("git cat-file --batch: short output at %q: %w", p, err)
		}
		line := strings.TrimRight(header, "\r\n")
		if strings.HasSuffix(line, " missing") || strings.HasSuffix(line, " ambiguous") {
			continue // the echoed request line carries no content
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return digests, fmt.Errorf("git cat-file --batch: unexpected header %q", line)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return digests, fmt.Errorf("git cat-file --batch: bad header %q", line)
		}
		if fields[1] != "blob" {
			// A tree or submodule commit has no content digest.
			if _, err := out.Discard(int(size) + 1); err != nil {
				return digests, fmt.Errorf("git cat-file --batch: %q: %w", p, err)
			}
			continue
		}
		h := sha256.New()
		if _, err := io.CopyN(h, out, size); err != nil {
			return digests, fmt.Errorf("git cat-file --batch: %q: %w", p, err)
		}
		if _, err := out.Discard(1); err != nil {
			return digests, fmt.Errorf("git cat-file --batch: %q: %w", p, err)
		}
		digests[p] = hex.EncodeToString(h.Sum(nil))
	}
	return digests, nil
}
