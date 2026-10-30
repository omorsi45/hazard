package impact

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/index"
)

// stubHistory is a map-driven History.
type stubHistory struct {
	cofail map[string][2]float64 // test ID -> {rate, support}
	fail   map[string]float64
	days   map[string]float64
	median map[string]int
}

func (h stubHistory) Cofailure(id string, _ []string) (float64, int) {
	v := h.cofail[id]
	return v[0], int(v[1])
}
func (h stubHistory) FailRate(id string) float64 { return h.fail[id] }
func (h stubHistory) DaysSinceLastFail(id string) (float64, bool) {
	d, ok := h.days[id]
	return d, ok
}
func (h stubHistory) MedianMS(id string) (int, bool) {
	ms, ok := h.median[id]
	return ms, ok
}

func near(got, want float64) bool { return math.Abs(got-want) < 1e-12 }

// rankingScenario is a ModeGraph change of core with MaxHops 0, so exactly the
// two core tests are CERTAIN and the other eight are candidates.
func rankingScenario() Input {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(3, 1, "Do", "(*Core).Run", "__file__", "_")),
		cf("README.md", "M", change.KindDocs, hunk(1, 2)),
	)
	in := graphInput(cs)
	in.Impact.MaxHops = 0
	in.History = stubHistory{
		cofail: map[string][2]float64{testID("m/left", "TestLeft"): {0.5, 4}, testID("m/solo", "TestSolo"): {0.625, 5}},
		fail:   map[string]float64{testID("m/left", "TestLeft"): 0.25},
		days:   map[string]float64{testID("m/left", "TestLeft"): 30},
		median: map[string]int{testID("m/left", "TestLeft"): 120},
	}
	files := map[string]string{
		"left/left_test.go": "package left\n\nfunc TestLeft() { core.Do(); core.Doing(); var _ = Core{} }\n",
	}
	in.ReadFile = func(rel string) ([]byte, error) {
		if s, ok := files[rel]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("not found")
	}
	return in
}

func TestRankFormulaVectors(t *testing.T) {
	r := mustL1(t, rankingScenario())
	wantOrder := []string{
		testID("m/left", "TestLeft"), testID("m/right", "TestRight"), testID("m/solo", "TestSolo"),
		testID("m/top", "TestTop"), testID("m/app", "TestApp"), testID("m/cli", "TestCli"),
		"py:tests.test_mod.TestK:test_b", "py:tests.test_mod:test_a",
	}
	if got := candidateIDs(r); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("candidate order = %v, want %v", got, wantOrder)
	}
	wantRank := []float64{
		0.25*0.5 + 0.20*0.5 + 0.10*math.Exp(-1) + 0.05*1, // hop 1, cofailure 0.5, 30 days, shared symbol
		0.25 * 0.5,       // hop 1
		0.20 * 0.625,     // unreachable, cofailure 0.625
		0.25 * (1.0 / 3), // hop 2
		0.25 * (1.0 / 4), // hop 3
		0.25 * (1.0 / 5), // hop 4
		0, 0,
	}
	for i, c := range r.Candidates {
		if !near(c.Rank, wantRank[i]) {
			t.Errorf("%s: Rank = %.15f, want %.15f", c.TestID, c.Rank, wantRank[i])
		}
	}
	if r.RawCandidates != 8 || r.Truncated != 0 {
		t.Fatalf("RawCandidates %d Truncated %d, want 8 and 0", r.RawCandidates, r.Truncated)
	}
	if r.Candidates[1].Rank != r.Candidates[2].Rank {
		t.Fatalf("the hop and cofailure candidates must tie exactly: %.20f vs %.20f", r.Candidates[1].Rank, r.Candidates[2].Rank)
	}
}

