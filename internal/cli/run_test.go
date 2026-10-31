package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/runner"
	"github.com/omorsi45/hazard/internal/store"
	"github.com/omorsi45/hazard/internal/telemetry"
)

// runRunCmd runs the run command built around fakes inside a root command with
// the persistent flags of the real one, mapping errors to exit codes with the
// same function Execute uses.
func runRunCmd(ctx context.Context, sel selectFunc, rn runners, args ...string) (int, string, string) {
	root := &cobra.Command{Use: "hazard", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().CountP("verbose", "v", "")
	root.AddCommand(newRunCommand(sel, rn))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"run"}, args...))
	err := root.ExecuteContext(ctx)
	code := 0
	if err != nil {
		var silent bool
		code, silent = exitCodeOf(err, false)
		if !silent {
			errOut.WriteString("error: " + err.Error() + "\n")
		}
	}
	return code, out.String(), errOut.String()
}

// runStep is one scripted answer of a fake runner.
type runStep struct {
	res runner.Result
	err error
}

// fakeRun is a runner adapter that records its requests, answers from steps
// (the last one repeats) and, like the real adapters, writes a JUnit report.
type fakeRun struct {
	name  string
	log   *[]string
	reqs  []runner.Request
	steps []runStep
	junit string // written to the request's JUnitPath on success; empty writes nothing
}

func (f *fakeRun) fn(_ context.Context, req runner.Request) (runner.Result, error) {
	*f.log = append(*f.log, f.name)
	f.reqs = append(f.reqs, req)
	step := runStep{}
	if len(f.steps) > 0 {
		step = f.steps[min(len(f.reqs)-1, len(f.steps)-1)]
	}
	if step.err == nil && f.junit != "" && req.JUnitPath != "" {
		if err := os.MkdirAll(filepath.Dir(req.JUnitPath), 0o755); err != nil {
			return runner.Result{ExitCode: 70}, err
		}
		if err := os.WriteFile(req.JUnitPath, []byte(f.junit), 0o644); err != nil {
			return runner.Result{ExitCode: 70}, err
		}
	}
	return step.res, step.err
}

func exits(codes ...int) []runStep {
	steps := make([]runStep, len(codes))
	for i, c := range codes {
		steps[i] = runStep{res: runner.Result{ExitCode: c}}
	}
	return steps
}

func junitXML(suite string, cases ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<testsuites>\n  <testsuite name=%q tests=\"%d\">\n", suite, len(cases))
	for _, c := range cases {
		b.WriteString("    " + c + "\n")
	}
	b.WriteString("  </testsuite>\n</testsuites>\n")
	return b.String()
}

// rig is a run command wired to a fake selector and fake runners inside a git
// repository that holds one Python file (pythonMarker), so both adapters have
// something to run. A test of a Go-only repository removes it.
type rig struct {
	t        *testing.T
	root     string
	sel      *fakeSelect
	gor, pyr *fakeRun
	log      []string
}

// pythonMarker is the Python file every rig repository starts with.
const pythonMarker = "app.py"

func addPythonMarker(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, pythonMarker), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRig(t *testing.T, d engine.Decision) *rig {
	t.Helper()
	r := &rig{t: t, root: chdirRepo(t), sel: &fakeSelect{d: d}}
	addPythonMarker(t, r.root)
	r.gor = &fakeRun{name: "go", log: &r.log}
	r.pyr = &fakeRun{name: "python", log: &r.log}
	return r
}

func (r *rig) run(args ...string) (int, string, string) {
	r.t.Helper()
	return runRunCmd(context.Background(), r.sel.fn, runners{Go: r.gor.fn, Python: r.pyr.fn}, args...)
}

func (r *rig) config(yaml string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.root, "hazard.yml"), []byte("version: 1\n"+yaml), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) records() []telemetry.Record {
	r.t.Helper()
	recs, err := telemetry.Read(filepath.Join(r.root, ".hazard", "runs.jsonl"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.t.Fatal(err)
	}
	return recs
}

func (r *rig) onlyRecord() telemetry.Record {
	r.t.Helper()
	recs := r.records()
	if len(recs) != 1 {
		r.t.Fatalf("want exactly one telemetry record, got %d: %+v", len(recs), recs)
	}
	return recs[0]
}

func wantLog(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runner calls = %v, want %v", got, want)
	}
}

func TestRunNarrowedBuildsTargetsFromTheDecision(t *testing.T) {
	r := newRig(t, narrowedDecision())
	code, _, errOut := r.run()
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	wantLog(t, r.log, "go", "python")
	g, p := r.gor.reqs[0], r.pyr.reqs[0]
	if g.Full || p.Full {
		t.Errorf("a narrowed decision must not request the full suite: go=%v python=%v", g.Full, p.Full)
	}
	wantGo := []runner.Target{
		{ID: goA, Language: "go", Package: "example.com/m/a", Name: "TestA"},
		{ID: goA2, Language: "go", Package: "example.com/m/a", Name: "TestA.2"},
		{ID: goB, Language: "go", Package: "example.com/m/b", Name: "TestB"},
	}
	if !reflect.DeepEqual(g.Targets, wantGo) {
		t.Errorf("go targets = %+v, want %+v", g.Targets, wantGo)
	}
	wantPy := []runner.Target{{ID: pyK, Language: "python", Package: "tests.test_x.TestK", RunnerID: "tests/test_x.py::TestK::test_a[1]"}}
	if !reflect.DeepEqual(p.Targets, wantPy) {
		t.Errorf("python targets = %+v, want %+v", p.Targets, wantPy)
	}
	if !sameDir(t, g.RepoDir, r.root) || !sameDir(t, p.RepoDir, r.root) {
		t.Errorf("runners must run at the repository root %q: go=%q python=%q", r.root, g.RepoDir, p.RepoDir)
	}
}

func TestRunNarrowedRunsOnlyTheLanguagesWithTargets(t *testing.T) {
	all := narrowedDecision()
	goOnly, pyOnly := all, all
	goOnly.Tests = all.Tests[:3]
	pyOnly.Tests = all.Tests[3:]
	for name, tc := range map[string]struct {
		d    engine.Decision
		want []string
	}{
		"only Go tests selected":     {goOnly, []string{"go"}},
		"only Python tests selected": {pyOnly, []string{"python"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, tc.d)
			if code, _, errOut := r.run(); code != 0 {
				t.Fatalf("code=%d err=%q", code, errOut)
			}
			wantLog(t, r.log, tc.want...)
		})
	}
	t.Run("--runner restricts a mixed selection to one adapter", func(t *testing.T) {
		r := newRig(t, all)
		if code, _, errOut := r.run("--runner", "gotest"); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "go")
		if len(r.gor.reqs[0].Targets) != 3 {
			t.Errorf("go targets = %+v", r.gor.reqs[0].Targets)
		}
	})
}

