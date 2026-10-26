package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/graph"
	"github.com/omorsi45/hazard/internal/index"
	"github.com/spf13/cobra"
)

// runIndex executes the index command built from fakes inside a root command.
func runIndex(ctx context.Context, build indexBuildFunc, publish indexPublishFunc, args ...string) (int, string, string) {
	root := &cobra.Command{Use: "hazard", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().CountP("verbose", "v", "")
	root.AddCommand(newIndexCommand(build, publish))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"index"}, args...))
	err := root.ExecuteContext(ctx)
	code := 0
	if err != nil {
		code = 70
		var usage usageError
		if errors.As(err, &usage) {
			code = 2
		}
		errOut.WriteString(err.Error())
	}
	return code, out.String(), errOut.String()
}

func fakeManifest() *index.Manifest {
	return &index.Manifest{
		Schema:   index.SchemaVersion,
		HeadSHA:  "abc123",
		Complete: true,
		Graph:    &graph.Graph{Packages: map[string]*graph.Package{"m/a": {ID: "m/a"}, "py:b": {ID: "py:b"}}},
		Tests:    []index.Test{{ID: "go:m/a:TestA"}, {ID: "py:t:test_b"}, {ID: "py:t:test_c"}},
		Coverage: &index.Coverage{Granularity: "go-top-level-test", Tests: map[string]index.TestCoverage{
			"go:m/a:TestA": {Status: "covered"}, "py:t:test_b": {Status: "failed"}, "py:t:test_c": {Status: "not_run"},
		}},
		Diagnostics: []string{"go coverage: example diagnostic"},
	}
}

func TestIndexHelpStatesSideEffects(t *testing.T) {
	code, out, errOut := invoke(context.Background(), "index", "--help")
	if code != 0 || errOut != "" {
		t.Fatalf("help code=%d err=%q", code, errOut)
	}
	for _, phrase := range []string{"test package initialization", "--coverage executes the test suite", "--coverage", "--config", "--python"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("help omitted %q:\n%s", phrase, out)
		}
	}
	code, out, _ = invoke(context.Background(), "--help")
	if code != 0 || !strings.Contains(out, "index") {
		t.Errorf("root help omitted index: %q", out)
	}
}

func TestIndexSummaryTextAndPublish(t *testing.T) {
	var gotOpts index.Options
	var gotCfg config.Config
	var gotDir string
	var published *index.Manifest
	build := func(_ context.Context, dir string, cfg config.Config, opts index.Options) (*index.Manifest, error) {
		gotDir, gotCfg, gotOpts = dir, cfg, opts
		return fakeManifest(), nil
	}
	publish := func(dir string, m *index.Manifest) error {
		if dir != gotDir {
			t.Errorf("published to %q, built in %q", dir, gotDir)
		}
		published = m
		return nil
	}
	code, out, errOut := runIndex(context.Background(), build, publish, "--coverage", "--python", "py3")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !gotOpts.Coverage || gotOpts.Python != "py3" || gotCfg.Version != 1 {
		t.Errorf("opts=%+v cfg.Version=%d", gotOpts, gotCfg.Version)
	}
	if cwd, _ := os.Getwd(); !strings.EqualFold(filepath.Clean(gotDir), filepath.Clean(cwd)) {
		t.Errorf("must build in the current directory: %q vs %q", gotDir, cwd)
	}
	if published == nil || len(published.Tests) != 3 {
		t.Fatalf("manifest not published: %+v", published)
	}
	for _, want := range []string{"tests=3", "packages=2", "complete=true", "covered=1", "failed=1", "not_run=1", "example diagnostic"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary omitted %q:\n%s", want, out)
		}
	}
}

func TestIndexSummaryJSON(t *testing.T) {
	build := func(context.Context, string, config.Config, index.Options) (*index.Manifest, error) {
		return fakeManifest(), nil
	}
	code, out, errOut := runIndex(context.Background(), build, func(string, *index.Manifest) error { return nil }, "--json", "--coverage")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	var got struct {
		Tests, Packages int
		Complete        bool
		HeadSHA         string `json:"head_sha"`
		Manifest        string
		Coverage        map[string]any
		Diagnostics     []string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %q: %v", out, err)
	}
	if got.Tests != 3 || got.Packages != 2 || !got.Complete || got.HeadSHA != "abc123" || got.Coverage == nil || len(got.Diagnostics) != 1 || !strings.HasSuffix(got.Manifest, "manifest.json") {
		t.Errorf("summary = %+v", got)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(out), &raw)
	cov, _ := raw["coverage"].(map[string]any)
	if cov["covered"] != float64(1) || cov["failed"] != float64(1) || cov["not_run"] != float64(1) {
		t.Errorf("coverage counts = %v", cov)
	}
}

