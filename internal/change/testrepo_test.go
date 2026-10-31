package change

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Fixture repositories are built with a single `git fast-import` process and
// shared between read-only tests, because starting git is the dominant cost of
// this package's tests. Nothing here touches the hazard repository itself.

var worldRoot string // parent of the shared fixtures, removed after the run

// TestMain removes repository-redirecting variables a surrounding git hook may
// have exported and ignores the user and system git configuration, so fixtures
// behave the same on every machine.
func TestMain(m *testing.M) {
	code := runTests(m)
	if worldRoot != "" {
		if err := os.RemoveAll(worldRoot); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", err)
		}
	}
	os.Exit(code)
}

func runTests(m *testing.M) int {
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX",
	} {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	root, err := os.MkdirTemp("", "change-fixtures-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	worldRoot = root
	return m.Run()
}

// gitRun runs git in dir and returns trimmed stdout.
func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// op is one file change inside a fast-import commit.
type op struct {
	kind     byte // 'M', 'D' or 'R'
	path, to string
	mode     string
	data     []byte
}

func put(path, content string) op {
	return op{kind: 'M', path: path, mode: "100644", data: []byte(content)}
}

func putBytes(path string, b []byte) op {
	return op{kind: 'M', path: path, mode: "100644", data: b}
}

func putMode(path, mode, content string) op {
	return op{kind: 'M', path: path, mode: mode, data: []byte(content)}
}

func del(path string) op        { return op{kind: 'D', path: path} }
func rename(from, to string) op { return op{kind: 'R', path: from, to: to} }

func heads(branch string) string  { return "refs/heads/" + branch }
func remote(branch string) string { return "refs/remotes/origin/" + branch }

// dataBlock is the fast-import "data" command for b.
func dataBlock(b []byte) []byte {
	out := []byte(fmt.Sprintf("data %d\n", len(b)))
	out = append(out, b...)
	return append(out, '\n')
}

// history accumulates a fast-import stream.
type history struct {
	stream bytes.Buffer
	marks  int
}

// commit appends a commit to ref and returns its mark. from is the mark to
// start ref from; 0 continues ref (or starts a root commit).
func (h *history) commit(ref string, from int, msg string, ops ...op) int {
	h.marks++
	fmt.Fprintf(&h.stream, "commit %s\nmark :%d\ncommitter t <t@example.com> %d +0000\n", ref, h.marks, 1700000000+h.marks*60)
	h.stream.Write(dataBlock([]byte(msg)))
	if from != 0 {
		fmt.Fprintf(&h.stream, "from :%d\n", from)
	}
	for _, o := range ops {
		switch o.kind {
		case 'M':
			fmt.Fprintf(&h.stream, "M %s inline %s\n", o.mode, strconv.Quote(o.path))
			h.stream.Write(dataBlock(o.data))
		case 'D':
			fmt.Fprintf(&h.stream, "D %s\n", strconv.Quote(o.path))
		case 'R':
			fmt.Fprintf(&h.stream, "R %s %s\n", strconv.Quote(o.path), strconv.Quote(o.to))
		}
	}
	h.stream.WriteString("\n")
	return h.marks
}

// point makes ref point at the commit with the given mark.
func (h *history) point(ref string, mark int) {
	fmt.Fprintf(&h.stream, "reset %s\nfrom :%d\n\n", ref, mark)
}

// repo is a materialised fixture: a directory and the SHA of each mark.
type repo struct {
	dir  string
	shas map[int]string
}

func (r *repo) sha(mark int) string { return r.shas[mark] }

func (r *repo) path(rel string) string { return filepath.Join(r.dir, filepath.FromSlash(rel)) }

// build creates a repository in dir whose HEAD is the branch headBranch and
// imports the history with one git process. With checkout the working tree and
// index are populated from HEAD.
func (h *history) build(dir, headBranch string, checkout bool) (*repo, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := gitRun(dir, "init", "-q", "-b", headBranch); err != nil {
		return nil, err
	}
	marksFile := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".marks")
	cmd := exec.Command("git", "fast-import", "--quiet", "--export-marks="+filepath.ToSlash(marksFile))
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(h.stream.Bytes())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git fast-import: %w: %s", err, stderr.String())
	}
	data, err := os.ReadFile(marksFile)
	if err != nil {
		return nil, err
	}
	r := &repo{dir: dir, shas: map[int]string{}}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var n int
		var sha string
		if _, err := fmt.Sscanf(line, ":%d %s", &n, &sha); err != nil {
			return nil, fmt.Errorf("marks line %q: %w", line, err)
		}
		r.shas[n] = sha
	}
	if checkout {
		if _, err := gitRun(dir, "reset", "-q", "--hard"); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// newRepo builds a private fixture under t.TempDir.
func newRepo(t *testing.T, h *history, headBranch string, checkout bool) *repo {
	t.Helper()
	r, err := h.build(filepath.Join(t.TempDir(), "repo"), headBranch, checkout)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// shared lazily builds a read-only fixture once for the whole test binary.
type shared struct {
	once sync.Once
	r    *repo
	err  error
}

func (s *shared) get(t *testing.T, name string, build func() (*history, string)) *repo {
	t.Helper()
	s.once.Do(func() {
		h, head := build()
		s.r, s.err = h.build(filepath.Join(worldRoot, name), head, false)
	})
	if s.err != nil {
		t.Fatalf("shared fixture %s: %v", name, s.err)
	}
	return s.r
}

func (r *repo) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gitRun(r.dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *repo) write(t *testing.T, rel, content string) {
	t.Helper()
	r.writeBytes(t, rel, []byte(content))
}

func (r *repo) writeBytes(t *testing.T, rel string, b []byte) {
	t.Helper()
	p := r.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *repo) remove(t *testing.T, rel string) {
	t.Helper()
	if err := os.Remove(r.path(rel)); err != nil {
		t.Fatal(err)
	}
}

func (r *repo) mv(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.path(to)), 0o755); err != nil {
		t.Fatal(err)
	}
	r.git(t, "mv", from, to)
}

// replaceLine returns src with 1-based line n replaced by text ("" deletes it).
// The line helpers panic on a bad line number: that is a fixture bug.
func replaceLine(src string, n int, text string) string {
	lines := strings.SplitAfter(src, "\n")
	if n < 1 || n > len(lines) {
		panic(fmt.Sprintf("line %d out of range", n))
	}
	lines[n-1] = text
	return strings.Join(lines, "")
}

// insertAfter returns src with text inserted after 1-based line n (0 = top).
func insertAfter(src string, n int, text string) string {
	lines := strings.SplitAfter(src, "\n")
	if n < 0 || n > len(lines) {
		panic(fmt.Sprintf("line %d out of range", n))
	}
	return strings.Join(lines[:n], "") + text + strings.Join(lines[n:], "")
}

// dropLines returns src without the 1-based inclusive line range.
func dropLines(src string, from, to int) string {
	lines := strings.SplitAfter(src, "\n")
	if from < 1 || to < from || to > len(lines) {
		panic(fmt.Sprintf("line range %d-%d out of range", from, to))
	}
	return strings.Join(lines[:from-1], "") + strings.Join(lines[to:], "")
}
