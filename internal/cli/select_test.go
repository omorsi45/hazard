package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/index"
)

const (
	goA  = "go:example.com/m/a:TestA"
	goA2 = "go:example.com/m/a:TestA2"
	goB  = "go:example.com/m/b:TestB"
	goC  = "go:example.com/m/c:TestC"
	pyK  = "py:tests.test_x.TestK:test_a[1]"
)

// runSelectCmd runs the select command built around sel inside a root command
// with the persistent flags of the real one, mapping errors to exit codes the
// way Execute does.
func runSelectCmd(ctx context.Context, sel selectFunc, args ...string) (int, string, string) {
	return runLeaf(ctx, newSelectCommand(sel), "select", args...)
}

func runLeaf(ctx context.Context, cmd *cobra.Command, name string, args ...string) (int, string, string) {
	root := &cobra.Command{Use: "hazard", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().CountP("verbose", "v", "")
	root.AddCommand(cmd)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{name}, args...))
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

// fakeSelect records the options it receives and returns a canned result.
type fakeSelect struct {
	d     engine.Decision
	err   error
	calls int
	got   engine.Options
}

func (f *fakeSelect) fn(_ context.Context, o engine.Options) (engine.Decision, error) {
	f.calls++
	f.got = o
	return f.d, f.err
}

func narrowedDecision() engine.Decision {
	return engine.Decision{
		ID:       "id1",
		AllTests: []string{goA, goA2, goB, goC, pyK},
		Tests: []engine.Selected{
			{TestID: goA, Source: "certain", Reason: "direct", Package: "example.com/m/a", RunnerID: "TestA"},
			{TestID: goA2, Source: "certain", Reason: "graph", Package: "example.com/m/a", RunnerID: "TestA.2"},
			{TestID: goB, Source: "always_run", Reason: "quarantined", Package: "example.com/m/b", RunnerID: "TestB"},
			{TestID: pyK, Source: "certain", Reason: "graph", Package: "tests.test_x.TestK", RunnerID: "tests/test_x.py::TestK::test_a[1]"},
		},
	}
}

func fullDecision() engine.Decision {
	return engine.Decision{
		ID: "id2", FullSuite: true, Abstained: true, AbstainReason: "no_lock",
		AllTests: []string{goA, goB, pyK},
		Hypothetical: []engine.Selected{
			{TestID: goA, Source: "certain", Reason: "graph", Package: "example.com/m/a", RunnerID: "TestA"},
			{TestID: pyK, Source: "always_run", Reason: "unstable_id", Package: "tests.test_x.TestK", RunnerID: "tests/test_x.py::TestK::test_a[1]"},
		},
		Warnings: []string{"first warning"},
	}
}

// gitTest runs git in dir for fixture setup.
func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// chdirRepo makes an empty git repository the working directory.
func chdirRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q", "-b", "main")
	t.Chdir(dir)
	return dir
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ia, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(ia, ib)
}

