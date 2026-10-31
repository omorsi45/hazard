package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureModule = "example.com/hazardfixture"

func copyDir(src, dst string) error {
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

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
}

// fixtureCopy copies testdata/repos/<name> into a fresh temp dir.
func fixtureCopy(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	copyTree(t, filepath.Join("..", "..", "testdata", "repos", name), dir)
	return dir
}

var (
	goFixtureOnce sync.Once
	goFixtureDir  string
	goFixtureErr  error

	bigModuleOnce  sync.Once
	bigModuleDir   string
	bigModuleNames []string
	bigModuleErr   error
)

const (
	bigModulePath = "example.com/big"
	bigModulePkg  = bigModulePath + "/pkg"
)

// nativeGoFixture returns one copy of testdata/repos/go, shared by every test
// that runs the real go command, so the packages they build are compiled once
// and reused from go's build cache. Besides the fixture it holds the extra test
// files those tests need, each in a package no other test runs. It is read-only:
// a test sets an absolute JUnitPath so no report lands in it.
func nativeGoFixture(t *testing.T) string {
	t.Helper()
	goFixtureOnce.Do(func() {
		dir := filepath.Join(nativeRoot, "gofixture")
		goFixtureDir, goFixtureErr = dir, copyDir(filepath.Join("..", "..", "testdata", "repos", "go"), dir)
		if goFixtureErr != nil {
			return
		}
		for rel, content := range map[string]string{
			// A selected failing test, and a test that exists only with -tags=integration.
			"core/fail_test.go":   "package core\n\nimport \"testing\"\n\nfunc TestFails(t *testing.T) { t.Fatal(\"deliberate failure\") }\n",
			"core/tagged_test.go": "//go:build integration\n\npackage core\n\nimport \"testing\"\n\nfunc TestTagged(t *testing.T) {}\n",
			// A package whose test binary does not build.
			"brokenbuild/brokenbuild_test.go": "package brokenbuild\n\nimport \"testing\"\n\nfunc TestNeverBuilt(t *testing.T) {}\n\nfunc init() { undefinedSymbol() }\n",
		} {
			path := filepath.Join(dir, filepath.FromSlash(rel))
			if goFixtureErr = os.MkdirAll(filepath.Dir(path), 0o755); goFixtureErr != nil {
				return
			}
			if goFixtureErr = os.WriteFile(path, []byte(content), 0o644); goFixtureErr != nil {
				return
			}
		}
	})
	if goFixtureErr != nil {
		t.Fatalf("prepare the shared Go fixture: %v", goFixtureErr)
	}
	return goFixtureDir
}

// nativeBigModule returns a module with one package of 500 tests whose names
// are 40 characters long, shared by the tests that select from it: its
// directory, the package's import path and the test names. It is read-only.
func nativeBigModule(t *testing.T) (dir, pkg string, names []string) {
	t.Helper()
	bigModuleOnce.Do(func() {
		bigModuleDir = filepath.Join(nativeRoot, "big")
		bigModuleNames, bigModuleErr = writeBigModule(bigModuleDir, 500)
	})
	if bigModuleErr != nil {
		t.Fatalf("prepare the shared 500-test module: %v", bigModuleErr)
	}
	return bigModuleDir, bigModulePkg, bigModuleNames
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJUnitFile(t *testing.T, path string) []JUnitCase {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("JUnit report missing: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close JUnit report: %v", err)
		}
	}()
	cases, err := ParseJUnit(f)
	if err != nil {
		t.Fatalf("ParseJUnit: %v", err)
	}
	return cases
}

func caseNames(cases []JUnitCase) []string {
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.Name
	}
	return names
}

func statusCounts(cases []JUnitCase) map[string]int {
	counts := map[string]int{}
	for _, c := range cases {
		counts[c.Status]++
	}
	return counts
}

func casesByName(cases []JUnitCase) map[string][]JUnitCase {
	m := map[string][]JUnitCase{}
	for _, c := range cases {
		m[c.Name] = append(m[c.Name], c)
	}
	return m
}

// runGoRequest runs RunGo with captured output and returns what it reported.
func runGoRequest(t *testing.T, req Request) (res Result, stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	req.Stdout, req.Stderr = &out, &errb
	if req.JUnitPath == "" {
		req.JUnitPath = "junit.xml"
	}
	res, err = RunGo(context.Background(), req)
	return res, out.String(), errb.String(), err
}

