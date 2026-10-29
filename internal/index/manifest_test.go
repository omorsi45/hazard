package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/config"
)

// gitRepo creates a git repository in a temp dir with the files committed.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := initRepo(dir, files); err != nil {
		t.Fatal(err)
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

const goFixtureMod = "module example.com/m\n\ngo 1.21\n"

func goFixture() map[string]string {
	return map[string]string{
		"go.mod":      goFixtureMod,
		"README.md":   "# readme\n",
		".gitignore":  "ignored.go\n.hazard/\n",
		"a/a.go":      "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go": "package a\n\nimport \"testing\"\n\n// TestA checks A.\nfunc TestA(t *testing.T) {\n\tif A() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
		"b/b.go":      "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.A() }\n",
	}
}

// goOnly returns the default configuration with Python disabled.
func goOnly() config.Config {
	cfg := config.Defaults()
	cfg.Languages.Python.Enabled = false
	return cfg
}

func TestManifestBuildGo(t *testing.T) {
	t.Parallel()
	dir, m := sharedGoRepo(t)
	if m.Schema != SchemaVersion || !m.Complete || m.CreatedAt.IsZero() {
		t.Errorf("header = schema %d complete %v created %v", m.Schema, m.Complete, m.CreatedAt)
	}
	if want := git(t, dir, "rev-parse", "HEAD"); m.HeadSHA != want {
		t.Errorf("HeadSHA = %q, want %q", m.HeadSHA, want)
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	if !strings.EqualFold(m.RepoRoot, filepath.ToSlash(resolved)) && !strings.EqualFold(m.RepoRoot, filepath.ToSlash(dir)) {
		t.Errorf("RepoRoot = %q, want %q", m.RepoRoot, filepath.ToSlash(dir))
	}
	if m.Dirty {
		t.Error("an ignored file must not make the tree dirty")
	}
	wantFiles := map[string]string{
		"go.mod":      hexSHA(goFixtureMod),
		"a/a.go":      hexSHA(goFixture()["a/a.go"]),
		"a/a_test.go": hexSHA(goFixture()["a/a_test.go"]),
		"b/b.go":      hexSHA(goFixture()["b/b.go"]),
	}
	if len(m.Files) != len(wantFiles) {
		t.Errorf("Files = %v", m.Files)
	}
	for p, h := range wantFiles {
		if m.Files[p] != h {
			t.Errorf("Files[%s] = %q, want %q", p, m.Files[p], h)
		}
	}
	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var digest bytes.Buffer
	for _, p := range paths {
		digest.WriteString(p + "\x00" + m.Files[p] + "\n")
	}
	if m.ContentDigest != hexSHA(digest.String()) {
		t.Errorf("ContentDigest = %q", m.ContentDigest)
	}
	cfgJSON, _ := json.Marshal(config.Defaults())
	if m.ConfigDigest != hexSHA(string(cfgJSON)) {
		t.Errorf("ConfigDigest = %q", m.ConfigDigest)
	}
	if !strings.HasPrefix(m.Tools["go"], "go1") {
		t.Errorf("Tools = %v", m.Tools)
	}
	if m.Graph == nil || m.Graph.Packages["example.com/m/b"] == nil || len(m.Graph.Packages["example.com/m/b"].Imports) != 1 {
		t.Errorf("graph = %+v", m.Graph)
	}
	if len(m.Tests) != 1 || m.Tests[0].ID != "go:example.com/m/a:TestA" || m.Tests[0].Purpose != "TestA checks A." {
		t.Errorf("tests = %+v", m.Tests)
	}
	var sawList, sawVersion bool
	for _, c := range m.Commands {
		if len(c.Argv) > 1 && c.Argv[0] == "go" && c.Argv[1] == "list" && c.Exit == 0 && c.Dir != "" {
			sawList = true
		}
		if strings.Join(c.Argv, " ") == "go version" && c.Exit == 0 && c.Dir != "" && c.Seconds > 0 {
			sawVersion = true
		}
	}
	if !sawList {
		t.Errorf("go list command not recorded: %+v", m.Commands)
	}
	if !sawVersion {
		t.Errorf("the go version probe behind Tools[go] must be recorded in Commands: %+v", m.Commands)
	}
	if m.Coverage != nil {
		t.Error("coverage was not requested")
	}
}

func TestManifestGoOnlyRepositoryWithDefaultConfigIsComplete(t *testing.T) {
	t.Parallel()
	_, m := sharedGoRepo(t)
	if !m.Complete {
		t.Errorf("a Go-only repository must stay complete under the default configuration that enables Python: %v", m.Diagnostics)
	}
	if want := []string{"python: absent"}; !reflect.DeepEqual(m.Diagnostics, want) {
		t.Errorf("diagnostics = %q, want %q", m.Diagnostics, want)
	}
	if _, ok := m.Tools["python"]; ok {
		t.Errorf("no Python tool may be probed without Python sources: %v", m.Tools)
	}
}

func TestManifestBuildDirtyTree(t *testing.T) {
	t.Parallel()
	shared, _ := sharedGoRepo(t)
	dir := copyRepo(t, shared)
	writeFiles(t, dir, map[string]string{"c/c.go": "package c\n"})
	m, err := Build(context.Background(), dir, goOnly(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Dirty || m.Files["c/c.go"] == "" {
		t.Errorf("untracked source must be recorded and mark the tree dirty: dirty=%v files=%v", m.Dirty, m.Files)
	}
}

func TestManifestSkipsDisabledAndAbsentLanguages(t *testing.T) {
	t.Parallel()
	shared, _ := sharedGoRepo(t)
	dir := copyRepo(t, shared)
	cfg := config.Defaults()
	cfg.Languages.Go.Enabled = false
	cfg.Languages.Python.Enabled = true
	cfg.Languages.Python.Testpaths = []string{"nothing_here"}
	m, err := Build(context.Background(), dir, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tests) != 0 || len(m.Graph.Packages) != 0 || len(m.Files) != 0 {
		t.Errorf("disabled Go must contribute nothing: tests=%v packages=%v files=%v", m.Tests, m.Graph.Packages, m.Files)
	}
	for _, c := range m.Commands {
		if len(c.Argv) > 0 && (c.Argv[0] == "go" || strings.Contains(strings.Join(c.Argv, " "), "pytest")) {
			t.Errorf("no go or pytest command may run: %+v", c)
		}
	}
	if !m.Complete {
		t.Error("a repository without Python sources is still complete")
	}
	pythonOnly := gitRepo(t, map[string]string{"a.py": "x = 1\n"})
	m, err = Build(context.Background(), pythonOnly, config.Defaults(), Options{})
	if err != nil {
		t.Fatalf("a repository without go.mod and without pytest tests must build: %v", err)
	}
	if m.Complete || len(m.Tests) != 0 || m.Graph.Packages["py:a"] == nil || m.Files["a.py"] == "" {
		t.Errorf("python-only repository without testpaths must build but be incomplete: complete=%v tests=%v graph=%v files=%v", m.Complete, m.Tests, m.Graph.Packages, m.Files)
	}
}

func TestManifestCompletenessPerLanguage(t *testing.T) {
	t.Parallel()
	const (
		goNotAnalyzed     = "go: Go source files exist but there is no go.mod in the repository root"
		pythonNotAnalyzed = "python: Python source files exist but none of the configured testpaths exist"
	)
	bothDisabled := config.Defaults()
	bothDisabled.Languages.Go.Enabled = false
	bothDisabled.Languages.Python.Enabled = false
	for _, tc := range []struct {
		name     string
		files    map[string]string
		cfg      config.Config
		complete bool
		diags    []string // each must be contained in some diagnostic
		noDiags  []string // none may be contained in any diagnostic
	}{
		{"python sources without testpaths", map[string]string{"a.py": "x = 1\n"}, config.Defaults(), false,
			[]string{pythonNotAnalyzed, "go: absent"}, []string{"python: absent", goNotAnalyzed}},
		{"go sources without a root go.mod", map[string]string{"sub/a.go": "package a\n"}, config.Defaults(), false,
			[]string{goNotAnalyzed, "python: absent"}, []string{"go: absent", pythonNotAnalyzed}},
		{"go and python sources, neither analyzed", map[string]string{"sub/a.go": "package a\n", "a.py": "x = 1\n"}, config.Defaults(), false,
			[]string{goNotAnalyzed, pythonNotAnalyzed}, []string{"go: absent", "python: absent"}},
		{"no sources at all", map[string]string{"README.md": "# readme\n"}, config.Defaults(), true,
			[]string{"go: absent", "python: absent"}, []string{goNotAnalyzed, pythonNotAnalyzed}},
		{"disabled languages are not reported", map[string]string{"sub/a.go": "package a\n", "a.py": "x = 1\n"}, bothDisabled, true,
			nil, []string{"go:", "python:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := gitRepo(t, tc.files)
			m, err := Build(context.Background(), dir, tc.cfg, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if m.Complete != tc.complete {
				t.Errorf("Complete = %v, want %v (diagnostics %q)", m.Complete, tc.complete, m.Diagnostics)
			}
			for _, want := range tc.diags {
				if !anyContains(m.Diagnostics, want) {
					t.Errorf("no diagnostic contains %q: %q", want, m.Diagnostics)
				}
			}
			for _, unwanted := range tc.noDiags {
				if anyContains(m.Diagnostics, unwanted) {
					t.Errorf("a diagnostic contains %q: %q", unwanted, m.Diagnostics)
				}
			}
		})
	}
}

func anyContains(list []string, substr string) bool {
	for _, s := range list {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func TestManifestPublishLoadRoundTrip(t *testing.T) {
	t.Parallel()
	shared, m := sharedGoRepo(t)
	dir := copyRepo(t, shared)
	if err := Publish(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(m)
	have, _ := json.Marshal(got)
	if !bytes.Equal(want, have) {
		t.Errorf("round trip changed the manifest:\n%s\n%s", want, have)
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".hazard", "index"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "manifest.json" {
		t.Errorf("only manifest.json may remain after Publish: %v %v", entries, err)
	}
	if dirty, _ := m.Stale(dir, config.Defaults()); dirty != "" {
		t.Errorf("publishing under .hazard must not make the index stale: %q", dirty)
	}
}

func TestManifestLoadValidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Load(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing manifest must wrap ErrNotExist: %v", err)
	}
	for name, content := range map[string]string{
		"corrupt":       "{not json",
		"wrong schema":  `{"Schema": 99}`,
		"missing field": `{}`,
	} {
		writeFiles(t, dir, map[string]string{".hazard/index/manifest.json": content})
		if m, err := Load(dir); err == nil || m != nil {
			t.Errorf("%s: want error, got %v %v", name, m, err)
		}
	}
}

func TestManifestFailedBuildKeepsPublishedManifest(t *testing.T) {
	t.Parallel()
	shared, first := sharedGoRepo(t)
	dir := copyRepo(t, shared)
	if err := Publish(dir, first); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, ".hazard", "index", "manifest.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// A package error makes go list fail: discovery fails, nothing is published.
	writeFiles(t, dir, map[string]string{"a/a.go": "package a\n\nimport \"example.com/m/missing\"\n\nfunc A() int { return missing.X }\n"})
	if m, err := Build(context.Background(), dir, config.Defaults(), Options{}); err == nil || m != nil {
		t.Fatalf("a package error must fail the build: %v %v", m, err)
	}
	// A manifest that cannot be encoded fails Publish without touching the file.
	broken := *first
	broken.Commands = []Command{{Argv: []string{"x"}, Seconds: math.NaN()}}
	if err := Publish(dir, &broken); err == nil {
		t.Fatal("an unencodable manifest must fail Publish")
	}
	// A canceled context fails the build.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if m, err := Build(ctx, dir, config.Defaults(), Options{}); err == nil || m != nil {
		t.Fatalf("canceled build: %v %v", m, err)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the previously published manifest changed: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil || loaded.ContentDigest != first.ContentDigest {
		t.Fatalf("Load after failures = %+v %v", loaded, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(manifestPath))
	if len(entries) != 1 {
		t.Errorf("failed Publish left files behind: %v", entries)
	}
}

func TestManifestStale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, dir string, cfg *config.Config)
		want   string
	}{
		{"unchanged", func(t *testing.T, dir string, cfg *config.Config) {}, ""},
		{"timestamp only", func(t *testing.T, dir string, cfg *config.Config) {
			now := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(dir, "a", "a.go"), now, now); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"changed file hash", func(t *testing.T, dir string, cfg *config.Config) {
			writeFiles(t, dir, map[string]string{"a/a.go": "package a\n\nfunc A() int { return 2 }\n"})
		}, "a/a.go"},
		{"removed file", func(t *testing.T, dir string, cfg *config.Config) {
			if err := os.Remove(filepath.Join(dir, "b", "b.go")); err != nil {
				t.Fatal(err)
			}
		}, "b/b.go"},
		{"new relevant file", func(t *testing.T, dir string, cfg *config.Config) {
			writeFiles(t, dir, map[string]string{"c/c.go": "package c\n"})
		}, "c/c.go"},
		{"new test file", func(t *testing.T, dir string, cfg *config.Config) {
			writeFiles(t, dir, map[string]string{"b/b_test.go": "package b\n"})
		}, "b/b_test.go"},
		{"new irrelevant file", func(t *testing.T, dir string, cfg *config.Config) {
			writeFiles(t, dir, map[string]string{"notes.txt": "x", "README.md": "changed"})
		}, ""},
		{"changed config", func(t *testing.T, dir string, cfg *config.Config) {
			cfg.Impact.MaxHops++
		}, "config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shared, m := sharedGoRepo(t)
			dir := copyRepo(t, shared)
			cfg := config.Defaults()
			tc.mutate(t, dir, &cfg)
			reason, err := m.Stale(dir, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && reason != "" {
				t.Fatalf("unexpectedly stale: %q", reason)
			}
			if tc.want != "" && !strings.Contains(reason, tc.want) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
}
