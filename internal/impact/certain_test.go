package impact

import (
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

func sourceChange(path string) *change.ChangeSet {
	return changeSet(cf(path, "M", change.KindSource, hunk(3, 1, "Do")))
}

func TestGraphModeHopBound(t *testing.T) {
	coreTests := []string{testID("m/core", "TestCore"), testID("m/core", "TestCoreAux")}
	byHop := [][]string{
		coreTests,
		{testID("m/left", "TestLeft"), testID("m/right", "TestRight")},
		{testID("m/top", "TestTop")},
		{testID("m/app", "TestApp")},
		{testID("m/cli", "TestCli")},
	}
	for maxHops := 0; maxHops <= 5; maxHops++ {
		t.Run("max_hops="+string(rune('0'+maxHops)), func(t *testing.T) {
			in := graphInput(sourceChange("core/core.go"))
			in.Impact.MaxHops = maxHops
			r := mustL1(t, in)
			want := map[string]string{}
			for hop, ids := range byHop {
				if hop <= maxHops {
					for _, id := range ids {
						want[id] = "graph"
					}
				}
			}
			if got := certainMap(r); !reflect.DeepEqual(got, want) {
				t.Fatalf("certain = %v, want %v", got, want)
			}
			if r.FullReason != "" {
				t.Fatalf("FullReason = %q", r.FullReason)
			}
		})
	}
}

func TestGraphModeTestHopsAreUnbounded(t *testing.T) {
	in := graphInput(sourceChange("core/core.go"))
	in.Impact.MaxHops = 1
	r := mustL1(t, in)
	want := map[string]int{
		testID("m/core", "TestCore"): 0, testID("m/core", "TestCoreAux"): 0,
		testID("m/left", "TestLeft"): 1, testID("m/right", "TestRight"): 1,
		testID("m/top", "TestTop"): 2, testID("m/app", "TestApp"): 3, testID("m/cli", "TestCli"): 4,
	}
	if !reflect.DeepEqual(r.TestHops, want) {
		t.Fatalf("TestHops = %v, want %v", r.TestHops, want)
	}
	if !reflect.DeepEqual(r.ChangedPackages, []string{"m/core"}) {
		t.Fatalf("ChangedPackages = %v", r.ChangedPackages)
	}
	if r.PackageCount != 8 {
		t.Fatalf("PackageCount = %d, want 8 (7 Go packages and the Python test module own tests; py:app.mod owns none)", r.PackageCount)
	}
}

func TestChangedPackagesSortedUnion(t *testing.T) {
	cs := changeSet(
		cf("top/top.go", "M", change.KindSource, hunk(1, 1)),
		cf("core/extra.go", "A", change.KindSource, hunk(0, 0)),
		cf("left/left.go", "M", change.KindSource, hunk(1, 1)),
	)
	in := graphInput(cs)
	in.Impact.MaxHops = 0
	r := mustL1(t, in)
	if want := []string{"m/core", "m/left", "m/top"}; !reflect.DeepEqual(r.ChangedPackages, want) {
		t.Fatalf("ChangedPackages = %v, want %v", r.ChangedPackages, want)
	}
}

func TestDirectRule(t *testing.T) {
	renamedTest := func() change.ChangedFile {
		f := cf("left/renamed_test.go", "R", change.KindTest, hunk(1, 1))
		f.OldPath = "left/left_test.go"
		return f
	}
	cases := []struct {
		name  string
		files []change.ChangedFile
		want  map[string]string
	}{
		{
			name:  "a changed test file selects every test of its package",
			files: []change.ChangedFile{cf("core/core_test.go", "M", change.KindTest, hunk(5, 2, "TestCore"))},
			want:  map[string]string{testID("m/core", "TestCore"): "direct", testID("m/core", "TestCoreAux"): "direct"},
		},
		{
			name:  "a deleted test file selects the tests it held",
			files: []change.ChangedFile{cf("left/left_test.go", "D", change.KindTest, hunk(1, 9))},
			want:  map[string]string{testID("m/left", "TestLeft"): "direct"},
		},
		{
			name:  "a renamed test file selects through its old path",
			files: []change.ChangedFile{renamedTest()},
			want:  map[string]string{testID("m/left", "TestLeft"): "direct"},
		},
		{
			name:  "a changed Python test file selects the module and class tests",
			files: []change.ChangedFile{cf("tests/test_mod.py", "M", change.KindTest, hunk(4, 1))},
			want: map[string]string{
				"py:tests.test_mod:test_a": "direct", "py:tests.test_mod.TestK:test_b": "direct",
			},
		},
		{
			name:  "test data under a tests directory selects that package",
			files: []change.ChangedFile{{Path: "tests/data/sample.json", Status: "M", Kind: change.KindTest, Hunks: []change.Hunk{hunk(1, 1)}}},
			want: map[string]string{
				"py:tests.test_mod:test_a": "direct", "py:tests.test_mod.TestK:test_b": "direct",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := graphInput(changeSet(tc.files...))
			in.Impact.MaxHops = 0
			r := mustL1(t, in)
			if got := certainMap(r); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("certain = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDirectRuleBeatsGraphReason(t *testing.T) {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(3, 1)),
		cf("left/left_test.go", "M", change.KindTest, hunk(3, 1)),
	)
	in := graphInput(cs)
	in.Impact.MaxHops = 1
	got := certainMap(mustL1(t, in))
	// The changed test file puts m/left in ChangedPackages, so its importer m/top is one hop away.
	want := map[string]string{
		testID("m/core", "TestCore"): "graph", testID("m/core", "TestCoreAux"): "graph",
		testID("m/left", "TestLeft"):   "direct",
		testID("m/right", "TestRight"): "graph",
		testID("m/top", "TestTop"):     "graph",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestPythonSourceChangeReachesTestsThroughTheGraph(t *testing.T) {
	cs := changeSet(cf("app/mod.py", "M", change.KindSource, hunk(2, 1, "run")))
	for _, tc := range []struct {
		maxHops int
		want    map[string]string
	}{
		{0, map[string]string{}},
		{1, map[string]string{"py:tests.test_mod:test_a": "graph", "py:tests.test_mod.TestK:test_b": "graph"}},
	} {
		in := graphInput(cs)
		in.Impact.MaxHops = tc.maxHops
		if got := certainMap(mustL1(t, in)); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("max_hops %d: certain = %v, want %v", tc.maxHops, got, tc.want)
		}
	}
}

func TestFixtureRule(t *testing.T) {
	fixture := func(path string) change.ChangedFile {
		return change.ChangedFile{Path: path, Status: "M", Kind: change.KindFixture, Hunks: []change.Hunk{hunk(1, 1, "__file__")}}
	}
	movedFixture := func() change.ChangedFile {
		f := fixture("elsewhere/case.yaml")
		f.Status, f.OldPath = "R", "left/testdata/case.yaml"
		return f
	}
	cases := []struct {
		name  string
		files []change.ChangedFile
		want  map[string]string
	}{
		{"fixture under a package directory", []change.ChangedFile{fixture("left/testdata/case.yaml")}, map[string]string{testID("m/left", "TestLeft"): "fixture"}},
		{"fixture selects only its own package", []change.ChangedFile{fixture("core/testdata/x.json")}, map[string]string{testID("m/core", "TestCore"): "fixture", testID("m/core", "TestCoreAux"): "fixture"}},
		{"fixture outside every package", []change.ChangedFile{fixture("elsewhere/data.yaml")}, map[string]string{}},
		{"fixture moved out of a package", []change.ChangedFile{movedFixture()}, map[string]string{testID("m/left", "TestLeft"): "fixture"}},
		{"sibling directory sharing a prefix", []change.ChangedFile{fixture("leftover/data.yaml")}, map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := graphInput(changeSet(tc.files...))
			in.Impact.MaxHops = 0
			if got := certainMap(mustL1(t, in)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("certain = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFixtureReasonRanksAfterGraph(t *testing.T) {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(3, 1)),
		change.ChangedFile{Path: "core/testdata/x.json", Status: "M", Kind: change.KindFixture, Hunks: []change.Hunk{hunk(1, 1)}},
	)
	in := graphInput(cs)
	in.Impact.MaxHops = 0
	got := certainMap(mustL1(t, in))
	want := map[string]string{testID("m/core", "TestCore"): "graph", testID("m/core", "TestCoreAux"): "graph"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestDocsOnlyChangeSelectsOnlyAlwaysRun(t *testing.T) {
	cs := changeSet(
		cf("README.md", "M", change.KindDocs, hunk(1, 1)),
		cf("config/app.yaml", "M", change.KindConfig, hunk(1, 1)),
	)
	in := graphInput(cs)
	if got := mustL1(t, in).Certain; len(got) != 0 {
		t.Fatalf("docs-only change selected %v", got)
	}
	in = graphInput(cs)
	in.AlwaysRun = map[string]string{testID("m/solo", "TestSolo"): "quarantined"}
	for i := range in.Index.Tests {
		if in.Index.Tests[i].ID == testID("m/cli", "TestCli") {
			in.Index.Tests[i].Unstable = true
		}
	}
	got := certainMap(mustL1(t, in))
	want := map[string]string{testID("m/solo", "TestSolo"): "always_run", testID("m/cli", "TestCli"): "always_run"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestAlwaysRunKeepsUnknownIDsAndLowestPrecedence(t *testing.T) {
	in := graphInput(sourceChange("core/core.go"))
	in.Impact.MaxHops = 0
	in.AlwaysRun = map[string]string{
		testID("m/core", "TestCore"): "quarantined", // also graph: graph wins
		"go:m/ghost:TestGhost":       "manual",     // not in the inventory: still always-run
	}
	got := certainMap(mustL1(t, in))
	want := map[string]string{
		testID("m/core", "TestCore"): "graph", testID("m/core", "TestCoreAux"): "graph",
		"go:m/ghost:TestGhost": "always_run",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestCertainSortedAndUnique(t *testing.T) {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(3, 1)),
		cf("core/core_test.go", "M", change.KindTest, hunk(3, 1)),
	)
	in := graphInput(cs)
	in.AlwaysRun = map[string]string{testID("m/core", "TestCore"): "quarantined"}
	r := mustL1(t, in)
	seen := map[string]bool{}
	prev := ""
	for _, m := range r.Certain {
		if m.TestID <= prev {
			t.Fatalf("Certain not strictly sorted at %q after %q: %v", m.TestID, prev, r.Certain)
		}
		if seen[m.TestID] {
			t.Fatalf("duplicate member %q", m.TestID)
		}
		seen[m.TestID], prev = true, m.TestID
	}
	if len(r.Certain) == 0 {
		t.Fatal("the change must select tests for this check to mean anything")
	}
}

func TestUnresolvedTestPackageIsNeverReachable(t *testing.T) {
	in := graphInput(sourceChange("core/core.go"))
	in.Index.Tests = append(in.Index.Tests, index.Test{ID: "go:m/ghost:TestGhost", Language: "go", Package: "m/ghost", File: "ghost/ghost_test.go"})
	r := mustL1(t, in)
	if _, ok := certainMap(r)["go:m/ghost:TestGhost"]; ok {
		t.Fatalf("a test outside the graph must not be CERTAIN through the graph: %v", r.Certain)
	}
	if _, ok := r.TestHops["go:m/ghost:TestGhost"]; ok {
		t.Fatalf("a test outside the graph has no hop distance: %v", r.TestHops)
	}
}

func TestDirContains(t *testing.T) {
	for _, tc := range []struct {
		dir, path string
		want      bool
	}{
		{"left", "left/testdata/a", true},
		{"left", "leftover/a", false},
		{"left", "left", false},
		{".", "any/where", true},
		{"", "any/where", true},
		{"a/b/", "a/b/c", true},
	} {
		if got := dirContains(tc.dir, tc.path); got != tc.want {
			t.Errorf("dirContains(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}