func TestSelectTextFormat(t *testing.T) {
	chdirRepo(t)
	t.Run("a narrowed selection", func(t *testing.T) {
		f := &fakeSelect{d: narrowedDecision()}
		code, out, errOut := runSelectCmd(context.Background(), f.fn)
		want := "selected 4 of 5 tests\n" + goA + "\n" + goA2 + "\n" + goB + "\n" + pyK + "\n"
		if code != 0 || out != want || errOut != "" {
			t.Errorf("code=%d out=%q err=%q, want out %q", code, out, errOut, want)
		}
		code, out, _ = runSelectCmd(context.Background(), f.fn, "--explain")
		want = "selected 4 of 5 tests\n" + goA + "\tcertain\tdirect\n" + goA2 + "\tcertain\tgraph\n" + goB + "\talways_run\tquarantined\n" + pyK + "\tcertain\tgraph\n"
		if code != 0 || out != want {
			t.Errorf("--explain: code=%d out=%q, want %q", code, out, want)
		}
	})
	t.Run("a full suite lists the hypothetical set and warns on stderr", func(t *testing.T) {
		f := &fakeSelect{d: fullDecision()}
		code, out, errOut := runSelectCmd(context.Background(), f.fn)
		want := "full suite: no_lock\nhypothetical " + goA + "\nhypothetical " + pyK + "\n"
		if code != 0 || out != want {
			t.Errorf("code=%d out=%q, want %q", code, out, want)
		}
		if !strings.Contains(errOut, "warning: first warning") {
			t.Errorf("stderr = %q, want the warning", errOut)
		}
		_, out, _ = runSelectCmd(context.Background(), f.fn, "--explain")
		want = "full suite: no_lock\nhypothetical " + goA + "\tcertain\tgraph\nhypothetical " + pyK + "\talways_run\tunstable_id\n"
		if out != want {
			t.Errorf("--explain out=%q, want %q", out, want)
		}
	})
	t.Run("a full suite demanded by L1 has no hypothetical lines", func(t *testing.T) {
		d := fullDecision()
		d.Hypothetical, d.Warnings, d.AbstainReason = nil, nil, "trigger:go.mod:go.mod"
		f := &fakeSelect{d: d}
		code, out, errOut := runSelectCmd(context.Background(), f.fn)
		if code != 0 || out != "full suite: trigger:go.mod:go.mod\n" || errOut != "" {
			t.Errorf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
}

func TestSelectGoRunFormat(t *testing.T) {
	chdirRepo(t)
	run := func(d engine.Decision, args ...string) string {
		f := &fakeSelect{d: d}
		code, out, errOut := runSelectCmd(context.Background(), f.fn, append([]string{"--format", "go-run"}, args...)...)
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		return out
	}
	if got, want := run(narrowedDecision()), "example.com/m/a ^(TestA|TestA\\.2)$\nexample.com/m/b ^(TestB)$\n"; got != want {
		t.Errorf("go-run = %q, want %q (one line per package, sorted, names quoted, Python tests left to pytest)", got, want)
	}
	d := fullDecision()
	d.Warnings = nil
	if got := run(d); got != "#full-suite no_lock\n" {
		t.Errorf("full suite go-run = %q", got)
	}
	if got := run(d, "--shadow"); got != "#full-suite shadow\n" {
		t.Errorf("shadow go-run = %q", got)
	}
	missing := narrowedDecision()
	missing.Tests[0].RunnerID = ""
	if got := run(missing); got != "#full-suite runner_limit\n" {
		t.Errorf("a Go test the runner cannot name must widen to the full suite: %q", got)
	}
	foreign := narrowedDecision()
	foreign.Tests = append(foreign.Tests, engine.Selected{TestID: "zz:x:y", Source: "certain", Reason: "graph"})
	if got := run(foreign); got != "#full-suite runner_limit\n" {
		t.Errorf("an unknown language must widen to the full suite: %q", got)
	}
	empty := narrowedDecision()
	empty.Tests = nil
	if got := run(empty); got != "" {
		t.Errorf("an empty selection prints nothing: %q", got)
	}
}

func TestSelectPytestFileFormat(t *testing.T) {
	chdirRepo(t)
	run := func(d engine.Decision, args ...string) string {
		f := &fakeSelect{d: d}
		code, out, errOut := runSelectCmd(context.Background(), f.fn, append([]string{"--format", "pytest-file"}, args...)...)
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		return out
	}
	if got, want := run(narrowedDecision()), "tests/test_x.py::TestK::test_a[1]\n"; got != want {
		t.Errorf("pytest-file = %q, want %q", got, want)
	}
	d := fullDecision()
	d.Warnings = nil
	if got := run(d); got != "#full-suite no_lock\n" {
		t.Errorf("full suite pytest-file = %q", got)
	}
	missing := narrowedDecision()
	missing.Tests[3].RunnerID = ""
	if got := run(missing); got != "#full-suite runner_limit\n" {
		t.Errorf("a Python test with no node ID must widen to the full suite: %q", got)
	}
}

func TestSelectJSONFormat(t *testing.T) {
	chdirRepo(t)
	d := fullDecision()
	for _, args := range [][]string{{"--format", "json"}, {"--json"}} {
		f := &fakeSelect{d: d}
		code, out, errOut := runSelectCmd(context.Background(), f.fn, args...)
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 {
			t.Fatalf("%v: code=%d stdout %q is not a single JSON document: %v", args, code, out, err)
		}
		if doc["full_suite"] != true || doc["abstain_reason"] != "no_lock" || doc["id"] != "id2" {
			t.Errorf("%v: doc = %v", args, doc)
		}
		if errOut != "" {
			t.Errorf("%v: stderr = %q, want empty (warnings travel inside the document)", args, errOut)
		}
		if !strings.HasSuffix(out, "}\n") || strings.Count(out, "\n") != 1 {
			t.Errorf("%v: want one JSON line, got %q", args, out)
		}
	}
	f := &fakeSelect{d: d}
	if code, _, _ := runSelectCmd(context.Background(), f.fn, "--json", "--format", "go-run"); code != 2 || f.calls != 0 {
		t.Errorf("--json with another format: code=%d calls=%d, want a usage error", code, f.calls)
	}
}

func TestSelectShadowMakesTheFullSuiteTheRunSet(t *testing.T) {
	chdirRepo(t)
	f := &fakeSelect{d: fullDecision()}
	code, out, errOut := runSelectCmd(context.Background(), f.fn, "--shadow")
	want := "full suite: shadow\nhypothetical " + goA + "\nhypothetical " + pyK + "\n"
	if code != 0 || out != want || !f.got.Shadow {
		t.Errorf("code=%d out=%q shadow=%v, want %q", code, out, f.got.Shadow, want)
	}
	if !strings.Contains(errOut, "first warning") {
		t.Errorf("stderr = %q", errOut)
	}
	// Even if a caller returned a narrowed decision, shadow never narrows.
	n := &fakeSelect{d: narrowedDecision()}
	_, out, _ = runSelectCmd(context.Background(), n.fn, "--shadow")
	if !strings.HasPrefix(out, "full suite: shadow\n") || strings.Contains(out, "selected") {
		t.Errorf("shadow must print the full suite: %q", out)
	}
	_, out, _ = runSelectCmd(context.Background(), n.fn, "--shadow", "--format", "json")
	var doc struct {
		FullSuite bool             `json:"full_suite"`
		Tests     []map[string]any `json:"tests"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || !doc.FullSuite || len(doc.Tests) != 0 {
		t.Errorf("shadow JSON = %q (%v)", out, err)
	}
}

func TestSelectDryRunPayloadPrintsThePayloadOnly(t *testing.T) {
	chdirRepo(t)
	d := fullDecision()
	d.Payload = []byte(`{"requests":[]}`)
	d.Warnings = []string{"no external predictor in the active policy; nothing would be sent"}
	for _, args := range [][]string{{"--dry-run-payload"}, {"--dry-run-payload", "--format", "json"}} {
		f := &fakeSelect{d: d}
		code, out, errOut := runSelectCmd(context.Background(), f.fn, args...)
		if code != 0 || out != `{"requests":[]}` || !f.got.DryRunPayload {
			t.Errorf("%v: code=%d out=%q", args, code, out)
		}
		if !strings.Contains(errOut, "nothing would be sent") {
			t.Errorf("%v: stderr = %q, want the warning", args, errOut)
		}
	}
}

func TestSelectOutWritesAtomically(t *testing.T) {
	dir := chdirRepo(t)
	target := filepath.Join(dir, "sel", "nested", "tests.txt")
	f := &fakeSelect{d: narrowedDecision()}
	code, out, errOut := runSelectCmd(context.Background(), f.fn, "--out", target)
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q, want nothing on the console", code, out, errOut)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	_, want, _ := runSelectCmd(context.Background(), f.fn)
	if string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	// Overwrite with a different result; no temporary file may remain.
	g := &fakeSelect{d: fullDecision()}
	if code, _, _ := runSelectCmd(context.Background(), g.fn, "--out", target, "--format", "go-run"); code != 0 {
		t.Fatalf("second write code=%d", code)
	}
	got, _ = os.ReadFile(target)
	if !strings.HasPrefix(string(got), "#full-suite no_lock") {
		t.Errorf("file after overwrite = %q", got)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 {
		t.Errorf("directory = %v (%v), want only the output file", entries, err)
	}
}

func TestSelectMapsFlagsToEngineOptions(t *testing.T) {
	dir := chdirRepo(t)
	f := &fakeSelect{d: fullDecision()}
	if code, _, errOut := runSelectCmd(context.Background(), f.fn); code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !sameDir(t, f.got.RepoDir, dir) || !filepath.IsAbs(f.got.RepoDir) {
		t.Errorf("RepoDir = %q, want the repository root %q", f.got.RepoDir, dir)
	}
	// Unset limits must not override the configuration.
	if f.got.MaxCandidates != 0 || f.got.BudgetMS != 0 || f.got.BudgetUSD != 0 || f.got.LockPath != "" || f.got.Recall != 0 {
		t.Errorf("defaults leaked into the options: %+v", f.got)
	}

	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	lock := filepath.Join(dir, "my.lock")
	g := &fakeSelect{d: fullDecision()}
	code, _, errOut := runSelectCmd(context.Background(), g.fn,
		"--base", "main", "--head", "HEAD", "--config", "cfg.yml", "--lock", lock, "--predictor", "static",
		"--recall", "0.9", "--i-know-what-im-doing", "--max-candidates", "12", "--budget-ms", "700", "--budget-usd", "0.5",
		"--redact", "hash", "--explain")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	o := g.got
	if !sameDir(t, o.RepoDir, dir) {
		t.Errorf("RepoDir = %q, want the top level %q even from a subdirectory", o.RepoDir, dir)
	}
	if o.Base != "main" || o.Head != "HEAD" || o.ConfigPath != "cfg.yml" || o.LockPath != lock || o.Predictor != "static" || o.Redact != "hash" ||
		o.Recall != 0.9 || o.MaxCandidates != 12 || o.BudgetMS != 700 || o.BudgetUSD != 0.5 || !o.Explain || o.Shadow || o.DryRunPayload {
		t.Errorf("options = %+v", o)
	}
	rel := &fakeSelect{d: fullDecision()}
	if code, _, _ := runSelectCmd(context.Background(), rel.fn, "--lock", "rel.lock"); code != 0 {
		t.Fatal("relative lock rejected")
	}
	if want := filepath.Join(sub, "rel.lock"); !strings.EqualFold(rel.got.LockPath, want) {
		t.Errorf("LockPath = %q, want the cwd-relative path made absolute %q", rel.got.LockPath, want)
	}
}

func TestSelectValidatesFlags(t *testing.T) {
	chdirRepo(t)
	for _, args := range [][]string{
		{"--predictor", "bogus"},
		{"--redact", "bogus"},
		{"--format", "yaml"},
		{"--max-candidates", "0"},
		{"--max-candidates", "-3"},
		{"--budget-ms", "0"},
		{"--budget-ms", "-1"},
		{"--budget-usd", "-0.1"},
		{"--budget-usd", "NaN"},
		{"--budget-usd", "+Inf"},
		{"--recall", "0.9"},
		{"--recall", "0", "--i-know-what-im-doing"},
		{"--recall", "1", "--i-know-what-im-doing"},
		{"--recall", "NaN", "--i-know-what-im-doing"},
		{"--json", "--format", "go-run"},
		{"stray-positional"},
	} {
		f := &fakeSelect{d: fullDecision()}
		code, out, _ := runSelectCmd(context.Background(), f.fn, args...)
		if code != 2 || f.calls != 0 || out != "" {
			t.Errorf("%v: code=%d calls=%d out=%q, want a usage error before any selection", args, code, f.calls, out)
		}
	}
	for _, args := range [][]string{
		{"--predictor", "ensemble"}, {"--predictor", "jev"}, {"--redact", "none"}, {"--redact", "structure"},
		{"--format", "pytest-file"}, {"--budget-usd", "0"}, {"--recall", "0.9", "--i-know-what-im-doing"},
	} {
		f := &fakeSelect{d: fullDecision()}
		if code, _, errOut := runSelectCmd(context.Background(), f.fn, args...); code != 0 {
			t.Errorf("%v: code=%d err=%q, want accepted", args, code, errOut)
		}
	}
}

func TestSelectErrorsMapToExitCodes(t *testing.T) {
	chdirRepo(t)
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"invalid ref", fmt.Errorf("%w: base %q", engine.ErrInvalidRef, "x"), 2},
		{"invalid config", fmt.Errorf("%w: bad", engine.ErrInvalidConfig), 2},
		{"anything else", errors.New("boom"), 70},
		{"canceled", context.Canceled, 70},
	} {
		f := &fakeSelect{err: tc.err}
		code, out, errOut := runSelectCmd(context.Background(), f.fn)
		if code != tc.want || out != "" || !strings.Contains(errOut, tc.err.Error()) {
			t.Errorf("%s: code=%d out=%q err=%q, want exit %d", tc.name, code, out, errOut, tc.want)
		}
	}
}

func TestSelectOutsideAGitRepositoryIsAUsageError(t *testing.T) {
	t.Chdir(t.TempDir())
	f := &fakeSelect{d: fullDecision()}
	code, _, errOut := runSelectCmd(context.Background(), f.fn)
	if code != 2 || f.calls != 0 || !strings.Contains(errOut, "git repository") {
		t.Errorf("code=%d calls=%d err=%q", code, f.calls, errOut)
	}
}

func TestSelectRespectsCancellation(t *testing.T) {
	chdirRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeSelect{d: fullDecision()}
	code, _, errOut := runSelectCmd(ctx, f.fn)
	if code != 70 || f.calls != 0 || !strings.Contains(errOut, "canceled") {
		t.Errorf("code=%d calls=%d err=%q", code, f.calls, errOut)
	}
}

func TestSelectHelpAdvertisesTheFormats(t *testing.T) {
	code, out, _ := invoke(context.Background(), "select", "--help")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, phrase := range []string{"text|json|go-run|pytest-file", "--dry-run-payload", "--shadow"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("help omitted %q", phrase)
		}
	}
}

// e2eRepo builds a committed Go module (core <- left), edits core, indexes the
// working tree and makes the repository the working directory.
func e2eRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	files := map[string]string{
		"go.mod":            "module example.com/m\n\ngo 1.21\n",
		"hazard.yml":        "version: 1\nlanguages:\n  python:\n    enabled: false\n",
		".gitignore":        ".hazard/\n",
		"README.md":         "# readme\n",
		"core/core.go":      "package core\n\nfunc Core() int { return 1 }\n",
		"core/core_test.go": "package core\n\nimport \"testing\"\n\nfunc TestCore(t *testing.T) {\n\tif Core() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
		"left/left.go":      "package left\n\nimport \"example.com/m/core\"\n\nfunc Left() int { return core.Core() }\n",
		"left/left_test.go": "package left\n\nimport \"testing\"\n\nfunc TestLeft(t *testing.T) {\n\tif Left() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
		"solo/solo.go":      "package solo\n\nfunc Solo() int { return 1 }\n",
		"solo/solo_test.go": "package solo\n\nimport \"testing\"\n\nfunc TestSolo(t *testing.T) {\n\tif Solo() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "init", "-q", "-b", "main")
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "core", "core.go"), []byte("package core\n\nfunc Core() int { return 1 + 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	t.Chdir(dir)
	return dir
}

const (
	e2eCore = "go:example.com/m/core:TestCore"
	e2eLeft = "go:example.com/m/left:TestLeft"
)

// TestSelectEndToEnd drives the real command tree and engine on a real
// repository. The sub-tests share one indexed repository and run in order.
func TestSelectEndToEnd(t *testing.T) {
	dir := e2eRepo(t)
	ctx := context.Background()
	wantText := "full suite: no_lock\nhypothetical " + e2eCore + "\nhypothetical " + e2eLeft + "\n"

	t.Run("text", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "select")
		if code != 0 || out != wantText || errOut != "" {
			t.Errorf("code=%d out=%q err=%q, want out %q", code, out, errOut, wantText)
		}
	})
	t.Run("explain suffix", func(t *testing.T) {
		code, out, _ := invoke(ctx, "select", "--explain")
		want := "full suite: no_lock\nhypothetical " + e2eCore + "\tcertain\tgraph\nhypothetical " + e2eLeft + "\tcertain\tgraph\n"
		if code != 0 || out != want {
			t.Errorf("code=%d out=%q, want %q", code, out, want)
		}
	})
	t.Run("json isolation", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "select", "--json")
		var doc struct {
			FullSuite     bool             `json:"full_suite"`
			AbstainReason string           `json:"abstain_reason"`
			Hypothetical  []map[string]any `json:"hypothetical"`
			Change        struct {
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
			} `json:"change"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("stdout is not one JSON document: %v\n%q", err, out)
		}
		if code != 0 || errOut != "" || !doc.FullSuite || doc.AbstainReason != "no_lock" || len(doc.Hypothetical) != 2 || len(doc.Change.Files) != 1 || doc.Change.Files[0].Path != "core/core.go" {
			t.Errorf("code=%d err=%q doc=%+v", code, errOut, doc)
		}
	})
	t.Run("go-run and pytest-file", func(t *testing.T) {
		for _, format := range []string{"go-run", "pytest-file"} {
			code, out, _ := invoke(ctx, "select", "--format", format)
			if code != 0 || out != "#full-suite no_lock\n" {
				t.Errorf("%s: code=%d out=%q", format, code, out)
			}
		}
	})
	t.Run("shadow", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "select", "--shadow")
		want := "full suite: shadow\nhypothetical " + e2eCore + "\nhypothetical " + e2eLeft + "\n"
		if code != 0 || out != want || !strings.Contains(errOut, "shadow") {
			t.Errorf("code=%d out=%q err=%q, want out %q", code, out, errOut, want)
		}
	})
	t.Run("out file", func(t *testing.T) {
		target := filepath.Join(dir, ".hazard", "sel.txt")
		code, out, errOut := invoke(ctx, "select", "--out", target)
		got, err := os.ReadFile(target)
		if code != 0 || out != "" || errOut != "" || err != nil || string(got) != wantText {
			t.Errorf("code=%d out=%q err=%q file=%q (%v)", code, out, errOut, got, err)
		}
	})
	t.Run("dry run payload", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "select", "--dry-run-payload")
		if code != 0 || out != `{"requests":[]}` || !strings.Contains(errOut, "nothing would be sent") {
			t.Errorf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
	t.Run("a confirmed recall override is accepted", func(t *testing.T) {
		if code, out, errOut := invoke(ctx, "select", "--recall", "0.9", "--i-know-what-im-doing"); code != 0 || out != wantText {
			t.Errorf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
	t.Run("an unresolvable base is a usage error", func(t *testing.T) {
		code, out, errOut := invoke(ctx, "select", "--base", "no-such-ref")
		if code != 2 || out != "" || !strings.Contains(errOut, "no-such-ref") {
			t.Errorf("code=%d out=%q err=%q", code, out, errOut)
		}
		code, out, errOut = invoke(ctx, "select", "--base", "no-such-ref", "--json")
		var env struct {
			Error string `json:"error"`
			Code  int    `json:"code"`
		}
		if err := json.Unmarshal([]byte(errOut), &env); err != nil || code != 2 || out != "" || env.Code != 2 || env.Error == "" {
			t.Errorf("JSON error envelope: code=%d out=%q err=%q (%v)", code, out, errOut, err)
		}
	})
	t.Run("an edit inside the change set after indexing keeps the index", func(t *testing.T) {
		path := filepath.Join(dir, "left", "left.go")
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(path, orig, 0o644); err != nil {
				t.Error(err)
			}
		})
		if err := os.WriteFile(path, append(orig, []byte("\n// edited after indexing\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := invoke(ctx, "select")
		if code != 0 || out != wantText || errOut != "" {
			t.Errorf("code=%d out=%q err=%q, want out %q", code, out, errOut, wantText)
		}
	})
	t.Run("a stale index runs the full suite", func(t *testing.T) {
		// left.go is edited and committed after indexing, so it is no longer in
		// the change set (HEAD against the work tree, which now holds only core)
		// and the index no longer describes it. This leaves a commit behind: it
		// is the last sub-test.
		path := filepath.Join(dir, "left", "left.go")
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(orig, []byte("\n// committed after indexing\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, dir, "add", "left/left.go")
		gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "edit left")
		code, out, errOut := invoke(ctx, "select")
		if code != 0 || out != "full suite: index_stale\n" || !strings.Contains(errOut, "left/left.go") {
			t.Errorf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
}
