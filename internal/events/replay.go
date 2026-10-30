package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omorsi45/hazard/internal/engine"
)

// replayCommitDate is the fixed author and committer date of a replay commit,
// so the same change on the same base is the same commit on every run.
const replayCommitDate = "2000-01-01T00:00:00+0000"

// Replay is a scratch clone of a repository at one base commit. Changes are
// committed into the clone without touching its working tree or index, and the
// repository it was cloned from is only ever read.
type Replay struct {
	repoDir string // the studied repository: read only
	root    string // temporary directory that holds the clone and the private index files
	dir     string // the scratch clone, inside root
	base    string // full SHA of the base commit
	seq     int
	closed  bool
}

// NewReplay clones repoDir into a temporary directory (`git clone --shared`,
// so no objects are copied) and checks out baseSHA there. When present, the
// index manifest, the configuration (hazard.yml and .hazard/hazard.yml) and
// the history database of repoDir are copied into the clone, because the
// engine reads them from the repository it is given. The caller must Close
// the Replay.
func NewReplay(ctx context.Context, repoDir, baseSHA string) (r *Replay, err error) {
	if baseSHA == "" || strings.HasPrefix(baseSHA, "-") {
		return nil, fmt.Errorf("events: %q is not a usable base revision", baseSHA)
	}
	root, err := os.MkdirTemp("", "hazard-replay-*")
	if err != nil {
		return nil, fmt.Errorf("events: create the scratch directory: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeAll(root))
		}
	}()
	dir := filepath.Join(root, "clone")
	// No line-ending conversion: the index records the hash of the files of a
	// checkout and the engine compares it with the blob at the head.
	if _, err := runGit(ctx, root, nil, nil, "clone", "--shared", "--quiet", "--no-checkout", "-c", "core.autocrlf=false", "--", repoDir, dir); err != nil {
		return nil, fmt.Errorf("events: clone %s: %w", repoDir, err)
	}
	out, err := runGit(ctx, dir, nil, nil, "rev-parse", "--verify", "--quiet", baseSHA+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("events: base %q is not a commit of %s: %w", baseSHA, repoDir, err)
	}
	base := strings.TrimSpace(string(out))
	if _, err := runGit(ctx, dir, nil, nil, "checkout", "--quiet", "--detach", base); err != nil {
		return nil, fmt.Errorf("events: check out %s: %w", base, err)
	}
	// Whatever the engine needs from .hazard is copied in below; keep it out
	// of the clone's status.
	if err := appendLine(filepath.Join(dir, ".git", "info", "exclude"), ".hazard/"); err != nil {
		return nil, err
	}
	for _, rel := range []string{".hazard/index/manifest.json", "hazard.yml", ".hazard/hazard.yml", ".hazard/hazard.db"} {
		if err := copyIfPresent(filepath.Join(repoDir, filepath.FromSlash(rel)), filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return nil, err
		}
	}
	return &Replay{repoDir: repoDir, root: root, dir: dir, base: base}, nil
}

// Dir is the scratch clone.
func (r *Replay) Dir() string { return r.dir }

// Base is the full SHA the clone was checked out at.
func (r *Replay) Base() string { return r.base }

