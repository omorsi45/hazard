package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pytestPython returns the interpreter for tests that need pytest: from
// HAZARD_TEST_PYTHON, else python on PATH. When pytest is unavailable the test
// is skipped only if HAZARD_REQUIRE_PYTEST is not "1" (contract D054).
func pytestPython(t *testing.T) string {
	t.Helper()
	py := os.Getenv("HAZARD_TEST_PYTHON")
	if py == "" {
		py = "python"
	}
	out, err := exec.Command(py, "-m", "pytest", "--version").CombinedOutput()
	if err != nil {
		if os.Getenv("HAZARD_REQUIRE_PYTEST") == "1" {
			t.Fatalf("pytest is required but unavailable (%s): %v\n%s", py, err, out)
		}
		t.Skipf("pytest unavailable (%s): %v", py, err)
	}
	return py
}

func pyTarget(nodeID string) Target {
	return Target{ID: "py:" + nodeID, Language: "python", RunnerID: nodeID}
}

func pyTargets(nodeIDs ...string) []Target {
	targets := make([]Target, len(nodeIDs))
	for i, id := range nodeIDs {
		targets[i] = pyTarget(id)
	}
	return targets
}

func runPytestRequest(t *testing.T, req Request) (Result, []JUnitCase, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	req.Stdout, req.Stderr = &out, &errb
	if req.JUnitPath == "" {
		req.JUnitPath = "junit.xml"
	}
	res, err := RunPytest(context.Background(), req)
	path := req.JUnitPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(req.RepoDir, path)
	}
	var cases []JUnitCase
	if f, openErr := os.Open(path); openErr == nil {
		var parseErr error
		cases, parseErr = ParseJUnit(f)
		if closeErr := f.Close(); closeErr != nil {
			t.Errorf("close JUnit report: %v", closeErr)
		}
		if parseErr != nil {
			t.Fatalf("ParseJUnit: %v", parseErr)
		}
	}
	return res, cases, out.String() + errb.String(), err
}

// pyCaseKeys renders each case as "<classname>::<name>".
func pyCaseKeys(cases []JUnitCase) []string {
	keys := make([]string, len(cases))
	for i, c := range cases {
		keys[i] = c.Classname + "::" + c.Name
	}
	slices.Sort(keys)
	return keys
}

func pyFixture(t *testing.T) (dir, python string) {
	t.Helper()
	python = pytestPython(t)
	return fixtureCopy(t, "python"), python
}

