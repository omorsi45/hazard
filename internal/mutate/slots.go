package mutate

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// safeEntryPath validates a tar entry name and returns it as a slash-separated
// relative path. Absolute names, drive letters, backslash separators that could
// escape, and ".." segments are rejected.
func safeEntryPath(name string) (string, error) {
	unsafe := func() (string, error) { return "", fmt.Errorf("mutate: unsafe path %q in the base archive", name) }
	if name == "" || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || path.IsAbs(name) || filepath.VolumeName(name) != "" {
		return unsafe()
	}
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return unsafe()
		}
	}
	if first, _, _ := strings.Cut(name, "/"); len(first) >= 2 && first[1] == ':' {
		return unsafe()
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return unsafe()
	}
	return clean, nil
}

// extractTar unpacks a `git archive` tar stream into dst. Regular files and
// directories are written; the pax global header and symlinks are skipped (a
// missing symlink can only make the baseline fail, never hide a test). Any
// other entry type, and any entry that could land outside dst, is an error.
func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("mutate: read the base archive: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeSymlink:
			continue
		case tar.TypeDir, tar.TypeReg:
		default:
			return fmt.Errorf("mutate: unsupported entry type %q for %q in the base archive", hdr.Typeflag, hdr.Name)
		}
		rel, err := safeEntryPath(hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if inside, relErr := filepath.Rel(dst, target); relErr != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return fmt.Errorf("mutate: unsafe path %q in the base archive", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mutate: create %s: %w", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mutate: create %s: %w", filepath.Dir(target), err)
		}
		mode := os.FileMode(0o644)
		if hdr.Mode&0o111 != 0 {
			mode = 0o755
		}
		if err := writeEntry(target, mode, tr); err != nil {
			return err
		}
	}
}

func writeEntry(target string, mode os.FileMode, r io.Reader) (err error) {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("mutate: create %s: %w", target, err)
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("mutate: close %s: %w", target, closeErr)
		}
	}()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("mutate: write %s: %w", target, err)
	}
	return nil
}

// scrubbedGitEnv lists variables that redirect git to another repository,
// work tree or index; a surrounding git hook exports them.
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
	return append(env, "GIT_OPTIONAL_LOCKS=0")
}

// stderrTail keeps the last part of a command's stderr for error messages.
func stderrTail(b []byte) string {
	const keep = 600
	s := strings.TrimSpace(string(b))
	if len(s) > keep {
		s = "..." + s[len(s)-keep:]
	}
	return s
}

// gitOutput runs git in dir and returns its stdout.
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderrTail(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// TopLevel returns the root of the git work tree that contains dir.
func TopLevel(ctx context.Context, dir string) (string, error) {
	out, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("mutate: %s is not inside a git work tree: %w", dir, err)
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", fmt.Errorf("mutate: git reports no work tree root for %s", dir)
	}
	return filepath.Clean(top), nil
}

// resolveBase returns the commit SHA that base names in repoDir.
func resolveBase(ctx context.Context, repoDir, base string) (string, error) {
	if base == "" || strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("mutate: invalid base revision %q", base)
	}
	out, err := gitOutput(ctx, repoDir, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("mutate: base %q is not a commit of %s: %w", base, repoDir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// listTracked returns the files tracked at the commit sha, in git's order.
func listTracked(ctx context.Context, repoDir, sha string) ([]string, error) {
	out, err := gitOutput(ctx, repoDir, "ls-tree", "-r", "--name-only", "-z", sha)
	if err != nil {
		return nil, fmt.Errorf("mutate: list the files at %s: %w", sha, err)
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// extractArchive writes the tree of commit sha into dst using git archive.
func extractArchive(ctx context.Context, repoDir, sha, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mutate: create %s: %w", dst, err)
	}
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", sha)
	cmd.Dir = repoDir
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mutate: git archive: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mutate: git archive: %w", err)
	}
	extractErr := extractTar(stdout, dst)
	if extractErr != nil {
		// Stop git so Wait cannot block on a full pipe.
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			extractErr = errors.Join(extractErr, killErr)
		}
	} else if _, drainErr := io.Copy(io.Discard, stdout); drainErr != nil {
		extractErr = fmt.Errorf("mutate: read the base archive: %w", drainErr)
	}
	waitErr := cmd.Wait()
	switch {
	case extractErr != nil:
		return extractErr
	case waitErr != nil:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("mutate: git archive %s: %w: %s", sha, waitErr, stderrTail(stderr.Bytes()))
	}
	return nil
}

// removeAllRetry removes dir, retrying briefly: on Windows a just-exited test
// binary can hold a file open for a moment.
func removeAllRetry(dir string) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("mutate: remove %s: %w", dir, err)
}