func TestCandidateFeatures(t *testing.T) {
	r := mustL1(t, rankingScenario())
	byID := map[string]Candidate{}
	for _, c := range r.Candidates {
		byID[c.TestID] = c
	}
	kinds := [8]int{}
	kinds[change.KindSource], kinds[change.KindDocs] = 1, 1
	left := byID[testID("m/left", "TestLeft")]
	wantLeft := Features{
		Hops: 1, Reachable: true, CofailureRate: 0.5, CofailureSupport: 4, TestFailRate: 0.25,
		DaysSinceLastFail: 30, SharedSymbolHits: 2, ChangedFileKinds: kinds,
		LinesAdded: 3, LinesRemoved: 3, FilesChanged: 2, TestMedianMS: 120,
	}
	if !reflect.DeepEqual(left.Features, wantLeft) {
		t.Fatalf("TestLeft features = %+v, want %+v", left.Features, wantLeft)
	}
	if left.TestName != "TestLeft" || left.TestFile != "left/left_test.go" || left.Package != "m/left" {
		t.Fatalf("candidate identity = %+v", left)
	}
	solo := byID[testID("m/solo", "TestSolo")]
	wantSolo := Features{
		Hops: 99, CofailureRate: 0.625, CofailureSupport: 5, DaysSinceLastFail: 365, ChangedFileKinds: kinds,
		LinesAdded: 3, LinesRemoved: 3, FilesChanged: 2,
	}
	if !reflect.DeepEqual(solo.Features, wantSolo) {
		t.Fatalf("TestSolo features = %+v, want %+v", solo.Features, wantSolo)
	}
	v := left.Features.Vector()
	want := []float64{
		1, 1, 0, 0, 0, 0.5, 4, 0.25, 30, 2, 0, 0,
		1, 0, 0, 0, 0, 1, 0, 0,
		3, 3, 2, 120, 0,
	}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("Vector() = %v, want %v", v, want)
	}
}

func TestSharedSymbolIsWholeIdentifierAndNeedsReadFile(t *testing.T) {
	in := rankingScenario()
	in.ReadFile = nil
	r := mustL1(t, in)
	for _, c := range r.Candidates {
		if c.Features.SharedSymbolHits != 0 {
			t.Fatalf("%s: SharedSymbolHits = %d without a reader, want 0", c.TestID, c.Features.SharedSymbolHits)
		}
	}
	in = rankingScenario()
	in.ReadFile = func(string) ([]byte, error) {
		return []byte("func TestX() { Doing(); undo(); Do_it(); x.Do }\n"), nil // Do only as a substring or as x.Do
	}
	r = mustL1(t, in)
	for _, c := range r.Candidates {
		if c.Features.SharedSymbolHits != 1 {
			t.Fatalf("%s: SharedSymbolHits = %d, want 1 (Do as a whole identifier once)", c.TestID, c.Features.SharedSymbolHits)
		}
	}
	for _, c := range r.Candidates {
		want := 0.25 * (1 / (1 + float64(c.Features.Hops)))
		if !c.Features.Reachable {
			want = 0
		}
		want += 0.05
		if c.TestID == testID("m/solo", "TestSolo") {
			want += 0.20 * 0.625
		}
		if c.TestID == testID("m/left", "TestLeft") {
			want += 0.20*0.5 + 0.10*math.Exp(-1)
		}
		if !near(c.Rank, want) {
			t.Errorf("%s: Rank = %.15f, want %.15f", c.TestID, c.Rank, want)
		}
	}
}

