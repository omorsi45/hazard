package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain removes repository-redirecting variables a surrounding git hook may
// have exported and ignores the user and system git configuration, so the
// fixture repositories behave the same on every machine.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX",
	} {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// baseTime is the committer time of the fixture's first commit: exactly UTC
// midnight, so a cutoff on that date must exclude it.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// gitIn runs git in dir and returns its trimmed stdout.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitTry(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// commitAt stages everything and commits with author and committer time at.
// Signing is disabled for the fixture repository only.
func commitAt(t *testing.T, dir, msg string, at time.Time) string {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	stamp := at.UTC().Format("2006-01-02T15:04:05+0000")
	cmd := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// newRepo creates a repository on main whose first commit holds files, made
// at baseTime. It returns the directory and that commit.
func newRepo(t *testing.T, files map[string]string) (dir, sha string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, files)
	gitIn(t, dir, "init", "-q", "-b", "main")
	return dir, commitAt(t, dir, "init", baseTime)
}

// hashTree returns the SHA-256 of every file under dir (the .git directory
// included), keyed by slash path, so a test can prove nothing was written.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// diffTrees describes the differences between two hashTree results.
func diffTrees(before, after map[string]string) []string {
	var out []string
	for p, h := range after {
		switch old, ok := before[p]; {
		case !ok:
			out = append(out, "added "+p)
		case old != h:
			out = append(out, "changed "+p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, "removed "+p)
		}
	}
	return out
}