func TestIndexFailuresDoNotPublish(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx   func() context.Context
		build indexBuildFunc
		want  string
	}{
		"build error": {
			func() context.Context { return context.Background() },
			func(context.Context, string, config.Config, index.Options) (*index.Manifest, error) {
				return nil, errors.New("go package discovery failed")
			},
			"go package discovery failed",
		},
		"canceled": {
			func() context.Context { c, cancel := context.WithCancel(context.Background()); cancel(); return c },
			func(ctx context.Context, _ string, _ config.Config, _ index.Options) (*index.Manifest, error) {
				return nil, ctx.Err()
			},
			"canceled",
		},
		"canceled after build": {
			func() context.Context { c, cancel := context.WithCancel(context.Background()); cancel(); return c },
			func(context.Context, string, config.Config, index.Options) (*index.Manifest, error) {
				return fakeManifest(), nil
			},
			"canceled",
		},
	} {
		t.Run(name, func(t *testing.T) {
			published := false
			code, out, errOut := runIndex(tc.ctx(), tc.build, func(string, *index.Manifest) error { published = true; return nil })
			if code != 70 || published || out != "" || !strings.Contains(errOut, tc.want) {
				t.Fatalf("code=%d published=%v out=%q err=%q", code, published, out, errOut)
			}
		})
	}
	code, _, errOut := runIndex(context.Background(),
		func(context.Context, string, config.Config, index.Options) (*index.Manifest, error) {
			return fakeManifest(), nil
		},
		func(string, *index.Manifest) error { return errors.New("disk full") })
	if code != 70 || !strings.Contains(errOut, "disk full") {
		t.Errorf("publish failure: code=%d err=%q", code, errOut)
	}
}

func TestIndexUsageErrors(t *testing.T) {
	build := func(context.Context, string, config.Config, index.Options) (*index.Manifest, error) {
		t.Error("build must not run on a usage error")
		return nil, errors.New("unreachable")
	}
	missing := filepath.Join(t.TempDir(), "absent.yml")
	badConfig := filepath.Join(t.TempDir(), "bad.yml")
	if err := os.WriteFile(badConfig, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"extra"}, {"--no-such-flag"}, {"--config", missing}, {"--config", badConfig}} {
		code, _, errOut := runIndex(context.Background(), build, nil, args...)
		if code != 2 || errOut == "" {
			t.Errorf("%v: code=%d err=%q, want 2", args, code, errOut)
		}
	}
}

func gitInit(t *testing.T, dir string, files map[string]string) {
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
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

const indexGoOnlyConfig = "version: 1\nlanguages:\n  python:\n    enabled: false\n"

func TestIndexRealBuildPublishesAndFailureKeepsPreviousManifest(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{
		"hazard.yml":  indexGoOnlyConfig,
		".gitignore":  ".hazard/\n",
		"go.mod":      "module example.com/m\n\ngo 1.21\n",
		"a/a.go":      "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	})
	t.Chdir(dir)
	code, out, errOut := invoke(context.Background(), "index", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	m, err := index.Load(dir)
	if err != nil || len(m.Tests) != 1 || m.Tests[0].ID != "go:example.com/m/a:TestA" || !m.Complete {
		t.Fatalf("published manifest = %+v %v", m, err)
	}
	if reason, err := m.Stale(dir, goOnlyConfigForTest(t)); err != nil || reason != "" {
		t.Errorf("fresh index reported stale: %q %v", reason, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, ".hazard", "index", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A package error fails discovery: exit 70 and the previous manifest stays.
	if err := os.WriteFile(filepath.Join(dir, "a", "a.go"), []byte("package a\n\nimport \"example.com/m/missing\"\n\nfunc A() int { return missing.X }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = invoke(context.Background(), "index")
	if code != 70 || out != "" || errOut == "" {
		t.Fatalf("failing build: code=%d out=%q err=%q", code, out, errOut)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".hazard", "index", "manifest.json"))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("the previous manifest changed after a failed build: %v", err)
	}
}

func TestIndexFirstFailureCreatesNoManifest(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{
		"hazard.yml": indexGoOnlyConfig,
		".gitignore": ".hazard/\n",
		"go.mod":     "module example.com/m\n\ngo 1.21\n",
		"a/a.go":     "package a\n\nimport \"example.com/m/missing\"\n\nfunc A() int { return missing.X }\n",
	})
	t.Chdir(dir)
	code, _, errOut := invoke(context.Background(), "index")
	if code != 70 || errOut == "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, ".hazard", "index", "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed first build must not publish: %v", err)
	}
}

func goOnlyConfigForTest(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load("hazard.yml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
