package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitIn runs git in dir for fixture setup; signing is disabled for these
// temporary repositories only.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	// Ignore the user's git configuration (hooks, signing): a fixture
	// repository behaves the same on every machine.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// commitFile writes content to rel and commits it; it returns the new SHA.
func commitFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "change "+rel)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// linearRepo makes a repository with n commits, each rewriting marker.txt to
// "v<i>", and returns it with the commit SHAs oldest first.
func linearRepo(t *testing.T, n int) (string, []string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	var shas []string
	for i := 1; i <= n; i++ {
		shas = append(shas, commitFile(t, dir, "marker.txt", fmt.Sprintf("v%d", i)))
	}
	return dir, shas
}

func TestGitResolve(t *testing.T) {
	dir, shas := linearRepo(t, 2)
	g := Git{Dir: dir}
	for ref, want := range map[string]string{"HEAD": shas[1], shas[0][:10]: shas[0], "main": shas[1], "HEAD~1": shas[0]} {
		got, err := g.Resolve(context.Background(), ref)
		if err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := g.Resolve(context.Background(), "no-such-ref"); err == nil {
		t.Error("an unknown ref must be an error")
	}
	if _, err := g.Resolve(context.Background(), "--output=x"); err == nil {
		t.Error("an option-shaped ref must be refused, not passed to git")
	}
}

func TestGitIsAncestor(t *testing.T) {
	dir, shas := linearRepo(t, 3)
	g := Git{Dir: dir}
	for _, tc := range []struct {
		a, b string
		want bool
	}{{shas[0], shas[2], true}, {shas[2], shas[0], false}, {shas[1], shas[1], true}} {
		got, err := g.IsAncestor(context.Background(), tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Errorf("IsAncestor(%.7s, %.7s) = %v, %v; want %v", tc.a, tc.b, got, err, tc.want)
		}
	}
	if _, err := g.IsAncestor(context.Background(), "0123456789012345678901234567890123456789", shas[2]); err == nil {
		t.Error("a commit that does not exist must be an error, not 'not an ancestor'")
	}
}

func TestGitRange(t *testing.T) {
	dir, shas := linearRepo(t, 5)
	g := Git{Dir: dir}
	commits, parent, err := g.Range(context.Background(), shas[1], shas[3])
	if err != nil || !slices.Equal(commits, shas[1:4]) || parent != shas[0] {
		t.Errorf("Range = %v, %q, %v; want %v with parent %q", commits, parent, err, shas[1:4], shas[0])
	}
	commits, parent, err = g.Range(context.Background(), shas[0], shas[4])
	if err != nil || !slices.Equal(commits, shas) || parent != "" {
		t.Errorf("a range from the root = %v, %q, %v; want all commits and no parent", commits, parent, err)
	}
	commits, parent, err = g.Range(context.Background(), shas[2], shas[2])
	if err != nil || !slices.Equal(commits, shas[2:3]) || parent != shas[1] {
		t.Errorf("a one-commit range = %v, %q, %v", commits, parent, err)
	}
	if _, _, err := g.Range(context.Background(), shas[3], shas[1]); err == nil {
		t.Error("newest before oldest is not a range")
	}
}

func TestGitRangeFollowsTheAncestryPathThroughAMerge(t *testing.T) {
	dir, shas := linearRepo(t, 1)
	gitIn(t, dir, "checkout", "-q", "-b", "side")
	side := commitFile(t, dir, "side.txt", "side")
	gitIn(t, dir, "checkout", "-q", "main")
	next := commitFile(t, dir, "main.txt", "main")
	gitIn(t, dir, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	merge := gitIn(t, dir, "rev-parse", "HEAD")
	g := Git{Dir: dir}

	commits, parent, err := g.Range(context.Background(), shas[0], merge)
	if err != nil || parent != "" || commits[0] != shas[0] || commits[len(commits)-1] != merge || len(commits) != 4 {
		t.Fatalf("Range = %v, %q, %v; want the root, the side and main commits, then the merge", commits, parent, err)
	}
	pos := func(sha string) int { return slices.Index(commits, sha) }
	if pos(side) < 0 || pos(next) < 0 || pos(side) > pos(merge) || pos(next) > pos(merge) {
		t.Errorf("commits %v must list parents before the merge", commits)
	}

	// From the main-line commit only its own descendants remain: the side
	// commit is not on an ancestry path from it.
	commits, _, err = g.Range(context.Background(), next, merge)
	if err != nil || !slices.Equal(commits, []string{next, merge}) {
		t.Errorf("Range from the main-line commit = %v, %v; want [%s %s]", commits, err, next, merge)
	}
}