func TestRunFullSuiteDecisionRunsTheNativeFullSuites(t *testing.T) {
	r := newRig(t, fullDecision())
	r.config("languages:\n  go:\n    tags: [integration]\n    packages: [\"./a/...\", \"./b/...\"]\n  python:\n    testpaths: [spec]\n    pytest_args: [\"-x\"]\n")
	code, _, errOut := r.run()
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	wantLog(t, r.log, "go", "python")
	g, p := r.gor.reqs[0], r.pyr.reqs[0]
	if !g.Full || len(g.Targets) != 0 || !reflect.DeepEqual(g.GoPackages, []string{"./a/...", "./b/..."}) || !reflect.DeepEqual(g.GoTags, []string{"integration"}) {
		t.Errorf("go request = %+v", g)
	}
	if !p.Full || len(p.Targets) != 0 || !reflect.DeepEqual(p.Testpaths, []string{"spec"}) || !reflect.DeepEqual(p.PytestArgs, []string{"-x"}) {
		t.Errorf("python request = %+v", p)
	}
}

// goOnly makes the rig repository a Go-only one: no Python file is left in it.
func (r *rig) goOnly() {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.root, pythonMarker)); err != nil {
		r.t.Fatal(err)
	}
}

// TestRunFullSuiteOfAGoOnlyRepositoryLeavesPytestOut: with the default
// configuration Python is enabled with testpaths "tests", but a repository
// that holds no Python file has nothing for pytest to run. The full suite runs
// Go alone and says why. Any Python file keeps pytest in: a misconfigured
// Python project stays red.
func TestRunFullSuiteOfAGoOnlyRepositoryLeavesPytestOut(t *testing.T) {
	t.Run("no Python file", func(t *testing.T) {
		r := newRig(t, fullDecision())
		r.goOnly()
		r.config("")

		code, _, errOut := r.run("--full")

		if code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "go")
		if !strings.Contains(errOut, "warning: python_absent") {
			t.Errorf("stderr = %q, want the python_absent warning", errOut)
		}
	})
	t.Run("a full-suite decision from the selector is the same", func(t *testing.T) {
		r := newRig(t, fullDecision())
		r.goOnly()

		code, _, errOut := r.run()

		if code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "go")
	})
	t.Run("one Python file keeps pytest", func(t *testing.T) {
		r := newRig(t, fullDecision())
		r.goOnly()
		if err := os.WriteFile(filepath.Join(r.root, "tool.py"), []byte("print(2)\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		code, _, errOut := r.run("--full")

		if code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "go", "python")
		if strings.Contains(errOut, "python_absent") {
			t.Errorf("stderr = %q, want no python_absent warning", errOut)
		}
	})
	t.Run("Python files that are ignored or under .hazard do not count", func(t *testing.T) {
		r := newRig(t, fullDecision())
		r.goOnly()
		for name, content := range map[string]string{".gitignore": "ignored.py\n", "ignored.py": "x = 1\n", ".hazard/state.py": "x = 2\n"} {
			full := filepath.Join(r.root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		if code, _, errOut := r.run("--full"); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "go")
	})
	t.Run("--runner pytest always runs pytest", func(t *testing.T) {
		r := newRig(t, fullDecision())
		r.goOnly()

		if code, _, errOut := r.run("--full", "--runner", "pytest"); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "python")
	})
	t.Run("pytest is not left out when it is the only adapter", func(t *testing.T) {
		r := newRig(t, fullDecision())
		r.goOnly()
		r.config("languages:\n  go:\n    enabled: false\n")

		if code, _, errOut := r.run("--full"); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "python")
	})
}

func TestRunRunnerFlagAndEnabledLanguages(t *testing.T) {
	for name, tc := range map[string]struct {
		args   []string
		config string
		want   []string
		code   int
	}{
		"auto runs go then python":          {nil, "", []string{"go", "python"}, 0},
		"gotest runs only go":               {[]string{"--runner", "gotest"}, "", []string{"go"}, 0},
		"pytest runs only python":           {[]string{"--runner", "pytest"}, "", []string{"python"}, 0},
		"auto skips a disabled python":      {nil, "languages:\n  python:\n    enabled: false\n", []string{"go"}, 0},
		"auto skips a disabled go":          {nil, "languages:\n  go:\n    enabled: false\n", []string{"python"}, 0},
		"gotest with go disabled is misuse": {[]string{"--runner", "gotest"}, "languages:\n  go:\n    enabled: false\n", nil, 2},
		"an unknown runner is misuse":       {[]string{"--runner", "bogus"}, "", nil, 2},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, fullDecision())
			if tc.config != "" {
				r.config(tc.config)
			}
			code, _, errOut := r.run(tc.args...)
			if code != tc.code {
				t.Fatalf("code=%d err=%q, want %d", code, errOut, tc.code)
			}
			wantLog(t, r.log, tc.want...)
			if tc.code == 2 && r.sel.calls != 0 {
				t.Errorf("a usage error must be reported before selection: %d Select calls", r.sel.calls)
			}
		})
	}
}

func TestRunExitCodePassthrough(t *testing.T) {
	for _, tc := range []struct{ goExit, pyExit, want int }{
		{0, 0, 0}, {1, 0, 1}, {0, 1, 1}, {2, 0, 2}, {0, 7, 7}, {1, 7, 1}, {7, 1, 7}, {3, 3, 3}, {137, 0, 137},
	} {
		t.Run(fmt.Sprintf("go=%d_python=%d", tc.goExit, tc.pyExit), func(t *testing.T) {
			r := newRig(t, narrowedDecision())
			r.gor.steps, r.pyr.steps = exits(tc.goExit), exits(tc.pyExit)
			code, _, errOut := r.run()
			if code != tc.want {
				t.Errorf("code=%d, want %d (the first nonzero runner exit, unchanged); err=%q", code, tc.want, errOut)
			}
			wantLog(t, r.log, "go", "python") // a red Go run does not skip Python
			if rec := r.onlyRecord(); rec.RunnerExit == nil || *rec.RunnerExit != tc.want {
				t.Errorf("telemetry runner_exit = %v, want %d", rec.RunnerExit, tc.want)
			}
		})
	}
}

// TestRunNeverTurnsAFailingTestGreen is guard G11 at command level: a failing
// selected test yields exit 1, never 0.
func TestRunNeverTurnsAFailingTestGreen(t *testing.T) {
	r := newRig(t, narrowedDecision())
	r.gor.steps = exits(1)
	code, _, errOut := r.run()
	if code != 1 {
		t.Fatalf("a failing selected test must exit 1, got %d; err=%q", code, errOut)
	}
	if strings.Contains(errOut, "error:") {
		t.Errorf("a red suite is not a hazard error; stderr = %q", errOut)
	}
}