// runGoNative runs the real go command and parses the JUnit report.
func runGoNative(t *testing.T, req Request) (Result, []JUnitCase, string) {
	t.Helper()
	res, stdout, stderr, err := runGoRequest(t, req)
	if err != nil {
		t.Fatalf("RunGo: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	path := req.JUnitPath
	if path == "" {
		path = "junit.xml"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(req.RepoDir, path)
	}
	return res, readJUnitFile(t, path), stdout
}

// passPackage and failPackage build the events go test -json prints.
func passPackage(pkg string, tests ...string) fakeGoPackage {
	return fakeGoPackage{Events: packageEvents(pkg, "", tests...)}
}

func failPackage(pkg string, exit int, failing string, passing ...string) fakeGoPackage {
	return fakeGoPackage{Exit: exit, Events: packageEvents(pkg, failing, passing...)}
}

func packageEvents(pkg, failing string, passing ...string) []goEvent {
	var evs []goEvent
	for _, name := range passing {
		evs = append(evs,
			goEvent{Action: "run", Package: pkg, Test: name},
			goEvent{Action: "output", Package: pkg, Test: name, Output: "--- PASS: " + name + "\n"},
			goEvent{Action: "pass", Package: pkg, Test: name, Elapsed: 0.01})
	}
	result := "pass"
	if failing != "" {
		result = "fail"
		evs = append(evs,
			goEvent{Action: "run", Package: pkg, Test: failing},
			goEvent{Action: "output", Package: pkg, Test: failing, Output: "    boom in " + failing + "\n"},
			goEvent{Action: "fail", Package: pkg, Test: failing, Elapsed: 0.01})
	}
	return append(evs, goEvent{Action: "output", Package: pkg, Output: strings.ToUpper(result) + "\n"}, goEvent{Action: result, Package: pkg, Elapsed: 0.02})
}

// ---- RunGo against a stand-in go command (fast, deterministic) ----

func TestRunGoExitCodeIsTheFirstNonzeroAndEveryInvocationRuns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exits []int
		want  int
	}{
		{"all pass", []int{0, 0}, 0},
		{"failure after a pass", []int{0, 1}, 1},
		{"failure first, a pass after", []int{1, 0}, 1},
		{"first nonzero wins", []int{3, 1}, 3},
		{"other nonzero code passes through unchanged", []int{0, 0, 5}, 5},
		{"single failure", []int{1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := fakeGoSpec{Packages: map[string]fakeGoPackage{}}
			var targets []Target
			for i, exit := range tc.exits {
				pkg := fmt.Sprintf("example.com/m/p%d", i)
				name := fmt.Sprintf("TestP%d", i)
				if exit == 0 {
					spec.Packages[pkg] = passPackage(pkg, name)
				} else {
					spec.Packages[pkg] = failPackage(pkg, exit, name)
				}
				targets = append(targets, goTarget(pkg, name))
			}
			logPath := useFakeGo(t, spec)
			dir := t.TempDir()
			res, _, _, err := runGoRequest(t, Request{RepoDir: dir, Targets: targets})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != tc.want {
				t.Fatalf("exit code %d, want %d", res.ExitCode, tc.want)
			}
			calls := readFakeGoLog(t, logPath)
			if len(calls) != len(tc.exits) {
				t.Fatalf("%d of %d invocations ran after a failure: %+v", len(calls), len(tc.exits), calls)
			}
			for i, call := range calls {
				if !slices.Equal(call.Args, res.Invocations[i].Argv[1:]) || filepath.Clean(call.Dir) != filepath.Clean(dir) {
					t.Errorf("call %d: %+v does not match invocation %+v", i, call, res.Invocations[i])
				}
			}
			cases := readJUnitFile(t, filepath.Join(dir, "junit.xml"))
			if len(cases) != len(tc.exits) {
				t.Fatalf("report has %d cases, want one per package: %q", len(cases), caseNames(cases))
			}
		})
	}
}

func TestRunGoStreamsHumanOutputAndStderr(t *testing.T) {
	pkg := failPackage("p/a", 1, "TestBad", "TestGood")
	pkg.Stderr = "warning on stderr\n"
	useFakeGo(t, fakeGoSpec{Packages: map[string]fakeGoPackage{"p/a": pkg}})
	res, stdout, stderr, err := runGoRequest(t, Request{RepoDir: t.TempDir(), Targets: []Target{goTarget("p/a", "TestBad")}})
	if err != nil || res.ExitCode != 1 {
		t.Fatalf("exit %d, err %v", res.ExitCode, err)
	}
	if !strings.Contains(stdout, "--- PASS: TestGood") || !strings.Contains(stdout, "boom in TestBad") {
		t.Fatalf("test output not streamed to Stdout:\n%s", stdout)
	}
	if stderr != "warning on stderr\n" {
		t.Fatalf("Stderr %q", stderr)
	}
}

