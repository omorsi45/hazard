package events

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/engine"
)

const (
	baseB    = "package sub\n\nfunc B() int { return 1 }\n"
	mutantB  = "package sub\n\nfunc B() int { return 2 }\n"
	replayYM = "version: 1\nlanguages:\n  python:\n    enabled: false\n"
)

func replayFiles() map[string]string {
	return map[string]string{
		".gitignore": ".hazard/\n",
		"hazard.yml": replayYM,
		"go.mod":     "module example.com/m\n\ngo 1.21\n",
		"sub/b.go":   baseB,
		"a.txt":      "a\n",
		"run.sh":     "#!/bin/sh\n",
	}
}

// newReplayRepo is a repository whose head commit marks run.sh executable.
func newReplayRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir, _ = newRepo(t, replayFiles())
	gitIn(t, dir, "update-index", "--chmod=+x", "run.sh")
	return dir, commitAt(t, dir, "mark run.sh executable", baseTime.Add(time.Hour))
}

func openReplay(t *testing.T, dir, base string) *Replay {
	t.Helper()
	r, err := NewReplay(context.Background(), dir, base)
	if err != nil {
		t.Fatalf("NewReplay: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return r
}

// TestReplayCommitWritesOnlyTheScratchClone is the guard for the isolation of
// replay: the commit exists in the scratch clone, which stays clean and on its
// base, and not a single file of the studied repository changes.
func TestReplayCommitWritesOnlyTheScratchClone(t *testing.T) {
	dir, base := newReplayRepo(t)
	writeFiles(t, dir, map[string]string{".hazard/index/manifest.json": "{}\n", ".hazard/hazard.db": "db-bytes"})
	before := hashTree(t, dir)

	r, err := NewReplay(context.Background(), dir, base)
	if err != nil {
		t.Fatalf("NewReplay: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	sha, err := r.Commit(context.Background(), "sub/b.go", []byte(mutantB))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := gitIn(t, r.Dir(), "status", "--porcelain"); got != "" {
		t.Errorf("scratch clone is not clean after Commit:\n%s", got)
	}
	if got := gitIn(t, r.Dir(), "rev-parse", "HEAD"); got != base {
		t.Errorf("scratch HEAD = %s, want the base %s", got, base)
	}
	if got := gitIn(t, r.Dir(), "rev-parse", sha+"^"); got != base {
		t.Errorf("parent of the replay commit = %s, want the base %s", got, base)
	}
	if got := gitIn(t, r.Dir(), "diff", "--name-only", base, sha); got != "sub/b.go" {
		t.Errorf("the replay commit changes %q, want only sub/b.go", got)
	}
	if got := gitIn(t, r.Dir(), "show", sha+":sub/b.go"); got != strings.TrimSpace(mutantB) {
		t.Errorf("replay commit holds %q for sub/b.go, want the mutated content", got)
	}
	onDisk, err := os.ReadFile(filepath.Join(r.Dir(), "sub", "b.go"))
	if err != nil || string(onDisk) != baseB {
		t.Errorf("working tree sub/b.go = %q, %v, want the base content untouched", onDisk, err)
	}
	if _, err := gitTry(dir, "cat-file", "-e", sha+"^{commit}"); err == nil {
		t.Errorf("replay commit %s exists in the studied repository", sha)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if changed := diffTrees(before, hashTree(t, dir)); len(changed) != 0 {
		t.Errorf("the studied repository was written: %v", changed)
	}
}

func TestReplayCommitKeepsModeAndAddsNewFilesDeterministically(t *testing.T) {
	dir, base := newReplayRepo(t)
	r := openReplay(t, dir, base)
	ctx := context.Background()

	execSHA, err := r.Commit(ctx, "run.sh", []byte("#!/bin/sh\necho hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, r.Dir(), "ls-tree", execSHA, "run.sh"); !strings.HasPrefix(got, "100755 blob ") {
		t.Errorf("ls-tree of the changed executable = %q, want mode 100755", got)
	}
	newSHA, err := r.Commit(ctx, "docs/new.txt", []byte("new\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, r.Dir(), "ls-tree", newSHA, "docs/new.txt"); !strings.HasPrefix(got, "100644 blob ") {
		t.Errorf("ls-tree of the new file = %q, want mode 100644", got)
	}
	again, err := r.Commit(ctx, "run.sh", []byte("#!/bin/sh\necho hi\n"))
	if err != nil || again != execSHA {
		t.Errorf("same change committed again = %s, %v, want the same commit %s", again, err, execSHA)
	}
	other, err := r.Commit(ctx, "run.sh", []byte("#!/bin/sh\necho bye\n"))
	if err != nil || other == execSHA {
		t.Errorf("different content gave %s, %v, want a different commit than %s", other, err, execSHA)
	}
	if got := gitIn(t, r.Dir(), "status", "--porcelain"); got != "" {
		t.Errorf("scratch clone is not clean:\n%s", got)
	}
}

func TestReplayCommitRefusesPathsOutsideTheTree(t *testing.T) {
	dir, base := newReplayRepo(t)
	r := openReplay(t, dir, base)
	for _, rel := range []string{"", "../x.go", "a/../../x.go", "/abs.go", `sub\b.go`, "sub//b.go", "sub/", "."} {
		if sha, err := r.Commit(context.Background(), rel, []byte("x")); err == nil {
			t.Errorf("Commit(%q) = %s, want an error", rel, sha)
		}
	}
}

// TestReplayCommitIsNeverSigned: a user's signing configuration must not reach
// the replay commits. The argument list is what git reports it ran.
func TestReplayCommitIsNeverSigned(t *testing.T) {
	dir, base := newReplayRepo(t)
	r := openReplay(t, dir, base)
	trace := filepath.Join(t.TempDir(), "trace.txt")
	t.Setenv("GIT_TRACE", trace)
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "commit.gpgsign")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_KEY_1", "gpg.program")
	t.Setenv("GIT_CONFIG_VALUE_1", "no-such-signing-program")

	sha, err := r.Commit(context.Background(), "sub/b.go", []byte(mutantB))
	if err != nil {
		t.Fatalf("Commit with signing configured: %v", err)
	}
	if got := gitIn(t, r.Dir(), "cat-file", "-p", sha); strings.Contains(got, "gpgsig") {
		t.Errorf("replay commit is signed:\n%s", got)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	var commitTree string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "commit-tree") {
			commitTree = line
		}
	}
	if commitTree == "" || !strings.Contains(commitTree, "--no-gpg-sign") {
		t.Errorf("git commit-tree was run as %q, want --no-gpg-sign among its arguments", commitTree)
	}
}

func TestReplayCopiesIndexConfigAndDatabaseWhenPresent(t *testing.T) {
	dir, base := newReplayRepo(t)
	writeFiles(t, dir, map[string]string{
		".hazard/index/manifest.json": `{"copied":"manifest"}`,
		".hazard/hazard.db":           "db-bytes",
		"hazard.yml":                  replayYM + "# edited after the base commit\n",
		".hazard/hazard.yml":          "version: 1\n",
	})
	r := openReplay(t, dir, base)
	for rel, want := range map[string]string{
		".hazard/index/manifest.json": `{"copied":"manifest"}`,
		".hazard/hazard.db":           "db-bytes",
		"hazard.yml":                  replayYM + "# edited after the base commit\n",
		".hazard/hazard.yml":          "version: 1\n",
	} {
		got, err := os.ReadFile(filepath.Join(r.Dir(), filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("scratch %s = %q, %v, want %q", rel, got, err, want)
		}
	}
	if got := gitIn(t, r.Dir(), "rev-parse", "HEAD"); got != base {
		t.Errorf("scratch HEAD = %s, want %s", got, base)
	}
}

func TestReplayWithoutIndexConfigOrDatabaseStillOpens(t *testing.T) {
	files := replayFiles()
	delete(files, "hazard.yml")
	dir, _ := newRepo(t, files)
	base := gitIn(t, dir, "rev-parse", "HEAD")
	r := openReplay(t, dir, base)
	for _, rel := range []string{".hazard/index/manifest.json", ".hazard/hazard.db", ".hazard/hazard.yml", "hazard.yml"} {
		if _, err := os.Stat(filepath.Join(r.Dir(), filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("scratch has %s although the studied repository does not (%v)", rel, err)
		}
	}
	if got := gitIn(t, r.Dir(), "status", "--porcelain"); got != "" {
		t.Errorf("scratch clone is not clean:\n%s", got)
	}
}

func TestReplayCloseRemovesTheScratchCloneAndIsSafeTwice(t *testing.T) {
	dir, base := newReplayRepo(t)
	r, err := NewReplay(context.Background(), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(context.Background(), "sub/b.go", []byte(mutantB)); err != nil { // loose objects are read-only files
		t.Fatal(err)
	}
	scratch := r.Dir()
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("scratch clone missing before Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch clone still there after Close (%v)", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestNewReplayFailureLeavesNothingBehind(t *testing.T) {
	dir, _ := newReplayRepo(t)
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	if r, err := NewReplay(context.Background(), dir, strings.Repeat("0", 40)); err == nil {
		_ = r.Close()
		t.Fatal("NewReplay accepted a base that is not in the repository")
	}
	if r, err := NewReplay(context.Background(), filepath.Join(t.TempDir(), "missing"), "HEAD"); err == nil {
		_ = r.Close()
		t.Fatal("NewReplay accepted a directory that is not a repository")
	}
	left, err := os.ReadDir(tmp)
	if err != nil || len(left) != 0 {
		t.Errorf("failed NewReplay left %v behind (%v)", left, err)
	}
}

// TestReplaySelectMatchesChangeExtract: the decision of a replayed change
// carries the very change set Extract reports for the same refs, whatever
// repository, base or head the caller's options name.
func TestReplaySelectMatchesChangeExtract(t *testing.T) {
	dir, base := newReplayRepo(t)
	r := openReplay(t, dir, base)
	ctx := context.Background()
	head, err := r.Commit(ctx, "sub/b.go", []byte(mutantB))
	if err != nil {
		t.Fatal(err)
	}

	d, err := r.Select(ctx, head, engine.Options{RepoDir: filepath.Join(dir, "elsewhere"), Base: "no-such-ref", Head: "no-such-ref"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	cfg, err := config.Load(filepath.Join(r.Dir(), "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := change.Extract(ctx, r.Dir(), change.Options{
		Base: base, Head: head, DefaultBranch: cfg.Repo.DefaultBranch,
		FullSuiteTriggers: cfg.Repo.FullSuiteTriggers, GeneratedGlobs: cfg.Repo.GeneratedGlobs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Change == nil {
		t.Fatalf("Decision has no change set (abstain reason %q)", d.AbstainReason)
	}
	if !reflect.DeepEqual(d.Change, want) {
		t.Errorf("Select change set = %+v\nExtract change set = %+v", d.Change, want)
	}
	if d.Change.BaseSHA != base || d.Change.HeadSHA != head {
		t.Errorf("change set spans %s..%s, want %s..%s", d.Change.BaseSHA, d.Change.HeadSHA, base, head)
	}
	if len(d.Change.Files) != 1 || d.Change.Files[0].Path != "sub/b.go" || d.Change.Files[0].Status != "M" {
		t.Fatalf("change files = %+v, want exactly sub/b.go modified", d.Change.Files)
	}
	if h := d.Change.Files[0].Hunks; len(h) != 1 || h[0].OldLines != 1 || h[0].NewLines != 1 {
		t.Errorf("hunks = %+v, want one hunk replacing one line", h)
	}
}