func TestRunPytestNarrowedRunExecutesExactlyTheSelectedTests(t *testing.T) {
	t.Parallel()
	dir, py := pyFixture(t)
	res, cases, out, err := runPytestRequest(t, Request{
		RepoDir: dir,
		Python:  py,
		Targets: pyTargets("tests/test_core.py::test_add", "tests/test_core.py::TestCoreClass::test_add_negative"),
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.ExitCode != 0 || res.Abstained || len(res.Missing) != 0 {
		t.Fatalf("result %+v\n%s", res, out)
	}
	want := []string{"tests.test_core.TestCoreClass::test_add_negative", "tests.test_core::test_add"}
	if got := pyCaseKeys(cases); !slices.Equal(got, want) {
		t.Fatalf("executed %q, want %q (test_base must be deselected)", got, want)
	}
	// 2 of the fixture's 14 tests are selected. The plugin must tell pytest
	// about the other 12 (pytest_deselected), or pytest's own report and every
	// other plugin never learn that they were deselected.
	if !strings.Contains(out, "12 deselected") {
		t.Fatalf("pytest's output does not report the 12 deselected tests:\n%s", out)
	}
}

func TestRunPytestSpecialCharacterIDsSelectExactlyTheRequestedCases(t *testing.T) {
	t.Parallel()
	const params = "tests/test_params.py::test_value_is_positive"
	for _, tc := range []struct {
		name  string
		nodes []string
		want  []string
	}{
		{
			"space and non-ASCII only",
			[]string{params + "[with space]", params + "[café]"},
			[]string{"tests.test_params::test_value_is_positive[café]", "tests.test_params::test_value_is_positive[with space]"},
		},
		{
			"every special character",
			[]string{params + "[with space]", params + "[slash/inside]", params + "[open[bracket]", params + "[dash-ed]", params + "[café]"},
			[]string{
				"tests.test_params::test_value_is_positive[café]",
				"tests.test_params::test_value_is_positive[dash-ed]",
				"tests.test_params::test_value_is_positive[open[bracket]",
				"tests.test_params::test_value_is_positive[slash/inside]",
				"tests.test_params::test_value_is_positive[with space]",
			},
		},
		{
			"class parametrization",
			[]string{"tests/test_params.py::TestParamsInClass::test_word_is_not_empty[two words]", "tests/test_params.py::TestParamsInClass::test_word_is_not_empty[a-b/c]"},
			[]string{
				"tests.test_params.TestParamsInClass::test_word_is_not_empty[a-b/c]",
				"tests.test_params.TestParamsInClass::test_word_is_not_empty[two words]",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, py := pyFixture(t)
			res, cases, out, err := runPytestRequest(t, Request{RepoDir: dir, Python: py, Targets: pyTargets(tc.nodes...)})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if res.ExitCode != 0 || res.Abstained {
				t.Fatalf("result %+v\n%s", res, out)
			}
			if got := pyCaseKeys(cases); !slices.Equal(got, tc.want) {
				t.Fatalf("executed:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRunPytestMissingIDAbstainsBeforeAnyTestRuns(t *testing.T) {
	t.Parallel()
	dir, py := pyFixture(t)
	report := filepath.Join(dir, "junit.xml")
	writeTestFile(t, report, "stale")
	res, cases, out, err := runPytestRequest(t, Request{
		RepoDir: dir,
		Python:  py,
		Targets: pyTargets(
			"tests/test_params.py::test_value_is_positive[nope]",
			"tests/test_core.py::test_add",
			"tests/test_core.py::test_gone",
		),
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !res.Abstained || res.Reason != "pytest_missing_ids" {
		t.Fatalf("must abstain: %+v\n%s", res, out)
	}
	wantMissing := []string{"tests/test_core.py::test_gone", "tests/test_params.py::test_value_is_positive[nope]"}
	if !slices.Equal(res.Missing, wantMissing) {
		t.Fatalf("missing %q, want %q", res.Missing, wantMissing)
	}
	if res.ExitCode != 4 {
		t.Fatalf("pytest's exit code must pass through: %d", res.ExitCode)
	}
	if len(cases) != 0 {
		t.Fatalf("tests ran before the abstention: %q", pyCaseKeys(cases))
	}
	if data, readErr := os.ReadFile(report); readErr == nil && string(data) == "stale" {
		t.Fatal("a stale report survived the run")
	}
	if !strings.Contains(out, "hazard") {
		t.Fatalf("the reason is not visible in the pytest output:\n%s", out)
	}
}

func TestRunPytestAbstentionDoesNotDependOnTheExitCode(t *testing.T) {
	t.Setenv(fakePythonEnv, mustJSON(t, fakePythonSpec{Exit: 0, Missing: []string{"b::missing", "a::missing", "b::missing"}}))
	dir := t.TempDir()
	py := filepath.Join(fakeGoDir, "python"+exeSuffix())
	res, _, _, err := runPytestRequest(t, Request{RepoDir: dir, Python: py, Targets: pyTargets("a::missing", "b::missing")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Abstained || res.Reason != "pytest_missing_ids" || res.ExitCode != 0 {
		t.Fatalf("exit 0 with missing ids must still abstain: %+v", res)
	}
	if want := []string{"a::missing", "b::missing"}; !slices.Equal(res.Missing, want) {
		t.Fatalf("missing %q, want sorted and deduplicated %q", res.Missing, want)
	}

	t.Setenv(fakePythonEnv, mustJSON(t, fakePythonSpec{Exit: 4}))
	res, _, _, err = runPytestRequest(t, Request{RepoDir: dir, Python: py, Targets: pyTargets("a::x")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Abstained || len(res.Missing) != 0 || res.ExitCode != 4 {
		t.Fatalf("exit 4 without missing ids is not an abstention: %+v", res)
	}
}

func TestRunPytestExitCodePassesThroughAFailingSelectedTest(t *testing.T) {
	t.Parallel()
	failing := "tests/test_fail.py::test_fails"
	failingFixture := func(t *testing.T) (string, string) {
		t.Helper()
		dir, py := pyFixture(t)
		writeTestFile(t, filepath.Join(dir, "tests", "test_fail.py"), "def test_fails():\n    assert False, \"deliberate failure\"\n")
		return dir, py
	}

	t.Run("narrowed", func(t *testing.T) {
		t.Parallel()
		dir, py := failingFixture(t)
		res, cases, out, err := runPytestRequest(t, Request{RepoDir: dir, Python: py, Targets: pyTargets(failing, "tests/test_other.py::test_unrelated")})
		if err != nil {
			t.Fatalf("a failing test is a result, not an error: %v", err)
		}
		if res.ExitCode != 1 || res.Abstained {
			t.Fatalf("exit code %d, want 1: a red run must never be 0\n%s", res.ExitCode, out)
		}
		byName := casesByName(cases)
		if c := byName["test_fails"]; len(c) != 1 || c[0].Status != "fail" || !strings.Contains(c[0].Message, "deliberate failure") {
			t.Fatalf("failing case: %+v", c)
		}
		if c := byName["test_unrelated"]; len(c) != 1 || c[0].Status != "pass" {
			t.Fatalf("passing case: %+v", c)
		}
	})
	t.Run("full", func(t *testing.T) {
		t.Parallel()
		dir, py := failingFixture(t)
		res, cases, out, err := runPytestRequest(t, Request{RepoDir: dir, Python: py, Full: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 1 || len(cases) != 15 {
			t.Fatalf("exit %d with %d cases, want 1 with 15\n%s", res.ExitCode, len(cases), out)
		}
	})
	t.Run("other nonzero codes pass through and are not abstentions", func(t *testing.T) {
		t.Parallel()
		dir, py := pyFixture(t)
		res, _, _, err := runPytestRequest(t, Request{RepoDir: dir, Python: py, Full: true, Passthrough: []string{"--no-such-flag"}})
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 4 || res.Abstained {
			t.Fatalf("usage error: %+v", res)
		}
	})
}

func TestRunPytestUnreadableMissingFileIsAnErrorNeverGreen(t *testing.T) {
	// A pytest that exits 0 but leaves no readable missing-ids file cannot say
	// whether the selection held, so the run must not be reported as passing.
	t.Setenv(fakePythonEnv, mustJSON(t, fakePythonSpec{Exit: 0, DeleteMissingOut: true}))
	res, _, _, err := runPytestRequest(t, Request{
		RepoDir: t.TempDir(), Python: filepath.Join(fakeGoDir, "python"+exeSuffix()), Targets: pyTargets("a::x"),
	})
	if err == nil || res.ExitCode == 0 {
		t.Fatalf("exit %d, err %v", res.ExitCode, err)
	}
}

func TestRunPytestCleanupFailureAfterAPassingRunIsNeverExitZero(t *testing.T) {
	// pytest passes and the missing-ids file is read, then removing the
	// selection file fails: the error must come with the internal exit code,
	// never with 0, so a caller that ignores the error cannot report green.
	t.Setenv(fakePythonEnv, mustJSON(t, fakePythonSpec{Exit: 0, BlockSelection: true}))
	res, _, _, err := runPytestRequest(t, Request{
		RepoDir: t.TempDir(), Python: filepath.Join(fakeGoDir, "python"+exeSuffix()), Targets: pyTargets("a::x"),
	})
	if err == nil {
		t.Fatalf("the cleanup failure was swallowed: %+v", res)
	}
	if res.ExitCode != exitInternal {
		t.Fatalf("exit code %d with error %v, want %d", res.ExitCode, err, exitInternal)
	}
	if res.Abstained {
		t.Fatalf("a cleanup failure is not an abstention: %+v", res)
	}
}

func TestRunPytestFullRunExecutesEveryTest(t *testing.T) {
	t.Parallel()
	dir, py := pyFixture(t)
	res, cases, out, err := runPytestRequest(t, Request{RepoDir: dir, Python: py, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || len(cases) != 14 || res.Abstained || len(res.Invocations) != 1 || !res.Invocations[0].Whole {
		t.Fatalf("full run: exit %d, %d cases, %+v\n%s", res.ExitCode, len(cases), res, out)
	}
}

func TestRunPytestFullModeIgnoresAnInheritedSelection(t *testing.T) {
	t.Setenv("HAZARD_SELECTION", filepath.Join(t.TempDir(), "does-not-exist.txt"))
	t.Setenv(fakePythonEnv, mustJSON(t, fakePythonSpec{}))
	res, _, _, err := runPytestRequest(t, Request{RepoDir: t.TempDir(), Python: filepath.Join(fakeGoDir, "python"+exeSuffix()), Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Invocations) != 1 {
		t.Fatalf("invocations: %+v", res.Invocations)
	}
	env := res.Invocations[0].Env
	if !slices.Contains(env, "HAZARD_SELECTION=") {
		t.Fatalf("a full run must blank the selection variable so an inherited one cannot narrow it: %q", env)
	}
}

func TestRunPytestInvocationShape(t *testing.T) {
	t.Parallel()
	dir, py := pyFixture(t)
	nodes := []string{"tests/test_core.py::test_add", "tests/test_params.py::test_value_is_positive[café]"}
	res, _, out, err := runPytestRequest(t, Request{
		RepoDir:     dir,
		Python:      py,
		Targets:     pyTargets(nodes...),
		PytestArgs:  []string{"-p", "no:cacheprovider"},
		Testpaths:   []string{"tests"},
		Passthrough: []string{"-x", "-q"},
		JUnitPath:   "reports/out.xml",
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("exit %d, err %v\n%s", res.ExitCode, err, out)
	}
	if len(res.Invocations) != 1 || res.Invocations[0].Whole {
		t.Fatalf("invocations: %+v", res.Invocations)
	}
	inv := res.Invocations[0]
	wantArgv := []string{py, "-m", "pytest", "-p", "hazard_pytest", "--junitxml=" + filepath.Join(dir, "reports", "out.xml"), "-p", "no:cacheprovider", "tests", "-x", "-q"}
	if !slices.Equal(inv.Argv, wantArgv) {
		t.Fatalf("argv:\n got %q\nwant %q", inv.Argv, wantArgv)
	}
	for _, a := range inv.Argv {
		if strings.Contains(a, "::") || strings.Contains(a, "café") {
			t.Fatalf("node ID on argv: %q", a)
		}
	}
	if inv.Dir != dir {
		t.Fatalf("dir %q, want %q", inv.Dir, dir)
	}
	env := map[string]string{}
	for _, e := range inv.Env {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	if env["HAZARD_SELECTION"] == "" || env["HAZARD_MISSING_OUT"] == "" {
		t.Fatalf("env %v", inv.Env)
	}
	wantPath := filepath.Join(dir, ".hazard")
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		wantPath += string(os.PathListSeparator) + existing
	}
	if env["PYTHONPATH"] != wantPath {
		t.Fatalf("PYTHONPATH %q, want %q", env["PYTHONPATH"], wantPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "reports", "out.xml")); err != nil {
		t.Fatalf("JUnit not at the requested relative path: %v", err)
	}
}

func TestRunPytestWritesThePluginAndCleansItsTempFiles(t *testing.T) {
	t.Parallel()
	dir, py := pyFixture(t)
	pluginPath := filepath.Join(dir, ".hazard", "hazard_pytest.py")
	writeTestFile(t, pluginPath, "raise SystemExit('a tampered plugin must be replaced')\n")
	res, _, out, err := runPytestRequest(t, Request{RepoDir: dir, Python: py, Targets: pyTargets("tests/test_other.py::test_unrelated")})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("exit %d, err %v\n%s", res.ExitCode, err, out)
	}
	got, readErr := os.ReadFile(pluginPath)
	if readErr != nil || !bytes.Equal(got, pytestPlugin) {
		t.Fatalf("plugin on disk differs from the embedded one (%v)", readErr)
	}
	entries, readErr := os.ReadDir(filepath.Join(dir, ".hazard"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "selection-") || strings.HasPrefix(e.Name(), "missing-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestRunPytestRejectsNarrowingPassthrough(t *testing.T) {
	for _, full := range []bool{false, true} {
		res, err := RunPytest(context.Background(), Request{
			RepoDir: t.TempDir(), Full: full, Targets: pyTargets("tests/test_core.py::test_add"), Passthrough: []string{"-k", "add"},
		})
		if !errors.Is(err, ErrPassthroughNarrows) || res.ExitCode == 0 {
			t.Errorf("full=%v: exit %d, err %v", full, res.ExitCode, err)
		}
	}
}

func TestRunPytestInvalidRequestsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"no targets in a narrowed run", Request{}},
		{"empty node id", Request{Targets: []Target{{Language: "python"}}}},
		{"node id with a newline", Request{Targets: pyTargets("tests/a.py::t\ntests/b.py::u")}},
		{"node id with a carriage return", Request{Targets: pyTargets("tests/a.py::t\r")}},
		{"go target in a python run", Request{Targets: []Target{{Language: "go", Package: "p", Name: "TestA", RunnerID: "tests/a.py::t"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.RepoDir = t.TempDir()
			res, err := RunPytest(context.Background(), tc.req)
			if err == nil || res.ExitCode == 0 || errors.Is(err, ErrPassthroughNarrows) {
				t.Fatalf("exit %d, err %v", res.ExitCode, err)
			}
			if _, statErr := os.Stat(filepath.Join(tc.req.RepoDir, ".hazard")); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("an invalid request touched the repository: %v", statErr)
			}
		})
	}
}

func TestRunPytestMissingInterpreterIsAnErrorNeverGreen(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "junit.xml")
	writeTestFile(t, report, "stale")
	res, err := RunPytest(context.Background(), Request{
		RepoDir: dir, Python: filepath.Join(dir, "no-such-python"), Full: true, JUnitPath: report,
	})
	if err == nil || res.ExitCode == 0 {
		t.Fatalf("exit %d, err %v", res.ExitCode, err)
	}
	if _, statErr := os.Stat(report); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a stale report survived: %v", statErr)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exeSuffix() string {
	if filepath.Separator == '\\' {
		return ".exe"
	}
	return ""
}