func TestRunGoJUnitPathDefaultRelativeAbsoluteAndStale(t *testing.T) {
	useFakeGo(t, fakeGoSpec{Packages: map[string]fakeGoPackage{"p/a": passPackage("p/a", "TestA")}})
	dir := t.TempDir()
	req := Request{RepoDir: dir, Targets: []Target{goTarget("p/a", "TestA")}}
	run := func(t *testing.T, jp, want string) {
		t.Helper()
		r := req
		r.JUnitPath = jp
		if _, err := RunGo(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if got := caseNames(readJUnitFile(t, want)); !slices.Equal(got, []string{"TestA"}) {
			t.Fatalf("cases %q", got)
		}
	}
	t.Run("default", func(t *testing.T) { run(t, "", filepath.Join(dir, ".hazard", "junit.xml")) })
	t.Run("relative creates directories", func(t *testing.T) {
		run(t, filepath.Join("out", "sub", "r.xml"), filepath.Join(dir, "out", "sub", "r.xml"))
	})
	t.Run("absolute replaces a stale report", func(t *testing.T) {
		abs := filepath.Join(t.TempDir(), "abs.xml")
		writeTestFile(t, abs, "stale")
		run(t, abs, abs)
	})
}

func TestRunGoCannotStartLeavesNoReportAndNeverExitsZero(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no go command at all
	report := filepath.Join(t.TempDir(), "junit.xml")
	writeTestFile(t, report, "stale")
	res, _, _, err := runGoRequest(t, Request{RepoDir: t.TempDir(), Targets: []Target{goTarget("p/a", "TestA")}, JUnitPath: report})
	if err == nil || res.ExitCode == 0 {
		t.Fatalf("exit %d, err %v", res.ExitCode, err)
	}
	if _, statErr := os.Stat(report); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a stale report survived a run that could not start: %v", statErr)
	}
}

func TestRunGoCancellationIsNonZeroAndWritesNoReport(t *testing.T) {
	useFakeGo(t, fakeGoSpec{SleepMS: 60000, Packages: map[string]fakeGoPackage{"p/a": passPackage("p/a", "TestA")}})
	report := filepath.Join(t.TempDir(), "junit.xml")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	res, err := RunGo(ctx, Request{RepoDir: t.TempDir(), Targets: []Target{goTarget("p/a", "TestA")}, JUnitPath: report})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want context.DeadlineExceeded", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a canceled run reported exit 0")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("cancellation took %v", d)
	}
	if _, statErr := os.Stat(report); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a canceled run wrote a report: %v", statErr)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("sink closed") }

func TestRunGoUnwritableOutputIsAnErrorNotAGreenRun(t *testing.T) {
	useFakeGo(t, fakeGoSpec{Packages: map[string]fakeGoPackage{"p/a": passPackage("p/a", "TestA")}})
	res, err := RunGo(context.Background(), Request{
		RepoDir: t.TempDir(), Targets: []Target{goTarget("p/a", "TestA")}, JUnitPath: "junit.xml", Stdout: brokenWriter{},
	})
	if err == nil || res.ExitCode == 0 {
		t.Fatalf("exit %d, err %v", res.ExitCode, err)
	}
}

func TestRunGoReportWriteFailureAfterAPassingRunIsNeverExitZero(t *testing.T) {
	// Every process passes, then publishing the report fails (a non-empty
	// directory sits at the report path). The error must come with the
	// internal exit code, never with 0.
	report := filepath.Join(t.TempDir(), "junit.xml")
	useFakeGo(t, fakeGoSpec{BlockPath: report, Packages: map[string]fakeGoPackage{"p/a": passPackage("p/a", "TestA")}})
	res, _, _, err := runGoRequest(t, Request{RepoDir: t.TempDir(), Targets: []Target{goTarget("p/a", "TestA")}, JUnitPath: report})
	if err == nil {
		t.Fatalf("the report write failure was swallowed: %+v", res)
	}
	if res.ExitCode != exitInternal {
		t.Fatalf("exit code %d with error %v, want %d", res.ExitCode, err, exitInternal)
	}
}

