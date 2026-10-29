package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/config"
	"github.com/omorsi45/hazard/internal/index"
)

// mapHead is a HeadReader over an in-memory tree.
type mapHead map[string]string

func (m mapHead) List() ([]string, error) {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func (m mapHead) Read(rel string) ([]byte, error) {
	if s, ok := m[rel]; ok {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
}

const (
	goCoreTest = "package core\n\nimport \"testing\"\n\nfunc TestCore(t *testing.T) {\n\tif Core() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	goCoreNew  = goCoreTest + "\nfunc TestCoreNew(t *testing.T) {\n\tif Core() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	idCoreNew  = "go:example.com/m/core:TestCoreNew"
	idCoreMore = "go:example.com/m/core:TestCoreMore"

	pyModule  = "def test_a():\n    pass\n\n\ndef test_p(v):\n    pass\n\n\nclass TestK:\n    def test_b(self):\n        pass\n"
	pyHelpers = "def test_zzz():\n    pass\n"
)

// inventoryManifest is an index inventory of one Go package and one Python
// module with a parametrized test and a class.
func inventoryManifest() *index.Manifest {
	py := func(pkg, name, runner string) index.Test {
		return index.Test{ID: "py:" + pkg + ":" + name, Language: "python", Package: pkg, Name: name, File: "tests/test_mod.py", RunnerID: runner}
	}
	tests := []index.Test{
		{ID: idCore, Language: "go", Package: "example.com/m/core", Name: "TestCore", File: "core/core_test.go", RunnerID: "TestCore"},
		py("tests.test_mod", "test_a", "tests/test_mod.py::test_a"),
		py("tests.test_mod", "test_p[1]", "tests/test_mod.py::test_p[1]"),
		py("tests.test_mod", "test_p[a::b]", "tests/test_mod.py::test_p[a::b]"),
		py("tests.test_mod.TestK", "test_b", "tests/test_mod.py::TestK::test_b"),
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
	return &index.Manifest{Tests: tests}
}

func goTestFile(path string) change.ChangedFile {
	return change.ChangedFile{Path: path, Status: "M", Kind: change.KindTest, OldKind: change.KindTest, Language: "go", Package: "example.com/m/core"}
}

func pyTestFile(path string) change.ChangedFile {
	return change.ChangedFile{Path: path, Status: "M", Kind: change.KindTest, OldKind: change.KindTest, Language: "python"}
}

func TestWithHeadTests(t *testing.T) {
	t.Parallel()
	renamedGo := goTestFile("core/core_more_test.go")
	renamedGo.Status, renamedGo.OldPath = "R", "core/core_test.go"
	renamedAwayGo := goTestFile("core/helper.go")
	renamedAwayGo.Status, renamedAwayGo.OldPath, renamedAwayGo.Kind = "R", "core/core_test.go", change.KindSource
	sourceGo := goTestFile("core/core.go")
	sourceGo.Kind, sourceGo.OldKind = change.KindSource, change.KindSource
	noPackage := goTestFile("core/core_test.go")
	noPackage.Package = ""
	deletedGo := goTestFile("core/core_test.go")
	deletedGo.Status = "D"

	deletedPy := pyTestFile("tests/test_mod.py")
	deletedPy.Status = "D"
	sourcePy := pyTestFile("app/mod.py")
	sourcePy.Kind, sourcePy.OldKind = change.KindSource, change.KindSource
	renamedPy := pyTestFile("tests/test_renamed.py")
	renamedPy.Status, renamedPy.OldPath = "R", "tests/test_mod.py"
	renamedAwayPy := pyTestFile("tests/helpers.py")
	renamedAwayPy.Status, renamedAwayPy.OldPath, renamedAwayPy.Kind = "R", "tests/test_mod.py", change.KindSource
	conftest := pyTestFile("tests/conftest.py")
	conftest.Kind, conftest.OldKind = change.KindFixture, change.KindFixture

	cases := []struct {
		name       string
		file       change.ChangedFile
		head       mapHead
		wantReason string
		wantDetail string
		wantAdded  []string
	}{
		{name: "go: a new test function joins the inventory", file: goTestFile("core/core_test.go"), head: mapHead{"core/core_test.go": goCoreNew}, wantAdded: []string{idCoreNew}},
		{name: "go: nothing new adds nothing", file: goTestFile("core/core_test.go"), head: mapHead{"core/core_test.go": goCoreTest}},
		{name: "go: a renamed test file is read at its new path", file: renamedGo, head: mapHead{"core/core_more_test.go": goCoreNew}, wantAdded: []string{idCoreNew}},
		{name: "go: a file renamed away from a test name is still read", file: renamedAwayGo, head: mapHead{"core/helper.go": goCoreNew}, wantAdded: []string{idCoreNew}},
		{name: "go: a source file is not a test file", file: sourceGo, head: mapHead{"core/core.go": goCoreNew}},
		{name: "go: an unknown package is left alone", file: noPackage, head: mapHead{"core/core_test.go": goCoreNew}},
		{name: "go: a deleted test file has no content to read", file: deletedGo, head: mapHead{}},
		{name: "go: a test file that does not parse is an inventory error", file: goTestFile("core/core_test.go"), head: mapHead{"core/core_test.go": "package core\n\nfunc ("}, wantReason: "inventory_error", wantDetail: "core/core_test.go"},
		{name: "go: a test file that cannot be read is an inventory error", file: goTestFile("core/core_test.go"), head: mapHead{}, wantReason: "inventory_error", wantDetail: "core/core_test.go"},

		{name: "python: the same names, parametrized ones included, change nothing", file: pyTestFile("tests/test_mod.py"), head: mapHead{"tests/test_mod.py": pyModule}},
		{name: "python: a removed test needs no widening", file: pyTestFile("tests/test_mod.py"), head: mapHead{"tests/test_mod.py": "def test_a():\n    pass\n"}},
		{name: "python: a new function widens", file: pyTestFile("tests/test_mod.py"), head: mapHead{"tests/test_mod.py": pyModule + "\n\ndef test_new():\n    pass\n"}, wantReason: "python_inventory_changed", wantDetail: "test_new"},
		{name: "python: a new method widens", file: pyTestFile("tests/test_mod.py"), head: mapHead{"tests/test_mod.py": pyModule + "\n    def test_c(self):\n        pass\n"}, wantReason: "python_inventory_changed", wantDetail: "TestK::test_c"},
		{name: "python: a new class widens", file: pyTestFile("tests/test_mod.py"), head: mapHead{"tests/test_mod.py": pyModule + "\n\nclass TestNew:\n    def test_x(self):\n        pass\n"}, wantReason: "python_inventory_changed", wantDetail: "TestNew::test_x"},
		{name: "python: a source module is not a test module", file: sourcePy, head: mapHead{"app/mod.py": "def test_not_a_test():\n    pass\n"}},
		{name: "python: a deleted module has no content to read", file: deletedPy, head: mapHead{}},
		{name: "python: a fixture module is not a test module", file: conftest, head: mapHead{"tests/conftest.py": "def test_hook():\n    pass\n"}},
		{name: "python: a renamed module has new node IDs", file: renamedPy, head: mapHead{"tests/test_renamed.py": pyModule}, wantReason: "python_inventory_changed", wantDetail: "tests/test_renamed.py"},
		{name: "python: a module renamed away from a test name is still checked", file: renamedAwayPy, head: mapHead{"tests/helpers.py": pyHelpers}, wantReason: "python_inventory_changed", wantDetail: "test_zzz"},
		{name: "python: a module that cannot be read is an inventory error", file: pyTestFile("tests/test_mod.py"), head: mapHead{}, wantReason: "inventory_error", wantDetail: "tests/test_mod.py"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := inventoryManifest()
			before := ids2(m.Tests)
			cs := &change.ChangeSet{Files: []change.ChangedFile{tc.file}}

			out, reason, detail := withHeadTests(m, cs, tc.head)

			if reason != tc.wantReason || !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("reason %q detail %q, want reason %q with detail containing %q", reason, detail, tc.wantReason, tc.wantDetail)
			}
			if reason != "" {
				return
			}
			if !sameStrings(ids2(m.Tests), before) {
				t.Errorf("the index manifest was modified: %v, was %v", ids2(m.Tests), before)
			}
			added := difference(ids2(out.Tests), before)
			if !sameStrings(added, tc.wantAdded) {
				t.Errorf("added %v, want %v", added, tc.wantAdded)
			}
			if len(tc.wantAdded) == 0 && out != m {
				t.Error("nothing to add: the manifest must be returned as is")
			}
			if !sort.StringsAreSorted(ids2(out.Tests)) {
				t.Errorf("Tests are not sorted by ID: %v", ids2(out.Tests))
			}
		})
	}
}

func ids2(tests []index.Test) []string {
	out := make([]string, len(tests))
	for i, t := range tests {
		out[i] = t.ID
	}
	return out
}

// difference lists the elements of all that are not in before, sorted.
func difference(all, before []string) []string {
	have := map[string]bool{}
	for _, s := range before {
		have[s] = true
	}
	out := []string{}
	for _, s := range all {
		if !have[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// TestSelectRediscoversGoTestsOfChangedTestFiles is D055: the index inventory
// is from before the change, so tests that a changed Go test file defines at
// head are found again and selected, never dropped.
func TestSelectRediscoversGoTestsOfChangedTestFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c1 := f.git(t, "rev-parse", "HEAD")
	f.indexNow(t, false)
	allWithNew := sorted(append(append([]string(nil), diamondIDs...), idCoreNew))

	checkNew := func(t *testing.T, d Decision, wantAll []string) {
		t.Helper()
		if d.AbstainReason != "no_lock" {
			t.Fatalf("reason %q warnings %v, want the index to be usable", d.AbstainReason, d.Warnings)
		}
		var found *Selected
		for i := range d.Hypothetical {
			if d.Hypothetical[i].TestID == idCoreNew {
				found = &d.Hypothetical[i]
			}
		}
		if found == nil {
			t.Fatalf("Hypothetical = %v, want the new test %s selected", ids(d.Hypothetical), idCoreNew)
		}
		if found.Source != "certain" || found.Reason != "direct" || found.Package != "example.com/m/core" || found.RunnerID != "TestCoreNew" {
			t.Errorf("new test = %+v, want certain/direct in package core with its runner name", *found)
		}
		if !sameStrings(d.AllTests, wantAll) {
			t.Errorf("AllTests = %v, want %v", d.AllTests, wantAll)
		}
	}

	t.Run("a changed test file that does not parse runs the full suite", func(t *testing.T) {
		f.write(t, map[string]string{"core/core_test.go": "package core\n\nfunc ("})
		d := selectOK(t, Options{RepoDir: f.dir})
		if !d.FullSuite || d.AbstainReason != "inventory_error" {
			t.Fatalf("FullSuite %v reason %q, want inventory_error", d.FullSuite, d.AbstainReason)
		}
		if !strings.Contains(strings.Join(d.Warnings, "\n"), "core/core_test.go") {
			t.Errorf("Warnings = %v, want the file named", d.Warnings)
		}
		if d.Hypothetical != nil || len(d.Tests) != 0 {
			t.Errorf("an unknown inventory must not narrow: %v %v", ids(d.Hypothetical), ids(d.Tests))
		}
	})
	t.Run("an edit that adds no test leaves the inventory as it was", func(t *testing.T) {
		f.write(t, map[string]string{"core/core_test.go": strings.Replace(goCoreTest, "!= 1", "!= 1+0", 1)})
		d := selectOK(t, Options{RepoDir: f.dir})
		if d.AbstainReason != "no_lock" || !sameStrings(d.AllTests, diamondIDs) {
			t.Errorf("reason %q AllTests %v, want the unchanged inventory", d.AbstainReason, d.AllTests)
		}
	})
	t.Run("a new test in the work tree", func(t *testing.T) {
		f.write(t, map[string]string{"core/core_test.go": goCoreNew})
		checkNew(t, selectOK(t, Options{RepoDir: f.dir}), allWithNew)
	})
	c2 := ""
	t.Run("a new test at an explicit head", func(t *testing.T) {
		c2 = commitAll(t, f.dir, "add TestCoreNew")
		checkNew(t, selectOK(t, Options{RepoDir: f.dir, Base: c1, Head: c2}), allWithNew)
	})
	t.Run("a new test in a renamed test file", func(t *testing.T) {
		f.git(t, "mv", "core/core_test.go", "core/core_more_test.go")
		more := goCoreNew + "\nfunc TestCoreMore(t *testing.T) {\n\tif Core() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
		f.write(t, map[string]string{"core/core_more_test.go": more})
		c3 := commitAll(t, f.dir, "rename and add TestCoreMore")
		d := selectOK(t, Options{RepoDir: f.dir, Base: c2, Head: c3})
		checkNew(t, d, sorted(append(append([]string(nil), allWithNew...), idCoreMore)))
		var selectedMore bool
		for _, s := range d.Hypothetical {
			selectedMore = selectedMore || s.TestID == idCoreMore && s.Reason == "direct"
		}
		if !selectedMore {
			t.Errorf("Hypothetical = %v, want %s selected directly", ids(d.Hypothetical), idCoreMore)
		}
	})
}

var (
	pythonProbeOnce sync.Once
	pythonProbeMsg  string
)

// testPython returns an interpreter with pytest, or skips the test when there
// is none and HAZARD_REQUIRE_PYTEST is not 1, in which case it fails.
func testPython(t *testing.T) string {
	t.Helper()
	python := os.Getenv("HAZARD_TEST_PYTHON")
	if python == "" {
		python = "python"
	}
	pythonProbeOnce.Do(func() {
		out, err := exec.Command(python, "-c", "import pytest").CombinedOutput()
		if err != nil {
			pythonProbeMsg = "pytest is unavailable through " + python + ": " + strings.TrimSpace(string(out)) + " " + err.Error()
		}
	})
	if pythonProbeMsg != "" {
		if os.Getenv("HAZARD_REQUIRE_PYTEST") == "1" {
			t.Fatal(pythonProbeMsg)
		}
		t.Skip(pythonProbeMsg + " (set HAZARD_TEST_PYTHON to an interpreter with pytest, HAZARD_REQUIRE_PYTEST=1 to forbid skipping)")
	}
	return python
}

// TestSelectPythonTestModuleWithNewTestsRunsTheFullSuite: parametrized node IDs
// cannot be derived without collection, so a changed test module that defines
// a test the index does not know widens to the full suite.
func TestSelectPythonTestModuleWithNewTestsRunsTheFullSuite(t *testing.T) {
	t.Parallel()
	python := testPython(t)
	f := newFixtureFiles(t, map[string]string{
		"hazard.yml":      "version: 1\nlanguages:\n  go:\n    enabled: false\n",
		".gitignore":      ".hazard/\n__pycache__/\n.pytest_cache/\n",
		"tests/test_a.py": "def test_one():\n    assert 1 == 1\n",
	})
	cfg, err := config.Load(filepath.Join(f.dir, "hazard.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := index.Build(context.Background(), f.dir, cfg, index.Options{Python: python})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Publish(f.dir, m); err != nil {
		t.Fatal(err)
	}

	t.Run("an edit that adds no test stays narrowed", func(t *testing.T) {
		f.write(t, map[string]string{"tests/test_a.py": "def test_one():\n    assert 2 == 2\n"})
		d := selectOK(t, Options{RepoDir: f.dir})
		if d.AbstainReason != "no_lock" || len(d.Hypothetical) == 0 {
			t.Errorf("reason %q warnings %v hypothetical %v, want the index used and test_one selected", d.AbstainReason, d.Warnings, ids(d.Hypothetical))
		}
	})
	t.Run("a new test function widens", func(t *testing.T) {
		f.write(t, map[string]string{"tests/test_a.py": "def test_one():\n    assert 1 == 1\n\n\ndef test_two():\n    assert 2 == 2\n"})
		d := selectOK(t, Options{RepoDir: f.dir})
		if !d.FullSuite || d.AbstainReason != "python_inventory_changed" {
			t.Fatalf("FullSuite %v reason %q warnings %v, want python_inventory_changed", d.FullSuite, d.AbstainReason, d.Warnings)
		}
		if w := strings.Join(d.Warnings, "\n"); !strings.Contains(w, "tests/test_a.py") || !strings.Contains(w, "test_two") {
			t.Errorf("Warnings = %v, want the module and the new test named", d.Warnings)
		}
		if d.Hypothetical != nil || len(d.Tests) != 0 {
			t.Errorf("an unknown inventory must not narrow: %v %v", ids(d.Hypothetical), ids(d.Tests))
		}
	})
}
