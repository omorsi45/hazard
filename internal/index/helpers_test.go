package index

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
)

// TestMain removes the fixtures that sharedGoRepo builds once per test binary.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedGo.dir != "" {
		_ = os.RemoveAll(sharedGo.dir) // best-effort cleanup of a scratch directory
	}
	os.Exit(code)
}

// writeTree creates files (slash-separated relative path -> content) under dir.
func writeTree(dir string, files map[string]string) error {
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeFiles creates files (slash-separated relative path -> content) under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := writeTree(dir, files); err != nil {
		t.Fatal(err)
	}
}

// runGit runs git in dir and returns its trimmed combined output.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// initRepo writes files into dir and commits them in a new git repository.
func initRepo(dir string, files map[string]string) error {
	if err := writeTree(dir, files); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"},
	} {
		if _, err := runGit(dir, args...); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies the directory tree src (a git repository included) to dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// sharedGo is a committed git repository of goFixture() plus an ignored file,
// and the manifest Build produced for it with the default configuration. It is
// built once per test binary: tests read dir and m, and a test that changes the
// repository changes a copy from copyRepo. This avoids the git and go processes
// each test would otherwise start for an identical fixture.
var sharedGo struct {
	once sync.Once
	dir  string
	m    *Manifest
	err  error
}

// sharedGoRepo returns the shared Go fixture repository and its manifest. Do
// not modify either; use copyRepo to get a repository to change.
func sharedGoRepo(t *testing.T) (dir string, m *Manifest) {
	t.Helper()
	sharedGo.once.Do(func() {
		sharedGo.dir, sharedGo.err = os.MkdirTemp("", "hazard-index-shared-*")
		if sharedGo.err != nil {
			return
		}
		files := goFixture()
		files["ignored.go"] = "package ignored\n" // matched by the fixture .gitignore
		if sharedGo.err = initRepo(sharedGo.dir, files); sharedGo.err != nil {
			return
		}
		sharedGo.m, sharedGo.err = Build(context.Background(), sharedGo.dir, config.Defaults(), Options{})
	})
	if sharedGo.err != nil {
		t.Fatalf("build the shared Go fixture: %v", sharedGo.err)
	}
	return sharedGo.dir, sharedGo.m
}

// copyRepo returns a copy of the repository at src in a directory removed when
// the test ends.
func copyRepo(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

// pythonProbe is the cached outcome of one "import <modules>" check.
type pythonProbe struct {
	once sync.Once
	msg  string // empty when every module imports
}

var (
	pythonProbesMu sync.Mutex
	pythonProbes   = map[string]*pythonProbe{}
)

// testPython returns the interpreter for tests that need Python modules. It
// uses HAZARD_TEST_PYTHON, else python on PATH. When a module cannot be
// imported the test is skipped with an explicit message, unless
// HAZARD_REQUIRE_PYTEST is "1", in which case it fails. The import check runs
// once per interpreter and module list.
func testPython(t *testing.T, modules ...string) string {
	t.Helper()
	python := os.Getenv("HAZARD_TEST_PYTHON")
	if python == "" {
		python = "python"
	}
	key := python + "\x00" + strings.Join(modules, ",")
	pythonProbesMu.Lock()
	probe := pythonProbes[key]
	if probe == nil {
		probe = &pythonProbe{}
		pythonProbes[key] = probe
	}
	pythonProbesMu.Unlock()
	probe.once.Do(func() {
		out, err := exec.Command(python, "-c", "import "+strings.Join(modules, ", ")).CombinedOutput()
		if err != nil {
			probe.msg = "Python modules " + strings.Join(modules, ", ") + " are unavailable through " + python + ": " + strings.TrimSpace(string(out)) + " " + err.Error()
		}
	})
	if probe.msg != "" {
		if os.Getenv("HAZARD_REQUIRE_PYTEST") == "1" {
			t.Fatal(probe.msg)
		}
		t.Skip(probe.msg + " (set HAZARD_TEST_PYTHON to an interpreter with pytest and coverage, HAZARD_REQUIRE_PYTEST=1 to forbid skipping)")
	}
	return python
}