func TestRunGoInvalidRequestFailsClosed(t *testing.T) {
	res, err := RunGo(context.Background(), Request{RepoDir: t.TempDir(), Passthrough: []string{"-run", "X"}, Full: true})
	if !errors.Is(err, ErrPassthroughNarrows) {
		t.Fatalf("err %v, want ErrPassthroughNarrows", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("an invalid request must not report exit 0")
	}
}

// ---- RunGo against the real go command ----
//
// Every real run pays go's fixed start-up cost (loading and cache-checking the
// standard library), so these tests share as few runs as possible, run in
// parallel, and build in shared read-only repositories (nativeGoFixture,
// nativeBigModule) so go's build cache is reused across them.

func TestRunGoNativeSelectionAnchoringAndExitCode(t *testing.T) {
	t.Parallel()
	dir := nativeGoFixture(t)
	core := fixtureModule + "/core"
	req := Request{
		RepoDir: dir,
		Targets: []Target{
			goTarget(core, "TestFoo"),
			goTarget(core, "TestAddSubtestsWithSpaces/adds_small_numbers"),
			goTarget(core, "TestFails"),
			goTarget(core, "TestTagged"),
		},
		GoTags:      []string{"integration"},
		Passthrough: []string{"-count=2", "-vet=off"},
		JUnitPath:   filepath.Join(t.TempDir(), "junit.xml"),
	}
	res, cases, stdout := runGoNative(t, req)
	byName := casesByName(cases)

	t.Run("exit code passes through a failing selected test", func(t *testing.T) {
		if res.ExitCode != 1 {
			t.Fatalf("exit code %d, want 1: a red run must never be 0", res.ExitCode)
		}
		if len(byName["TestFails"]) == 0 {
			t.Fatalf("TestFails did not run: %q", caseNames(cases))
		}
		for _, c := range byName["TestFails"] {
			if c.Status != "fail" || !strings.Contains(c.Message, "deliberate failure") {
				t.Errorf("failing case: %+v", c)
			}
		}
		if !strings.Contains(stdout, "--- FAIL: TestFails") {
			t.Errorf("failure output not streamed:\n%s", stdout)
		}
	})
	t.Run("anchoring: TestFoo does not run TestFooBar", func(t *testing.T) {
		if _, ran := byName["TestFooBar"]; ran {
			t.Fatalf("TestFooBar ran for a selection of TestFoo: %q", caseNames(cases))
		}
		if len(byName["TestFoo"]) == 0 {
			t.Fatalf("TestFoo did not run: %q", caseNames(cases))
		}
		if _, ran := byName["TestLongNameWithoutRegexpMetacharactersToExerciseSelectionPatternLength"]; ran {
			t.Fatalf("an unselected test ran: %q", caseNames(cases))
		}
	})
	t.Run("a subtest target runs its parent and subtests", func(t *testing.T) {
		for _, name := range []string{
			"TestAddSubtestsWithSpaces",
			"TestAddSubtestsWithSpaces/adds_small_numbers",
			"TestAddSubtestsWithSpaces/adds_with_a_zero_operand",
		} {
			if len(byName[name]) == 0 {
				t.Errorf("%s did not run: %q", name, caseNames(cases))
			}
		}
	})
	t.Run("tags and count reach go test", func(t *testing.T) {
		if len(byName["TestTagged"]) != 2 {
			t.Fatalf("TestTagged ran %d times, want 2 (-tags and -count=2): %q", len(byName["TestTagged"]), caseNames(cases))
		}
		if len(res.Invocations) != 1 || res.ExitCode == 0 {
			t.Fatalf("result: %+v", res)
		}
		for _, want := range []string{"-tags=integration", "-count=2"} {
			if !slices.Contains(res.Invocations[0].Argv, want) {
				t.Errorf("argv %q lacks %s", res.Invocations[0].Argv, want)
			}
		}
	})
	t.Run("identities stay compatible", func(t *testing.T) {
		for _, c := range cases {
			id, err := NormalizeJUnitCase("go", c.Classname, c)
			if err != nil || !strings.HasPrefix(id.ID, "go:"+core+":Test") {
				t.Errorf("%+v: %+v %v", c, id, err)
			}
		}
	})
}

func TestRunGoNativeFullSuite(t *testing.T) {
	t.Parallel()
	dir := nativeGoFixture(t)
	res, cases, _ := runGoNative(t, Request{
		RepoDir:     dir,
		Full:        true,
		GoPackages:  []string{"./left", "./right", "./top"},
		Passthrough: []string{"-count=1", "-vet=off"},
		JUnitPath:   filepath.Join(t.TempDir(), "junit.xml"),
	})
	if res.ExitCode != 0 || len(res.Invocations) != 1 || !res.Invocations[0].Whole {
		t.Fatalf("result: %+v", res)
	}
	var got []string
	for _, c := range cases {
		got = append(got, c.Suite+":"+c.Name+":"+c.Status)
		if c.SuiteIncomplete {
			t.Errorf("healthy full run marked incomplete: %+v", c)
		}
	}
	want := []string{
		fixtureModule + "/left:TestValue:pass",
		fixtureModule + "/right:TestValue:pass",
		fixtureModule + "/top:TestTotal:pass",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cases %q, want %q", got, want)
	}
}

// TestRunGoReportsASelectedTestThatDidNotRun: go test prints "ok ... [no tests
// to run]" and exits 0 when -run matches nothing, so a selected test that was
// renamed or removed since the index was built would pass unnoticed. A narrowed
// run compares the selected top-level tests with the ones that started, and a
// test that did not start makes the run abstain, like a missing pytest node ID.
func TestRunGoReportsASelectedTestThatDidNotRun(t *testing.T) {
	spec := fakeGoSpec{Packages: map[string]fakeGoPackage{
		"p/a": passPackage("p/a", "TestA", "TestParent"),
		"p/b": passPackage("p/b", "TestB"),
	}}
	targets := []Target{
		goTarget("p/a", "TestA"),
		goTarget("p/a", "TestParent/sub"), // a subtest widens to its parent, which ran
		goTarget("p/a", "TestRenamedAway"),
		goTarget("p/b", "TestB"),
		goTarget("p/b", "TestGone"),
	}
	useFakeGo(t, spec)
	dir := t.TempDir()

	res, _, _, err := runGoRequest(t, Request{RepoDir: dir, Targets: targets})

	if err != nil {
		t.Fatal(err)
	}
	if !res.Abstained || res.Reason != goMissingReason {
		t.Errorf("Abstained %v Reason %q, want an abstention with %s", res.Abstained, res.Reason, goMissingReason)
	}
	if want := []string{"go:p/a:TestRenamedAway", "go:p/b:TestGone"}; !slices.Equal(res.Missing, want) {
		t.Errorf("Missing = %v, want %v", res.Missing, want)
	}
	if got := caseNames(readJUnitFile(t, filepath.Join(dir, "junit.xml"))); len(got) == 0 {
		t.Error("the report of the narrowed attempt must still be written")
	}

	t.Run("nothing is missing when every selected test started", func(t *testing.T) {
		useFakeGo(t, spec)
		res, _, _, err := runGoRequest(t, Request{RepoDir: t.TempDir(), Targets: []Target{goTarget("p/a", "TestA"), goTarget("p/b", "TestB")}})
		if err != nil || res.Abstained || res.Reason != "" || len(res.Missing) != 0 {
			t.Errorf("Abstained %v Reason %q Missing %v err %v, want a plain run", res.Abstained, res.Reason, res.Missing, err)
		}
	})
	t.Run("a full run has nothing to compare", func(t *testing.T) {
		useFakeGo(t, spec)
		res, _, _, err := runGoRequest(t, Request{RepoDir: t.TempDir(), Full: true, Targets: targets})
		if err != nil || res.Abstained || len(res.Missing) != 0 {
			t.Errorf("Abstained %v Missing %v err %v, want a plain full run", res.Abstained, res.Missing, err)
		}
	})
}

// TestRunGoNativeSelectedTestThatDoesNotExistAbstains is the real go command
// printing "ok [no tests to run]" for a selected test that is not there.
func TestRunGoNativeSelectedTestThatDoesNotExistAbstains(t *testing.T) {
	t.Parallel()
	dir := nativeGoFixture(t)
	core := fixtureModule + "/core"
	res, _, _ := runGoNative(t, Request{
		RepoDir:     dir,
		Targets:     []Target{goTarget(core, "TestFoo"), goTarget(core, "TestNopeDoesNotExist")},
		Passthrough: []string{"-count=1", "-vet=off"},
		JUnitPath:   filepath.Join(t.TempDir(), "junit.xml"),
	})
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d: the fixture premise is that go test passes while TestNopeDoesNotExist matches nothing", res.ExitCode)
	}
	if !res.Abstained || res.Reason != goMissingReason || !slices.Equal(res.Missing, []string{"go:" + core + ":TestNopeDoesNotExist"}) {
		t.Errorf("Abstained %v Reason %q Missing %v, want the missing test reported", res.Abstained, res.Reason, res.Missing)
	}
}

