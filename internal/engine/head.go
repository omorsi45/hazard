package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/omorsi45/hazard/internal/index"
)

// treeHead is the selection head as the index sees it (the working tree or a
// commit) plus the release of whatever process serves it. Close must be called
// once the last Read is done.
type treeHead interface {
	index.HeadReader
	Close() error
}

// newTreeHead returns the reader of the work tree under root, or of the commit
// headSHA when the head is an explicit revision.
func newTreeHead(ctx context.Context, root string, workTree bool, headSHA string) treeHead {
	if workTree {
		return workTreeReader{ctx: ctx, root: root}
	}
	return &commitReader{ctx: ctx, root: root, sha: headSHA}
}

// errOutsideRepo reports a path that is not a plain repo-relative slash path.
// It never wraps fs.ErrNotExist: a refused path is not an absent file.
func errOutsideRepo(rel string) error {
	return fmt.Errorf("path %q is not a path inside the repository", rel)
}

// workTreeReader reads the working tree of a git repository.
type workTreeReader struct {
	ctx  context.Context
	root string
}

// List returns the tracked and untracked non-ignored files that exist on disk:
// a tracked file deleted from disk and a directory (a nested repository) are
// not listed.
func (w workTreeReader) List() ([]string, error) {
	res, err := runGit(w.ctx, w.root, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	if res.exit != 0 {
		return nil, fmt.Errorf("git ls-files failed with exit status %d: %s", res.exit, strings.TrimSpace(string(res.stderr)))
	}
	var paths []string
	seen := map[string]bool{}
	for _, rel := range strings.Split(string(res.stdout), "\x00") {
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		info, err := os.Stat(filepath.Join(w.root, filepath.FromSlash(rel)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, err
		case !info.IsDir():
			paths = append(paths, rel)
		}
	}
	return paths, nil
}

// Read returns the working tree content of rel. An absent file is an error
// wrapping fs.ErrNotExist.
func (w workTreeReader) Read(rel string) ([]byte, error) {
	native := filepath.FromSlash(rel)
	if !fs.ValidPath(rel) || !filepath.IsLocal(native) || filepath.Separator == '\\' && strings.Contains(rel, `\`) {
		return nil, errOutsideRepo(rel)
	}
	return os.ReadFile(filepath.Join(w.root, native))
}

// Close does nothing: the working tree needs no process.
func (workTreeReader) Close() error { return nil }

// commitReader reads one commit. List is one `git ls-tree`; Read talks to a
// single `git cat-file --batch` process that stays open until Close, so
// reading every recorded file costs one process, not one per file.
type commitReader struct {
	ctx       context.Context
	root, sha string

	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr bytes.Buffer
	failed error // a protocol or process failure: every later Read repeats it
	closed bool
}

// List returns every file of the commit.
func (c *commitReader) List() ([]string, error) {
	res, err := runGit(c.ctx, c.root, nil, "ls-tree", "-r", "-z", "--name-only", "--full-tree", c.sha)
	if err != nil {
		return nil, err
	}
	if res.exit != 0 {
		return nil, fmt.Errorf("git ls-tree %s failed with exit status %d: %s", c.sha, res.exit, strings.TrimSpace(string(res.stderr)))
	}
	var paths []string
	for _, rel := range strings.Split(string(res.stdout), "\x00") {
		if rel != "" {
			paths = append(paths, rel)
		}
	}
	return paths, nil
}

// Read returns the content of rel in the commit. A path that is absent, or is
// not a file there (a directory or a submodule), is an error wrapping
// fs.ErrNotExist.
func (c *commitReader) Read(rel string) ([]byte, error) {
	if c.closed {
		return nil, errors.New("engine: read from a closed head reader")
	}
	if c.failed != nil {
		return nil, c.failed
	}
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	if !fs.ValidPath(rel) || strings.ContainsAny(rel, "\r\n") {
		return nil, errOutsideRepo(rel)
	}
	if c.cmd == nil {
		if err := c.start(); err != nil {
			return nil, c.fail(err)
		}
	}
	if _, err := io.WriteString(c.in, c.sha+":"+rel+"\n"); err != nil {
		return nil, c.fail(fmt.Errorf("git cat-file --batch: write request for %q: %w", rel, err))
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return nil, c.fail(fmt.Errorf("git cat-file --batch: read header for %q: %w", rel, err))
	}
	line := strings.TrimRight(header, "\r\n")
	if strings.HasSuffix(line, " missing") {
		return nil, fmt.Errorf("%s at %s: %w", rel, c.sha, fs.ErrNotExist)
	}
	if strings.HasSuffix(line, " ambiguous") {
		return nil, fmt.Errorf("git cat-file --batch: %q is ambiguous", rel)
	}
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return nil, c.fail(fmt.Errorf("git cat-file --batch: unexpected header %q for %q", line, rel))
	}
	size, err := strconv.ParseInt(fields[2], 10, 32)
	if err != nil || size < 0 {
		return nil, c.fail(fmt.Errorf("git cat-file --batch: bad size in header %q for %q", line, rel))
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(c.out, data); err != nil {
		return nil, c.fail(fmt.Errorf("git cat-file --batch: read %q: %w", rel, err))
	}
	if _, err := c.out.Discard(1); err != nil { // the newline after the object
		return nil, c.fail(fmt.Errorf("git cat-file --batch: read %q: %w", rel, err))
	}
	if fields[1] != "blob" {
		return nil, fmt.Errorf("%s at %s is a %s, not a file: %w", rel, c.sha, fields[1], fs.ErrNotExist)
	}
	return data, nil
}

// start launches the batch process.
func (c *commitReader) start() error {
	cmd := exec.CommandContext(c.ctx, "git", "-c", "core.quotePath=false", "cat-file", "--batch")
	cmd.Dir = c.root
	cmd.Env = gitEnv()
	cmd.Stderr = &c.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git cat-file --batch: %w", err)
	}
	c.cmd, c.in, c.out = cmd, in, bufio.NewReaderSize(out, 64<<10)
	return nil
}

// fail records a failure that leaves the stream unusable, stops the process
// and returns the error.
func (c *commitReader) fail(err error) error {
	if ctxErr := c.ctx.Err(); ctxErr != nil {
		err = ctxErr
	} else if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	c.failed = err
	if c.cmd != nil && c.cmd.Process != nil {
		if kerr := c.cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			c.failed = errors.Join(err, kerr)
		}
	}
	return c.failed
}

// Close ends the batch process. It returns an error when git did not exit
// cleanly after the last request; a reader that already failed reports its
// failure on every Read instead and closes without a second error.
func (c *commitReader) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.cmd == nil {
		return nil
	}
	closeErr := c.in.Close()
	waitErr := c.cmd.Wait()
	switch {
	case c.failed != nil || c.ctx.Err() != nil:
		return nil
	case waitErr != nil:
		return fmt.Errorf("git cat-file --batch: %w: %s", waitErr, strings.TrimSpace(c.stderr.String()))
	}
	return closeErr
}