func TestRunPassthroughIsValidatedBeforeAnythingRuns(t *testing.T) {
	for name, tc := range map[string][]string{
		"go -run narrows":       {"--runner", "gotest", "--", "-run", "TestX"},
		"go -list narrows":      {"--runner", "gotest", "--", "-list", "."},
		"pytest -k narrows":     {"--runner", "pytest", "--", "-k", "expr"},
		"auto validates both":   {"--", "-k", "expr"},
		"auto validates go too": {"--", "-run", "X"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, narrowedDecision())
			code, _, errOut := r.run(tc...)
			if code != 2 || !strings.Contains(errOut, "narrow") {
				t.Errorf("code=%d err=%q, want a usage error naming the narrowing", code, errOut)
			}
			if r.sel.calls != 0 || len(r.log) != 0 {
				t.Errorf("nothing may run: %d Select calls, runner calls %v", r.sel.calls, r.log)
			}
		})
	}
}

func TestRunPassthroughIsForwardedToTheRunners(t *testing.T) {
	r := newRig(t, fullDecision())
	code, _, errOut := r.run("--runner", "gotest", "--", "-race", "-count=1")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if got := r.gor.reqs[0].Passthrough; !reflect.DeepEqual(got, []string{"-race", "-count=1"}) {
		t.Errorf("passthrough = %v", got)
	}
	r = newRig(t, fullDecision())
	r.config("languages:\n  python:\n    enabled: false\n")
	if code, _, errOut = r.run("--", "-k=x"); code != 0 {
		t.Errorf("a language that will not run is not validated: code=%d err=%q", code, errOut)
	}
}

func TestRunRejectsMisuse(t *testing.T) {
	for name, args := range map[string][]string{
		"a positional argument without --": {"./core"},
		"a positional before --":           {"./core", "--", "-race"},
		"--format":                         {"--format", "go-run"},
		"--out":                            {"--out", "sel.txt"},
		"--dry-run-payload":                {"--dry-run-payload"},
		"a recall override unconfirmed":    {"--recall", "0.9"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, narrowedDecision())
			code, _, errOut := r.run(args...)
			if code != 2 {
				t.Errorf("code=%d err=%q, want 2", code, errOut)
			}
			if r.sel.calls != 0 || len(r.log) != 0 {
				t.Errorf("nothing may run: %d Select calls, runner calls %v", r.sel.calls, r.log)
			}
		})
	}
}

func TestRunForwardsTheSelectionFlags(t *testing.T) {
	r := newRig(t, narrowedDecision())
	head := commitEmpty(t, r.root) // an explicit --head must be the checked-out commit
	code, _, errOut := r.run("--base", "main", "--head", head, "--max-candidates", "7", "--budget-ms", "99", "--lock", "x.lock")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	got := r.sel.got
	if got.Base != "main" || got.Head != head || got.MaxCandidates != 7 || got.BudgetMS != 99 || got.Shadow || !sameDir(t, got.RepoDir, r.root) {
		t.Errorf("Select options = %+v", got)
	}
	if !strings.HasSuffix(filepath.ToSlash(got.LockPath), "/x.lock") {
		t.Errorf("lock path = %q", got.LockPath)
	}
}

// commitFile commits one file with the given content and returns the commit.
func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", name)
	gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", content)
	return gitTest(t, dir, "rev-parse", "HEAD")
}