// Commit returns a commit on the base that differs from it only by replacing
// (or adding) the file rel with content. It is built with hash-object, a
// private index file, write-tree and commit-tree, never signed and with a
// fixed author, committer and date; the working tree and the index of the
// clone are not modified, and neither is the studied repository.
func (r *Replay) Commit(ctx context.Context, rel string, content []byte) (string, error) {
	if r.closed {
		return "", errors.New("events: commit on a closed replay")
	}
	if rel == "" || rel == "." || !fs.ValidPath(rel) || strings.ContainsAny(rel, "\\\x00") {
		return "", fmt.Errorf("events: %q is not a path inside the repository", rel)
	}
	blob, err := r.git(ctx, nil, content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	r.seq++
	index := filepath.Join(r.root, fmt.Sprintf("index-%d", r.seq))
	defer func() { _ = os.Remove(index) }()
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := r.git(ctx, env, nil, "read-tree", r.base); err != nil {
		return "", err
	}
	listing, err := r.git(ctx, env, nil, "ls-files", "-s", "-z", "--", rel)
	if err != nil {
		return "", err
	}
	mode := "100644"
	for _, entry := range strings.Split(listing, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); ok && path == rel && len(fields) == 3 {
			mode = fields[0]
		}
	}
	if _, err := r.git(ctx, env, nil, "update-index", "--add", "--cacheinfo", mode+","+blob+","+rel); err != nil {
		return "", err
	}
	tree, err := r.git(ctx, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	author := []string{
		"GIT_AUTHOR_NAME=hazard", "GIT_AUTHOR_EMAIL=hazard@localhost", "GIT_AUTHOR_DATE=" + replayCommitDate,
		"GIT_COMMITTER_NAME=hazard", "GIT_COMMITTER_EMAIL=hazard@localhost", "GIT_COMMITTER_DATE=" + replayCommitDate,
	}
	return r.git(ctx, author, nil, "commit-tree", tree, "-p", r.base, "--no-gpg-sign", "-m", "hazard replay")
}

// Select runs the engine on the change from the base to head in the clone.
// RepoDir, Base and Head of opts are replaced; everything else is kept.
func (r *Replay) Select(ctx context.Context, head string, opts engine.Options) (engine.Decision, error) {
	if r.closed {
		return engine.Decision{}, errors.New("events: select on a closed replay")
	}
	if head == "" {
		return engine.Decision{}, errors.New("events: select needs a head commit")
	}
	opts.RepoDir, opts.Base, opts.Head = r.dir, r.base, head
	return engine.Select(ctx, opts)
}

// Close removes the scratch clone. It is safe to call twice.
func (r *Replay) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return removeAll(r.root)
}

// git runs git in the clone.
func (r *Replay) git(ctx context.Context, env []string, stdin []byte, args ...string) (string, error) {
	out, err := runGit(ctx, r.dir, env, stdin, args...)
	return strings.TrimRight(string(out), "\r\n"), err
}

// scrubbedGitEnv lists variables that redirect git to another repository,
// work tree or index; a surrounding git hook exports them.
var scrubbedGitEnv = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_COMMON_DIR": true,
	"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_PREFIX": true,
}

// gitEnv is the environment of a git process: the process environment without
// the variables that redirect git, plus extra.
func gitEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !scrubbedGitEnv[strings.ToUpper(name)] {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_OPTIONAL_LOCKS=0")
	return append(env, extra...)
}

// runGit runs git with an argument array in dir and returns its stdout. A
// nonzero exit is an error carrying the end of stderr.
func runGit(ctx context.Context, dir string, extraEnv []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv(extraEnv)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// tail keeps the end of a command's stderr for error messages.
func tail(s string) string {
	const keep = 600
	s = strings.TrimSpace(s)
	if len(s) > keep {
		s = "..." + s[len(s)-keep:]
	}
	return s
}

// appendLine appends line and a newline to the file at path.
func appendLine(path, line string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("events: open %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("events: close %s: %w", path, cerr)
		}
	}()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("events: write %s: %w", path, err)
	}
	return nil
}

// copyIfPresent copies the regular file src to dst, creating its directory.
// A missing src is not an error; a src that is not a regular file is.
func copyIfPresent(src, dst string) error {
	info, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("events: stat %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("events: %s is not a regular file", src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("events: read %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("events: create %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("events: write %s: %w", dst, err)
	}
	return nil
}

// removeAll deletes path, retrying a few times: on Windows a scanner or an
// exiting git process can hold a file for a moment.
func removeAll(path string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return fmt.Errorf("events: remove %s: %w", path, err)
}
