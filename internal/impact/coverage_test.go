package impact

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

// coverageIndex returns the fixture inventory padded to 20 tests (so one
// missing coverage entry is exactly 5%) with every test covered and no blocks.
func coverageIndex() *index.Manifest {
	m := fixtureIndex()
	for i := 1; i <= 10; i++ {
		m.Tests = append(m.Tests, goTest("m/solo", fmt.Sprintf("TestFill%02d", i)))
	}
	sort.Slice(m.Tests, func(i, j int) bool { return m.Tests[i].ID < m.Tests[j].ID })
	m.Coverage = &index.Coverage{
		HeadSHA:     "cov",
		Granularity: "go-top-level-test",
		Tests:       map[string]index.TestCoverage{},
		FileDigests: map[string]string{},
	}
	for _, tt := range m.Tests {
		m.Coverage.Tests[tt.ID] = index.TestCoverage{Status: "covered"}
	}
	return m
}

// cover sets the coverage of one test: status and blocks as {file, start, end}.
func cover(m *index.Manifest, id, status string, blocks ...index.Block) {
	m.Coverage.Tests[id] = index.TestCoverage{Status: status, Blocks: blocks}
}

func blk(file string, start, end int) index.Block {
	return index.Block{File: file, StartLine: start, EndLine: end}
}

// coverageInput is a ModeCoverage input whose coverage digests match the base
// digests of every changed file.
func coverageInput(cs *change.ChangeSet) Input {
	in := Input{
		Change: cs, Index: coverageIndex(), Impact: defaultImpact(), Mode: ModeCoverage,
		CoverageAge: 5, BaseDigests: map[string]string{},
	}
	in.Impact.MaxHops = 1
	for _, f := range cs.Files {
		base := f.Path
		if f.OldPath != "" {
			base = f.OldPath
		}
		in.BaseDigests[base] = "sha:" + base
		in.Index.Coverage.FileDigests[base] = "sha:" + base
	}
	return in
}

func idsWithReason(r *Result, reason string) []string {
	var ids []string
	for _, m := range r.Certain {
		if m.Reason == reason {
			ids = append(ids, m.TestID)
		}
	}
	return ids
}