// TestRunRefusesAnExplicitHeadThatIsNotTheCheckout: the runners execute the
// checked-out tree, so a head that names another commit would select for one
// revision and test another: a red commit could exit 0. Such a head is a usage
// error before anything runs. The default head (the working tree) is unchanged.
func TestRunRefusesAnExplicitHeadThatIsNotTheCheckout(t *testing.T) {
	// c0 is checked out; c1 is its child, not checked out.
	setup := func(t *testing.T) (r *rig, c0, c1 string) {
		t.Helper()
		r = newRig(t, narrowedDecision())
		c0 = commitFile(t, r.root, "a.txt", "one\n")
		c1 = commitFile(t, r.root, "a.txt", "two\n")
		gitTest(t, r.root, "checkout", "-q", c0)
		return r, c0, c1
	}
	refused := func(t *testing.T, r *rig, args ...string) {
		t.Helper()
		code, _, errOut := r.run(args...)
		if code != 2 {
			t.Errorf("code=%d err=%q, want 2", code, errOut)
		}
		if !strings.Contains(errOut, "checked-out") {
			t.Errorf("stderr = %q, want it to say that hazard run executes the checked-out tree", errOut)
		}
		if r.sel.calls != 0 || len(r.log) != 0 {
			t.Errorf("nothing may run: %d Select calls, runner calls %v", r.sel.calls, r.log)
		}
	}

	t.Run("a commit that is not checked out", func(t *testing.T) {
		r, c0, c1 := setup(t)
		refused(t, r, "--base", c0, "--head", c1)
	})
	t.Run("a branch name that is not checked out", func(t *testing.T) {
		r, c0, c1 := setup(t)
		gitTest(t, r.root, "branch", "other", c1)
		refused(t, r, "--base", c0, "--head", "other")
	})
	t.Run("a ref that is not a commit", func(t *testing.T) {
		r, _, _ := setup(t)
		refused(t, r, "--head", "no-such-ref")
	})
	t.Run("the checked-out commit with a tracked modification", func(t *testing.T) {
		r, c0, _ := setup(t)
		if err := os.WriteFile(filepath.Join(r.root, "a.txt"), []byte("edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		refused(t, r, "--head", c0)
	})
	t.Run("a staged modification counts too", func(t *testing.T) {
		r, c0, _ := setup(t)
		if err := os.WriteFile(filepath.Join(r.root, "a.txt"), []byte("staged\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitTest(t, r.root, "add", "a.txt")
		refused(t, r, "--head", c0)
	})
	for name, ref := range map[string]string{"by SHA": "SHA", "by HEAD": "HEAD"} {
		t.Run("the checked-out commit "+name, func(t *testing.T) {
			r, c0, _ := setup(t)
			if ref == "SHA" {
				ref = c0
			}
			if err := os.WriteFile(filepath.Join(r.root, "untracked.txt"), []byte("x\n"), 0o644); err != nil { // untracked files do not count
				t.Fatal(err)
			}
			code, _, errOut := r.run("--head", ref)
			if code != 0 || r.sel.calls != 1 || r.sel.got.Head != ref {
				t.Errorf("code=%d err=%q Select calls %d head %q, want a normal run with the head forwarded", code, errOut, r.sel.calls, r.sel.got.Head)
			}
		})
	}
	t.Run("no --head is the working tree, modified or not", func(t *testing.T) {
		r, _, _ := setup(t)
		if err := os.WriteFile(filepath.Join(r.root, "a.txt"), []byte("edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := r.run(); code != 0 || r.sel.calls != 1 {
			t.Errorf("code=%d err=%q Select calls %d, want a normal run", code, errOut, r.sel.calls)
		}
	})
}

func TestRunExplainPrintsTheSelectionToStderr(t *testing.T) {
	r := newRig(t, narrowedDecision())
	code, out, errOut := r.run("--explain")
	if code != 0 || !strings.Contains(errOut, "selected 4 of 5 tests") || !strings.Contains(errOut, goA+"\tcertain\tdirect") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
	if strings.Contains(out, "selected 4 of 5") {
		t.Errorf("stdout belongs to the test runners: %q", out)
	}
}

func TestRunSelectionErrors(t *testing.T) {
	t.Run("an invalid ref is a usage error", func(t *testing.T) {
		r := newRig(t, engine.Decision{})
		r.sel.err = fmt.Errorf("%w: nope", engine.ErrInvalidRef)
		code, _, errOut := r.run()
		if code != 2 || len(r.log) != 0 {
			t.Errorf("code=%d runs=%v err=%q", code, r.log, errOut)
		}
	})
	t.Run("an internal error falls back to the full suite", func(t *testing.T) {
		r := newRig(t, engine.Decision{})
		r.sel.err = errors.New("graph exploded")
		r.gor.steps, r.pyr.steps = exits(1), exits(0)
		code, _, errOut := r.run()
		if code != 1 {
			t.Errorf("code=%d, want the full suite's exit code 1; err=%q", code, errOut)
		}
		wantLog(t, r.log, "go", "python")
		if !r.gor.reqs[0].Full || !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "graph exploded") {
			t.Errorf("want a full-suite fallback with a warning; go=%+v stderr=%q", r.gor.reqs[0], errOut)
		}
		if rec := r.onlyRecord(); !rec.FullSuite || rec.AbstainReason != "internal_error" || rec.RunnerExit == nil || *rec.RunnerExit != 1 {
			t.Errorf("record = %+v", rec)
		}
	})
	t.Run("--no-fallback exits 70 without running", func(t *testing.T) {
		r := newRig(t, engine.Decision{})
		r.sel.err = errors.New("graph exploded")
		code, _, errOut := r.run("--no-fallback")
		if code != 70 || len(r.log) != 0 || !strings.Contains(errOut, "graph exploded") {
			t.Errorf("code=%d runs=%v err=%q", code, r.log, errOut)
		}
	})
	t.Run("a canceled selection does not fall back", func(t *testing.T) {
		r := newRig(t, engine.Decision{})
		r.sel.err = context.Canceled
		code, _, errOut := r.run()
		if code != 70 || len(r.log) != 0 || !strings.Contains(errOut, "canceled") {
			t.Errorf("code=%d runs=%v err=%q", code, r.log, errOut)
		}
	})
}

func TestRunNoFallbackWithALockProblemExits3WithoutRunning(t *testing.T) {
	for _, reason := range []string{"no_lock", "lock_corrupt", "lock_error", "model_drift", "rubric_mismatch", "feature_mismatch", "policy_mismatch", "predictor_unavailable", "predictor_error"} {
		t.Run(reason, func(t *testing.T) {
			d := fullDecision()
			d.AbstainReason = reason
			r := newRig(t, d)
			code, _, errOut := r.run("--no-fallback")
			if code != 3 || len(r.log) != 0 || !strings.Contains(errOut, reason) {
				t.Errorf("code=%d runs=%v err=%q, want 3, nothing run, the reason named", code, r.log, errOut)
			}
			// Without the flag the same decision is just the full suite.
			r = newRig(t, d)
			if code, _, errOut = r.run(); code != 0 {
				t.Errorf("without --no-fallback: code=%d err=%q", code, errOut)
			}
			wantLog(t, r.log, "go", "python")
		})
	}
	t.Run("another full-suite reason still runs", func(t *testing.T) {
		d := fullDecision()
		d.AbstainReason = "index_stale"
		r := newRig(t, d)
		if code, _, errOut := r.run("--no-fallback"); code != 0 {
			t.Errorf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "go", "python")
	})
}

func TestRunForcedFullSkipsSelection(t *testing.T) {
	root := chdirRepo(t)
	addPythonMarker(t, root)
	head := commitEmpty(t, root)
	sel := &fakeSelect{d: narrowedDecision()}
	var log []string
	gor := &fakeRun{name: "go", log: &log, steps: exits(0)}
	pyr := &fakeRun{name: "python", log: &log, steps: exits(0)}
	code, _, errOut := runRunCmd(context.Background(), sel.fn, runners{Go: gor.fn, Python: pyr.fn}, "--full", "--junit", "audit.xml")
	if code != 0 || sel.calls != 0 {
		t.Fatalf("code=%d Select calls=%d err=%q", code, sel.calls, errOut)
	}
	wantLog(t, log, "go", "python")
	if !gor.reqs[0].Full || !pyr.reqs[0].Full {
		t.Error("--full must run the native full suite")
	}
	recs, err := telemetry.Read(filepath.Join(root, ".hazard", "runs.jsonl"))
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v, err %v", recs, err)
	}
	rec := recs[0]
	if rec.Mode != "run" || !rec.FullSuite || rec.AbstainReason != "forced_full" || rec.Commit != head || rec.JUnit != "audit.xml" || rec.ID == "" {
		t.Errorf("record = %+v, want mode run, full_suite, forced_full, commit %s", rec, head)
	}
}

func TestRunShadowRunsTheFullSuiteAndRecordsTheHypotheticalSelection(t *testing.T) {
	d := fullDecision() // what the engine returns under --shadow
	d.AbstainReason = "shadow"
	r := newRig(t, d)
	code, _, errOut := r.run("--shadow")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !r.sel.got.Shadow {
		t.Error("--shadow must reach the selector")
	}
	if !r.gor.reqs[0].Full || !r.pyr.reqs[0].Full {
		t.Error("shadow runs the full suite")
	}
	rec := r.onlyRecord()
	if rec.Mode != "shadow" || !rec.FullSuite || rec.AbstainReason != "shadow" || len(rec.Members) != 2 || rec.Members[0].TestID != goA {
		t.Errorf("record = %+v, want mode shadow with the hypothetical members", rec)
	}
}

func TestRunShadowForcesAFullSuiteFromAnySelector(t *testing.T) {
	r := newRig(t, narrowedDecision())
	code, _, errOut := r.run("--shadow")
	if code != 0 || !r.gor.reqs[0].Full {
		t.Fatalf("code=%d err=%q go=%+v", code, errOut, r.gor.reqs)
	}
	if rec := r.onlyRecord(); rec.AbstainReason != "shadow" || rec.Mode != "shadow" {
		t.Errorf("record = %+v", rec)
	}
}

func TestRunPytestAbstentionRerunsTheFullPythonSuite(t *testing.T) {
	r := newRig(t, narrowedDecision())
	r.pyr.steps = []runStep{
		{res: runner.Result{ExitCode: 5, Abstained: true, Reason: "pytest_missing_ids", Missing: []string{"tests/test_x.py::TestK::test_a[1]"}}},
		{res: runner.Result{ExitCode: 0}},
	}
	code, _, errOut := r.run()
	if code != 0 {
		t.Fatalf("code=%d err=%q; the abstained attempt ran no test, its exit code is not the result", code, errOut)
	}
	if len(r.pyr.reqs) != 2 || r.pyr.reqs[0].Full || !r.pyr.reqs[1].Full || len(r.pyr.reqs[1].Targets) != 0 {
		t.Fatalf("python requests = %+v, want a narrowed attempt then a full rerun", r.pyr.reqs)
	}
	if !strings.Contains(errOut, "warning:") {
		t.Errorf("stderr = %q, want a warning", errOut)
	}
	if rec := r.onlyRecord(); !rec.FullSuite || rec.AbstainReason != "runner_limit" {
		t.Errorf("record = %+v, want full_suite with runner_limit", rec)
	}

	r = newRig(t, narrowedDecision())
	r.pyr.steps = []runStep{{res: runner.Result{ExitCode: 0, Abstained: true}}, {res: runner.Result{ExitCode: 1}}}
	if code, _, _ = r.run(); code != 1 {
		t.Errorf("a failing rerun must exit 1, got %d", code)
	}
	r = newRig(t, narrowedDecision())
	r.pyr.steps = []runStep{{res: runner.Result{ExitCode: 0, Abstained: true}}, {res: runner.Result{ExitCode: 0}}}
	if code, _, _ = r.run("--no-fallback"); code != 0 {
		t.Errorf("an abstention is not an internal error, --no-fallback still reruns; got %d", code)
	}
}

// TestRunGoAbstentionRerunsTheFullGoSuite: a narrowed Go run whose selected
// tests did not all start is not a result. Like a pytest abstention it reruns
// the full Go suite once and the telemetry records runner_limit.
func TestRunGoAbstentionRerunsTheFullGoSuite(t *testing.T) {
	r := newRig(t, narrowedDecision())
	r.gor.steps = []runStep{
		{res: runner.Result{ExitCode: 0, Abstained: true, Reason: "go_missing_tests", Missing: []string{goA}}},
		{res: runner.Result{ExitCode: 0}},
	}
	code, _, errOut := r.run()
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if len(r.gor.reqs) != 2 || r.gor.reqs[0].Full || len(r.gor.reqs[0].Targets) == 0 || !r.gor.reqs[1].Full || len(r.gor.reqs[1].Targets) != 0 {
		t.Fatalf("go requests = %+v, want a narrowed attempt then a full rerun", r.gor.reqs)
	}
	if !strings.Contains(errOut, "go_missing_tests") {
		t.Errorf("stderr = %q, want the reason named", errOut)
	}
	if rec := r.onlyRecord(); !rec.FullSuite || rec.AbstainReason != "runner_limit" {
		t.Errorf("record = %+v, want full_suite with runner_limit", rec)
	}

	r = newRig(t, narrowedDecision())
	r.gor.steps = []runStep{{res: runner.Result{ExitCode: 0, Abstained: true, Reason: "go_missing_tests"}}, {res: runner.Result{ExitCode: 1}}}
	if code, _, _ = r.run(); code != 1 {
		t.Errorf("a failing rerun must exit 1, got %d", code)
	}
}

func TestRunRunnerInternalErrors(t *testing.T) {
	boom := errors.New("boom")
	t.Run("a narrowed run falls back to that language's full suite", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		r.gor.steps = []runStep{{res: runner.Result{ExitCode: 70}, err: boom}, {res: runner.Result{ExitCode: 0}}}
		code, _, errOut := r.run()
		if code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if len(r.gor.reqs) != 2 || r.gor.reqs[0].Full || !r.gor.reqs[1].Full {
			t.Errorf("go requests = %+v, want narrowed then full", r.gor.reqs)
		}
		wantLog(t, r.log, "go", "go", "python")
		if !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "boom") {
			t.Errorf("stderr = %q", errOut)
		}
		if rec := r.onlyRecord(); !rec.FullSuite || rec.AbstainReason != "internal_error" {
			t.Errorf("record = %+v", rec)
		}
	})
	t.Run("a failing fallback exits with its own code", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		r.gor.steps = []runStep{{res: runner.Result{ExitCode: 70}, err: boom}, {res: runner.Result{ExitCode: 1}}}
		if code, _, _ := r.run(); code != 1 {
			t.Errorf("code=%d, want 1", code)
		}
	})
	t.Run("an error in a full run is final", func(t *testing.T) {
		r := newRig(t, fullDecision())
		r.gor.steps = []runStep{{res: runner.Result{ExitCode: 70}, err: boom}}
		code, _, errOut := r.run()
		if code != 70 {
			t.Errorf("code=%d, want 70; err=%q", code, errOut)
		}
		if len(r.gor.reqs) != 1 {
			t.Errorf("a full run that failed internally is not retried: %d go calls", len(r.gor.reqs))
		}
		wantLog(t, r.log, "go", "python")
	})
	t.Run("--no-fallback exits 70 at once", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		r.gor.steps = []runStep{{res: runner.Result{ExitCode: 70}, err: boom}}
		code, _, errOut := r.run("--no-fallback")
		if code != 70 || !strings.Contains(errOut, "boom") {
			t.Errorf("code=%d err=%q", code, errOut)
		}
		wantLog(t, r.log, "go")
	})
	t.Run("a canceled run does not fall back", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		goCalls := 0
		canceling := func(context.Context, runner.Request) (runner.Result, error) {
			goCalls++
			cancel()
			return runner.Result{ExitCode: 70}, fmt.Errorf("go: %w", context.Canceled)
		}
		code, _, errOut := runRunCmd(ctx, r.sel.fn, runners{Go: canceling, Python: r.pyr.fn})
		if code != 70 || !strings.Contains(errOut, "canceled") {
			t.Errorf("code=%d err=%q, want 70 canceled", code, errOut)
		}
		if goCalls != 1 || len(r.pyr.reqs) != 0 {
			t.Errorf("a canceled run stops at once: go calls=%d python calls=%d", goCalls, len(r.pyr.reqs))
		}
	})
}

