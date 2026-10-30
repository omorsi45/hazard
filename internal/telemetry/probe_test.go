package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/runner"
)

// fakeProbeRun is a runner adapter that records what it was asked to run, looks
// at the worktree it was given and writes a scripted JUnit report.
type fakeProbeRun struct {
	t       *testing.T
	reqs    []runner.Request
	markers []string // marker.txt as seen in the worktree
	junit   string   // report written at JUnitPath; empty writes none
	res     runner.Result
	err     error
}

func (f *fakeProbeRun) fn(_ context.Context, req runner.Request) (runner.Result, error) {
	f.t.Helper()
	f.reqs = append(f.reqs, req)
	marker, err := os.ReadFile(filepath.Join(req.RepoDir, "marker.txt"))
	if err != nil {
		f.t.Errorf("the worktree must hold the commit's files: %v", err)
	}
	f.markers = append(f.markers, string(marker))
	if f.junit != "" {
		if err := os.WriteFile(req.JUnitPath, []byte(f.junit), 0o644); err != nil {
			f.t.Errorf("write the scripted report: %v", err)
		}
	}
	return f.res, f.err
}

func suiteXML(cases ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><testsuites><testsuite name="s">` + strings.Join(cases, "") + `</testsuite></testsuites>`
}

const (
	goPass = `<testcase classname="example.com/m/p" name="TestB"/>`
	goFail = `<testcase classname="example.com/m/p" name="TestB"><failure message="FAIL"/></testcase>`
)

// isolatedTemp points os.TempDir at a fresh directory so leftovers are visible.
func isolatedTemp(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	return tmp
}

func worktreeCount(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(gitIn(t, dir, "worktree", "list", "--porcelain"), "worktree ")
}

func newProber(t *testing.T, dir string) (Prober, *fakeProbeRun, *fakeProbeRun) {
	t.Helper()
	gor, pyr := &fakeProbeRun{t: t}, &fakeProbeRun{t: t}
	return Prober{Git: Git{Dir: dir}, Config: config.Defaults(), Runners: Runners{Go: gor.fn, Python: pyr.fn}}, gor, pyr
}

func sameDirNoFail(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

func TestProbeRunsTheTestInAnIsolatedWorktreeAtTheCommit(t *testing.T) {
	tmp := isolatedTemp(t)
	dir, shas := linearRepo(t, 2)
	p, gor, _ := newProber(t, dir)
	gor.junit = suiteXML(goFail)
	gor.res = runner.Result{ExitCode: 1}
	cfg := config.Defaults()
	cfg.Languages.Go.Tags = []string{"integration"}
	p.Config = cfg

	for i, want := range []string{"v1", "v2"} {
		st, err := p.Probe(context.Background(), shas[i], "go:example.com/m/p:TestB")
		if err != nil || st != StateFail {
			t.Fatalf("Probe(%d) = %v, %v; want StateFail", i, st, err)
		}
		if got := gor.markers[i]; got != want {
			t.Errorf("the worktree at commit %d held marker %q, want %q", i+1, got, want)
		}
	}
	req := gor.reqs[0]
	wantTarget := runner.Target{ID: "go:example.com/m/p:TestB", Language: "go", Package: "example.com/m/p", Name: "TestB"}
	if req.Full || len(req.Targets) != 1 || req.Targets[0] != wantTarget || len(req.GoTags) != 1 || req.GoTags[0] != "integration" {
		t.Errorf("request = %+v, want one Go target %+v narrowed, with the configured tags", req, wantTarget)
	}
	if sameDirNoFail(req.RepoDir, dir) || !filepath.IsAbs(req.JUnitPath) {
		t.Errorf("the test must run in a separate worktree (%q vs %q) with an absolute report path (%q)", req.RepoDir, dir, req.JUnitPath)
	}
	if _, err := os.Stat(req.RepoDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the worktree must be removed afterwards: %v", err)
	}
	if n := worktreeCount(t, dir); n != 1 {
		t.Errorf("git still lists %d worktrees, want only the main one", n)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("temporary files left behind: %v (err %v)", entries, err)
	}
}

func TestProbeStates(t *testing.T) {
	const id = "go:example.com/m/p:TestB"
	for name, tc := range map[string]struct {
		junit   string
		res     runner.Result
		err     error
		want    State
		wantErr bool
	}{
		"the test fails":                              {suiteXML(goFail), runner.Result{ExitCode: 1}, nil, StateFail, false},
		"the test errors":                             {suiteXML(`<testcase classname="example.com/m/p" name="TestB"><error message="x"/></testcase>`), runner.Result{ExitCode: 1}, nil, StateFail, false},
		"the test passes":                             {suiteXML(goPass), runner.Result{}, nil, StatePass, false},
		"the test is skipped":                         {suiteXML(`<testcase classname="example.com/m/p" name="TestB"><skipped/></testcase>`), runner.Result{}, nil, StatePass, false},
		"only another test fails":                     {suiteXML(`<testcase classname="example.com/m/p" name="TestA"><failure/></testcase>`, goPass), runner.Result{ExitCode: 1}, nil, StatePass, false},
		"the test does not exist and the run passed":  {suiteXML(), runner.Result{}, nil, StatePass, false},
		"the test does not exist and the build broke": {suiteXML(), runner.Result{ExitCode: 1}, nil, StateUnknown, false},
		"no report was written":                       {"", runner.Result{}, nil, StateUnknown, false},
		"the runner failed internally":                {"", runner.Result{ExitCode: 70}, errors.New("go vanished"), StateUnknown, true},
	} {
		t.Run(name, func(t *testing.T) {
			isolatedTemp(t)
			dir, shas := linearRepo(t, 1)
			p, gor, _ := newProber(t, dir)
			gor.junit, gor.res, gor.err = tc.junit, tc.res, tc.err
			got, err := p.Probe(context.Background(), shas[0], id)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("Probe = %v, %v; want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
			if n := worktreeCount(t, dir); n != 1 {
				t.Errorf("git still lists %d worktrees after a probe", n)
			}
		})
	}
}

func TestProbeNarrowsASubtestToItsParentAndMatchesTheSubtestCase(t *testing.T) {
	isolatedTemp(t)
	dir, shas := linearRepo(t, 1)
	p, gor, _ := newProber(t, dir)
	gor.junit = suiteXML(
		`<testcase classname="example.com/m/p" name="TestC"><failure/></testcase>`,
		`<testcase classname="example.com/m/p" name="TestC/with_space"><failure/></testcase>`)
	gor.res = runner.Result{ExitCode: 1}
	st, err := p.Probe(context.Background(), shas[0], "go:example.com/m/p:TestC/with_space")
	if err != nil || st != StateFail {
		t.Fatalf("Probe = %v, %v", st, err)
	}
	if got := gor.reqs[0].Targets[0].Name; got != "TestC/with_space" {
		t.Errorf("target name = %q", got)
	}
}

func TestProbeFailsCleanlyWhenTheWorktreeCannotBeCreated(t *testing.T) {
	tmp := isolatedTemp(t)
	dir, _ := linearRepo(t, 1)
	p, gor, _ := newProber(t, dir)
	st, err := p.Probe(context.Background(), "0123456789012345678901234567890123456789", "go:example.com/m/p:TestB")
	if err == nil || st != StateUnknown || len(gor.reqs) != 0 {
		t.Errorf("Probe = %v, %v with %d runs; want an error and no run", st, err, len(gor.reqs))
	}
	if entries, rerr := os.ReadDir(tmp); rerr != nil || len(entries) != 0 {
		t.Errorf("temporary files left behind: %v (err %v)", entries, rerr)
	}
}

func TestProbeRefusesAnIDItCannotRun(t *testing.T) {
	isolatedTemp(t)
	dir, shas := linearRepo(t, 1)
	p, gor, pyr := newProber(t, dir)
	for _, id := range []string{"zz:x:y", "go:nopackage", "go::TestB", "go:example.com/m/p:", "py:onlyqualifier", "", "go:-bad:TestB"} {
		if st, err := p.Probe(context.Background(), shas[0], id); err == nil || st != StateUnknown {
			t.Errorf("Probe(%q) = %v, %v; want an error", id, st, err)
		}
	}
	if len(gor.reqs) != 0 || len(pyr.reqs) != 0 {
		t.Error("nothing may run for an ID that cannot be named to a runner")
	}
}

func TestProbeCanceledContext(t *testing.T) {
	isolatedTemp(t)
	dir, shas := linearRepo(t, 1)
	p, gor, _ := newProber(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if st, err := p.Probe(ctx, shas[0], "go:example.com/m/p:TestB"); err == nil || st != StateUnknown || len(gor.reqs) != 0 {
		t.Errorf("Probe = %v, %v with %d runs", st, err, len(gor.reqs))
	}
	if n := worktreeCount(t, dir); n != 1 {
		t.Errorf("git lists %d worktrees", n)
	}
}

func TestProbePythonNamesTheNodeFromTheCommitsFiles(t *testing.T) {
	isolatedTemp(t)
	dir, _ := linearRepo(t, 1)
	commitFile(t, dir, "tests/test_x.py", "def test_b(): pass\n\nclass TestK:\n    def test_a(self): pass\n")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	failK := suiteXML(`<testcase classname="tests.test_x.TestK" name="test_a[1]"><failure/></testcase>`)
	for _, tc := range []struct {
		id, node string
		junit    string
		want     State
	}{
		{"py:tests.test_x.TestK:test_a[1]", "tests/test_x.py::TestK::test_a[1]", failK, StateFail},
		{"py:tests.test_x:test_b", "tests/test_x.py::test_b", suiteXML(`<testcase classname="tests.test_x" name="test_b"/>`), StatePass},
	} {
		p, _, pyr := newProber(t, dir)
		pyr.junit = tc.junit
		if tc.want == StateFail {
			pyr.res = runner.Result{ExitCode: 1}
		}
		st, err := p.Probe(context.Background(), head, tc.id)
		if err != nil || st != tc.want {
			t.Errorf("Probe(%s) = %v, %v; want %v", tc.id, st, err, tc.want)
			continue
		}
		req := pyr.reqs[0]
		if req.Full || len(req.Targets) != 1 || req.Targets[0].RunnerID != tc.node || req.Targets[0].Language != "python" {
			t.Errorf("Probe(%s) request = %+v, want node %s", tc.id, req, tc.node)
		}
		if len(req.Testpaths) == 0 {
			t.Errorf("the configured testpaths must be passed: %+v", req)
		}
	}
}

func TestProbePythonTestThatDoesNotExistAtTheCommitIsNotFailing(t *testing.T) {
	isolatedTemp(t)
	dir, shas := linearRepo(t, 1)
	p, _, pyr := newProber(t, dir)
	st, err := p.Probe(context.Background(), shas[0], "py:tests.test_gone.TestK:test_a")
	if err != nil || st != StatePass || len(pyr.reqs) != 0 {
		t.Errorf("a test whose file is absent: %v, %v with %d runs; want StatePass without running", st, err, len(pyr.reqs))
	}
	commitFile(t, dir, "tests/test_x.py", "def test_b(): pass\n")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	pyr.res = runner.Result{ExitCode: 5, Abstained: true, Reason: "pytest_missing_ids", Missing: []string{"x"}}
	st, err = p.Probe(context.Background(), head, "py:tests.test_x:test_b")
	if err != nil || st != StatePass {
		t.Errorf("an abstaining pytest means the node is absent here: %v, %v", st, err)
	}
}
