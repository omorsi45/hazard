package hazard

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/index"
)

func TestFromEngineCopiesTheDecisionWithoutInternalTypes(t *testing.T) {
	p := 0.25
	d := engine.Decision{
		ID: "abc", FullSuite: false, Abstained: false, LockDigest: "sha256:lock",
		Tests: []engine.Selected{
			{TestID: "go:m/a:TestA", Source: "certain", Reason: "direct", Package: "m/a", RunnerID: "TestA"},
			{TestID: "py:t.x:test_b", Source: "added", Reason: "p>=lambda", Package: "t.x", RunnerID: "t/x.py::test_b", P: &p},
		},
		Change: &change.ChangeSet{HeadSHA: "head"},
	}
	got := fromEngine(d)
	want := Decision{
		ID: "abc", LockDigest: "sha256:lock",
		Tests: []SelectedTest{
			{ID: "go:m/a:TestA", Source: "certain", Reason: "direct", Package: "m/a", RunnerID: "TestA"},
			{ID: "py:t.x:test_b", Source: "added", Reason: "p>=lambda", Package: "t.x", RunnerID: "t/x.py::test_b", P: &p},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fromEngine =\n%+v\nwant\n%+v", got, want)
	}
	if len(got.Tests) == 2 {
		if got.Tests[1].P == d.Tests[1].P {
			t.Error("P must be a copy: the caller may keep the Decision after the engine's data is reused")
		}
		if got.Tests[0].P != nil {
			t.Error("an unknown probability stays nil, never zero")
		}
	}
}

func TestFromEngineFullSuite(t *testing.T) {
	d := engine.Decision{
		ID: "id", FullSuite: true, Abstained: true, AbstainReason: "no_lock",
		Hypothetical: []engine.Selected{{TestID: "go:m/a:TestA", Source: "certain"}},
	}
	got := fromEngine(d)
	if !got.FullSuite || !got.Abstained || got.AbstainReason != "no_lock" || len(got.Tests) != 0 {
		t.Errorf("a full-suite decision is a directive to run everything, with no Tests slice standing in for the suite: %+v", got)
	}
}

func TestSelectRequiresARepoDir(t *testing.T) {
	if _, err := Select(context.Background(), Options{}); err == nil || !strings.Contains(err.Error(), "RepoDir") {
		t.Errorf("err = %v, want an error naming RepoDir", err)
	}
}

func TestSelectCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Select(ctx, Options{RepoDir: t.TempDir()}); err == nil {
		t.Error("a canceled context must be an error")
	}
}

func TestSelectReportsAnInvalidRefAsAnError(t *testing.T) {
	dir := newRepo(t, goModuleFiles())
	d, err := Select(context.Background(), Options{RepoDir: dir, Base: "no-such-ref"})
	if err == nil {
		t.Fatalf("an invalid ref is an invalid invocation, got decision %+v", d)
	}
	if d.FullSuite || len(d.Tests) != 0 {
		t.Errorf("on error the Decision must not look like a selection: %+v", d)
	}
}

// newRepo commits files into a fresh repository on main and returns it.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// A fixture repository ignores the user's configuration (hooks, signing).
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
	}
	return dir
}

func goModuleFiles() map[string]string {
	return map[string]string{
		"go.mod":            "module example.com/m\n\ngo 1.21\n",
		"hazard.yml":        "version: 1\nlanguages:\n  python:\n    enabled: false\n",
		".gitignore":        ".hazard/\n",
		"core/core.go":      "package core\n\nfunc Core() int { return 1 }\n",
		"core/core_test.go": "package core\n\nimport \"testing\"\n\nfunc TestCore(t *testing.T) {\n\tif Core() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	}
}

func TestSelectResolvesARelativeRepoDirAndRefusesWithoutAnIndex(t *testing.T) {
	dir := newRepo(t, goModuleFiles())
	t.Chdir(filepath.Dir(dir))
	d, err := Select(context.Background(), Options{RepoDir: filepath.Base(dir)})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !d.FullSuite || d.AbstainReason != "no_index" || !d.Abstained || d.ID == "" || len(d.Tests) != 0 {
		t.Errorf("decision = %+v, want a valid full-suite refusal (no_index)", d)
	}
}

func TestSelectResolvesARelativeConfigPathAgainstRepoDir(t *testing.T) {
	files := goModuleFiles()
	files["alt.yml"] = "version: 1\nunknown_key: true\n"
	dir := newRepo(t, files)
	t.Chdir(t.TempDir()) // elsewhere: a path relative to the working directory would not be found
	_, err := Select(context.Background(), Options{RepoDir: dir, ConfigPath: "alt.yml"})
	if err == nil || !strings.Contains(err.Error(), "alt.yml") {
		t.Errorf("err = %v; the relative config must be read from RepoDir, where it is invalid", err)
	}
}

// TestSelectAgreesWithTheEngine builds a real index and checks that the public
// package returns what the command line's engine decides, here a full-suite
// refusal because no lock exists.
func TestSelectAgreesWithTheEngine(t *testing.T) {
	dir := newRepo(t, goModuleFiles())
	cfg, err := config.Load(filepath.Join(dir, "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := index.Build(context.Background(), dir, cfg, index.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	want, err := engine.Select(context.Background(), engine.Options{RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Select(context.Background(), Options{RepoDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !got.FullSuite || got.AbstainReason != "no_lock" || got.FullSuite != want.FullSuite || got.AbstainReason != want.AbstainReason ||
		got.Abstained != want.Abstained || got.LockDigest != want.LockDigest || len(got.Tests) != len(want.Tests) {
		t.Errorf("public decision %+v disagrees with the engine's %+v", got, want)
	}
}