func TestRunWidensWhenASelectedTestCannotBeExpressed(t *testing.T) {
	for name, edit := range map[string]func(*engine.Decision){
		"a Go test without a runner ID": func(d *engine.Decision) { d.Tests[0].RunnerID = "" },
		"a Go test without a package":   func(d *engine.Decision) { d.Tests[0].Package = "" },
		"a Python test without a node":  func(d *engine.Decision) { d.Tests[3].RunnerID = "" },
		"an unknown language": func(d *engine.Decision) {
			d.Tests = append(d.Tests, engine.Selected{TestID: "zz:x:y", Source: "certain"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := narrowedDecision()
			edit(&d)
			r := newRig(t, d)
			code, _, errOut := r.run()
			if code != 0 {
				t.Fatalf("code=%d err=%q", code, errOut)
			}
			if !r.gor.reqs[0].Full || !r.pyr.reqs[0].Full {
				t.Errorf("an inexpressible selection must run the full suite: go=%+v python=%+v", r.gor.reqs[0], r.pyr.reqs[0])
			}
			if rec := r.onlyRecord(); !rec.FullSuite || rec.AbstainReason != "runner_limit" {
				t.Errorf("record = %+v", rec)
			}
		})
	}
	t.Run("a language that is disabled but selected", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		r.config("languages:\n  python:\n    enabled: false\n")
		if code, _, errOut := r.run(); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if !r.gor.reqs[0].Full {
			t.Error("selected tests with no enabled runner must widen to the full suite")
		}
	})
}