func TestCoverageFreshSelection(t *testing.T) {
	in := coverageInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do"))))
	m := in.Index
	cover(m, testID("m/core", "TestCore"), "covered", blk("core/core.go", 5, 11))
	cover(m, testID("m/core", "TestCoreAux"), "covered", blk("core/core.go", 20, 30)) // reachable but does not run the changed lines
	cover(m, testID("m/left", "TestLeft"), "covered", blk("left/left.go", 1, 5))
	cover(m, testID("m/solo", "TestSolo"), "covered", blk("core/core.go", 12, 12)) // unrelated by graph, runs the changed line
	cover(m, testID("m/right", "TestRight"), "failed")
	delete(m.Coverage.Tests, testID("m/top", "TestTop")) // missing from the map
	cover(m, testID("m/app", "TestApp"), "not_run")
	cover(m, testID("m/cli", "TestCli"), "covered", blk("cli/cli.go", 1, 9))
	r := mustL1(t, in)
	if r.CoverageState != "fresh" || r.FullReason != "" {
		t.Fatalf("state %q full %q, want fresh and no full reason", r.CoverageState, r.FullReason)
	}
	want := map[string]string{
		testID("m/core", "TestCore"): "coverage",
		testID("m/solo", "TestSolo"): "coverage",
		// No usable coverage: unbounded reverse reachability, past MaxHops=1.
		testID("m/right", "TestRight"): "graph",
		testID("m/top", "TestTop"):     "graph",
		testID("m/app", "TestApp"):     "graph",
	}
	if got := certainMap(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestCoverageDigestMismatchFallsBackForThatFileOnly(t *testing.T) {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")),
		cf("left/left.go", "M", change.KindSource, hunk(4, 2, "Run")),
	)
	in := coverageInput(cs)
	in.Index.Coverage.FileDigests["left/left.go"] = "sha:left/left.go:collected-earlier"
	m := in.Index
	cover(m, testID("m/core", "TestCore"), "covered", blk("core/core.go", 9, 12))
	cover(m, testID("m/core", "TestCoreAux"), "covered", blk("core/core.go", 50, 60))
	cover(m, testID("m/right", "TestRight"), "covered", blk("right/right.go", 1, 1))
	cover(m, testID("m/solo", "TestSolo"), "covered", blk("left/left.go", 3, 5)) // overlaps left.go, but that file's coverage is unusable
	r := mustL1(t, in)
	if r.CoverageState != "fresh" {
		t.Fatalf("CoverageState = %q, want fresh", r.CoverageState)
	}
	want := map[string]string{
		testID("m/core", "TestCore"): "coverage",
		// left.go is coverage-unknown: its package and everything above it, unbounded.
		testID("m/left", "TestLeft"): "graph",
		testID("m/top", "TestTop"):   "graph",
		testID("m/app", "TestApp"):   "graph",
		testID("m/cli", "TestCli"):   "graph",
	}
	if got := certainMap(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestCoverageMissingDigestIsUnknown(t *testing.T) {
	for name, edit := range map[string]func(*Input){
		"no base digest":     func(in *Input) { delete(in.BaseDigests, "core/core.go") },
		"no coverage digest": func(in *Input) { delete(in.Index.Coverage.FileDigests, "core/core.go") },
		"empty digests": func(in *Input) {
			in.BaseDigests["core/core.go"], in.Index.Coverage.FileDigests["core/core.go"] = "", ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := coverageInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do"))))
			cover(in.Index, testID("m/core", "TestCore"), "covered", blk("core/core.go", 9, 11))
			edit(&in)
			got := certainMap(mustL1(t, in))
			if got[testID("m/core", "TestCore")] != "graph" || got[testID("m/cli", "TestCli")] != "graph" {
				t.Fatalf("an unverifiable digest must fall back to the unbounded graph: %v", got)
			}
		})
	}
}

func TestCoverageSourceFileWithoutHunksIsUnknown(t *testing.T) {
	f := cf("core/core.go", "M", change.KindSource)
	in := coverageInput(changeSet(f))
	got := certainMap(mustL1(t, in))
	if got[testID("m/core", "TestCore")] != "graph" || got[testID("m/cli", "TestCli")] != "graph" {
		t.Fatalf("a changed source file with no hunks cannot be proven unreachable: %v", got)
	}
}

func TestCoverageOldSideLineRanges(t *testing.T) {
	cases := []struct {
		name  string
		hunks []change.Hunk
		block index.Block
		want  bool
	}{
		{"block ends before the range", []change.Hunk{hunk(10, 3)}, blk("core/core.go", 5, 9), false},
		{"block ends on the first line", []change.Hunk{hunk(10, 3)}, blk("core/core.go", 5, 10), true},
		{"block inside the range", []change.Hunk{hunk(10, 3)}, blk("core/core.go", 11, 11), true},
		{"block starts on the last line", []change.Hunk{hunk(10, 3)}, blk("core/core.go", 12, 20), true},
		{"block starts after the range", []change.Hunk{hunk(10, 3)}, blk("core/core.go", 13, 20), false},
		{"insertion covers the line before", []change.Hunk{hunk(20, 0)}, blk("core/core.go", 19, 20), true},
		{"insertion covers the line after", []change.Hunk{hunk(20, 0)}, blk("core/core.go", 21, 22), true},
		{"insertion two lines after", []change.Hunk{hunk(20, 0)}, blk("core/core.go", 22, 23), false},
		{"insertion two lines before", []change.Hunk{hunk(20, 0)}, blk("core/core.go", 18, 19), false},
		{"second hunk", []change.Hunk{hunk(10, 3), hunk(40, 2)}, blk("core/core.go", 41, 41), true},
		{"gap between hunks", []change.Hunk{hunk(10, 3), hunk(40, 2)}, blk("core/core.go", 20, 30), false},
		{"block in another file", []change.Hunk{hunk(10, 3)}, blk("core/other.go", 10, 12), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := coverageInput(changeSet(cf("core/core.go", "M", change.KindSource, tc.hunks...)))
			// A test that runs the whole file keeps every changed line inside some
			// block, so the file stays coverage-known and only TestSolo's block
			// decides whether TestSolo is selected.
			cover(in.Index, testID("m/solo", "TestFill01"), "covered", blk("core/core.go", 1, 100))
			cover(in.Index, testID("m/solo", "TestSolo"), "covered", tc.block)
			_, got := certainMap(mustL1(t, in))[testID("m/solo", "TestSolo")]
			if got != tc.want {
				t.Fatalf("selected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCoverageDirectAndAlwaysRunStillApply(t *testing.T) {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(10, 3)),
		cf("left/left_test.go", "M", change.KindTest, hunk(2, 1)),
	)
	in := coverageInput(cs)
	cover(in.Index, testID("m/core", "TestCore"), "covered", blk("core/core.go", 10, 12))
	in.AlwaysRun = map[string]string{testID("m/cli", "TestCli"): "quarantined"}
	got := certainMap(mustL1(t, in))
	want := map[string]string{
		testID("m/core", "TestCore"): "coverage",
		testID("m/left", "TestLeft"): "direct",
		testID("m/cli", "TestCli"):   "always_run",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestCoverageStalenessWidensToTheUnboundedGraph(t *testing.T) {
	core := changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")))
	added := changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")), cf("core/extra.go", "A", change.KindSource, hunk(0, 0)))
	renamed := func() *change.ChangeSet {
		r := cf("core/moved.go", "R", change.KindSource, hunk(1, 1))
		r.OldPath = "core/old.go"
		return changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")), r)
	}
	cases := []struct {
		name  string
		cs    *change.ChangeSet
		edit  func(*Input)
		state string
	}{
		{"missing", core, func(in *Input) { in.Index.Coverage = nil }, "missing"},
		{"not an ancestor", core, func(in *Input) { in.CoverageAge = -1 }, "stale:not_ancestor"},
		{"too old", core, func(in *Input) { in.CoverageAge = 201 }, "stale:too_old"},
		{"added file", added, nil, "stale:added_file"},
		{"renamed file", renamed(), nil, "stale:added_file"},
		{"inventory shrank", core, func(in *Input) {
			delete(in.Index.Coverage.Tests, testID("m/solo", "TestFill01"))
			delete(in.Index.Coverage.Tests, testID("m/solo", "TestFill02"))
		}, "stale:inventory_changed"},
		{"inventory grew", core, func(in *Input) {
			in.Index.Coverage.Tests["go:m/solo:TestOld1"] = index.TestCoverage{Status: "covered"}
			in.Index.Coverage.Tests["go:m/solo:TestOld2"] = index.TestCoverage{Status: "covered"}
		}, "stale:inventory_changed"},
		{"inventory swapped", core, func(in *Input) {
			delete(in.Index.Coverage.Tests, testID("m/solo", "TestFill01"))
			in.Index.Coverage.Tests["go:m/solo:TestOld1"] = index.TestCoverage{Status: "covered"}
		}, "stale:inventory_changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := coverageInput(tc.cs)
			in.Impact.MaxHops = 0 // the stale fallback must ignore the hop bound
			if in.Index.Coverage != nil {
				// Would be selected by coverage if the coverage were trusted.
				cover(in.Index, testID("m/solo", "TestSolo"), "covered", blk("core/core.go", 10, 12))
			}
			if tc.edit != nil {
				tc.edit(&in)
			}
			r := mustL1(t, in)
			if r.CoverageState != tc.state {
				t.Fatalf("CoverageState = %q, want %q", r.CoverageState, tc.state)
			}
			if r.FullReason != "" {
				t.Fatalf("FullReason = %q", r.FullReason)
			}
			got := certainMap(r)
			for id := range got {
				if got[id] == "coverage" {
					t.Fatalf("stale coverage must not select by coverage: %v", got)
				}
			}
			want := map[string]string{}
			for id := range r.TestHops {
				want[id] = "graph"
			}
			if len(want) < 7 {
				t.Fatalf("the change should reach the whole core chain, TestHops = %v", r.TestHops)
			}
			if _, ok := got[testID("m/solo", "TestSolo")]; ok {
				t.Fatalf("an unreachable test must not be selected: %v", got)
			}
			for id, reason := range want {
				if got[id] != reason {
					t.Fatalf("%s: reason %q, want %q (certain %v)", id, got[id], reason, got)
				}
			}
		})
	}
}

func TestCoverageFreshBoundaries(t *testing.T) {
	core := changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")))
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"age equal to the maximum", func(in *Input) { in.CoverageAge = 200 }},
		{"age zero", func(in *Input) { in.CoverageAge = 0 }},
		{"inventory differs by exactly 5%", func(in *Input) { delete(in.Index.Coverage.Tests, testID("m/solo", "TestFill01")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := coverageInput(core)
			tc.edit(&in)
			if got := mustL1(t, in).CoverageState; got != "fresh" {
				t.Fatalf("CoverageState = %q, want fresh", got)
			}
		})
	}
}

func TestCoverageStateByMode(t *testing.T) {
	in := graphInput(sourceChange("core/core.go"))
	in.Impact.CoverageRequired = true
	r := mustL1(t, in)
	if r.CoverageState != "not_requested" || r.FullReason != "" {
		t.Fatalf("ModeGraph: state %q full %q, want not_requested and no full reason", r.CoverageState, r.FullReason)
	}
}

func TestCoverageRequiredRefusesStaleCoverage(t *testing.T) {
	core := changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")))
	cases := []struct {
		name string
		edit func(*Input)
		want string
	}{
		{"missing", func(in *Input) { in.Index.Coverage = nil }, "coverage_required:missing"},
		{"not an ancestor", func(in *Input) { in.CoverageAge = -1 }, "coverage_required:stale:not_ancestor"},
		{"too old", func(in *Input) { in.CoverageAge = 500 }, "coverage_required:stale:too_old"},
		{"fresh", func(in *Input) {}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := coverageInput(core)
			in.Impact.CoverageRequired = true
			tc.edit(&in)
			r := mustL1(t, in)
			if r.FullReason != tc.want {
				t.Fatalf("FullReason = %q, want %q", r.FullReason, tc.want)
			}
			if tc.want != "" && (len(r.Certain) != 0 || len(r.Candidates) != 0) {
				t.Fatalf("a refusal carries no selection: %v", r.Certain)
			}
			if tc.want != "" && len(r.AllTests) != 20 {
				t.Fatalf("AllTests = %d entries, want the 20-test inventory", len(r.AllTests))
			}
		})
	}
}

func TestStaleCoverageWithoutRequirementStillSelects(t *testing.T) {
	in := coverageInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do"))))
	in.CoverageAge = -1
	in.Impact.CoverageRequired = false
	r := mustL1(t, in)
	if r.FullReason != "" || len(idsWithReason(r, "graph")) != 7 {
		t.Fatalf("stale coverage without the requirement must widen to the graph: full %q graph %v", r.FullReason, idsWithReason(r, "graph"))
	}
}

// TestCoverageChangedLineOutsideEveryBlockIsUnknown: a changed line that lies
// in no coverage block of any test carries no coverage information (Go never
// instruments a package-level variable initializer, and a profile of executed
// blocks cannot tell "not instrumented" from "never executed"). It is unknown,
// not evidence that no test depends on it, so the unbounded graph rule applies.
func TestCoverageChangedLineOutsideEveryBlockIsUnknown(t *testing.T) {
	in := coverageInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(32, 1, "table"))))
	cover(in.Index, testID("m/core", "TestCore"), "covered", blk("core/core.go", 10, 20))
	r := mustL1(t, in)
	if r.CoverageState != "fresh" || r.FullReason != "" {
		t.Fatalf("state %q full %q, want fresh and no full reason", r.CoverageState, r.FullReason)
	}
	got := certainMap(r)
	want := map[string]string{}
	for id := range r.TestHops {
		want[id] = "graph"
	}
	if len(want) < 7 {
		t.Fatalf("the change should reach the whole core chain, TestHops = %v", r.TestHops)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want every test reachable from m/core with reason graph: %v", got, want)
	}
	if _, ok := got[testID("m/solo", "TestSolo")]; ok {
		t.Fatalf("an unreachable test must not be selected: %v", got)
	}
}

// TestCoverageChangedLineInsideABlockStaysNarrow is the control of the test
// above: the same blocks and a change that lies inside them select by coverage
// only.
func TestCoverageChangedLineInsideABlockStaysNarrow(t *testing.T) {
	in := coverageInput(changeSet(cf("core/core.go", "M", change.KindSource, hunk(15, 1, "Do"))))
	cover(in.Index, testID("m/core", "TestCore"), "covered", blk("core/core.go", 10, 20))
	got := certainMap(mustL1(t, in))
	want := map[string]string{testID("m/core", "TestCore"): "coverage"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}

func TestCoverageUnknownLineClassification(t *testing.T) {
	core := testID("m/core", "TestCore")
	aux := testID("m/core", "TestCoreAux")
	cases := []struct {
		name   string
		hunks  []change.Hunk
		setup  func(*index.Manifest)
		narrow map[string]string // nil: the whole reverse graph of m/core, reason graph
	}{
		{
			name:  "no test has any block",
			hunks: []change.Hunk{hunk(10, 3)},
			setup: func(*index.Manifest) {},
		},
		{
			name:  "only part of the change is inside a block",
			hunks: []change.Hunk{hunk(18, 15)},
			setup: func(m *index.Manifest) { cover(m, core, "covered", blk("core/core.go", 10, 20)) },
		},
		{
			name:  "one of two hunks is outside every block",
			hunks: []change.Hunk{hunk(12, 1), hunk(40, 1)},
			setup: func(m *index.Manifest) { cover(m, core, "covered", blk("core/core.go", 10, 20)) },
		},
		{
			name:  "the blocks cover another file only",
			hunks: []change.Hunk{hunk(32, 1)},
			setup: func(m *index.Manifest) { cover(m, core, "covered", blk("core/other.go", 30, 35)) },
		},
		{
			name:  "the blocks of two tests together cover the change",
			hunks: []change.Hunk{hunk(10, 5)},
			setup: func(m *index.Manifest) {
				cover(m, core, "covered", blk("core/core.go", 10, 12))
				cover(m, aux, "covered", blk("core/core.go", 13, 14))
			},
			narrow: map[string]string{core: "coverage", aux: "coverage"},
		},
		{
			name:  "the lines are in the blocks of a test whose coverage is unusable",
			hunks: []change.Hunk{hunk(32, 1)},
			setup: func(m *index.Manifest) {
				cover(m, core, "covered", blk("core/core.go", 10, 20))
				cover(m, testID("m/right", "TestRight"), "failed", blk("core/core.go", 30, 35))
			},
			// The lines are instrumented and a test ran them, so they are known;
			// that test has no usable coverage and falls back to the graph.
			narrow: map[string]string{testID("m/right", "TestRight"): "graph"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := coverageInput(changeSet(cf("core/core.go", "M", change.KindSource, tc.hunks...)))
			tc.setup(in.Index)
			r := mustL1(t, in)
			want := tc.narrow
			if want == nil {
				want = map[string]string{}
				for id := range r.TestHops {
					want[id] = "graph"
				}
				if len(want) < 7 {
					t.Fatalf("the change should reach the whole core chain, TestHops = %v", r.TestHops)
				}
			}
			if got := certainMap(r); !reflect.DeepEqual(got, want) {
				t.Fatalf("certain = %v, want %v", got, want)
			}
		})
	}
}

// TestCoverageUnknownLineWidensOnlyItsOwnFile: the fallback is per file. A file
// whose every changed line lies in a block keeps coverage selection while the
// file with an uninstrumented changed line widens to the graph of its package.
func TestCoverageUnknownLineWidensOnlyItsOwnFile(t *testing.T) {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")),
		cf("left/left.go", "M", change.KindSource, hunk(4, 2, "table")),
	)
	in := coverageInput(cs)
	m := in.Index
	cover(m, testID("m/core", "TestCore"), "covered", blk("core/core.go", 9, 12))
	cover(m, testID("m/core", "TestCoreAux"), "covered", blk("core/core.go", 50, 60))
	cover(m, testID("m/solo", "TestSolo"), "covered", blk("left/left.go", 20, 30)) // left.go lines 4-5 are in no block
	got := certainMap(mustL1(t, in))
	want := map[string]string{
		testID("m/core", "TestCore"): "coverage",
		// left.go is unknown: its package and everything above it, unbounded.
		testID("m/left", "TestLeft"): "graph",
		testID("m/top", "TestTop"):   "graph",
		testID("m/app", "TestApp"):   "graph",
		testID("m/cli", "TestCli"):   "graph",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("certain = %v, want %v", got, want)
	}
}