func TestRunGoNativeBuildFailureIsNonZeroAndNeverGreen(t *testing.T) {
	t.Parallel()
	dir := nativeGoFixture(t)
	report := filepath.Join(t.TempDir(), "junit.xml")
	res, stdout, _, err := runGoRequest(t, Request{
		RepoDir:     dir,
		Targets:     []Target{goTarget(fixtureModule+"/brokenbuild", "TestNeverBuilt")},
		Passthrough: []string{"-count=1", "-vet=off"},
		JUnitPath:   report,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a package that does not build reported exit 0")
	}
	if !strings.Contains(stdout, "undefinedSymbol") {
		t.Fatalf("build error not streamed:\n%s", stdout)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<error") || !strings.Contains(string(data), "undefinedSymbol") {
		t.Fatalf("build failure missing from the JUnit report:\n%s", data)
	}
	if _, err := ParseJUnit(bytes.NewReader(data)); err == nil {
		t.Fatal("a report whose only package failed to build parsed as valid")
	}
}

// writeBigModule writes a module with one package of n tests whose names are 40
// characters long into dir and returns the test names.
func writeBigModule(dir string, n int) ([]string, error) {
	var sb strings.Builder
	sb.WriteString("package pkg\n\nimport \"testing\"\n\n")
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("Test%04d_%s", i, strings.Repeat("x", 31))
		if len(names[i]) != 40 {
			return nil, fmt.Errorf("name %q has %d characters", names[i], len(names[i]))
		}
		fmt.Fprintf(&sb, "func %s(t *testing.T) {}\n\n", names[i])
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+bigModulePath+"\n\ngo 1.26\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "pkg_test.go"), []byte(sb.String()), 0o644); err != nil {
		return nil, err
	}
	return names, nil
}