func TestRunWithNothingSelectedRunsNothingAndWritesAnEmptyReport(t *testing.T) {
	d := engine.Decision{ID: "empty", AllTests: []string{goA, pyK}}
	r := newRig(t, d)
	stale := filepath.Join(r.root, ".hazard", "junit.xml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(junitXML("old", `<testcase classname="c" name="n"><failure message="old"/></testcase>`)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := r.run()
	if code != 0 || len(r.log) != 0 || !strings.Contains(errOut, "no tests selected") {
		t.Fatalf("code=%d runs=%v err=%q", code, r.log, errOut)
	}
	cases := parseJUnitFile(t, stale)
	if len(cases) != 0 {
		t.Errorf("a stale report must be replaced by an empty one, got %+v", cases)
	}
}

func parseJUnitFile(t *testing.T, path string) []runner.JUnitCase {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	cases, err := runner.ParseJUnit(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return cases
}

func TestRunJUnitPaths(t *testing.T) {
	t.Run("the default is .hazard/junit.xml at the repository root", func(t *testing.T) {
		d := narrowedDecision()
		d.Tests = d.Tests[:3]
		r := newRig(t, d)
		if code, _, errOut := r.run(); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		got := r.gor.reqs[0].JUnitPath
		if filepath.Base(got) != "junit.xml" || !sameDir(t, filepath.Dir(got), filepath.Join(r.root, ".hazard")) {
			t.Errorf("JUnitPath = %q", got)
		}
	})
	t.Run("an explicit path is relative to the working directory", func(t *testing.T) {
		d := narrowedDecision()
		d.Tests = d.Tests[:3]
		r := newRig(t, d)
		if code, _, errOut := r.run("--junit", filepath.Join("out", "r.xml")); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		got := r.gor.reqs[0].JUnitPath
		if !filepath.IsAbs(got) || filepath.Base(got) != "r.xml" || !sameDirOrMissing(t, filepath.Dir(got), filepath.Join(r.root, "out")) {
			t.Errorf("JUnitPath = %q", got)
		}
	})
}

// sameDirOrMissing compares two directory paths that may not exist yet by
// comparing their existing parents and last elements.
func sameDirOrMissing(t *testing.T, a, b string) bool {
	t.Helper()
	return filepath.Base(a) == filepath.Base(b) && sameDir(t, filepath.Dir(a), filepath.Dir(b))
}

func TestRunMergesTheGoAndPythonReports(t *testing.T) {
	r := newRig(t, narrowedDecision())
	r.gor.junit = junitXML("example.com/m/a", `<testcase classname="example.com/m/a" name="TestA"><failure message="FAIL"/></testcase>`)
	r.pyr.junit = junitXML("pytest", `<testcase classname="tests.test_x.TestK" name="test_a[1]"/>`)
	r.gor.steps, r.pyr.steps = exits(1), exits(0)
	stale := filepath.Join(r.root, ".hazard", "junit.xml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(junitXML("old", `<testcase classname="c" name="old"/>`)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := r.run()
	if code != 1 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	gp, pp := r.gor.reqs[0].JUnitPath, r.pyr.reqs[0].JUnitPath
	if gp == pp || filepath.Base(gp) == "junit.xml" || filepath.Base(pp) == "junit.xml" {
		t.Errorf("with two runners each must write its own part: go=%q python=%q", gp, pp)
	}
	cases := parseJUnitFile(t, stale)
	var names []string
	for _, c := range cases {
		names = append(names, c.Name+":"+c.Status)
	}
	want := []string{"TestA:fail", "test_a[1]:pass"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("merged report cases = %v, want %v (and the stale case gone)", names, want)
	}
	for _, part := range []string{gp, pp} {
		if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("part %s must be removed after the merge: %v", part, err)
		}
	}
}

func TestRunDoesNotPublishAPartialMergedReport(t *testing.T) {
	r := newRig(t, narrowedDecision())
	r.gor.junit = junitXML("example.com/m/a", `<testcase classname="example.com/m/a" name="TestA"/>`)
	// The Python adapter writes no report (it failed internally).
	r.pyr.steps = []runStep{{res: runner.Result{ExitCode: 70}, err: errors.New("pytest vanished")}, {res: runner.Result{ExitCode: 70}, err: errors.New("pytest vanished")}}
	stale := filepath.Join(r.root, ".hazard", "junit.xml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(junitXML("old", `<testcase classname="c" name="old"/>`)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := r.run()
	if code != 70 {
		t.Errorf("code=%d err=%q, want 70", code, errOut)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a report missing one language must not be published, and a stale one must not survive: %v", err)
	}
}

func TestRunCanceledContext(t *testing.T) {
	r := newRig(t, narrowedDecision())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, _, errOut := runRunCmd(ctx, r.sel.fn, runners{Go: r.gor.fn, Python: r.pyr.fn})
	if code != 70 || !strings.Contains(errOut, "canceled") || r.sel.calls != 0 || len(r.log) != 0 {
		t.Errorf("code=%d err=%q Select calls=%d runs=%v", code, errOut, r.sel.calls, r.log)
	}
}

func TestRunAppendsATelemetryRecord(t *testing.T) {
	d := narrowedDecision()
	d.Change = &change.ChangeSet{BaseSHA: "base1", HeadSHA: "head1"}
	d.Lambda, d.LockDigest = 0.2, "sha256:lock"
	r := newRig(t, d)
	r.gor.steps = exits(1)
	if code, _, errOut := r.run("--junit", "results.xml"); code != 1 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	rec := r.onlyRecord()
	if rec.ID != "id1" || rec.Mode != "run" || rec.FullSuite || rec.Commit != "head1" || rec.JUnit != "results.xml" ||
		rec.RunnerExit == nil || *rec.RunnerExit != 1 || rec.HazardVersion != version || rec.Lambda != 0.2 || rec.LockDigest != "sha256:lock" ||
		rec.NTotal != 5 || len(rec.Members) != 4 {
		t.Errorf("record = %+v", rec)
	}
}

func TestRunTelemetryConfig(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		r.config("telemetry:\n  enabled: false\n  path: .hazard/runs.jsonl\n")
		if code, _, errOut := r.run(); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if got := r.records(); len(got) != 0 {
			t.Errorf("telemetry is disabled but %d records were written", len(got))
		}
	})
	t.Run("custom path", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		r.config("telemetry:\n  enabled: true\n  path: logs/t.jsonl\n")
		if code, _, errOut := r.run(); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		recs, err := telemetry.Read(filepath.Join(r.root, "logs", "t.jsonl"))
		if err != nil || len(recs) != 1 {
			t.Errorf("records = %d, err %v", len(recs), err)
		}
	})
	t.Run("a write failure is a warning and never changes the exit code", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		if err := os.MkdirAll(filepath.Join(r.root, ".hazard", "runs.jsonl"), 0o755); err != nil {
			t.Fatal(err)
		}
		r.gor.steps = exits(1)
		code, _, errOut := r.run()
		if code != 1 || !strings.Contains(errOut, "warning: telemetry") {
			t.Errorf("code=%d err=%q, want exit 1 and a telemetry warning", code, errOut)
		}
		r = newRig(t, narrowedDecision())
		if err := os.MkdirAll(filepath.Join(r.root, ".hazard", "runs.jsonl"), 0o755); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := r.run(); code != 0 {
			t.Errorf("a green run stays green when telemetry fails, got %d", code)
		}
	})
	t.Run("the database is mirrored when it exists", func(t *testing.T) {
		r := newRig(t, narrowedDecision())
		dbPath := filepath.Join(r.root, ".hazard", "hazard.db")
		s, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := r.run(); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		s, err = store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM selection_members WHERE selection_id = 'id1'`).Scan(&n); err != nil || n != 4 {
			t.Errorf("members in the database = %d, err %v", n, err)
		}
	})
}

// leakySentinel is text that must never reach a telemetry file.
const leakySentinel = "SENTINEL-leak-7c2e41"

func leakyDecision() engine.Decision {
	d := narrowedDecision()
	d.Payload = []byte(`{"requests":[{"hunk":"` + leakySentinel + `"}]}`)
	d.Warnings = []string{"warning " + leakySentinel}
	d.Tests[0].Reason = "reason " + leakySentinel
	d.Change = &change.ChangeSet{
		HeadSHA: "head1",
		Files: []change.ChangedFile{{
			Path: "core/core.go", Status: "M", Language: "go",
			Hunks: []change.Hunk{{OldStart: 1, NewStart: 1, Symbols: []string{"Add"}, Text: "+" + leakySentinel}},
		}},
	}
	d.L1 = &impact.Result{Candidates: []impact.Candidate{{TestID: goB, Purpose: "purpose " + leakySentinel, Asserts: []string{leakySentinel}}}}
	return d
}

func TestRunTelemetryNeverCarriesPayloadOrHunkText(t *testing.T) {
	r := newRig(t, leakyDecision())
	if code, _, errOut := r.run(); code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	data, err := os.ReadFile(filepath.Join(r.root, ".hazard", "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(leakySentinel)) {
		t.Fatalf("run telemetry leaked text that must stay local:\n%s", data)
	}
	if !bytes.Contains(data, []byte(goA)) {
		t.Fatalf("the record must still name its tests:\n%s", data)
	}
}

func TestSelectAppendsTelemetry(t *testing.T) {
	t.Run("a selection is recorded", func(t *testing.T) {
		root := chdirRepo(t)
		f := &fakeSelect{d: narrowedDecision()}
		code, out, errOut := runSelectCmd(context.Background(), recordSelection(f.fn))
		if code != 0 || !strings.Contains(out, "selected 4 of 5 tests") || errOut != "" {
			t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
		}
		recs, err := telemetry.Read(filepath.Join(root, ".hazard", "runs.jsonl"))
		if err != nil || len(recs) != 1 || recs[0].Mode != "select" || recs[0].RunnerExit != nil || len(recs[0].Members) != 4 {
			t.Fatalf("records = %+v, err %v", recs, err)
		}
	})
	t.Run("a shadow selection is recorded as shadow", func(t *testing.T) {
		root := chdirRepo(t)
		d := fullDecision()
		d.AbstainReason = "shadow"
		f := &fakeSelect{d: d}
		if code, _, errOut := runSelectCmd(context.Background(), recordSelection(f.fn), "--shadow"); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		recs, err := telemetry.Read(filepath.Join(root, ".hazard", "runs.jsonl"))
		if err != nil || len(recs) != 1 || recs[0].Mode != "shadow" || !recs[0].FullSuite {
			t.Fatalf("records = %+v, err %v", recs, err)
		}
	})
	t.Run("a payload dry run is not a selection", func(t *testing.T) {
		root := chdirRepo(t)
		d := narrowedDecision()
		d.Payload = []byte(`{"requests":[]}`)
		f := &fakeSelect{d: d}
		if code, _, errOut := runSelectCmd(context.Background(), recordSelection(f.fn), "--dry-run-payload"); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if _, err := os.Stat(filepath.Join(root, ".hazard", "runs.jsonl")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a dry run must not be recorded: %v", err)
		}
	})
	t.Run("a selection error is passed through unrecorded", func(t *testing.T) {
		root := chdirRepo(t)
		f := &fakeSelect{err: errors.New("boom")}
		if code, _, _ := runSelectCmd(context.Background(), recordSelection(f.fn)); code != 70 {
			t.Errorf("code=%d, want 70", code)
		}
		if _, err := os.Stat(filepath.Join(root, ".hazard", "runs.jsonl")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("no decision, no record: %v", err)
		}
	})
	t.Run("telemetry disabled by config", func(t *testing.T) {
		root := chdirRepo(t)
		if err := os.WriteFile(filepath.Join(root, "hazard.yml"), []byte("version: 1\ntelemetry:\n  enabled: false\n  path: .hazard/runs.jsonl\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f := &fakeSelect{d: narrowedDecision()}
		if code, _, errOut := runSelectCmd(context.Background(), recordSelection(f.fn)); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if _, err := os.Stat(filepath.Join(root, ".hazard", "runs.jsonl")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("telemetry is disabled: %v", err)
		}
	})
	t.Run("a write failure is a warning and never changes the exit code", func(t *testing.T) {
		root := chdirRepo(t)
		if err := os.MkdirAll(filepath.Join(root, ".hazard", "runs.jsonl"), 0o755); err != nil {
			t.Fatal(err)
		}
		f := &fakeSelect{d: narrowedDecision()}
		code, out, errOut := runSelectCmd(context.Background(), recordSelection(f.fn))
		if code != 0 || !strings.Contains(out, "selected 4 of 5 tests") || !strings.Contains(errOut, "warning: telemetry") {
			t.Errorf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
	t.Run("the selection output never carries the payload", func(t *testing.T) {
		root := chdirRepo(t)
		f := &fakeSelect{d: leakyDecision()}
		if code, _, errOut := runSelectCmd(context.Background(), recordSelection(f.fn)); code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		data, err := os.ReadFile(filepath.Join(root, ".hazard", "runs.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(leakySentinel)) {
			t.Fatalf("select telemetry leaked text that must stay local:\n%s", data)
		}
	})
}

// commitEmpty makes one empty commit in dir (signing disabled for this
// temporary repository only) and returns its SHA.
func commitEmpty(t *testing.T, dir string) string {
	t.Helper()
	gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-q", "-m", "init")
	return gitTest(t, dir, "rev-parse", "HEAD")
}

// copyFixtureTree copies a fixture repository tree.
func copyFixtureTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
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
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunEndToEndOnTheGoFixture drives the real binary entry point on a copy
// of the Go fixture: index it, run without a lock (so the full suite runs and
// the exit code and report are the native ones), then break a test.
func TestRunEndToEndOnTheGoFixture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	copyFixtureTree(t, filepath.Join("..", "..", "testdata", "repos", "go"), dir)
	for name, content := range map[string]string{
		"hazard.yml": "version: 1\nlanguages:\n  python:\n    enabled: false\n",
		".gitignore": ".hazard/\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "init", "-q", "-b", "main")
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	t.Chdir(dir)
	ctx := context.Background()

	if code, out, errOut := invoke(ctx, "index"); code != 0 {
		t.Fatalf("index: code=%d out=%q err=%q", code, out, errOut)
	}

	code, out, errOut := invoke(ctx, "run")
	if code != 0 {
		t.Fatalf("green run: code=%d out=%q err=%q", code, out, errOut)
	}
	cases := parseJUnitFile(t, filepath.Join(dir, ".hazard", "junit.xml"))
	status := map[string]string{}
	for _, c := range cases {
		status[c.Classname+"."+c.Name] = c.Status
	}
	if status["example.com/hazardfixture/core.TestFoo"] != "pass" || len(cases) < 8 {
		t.Errorf("JUnit cases = %d, statuses %v; want the whole fixture suite with TestFoo passing", len(cases), status)
	}
	recs, err := telemetry.Read(filepath.Join(dir, ".hazard", "runs.jsonl"))
	if err != nil || len(recs) != 1 {
		t.Fatalf("telemetry = %+v, err %v", recs, err)
	}
	if rec := recs[0]; rec.Mode != "run" || !rec.FullSuite || rec.AbstainReason != "no_lock" || rec.RunnerExit == nil || *rec.RunnerExit != 0 || rec.Commit == "" {
		t.Errorf("record = %+v, want a full-suite run recorded with no_lock and exit 0", rec)
	}

	failing := "package core\n\nimport \"testing\"\n\nfunc TestDeliberateFailure(t *testing.T) { t.Fatal(\"deliberate failure\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "core", "zz_fail_test.go"), []byte(failing), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = invoke(ctx, "run")
	if code != 1 {
		t.Fatalf("a failing test must exit 1 through the real entry point, got %d; out=%q err=%q", code, out, errOut)
	}
	if strings.Contains(errOut, "error:") {
		t.Errorf("a red suite is not a hazard error: %q", errOut)
	}
	status = map[string]string{}
	for _, c := range parseJUnitFile(t, filepath.Join(dir, ".hazard", "junit.xml")) {
		status[c.Name] = c.Status
	}
	if status["TestDeliberateFailure"] != "fail" || status["TestFoo"] != "pass" {
		t.Errorf("statuses = %v, want the broken test failing and the rest passing", status)
	}
	recs, err = telemetry.Read(filepath.Join(dir, ".hazard", "runs.jsonl"))
	if err != nil || len(recs) != 2 || recs[1].RunnerExit == nil || *recs[1].RunnerExit != 1 {
		t.Errorf("telemetry = %+v, err %v; want a second record with runner_exit 1", recs, err)
	}
}

// TestRunFullSuiteEndToEndOnAGoOnlyRepositoryWithTheDefaultConfiguration: the
// whole configuration is "version: 1", so Python is enabled with testpaths
// "tests" in a repository that has no Python at all. The full suite runs Go,
// leaves pytest out with a warning and exits 0.
func TestRunFullSuiteEndToEndOnAGoOnlyRepositoryWithTheDefaultConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	copyFixtureTree(t, filepath.Join("..", "..", "testdata", "repos", "go"), dir)
	for name, content := range map[string]string{"hazard.yml": "version: 1\n", ".gitignore": ".hazard/\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "init", "-q", "-b", "main")
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	if listed := gitTest(t, dir, "ls-files", "*.py"); listed != "" {
		t.Fatalf("fixture: the Go fixture holds Python files: %s", listed)
	}
	t.Chdir(dir)

	code, out, errOut := invoke(context.Background(), "run", "--full")

	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q, want a green Go-only run", code, out, errOut)
	}
	if !strings.Contains(errOut, "python_absent") || strings.Contains(errOut, "file or directory not found") {
		t.Errorf("stderr = %q, want the python_absent warning and no pytest failure", errOut)
	}
	if cases := parseJUnitFile(t, filepath.Join(dir, ".hazard", "junit.xml")); len(cases) < 8 {
		t.Errorf("JUnit cases = %d, want the whole Go fixture suite", len(cases))
	}
}