func TestCandidateTieBreakFollowsCandidateRanking(t *testing.T) {
	right, solo := testID("m/right", "TestRight"), testID("m/solo", "TestSolo")
	for _, tc := range []struct {
		name    string
		ranking []string
		first   string
	}{
		{"default ranking prefers hops over cofailure", []string{"coverage_overlap", "hops", "cofailure", "recency"}, right},
		{"cofailure first prefers the cofailing test", []string{"cofailure", "hops", "coverage_overlap", "recency"}, solo},
		{"no ranking tokens falls back to test ID", nil, right},
		{"unlisted token does not break the tie", []string{"recency"}, right},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := rankingScenario()
			in.Impact.CandidateRanking = tc.ranking
			ids := candidateIDs(mustL1(t, in))
			posRight, posSolo := indexOf(ids, right), indexOf(ids, solo)
			first := right
			if posSolo < posRight {
				first = solo
			}
			if first != tc.first {
				t.Fatalf("order = %v, want %s first", ids, tc.first)
			}
		})
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func TestRankedLess(t *testing.T) {
	mk := func(id string, rank float64, c components) ranked {
		return ranked{cand: Candidate{TestID: id, Rank: rank}, comp: c}
	}
	cases := []struct {
		name  string
		a, b  ranked
		order []string
		want  bool // a sorts before b
	}{
		{"higher rank wins whatever the components", mk("b", 0.6, components{}), mk("a", 0.5, components{overlap: 1, hop: 1, cofailure: 1, recency: 1}), []string{"coverage_overlap"}, true},
		{"overlap breaks a tie", mk("b", 0.5, components{overlap: 0.5}), mk("a", 0.5, components{cofailure: 1}), []string{"coverage_overlap", "cofailure"}, true},
		{"order decides which component first", mk("b", 0.5, components{overlap: 0.5}), mk("a", 0.5, components{cofailure: 1}), []string{"cofailure", "coverage_overlap"}, false},
		{"hop proximity breaks a tie", mk("b", 0.5, components{hop: 0.5}), mk("a", 0.5, components{}), []string{"hops"}, true},
		{"cofailure breaks a tie", mk("b", 0.5, components{cofailure: 0.3}), mk("a", 0.5, components{cofailure: 0.1}), []string{"cofailure"}, true},
		{"recency breaks a tie", mk("b", 0.5, components{recency: 0.9}), mk("a", 0.5, components{recency: 0.2}), []string{"recency"}, true},
		{"a later component is used when an earlier one is equal", mk("b", 0.5, components{hop: 0.5, recency: 0.9}), mk("a", 0.5, components{hop: 0.5, recency: 0.2}), []string{"hops", "recency"}, true},
		{"equal everywhere falls to ID ascending", mk("a", 0.5, components{}), mk("b", 0.5, components{}), []string{"hops"}, true},
		{"equal everywhere, later ID last", mk("b", 0.5, components{}), mk("a", 0.5, components{}), []string{"hops"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rankedLess(tc.a, tc.b, tc.order); got != tc.want {
				t.Fatalf("rankedLess = %v, want %v", got, tc.want)
			}
			if got := rankedLess(tc.b, tc.a, tc.order); got == tc.want {
				t.Fatalf("rankedLess is not antisymmetric: both directions gave %v", got)
			}
		})
	}
}

func TestComponentsRankWeights(t *testing.T) {
	for _, tc := range []struct {
		c    components
		want float64
	}{
		{components{overlap: 1}, 0.40},
		{components{hop: 1}, 0.25},
		{components{cofailure: 1}, 0.20},
		{components{recency: 1}, 0.10},
		{components{symbol: 1}, 0.05},
		{components{overlap: 1, hop: 1, cofailure: 1, recency: 1, symbol: 1}, 1.0},
		{components{overlap: 0.5, hop: 0.5, cofailure: 0.5, recency: 0.5, symbol: 1}, 0.2 + 0.125 + 0.1 + 0.05 + 0.05},
	} {
		if got := tc.c.rank(); !near(got, tc.want) {
			t.Errorf("%+v.rank() = %.15f, want %.15f", tc.c, got, tc.want)
		}
	}
}

func TestCandidateCapAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cap, override int
		wantKept      int
	}{
		{"config cap", 3, 0, 3},
		{"input overrides config", 400, 5, 5},
		{"cap larger than the candidates", 400, 0, 8},
		{"cap of one", 1, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := rankingScenario()
			in.Impact.MaxCandidates, in.MaxCandidates = tc.cap, tc.override
			r := mustL1(t, in)
			if len(r.Candidates) != tc.wantKept || r.RawCandidates != 8 || r.Truncated != 8-tc.wantKept {
				t.Fatalf("kept %d raw %d truncated %d, want kept %d raw 8 truncated %d", len(r.Candidates), r.RawCandidates, r.Truncated, tc.wantKept, 8-tc.wantKept)
			}
			full := candidateIDs(mustL1(t, rankingScenario()))
			if got := candidateIDs(r); !reflect.DeepEqual(got, full[:tc.wantKept]) {
				t.Fatalf("the cap must keep the highest ranked prefix: %v of %v", got, full)
			}
		})
	}
}

func TestCapNeverEvictsCertainMembers(t *testing.T) {
	in := graphInput(sourceChange("core/core.go"))
	in.Impact.MaxHops = 3
	in.Impact.MaxCandidates = 1
	r := mustL1(t, in)
	if len(r.Certain) != 6 {
		t.Fatalf("Certain = %v, want the 6 tests within 3 hops regardless of the cap", r.Certain)
	}
	if len(r.Candidates) != 1 || r.RawCandidates != 4 || r.Truncated != 3 {
		t.Fatalf("candidates %d raw %d truncated %d, want 1, 4 and 3", len(r.Candidates), r.RawCandidates, r.Truncated)
	}
}

func TestCandidatesExcludeCertainAndCoverEveryOtherTest(t *testing.T) {
	r := mustL1(t, rankingScenario())
	seen := map[string]bool{}
	for _, c := range r.Certain {
		seen[c.TestID] = true
	}
	for _, c := range r.Candidates {
		if seen[c.TestID] {
			t.Fatalf("%s is CERTAIN and a candidate", c.TestID)
		}
		seen[c.TestID] = true
	}
	if len(seen) != len(r.AllTests) {
		t.Fatalf("certain and candidates cover %d tests, the inventory has %d", len(seen), len(r.AllTests))
	}
}

func TestDocsOnlyChangeLeavesEveryTestACandidate(t *testing.T) {
	in := graphInput(changeSet(cf("README.md", "M", change.KindDocs, hunk(1, 1))))
	r := mustL1(t, in)
	if len(r.Certain) != 0 || r.RawCandidates != len(r.AllTests) {
		t.Fatalf("certain %v raw %d, want none and %d", r.Certain, r.RawCandidates, len(r.AllTests))
	}
	for _, c := range r.Candidates {
		if c.Rank != 0 || c.Features.Reachable || c.Features.Hops != 99 {
			t.Fatalf("%s: rank %v features %+v, want an unreachable zero rank", c.TestID, c.Rank, c.Features)
		}
	}
}

func TestIntegrationDetection(t *testing.T) {
	in := rankingScenario()
	for i := range in.Index.Tests {
		switch in.Index.Tests[i].ID {
		case testID("m/right", "TestRight"):
			in.Index.Tests[i].Name = "TestCheckoutE2E"
		case testID("m/top", "TestTop"):
			in.Index.Tests[i].File = "top/Integration_test.go"
		}
	}
	r := mustL1(t, in)
	got := map[string]bool{}
	for _, c := range r.Candidates {
		got[c.TestID] = c.Features.TestIsIntegration
	}
	if !got[testID("m/right", "TestRight")] || !got[testID("m/top", "TestTop")] {
		t.Fatalf("e2e name and integration file must be detected: %v", got)
	}
	if got[testID("m/left", "TestLeft")] || got[testID("m/cli", "TestCli")] {
		t.Fatalf("plain tests must not be integration tests: %v", got)
	}
}