func TestRunGoFiveHundredTestsWidenToTheWholePackage(t *testing.T) {
	t.Parallel()
	dir, pkg, names := nativeBigModule(t)
	targets := make([]Target, len(names))
	for i, n := range names {
		targets[i] = goTarget(pkg, n)
	}
	req := Request{
		RepoDir:     dir,
		Targets:     targets,
		Passthrough: []string{"-count=1", "-vet=off"},
		JUnitPath:   filepath.Join(t.TempDir(), "junit.xml"),
	}

	// Planning is checked before the slow native run so a regression fails fast.
	invs, widened, err := GoInvocations(req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(widened, []string{pkg}) || len(invs) != 1 || !invs[0].Whole || slices.Contains(invs[0].Argv, "-run") {
		t.Fatalf("500 names must widen to the whole package: widened %q, invocations %+v", widened, invs)
	}

	res, cases, _ := runGoNative(t, req)
	if res.ExitCode != 0 || !slices.Equal(res.Widened, []string{pkg}) {
		t.Fatalf("result: %+v", res)
	}
	if got := statusCounts(cases); len(cases) != 500 || got["pass"] != 500 {
		t.Fatalf("%d cases %v, want 500 passing", len(cases), got)
	}
}

func TestRunGoThreeOfFiveHundredTestsStayAnchored(t *testing.T) {
	t.Parallel()
	dir, pkg, names := nativeBigModule(t)
	picked := []string{names[7], names[250], names[499]}
	var targets []Target
	for _, n := range picked {
		targets = append(targets, goTarget(pkg, n))
	}
	req := Request{
		RepoDir:     dir,
		Targets:     targets,
		Passthrough: []string{"-count=1", "-vet=off"},
		JUnitPath:   filepath.Join(t.TempDir(), "junit.xml"),
	}

	invs, widened, err := GoInvocations(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(widened) != 0 || len(invs) != 1 || invs[0].Whole || !slices.Contains(invs[0].Argv, "-run") {
		t.Fatalf("3 names must stay anchored: widened %q, invocations %+v", widened, invs)
	}

	res, cases, _ := runGoNative(t, req)
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d", res.ExitCode)
	}
	if got := caseNames(cases); !slices.Equal(got, picked) {
		t.Fatalf("executed %q, want exactly %q", got, picked)
	}
}