func TestHistoryValuesAreSanitized(t *testing.T) {
	in := rankingScenario()
	in.History = stubHistory{
		cofail: map[string][2]float64{testID("m/left", "TestLeft"): {math.NaN(), 3}, testID("m/right", "TestRight"): {7, 1}},
		fail:   map[string]float64{testID("m/left", "TestLeft"): math.Inf(1)},
		days:   map[string]float64{testID("m/left", "TestLeft"): math.NaN(), testID("m/right", "TestRight"): -4},
	}
	r := mustL1(t, in)
	for _, c := range r.Candidates {
		if math.IsNaN(c.Rank) || math.IsInf(c.Rank, 0) {
			t.Fatalf("%s: rank %v is not finite", c.TestID, c.Rank)
		}
		for i, v := range c.Features.Vector() {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s: Vector()[%d] = %v is not finite", c.TestID, i, v)
			}
		}
	}
	for _, c := range r.Candidates {
		switch c.TestID {
		case testID("m/left", "TestLeft"):
			if c.Features.CofailureRate != 0 || c.Features.TestFailRate != 0 || c.Features.DaysSinceLastFail != 365 {
				t.Fatalf("non-finite history must read as unknown: %+v", c.Features)
			}
		case testID("m/right", "TestRight"):
			if c.Features.CofailureRate != 1 || c.Features.DaysSinceLastFail != 0 {
				t.Fatalf("rates clamp to [0,1] and negative ages to 0: %+v", c.Features)
			}
		}
	}
}

func TestCoverageOverlapRatio(t *testing.T) {
	cs := changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(10, 3)),
		cf("left/left.go", "M", change.KindSource, hunk(4, 2)),
		cf("left/left_test.go", "M", change.KindTest, hunk(1, 50)), // test files add no changed source lines
	)
	in := coverageInput(cs)
	in.Index.Coverage.FileDigests["left/left.go"] = "different"
	cover(in.Index, testID("m/core", "TestCore"), "covered", blk("core/core.go", 10, 12)) // core.go lines are all inside a block, so core.go is coverage-known
	files := sortedFiles(cs)
	ev := evalCoverage(in, files, mapChange(in.Index, files))
	if ev.total != 5 {
		t.Fatalf("total changed old-side source lines = %d, want 5", ev.total)
	}
	tc := index.TestCoverage{Status: "covered", Blocks: []index.Block{blk("core/core.go", 10, 11), blk("core/core.go", 11, 11), blk("left/left.go", 4, 5)}}
	if got := ev.coveredLines(tc); got != 2 {
		t.Fatalf("coveredLines = %d, want 2 (lines 10 and 11; left.go coverage does not apply)", got)
	}
	if want, got := 0.4, float64(ev.coveredLines(tc))/float64(ev.total); !near(got, want) {
		t.Fatalf("overlap = %v, want %v", got, want)
	}
}

func TestCandidateCoverageFlags(t *testing.T) {
	core := changeSet(cf("core/core.go", "M", change.KindSource, hunk(10, 3, "Do")))
	find := func(r *Result, id string) Candidate {
		for _, c := range r.Candidates {
			if c.TestID == id {
				return c
			}
		}
		t.Fatalf("%s is not a candidate: %v", id, candidateIDs(r))
		return Candidate{}
	}

	in := coverageInput(core)
	cover(in.Index, testID("m/core", "TestCore"), "covered", blk("core/core.go", 10, 12)) // keeps the changed lines inside a block, so the file stays coverage-known
	cover(in.Index, testID("m/core", "TestCoreAux"), "covered", blk("core/core.go", 50, 60))
	r := mustL1(t, in)
	aux := find(r, testID("m/core", "TestCoreAux")).Features
	if !aux.CoverageKnown || aux.CoverageStale || !aux.SamePackage || !aux.Reachable || aux.Hops != 0 || aux.CoverageOverlap != 0 {
		t.Fatalf("fresh coverage, reachable covered test without overlap: %+v", aux)
	}
	solo := find(r, testID("m/solo", "TestSolo")).Features
	if !solo.CoverageKnown || solo.Reachable || solo.SamePackage {
		t.Fatalf("fresh coverage, unreachable covered test: %+v", solo)
	}

	in = coverageInput(core)
	cover(in.Index, testID("m/solo", "TestSolo"), "failed")
	r = mustL1(t, in)
	if f := find(r, testID("m/solo", "TestSolo")).Features; f.CoverageKnown || f.CoverageStale {
		t.Fatalf("a failed test has fresh but unknown coverage: %+v", f)
	}

	in = coverageInput(core)
	in.CoverageAge = -1
	r = mustL1(t, in)
	if f := find(r, testID("m/solo", "TestSolo")).Features; f.CoverageKnown || !f.CoverageStale {
		t.Fatalf("stale coverage: %+v", f)
	}

	in = graphInput(core)
	in.Impact.MaxHops = 0
	r = mustL1(t, in)
	if f := find(r, testID("m/solo", "TestSolo")).Features; f.CoverageKnown || f.CoverageStale {
		t.Fatalf("ModeGraph has no coverage state: %+v", f)
	}
}

func TestSameFileFeature(t *testing.T) {
	cs := changeSet(cf("core/core_test.go", "M", change.KindTest, hunk(1, 1)))
	in := graphInput(cs)
	inv := newInventory(in.Index)
	res := &Result{ChangedPackages: []string{"m/core"}, TestHops: map[string]int{}, CoverageState: covNotRequested}
	r := newRanker(in, inv, sortedFiles(cs), res, coverageEval{})
	var core, left index.Test
	for _, tt := range inv.tests {
		switch tt.ID {
		case testID("m/core", "TestCore"):
			core = tt
		case testID("m/left", "TestLeft"):
			left = tt
		}
	}
	if f := r.features(core); !f.SameFile || !f.SamePackage {
		t.Fatalf("a test in the changed file: %+v", f)
	}
	if f := r.features(left); f.SameFile || f.SamePackage {
		t.Fatalf("a test elsewhere: %+v", f)
	}
}

func TestNoHistoryIsUnknown(t *testing.T) {
	var h History = NoHistory{}
	if rate, support := h.Cofailure("x", []string{"a"}); rate != 0 || support != 0 {
		t.Fatal("NoHistory.Cofailure must be zero")
	}
	if h.FailRate("x") != 0 {
		t.Fatal("NoHistory.FailRate must be zero")
	}
	if _, ok := h.DaysSinceLastFail("x"); ok {
		t.Fatal("NoHistory.DaysSinceLastFail must be unknown")
	}
	if _, ok := h.MedianMS("x"); ok {
		t.Fatal("NoHistory.MedianMS must be unknown")
	}
	in := graphInput(sourceChange("core/core.go"))
	in.Impact.MaxHops = 0
	in.History = nil
	for _, c := range mustL1(t, in).Candidates {
		if c.Features.DaysSinceLastFail != 365 || c.Features.CofailureSupport != 0 {
			t.Fatalf("nil History must behave as NoHistory: %+v", c.Features)
		}
	}
}

func TestCofailureReceivesTheChangedFiles(t *testing.T) {
	var got []string
	in := graphInput(changeSet(
		cf("core/core.go", "M", change.KindSource, hunk(1, 1)),
		func() change.ChangedFile {
			f := cf("left/new_test.go", "R", change.KindTest, hunk(1, 1))
			f.OldPath = "left/old_test.go"
			return f
		}(),
	))
	in.Impact.MaxHops = 0
	in.History = captureHistory{paths: &got}
	mustL1(t, in)
	want := []string{"core/core.go", "left/new_test.go", "left/old_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed files passed to Cofailure = %v, want %v", got, want)
	}
}

type captureHistory struct{ paths *[]string }

func (h captureHistory) Cofailure(_ string, files []string) (float64, int) {
	*h.paths = append([]string(nil), files...)
	return 0, 0
}
func (captureHistory) FailRate(string) float64                  { return 0 }
func (captureHistory) DaysSinceLastFail(string) (float64, bool) { return 0, false }
func (captureHistory) MedianMS(string) (int, bool)              { return 0, false }
