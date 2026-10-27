package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omorsi45/hazard/internal/change"
	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
)

func runExplainCmd(ctx context.Context, sel selectFunc, args ...string) (int, string, string) {
	return runLeaf(ctx, newExplainCommand(sel), "explain", args...)
}

// explainDecision is a graph-mode result over five tests: goA is CERTAIN by
// a direct change, goC is a ranked candidate, goB is a residual cut by the
// candidate cap and pyK is unreachable.
func explainDecision() engine.Decision {
	return engine.Decision{
		ID: "ex1", FullSuite: true, Abstained: true, AbstainReason: "no_lock",
		AllTests: []string{goA, goB, goC, pyK},
		L1: &impact.Result{
			Certain: []impact.Member{{TestID: goA, Reason: "direct"}},
			Candidates: []impact.Candidate{{
				TestID: goC, Rank: 0.125,
				Features: impact.Features{Hops: 2, Reachable: true, LinesAdded: 3, FilesChanged: 1, ChangedFileKinds: [8]int{change.KindSource: 1}},
			}},
			RawCandidates: 2, Truncated: 1,
			AllTests:      []string{goA, goB, goC, pyK},
			TestHops:      map[string]int{goA: 0, goB: 3, goC: 2},
			CoverageState: "not_requested",
		},
		Hypothetical: []engine.Selected{{TestID: goA, Source: "certain", Reason: "direct"}},
	}
}

func TestFeatureNamesMatchTheVectorOrder(t *testing.T) {
	t.Parallel()
	vec := impact.Features{Hops: 7, Reachable: true, CoverageOverlap: 0.25, CoverageKnown: true, CoverageStale: true,
		CofailureRate: 0.5, CofailureSupport: 4, TestFailRate: 0.125, DaysSinceLastFail: 9, SharedSymbolHits: 2,
		SameFile: true, SamePackage: true, ChangedFileKinds: [8]int{1, 2, 3, 4, 5, 6, 7, 8},
		LinesAdded: 11, LinesRemoved: 12, FilesChanged: 13, TestMedianMS: 14, TestIsIntegration: true}.Vector()
	if len(featureNames) != len(vec) {
		t.Fatalf("%d feature names for a %d-value vector", len(featureNames), len(vec))
	}
	byName := map[string]float64{}
	for i, n := range featureNames {
		byName[n] = vec[i]
	}
	for name, want := range map[string]float64{
		"hops": 7, "reachable": 1, "coverage_overlap": 0.25, "coverage_known": 1, "coverage_stale": 1,
		"cofailure_rate": 0.5, "cofailure_support": 4, "test_fail_rate": 0.125, "days_since_last_fail": 9,
		"shared_symbol_hits": 2, "same_file": 1, "same_package": 1,
		"kind_source": 1, "kind_test": 2, "kind_config": 3, "kind_fixture": 4, "kind_generated": 5, "kind_docs": 6, "kind_lock": 7, "kind_build": 8,
		"lines_added": 11, "lines_removed": 12, "files_changed": 13, "test_median_ms": 14, "test_is_integration": 1,
	} {
		if got, ok := byName[name]; !ok || got != want {
			t.Errorf("feature %q = %v (present %v), want %v", name, got, ok, want)
		}
	}
}

func TestExplainTextForEachKindOfTest(t *testing.T) {
	chdirRepo(t)
	for _, tc := range []struct {
		name, test string
		want       []string
	}{
		{"certain", goA, []string{
			"test: " + goA, "in inventory: yes", "hops: 0", "l1: certain (direct)", "coverage state: not_requested",
			"predictor probability: n/a (no predictor in M3)", "shard: n/a", "lambda: n/a (no lock)", "decision: full suite: no_lock",
		}},
		{"candidate", goC, []string{
			"in inventory: yes", "hops: 2", "l1: candidate (rank 0.1250)", "features: hops=2 reachable=1 ", "lines_added=3", "kind_source=1",
		}},
		{"cut by the cap", goB, []string{"in inventory: yes", "hops: 3", "l1: residual, not ranked (candidate cap cut 1 of 2)"}},
		{"unreachable", pyK, []string{"in inventory: yes", "hops: unreachable", "l1: residual, not ranked"}},
	} {
		f := &fakeSelect{d: explainDecision()}
		code, out, errOut := runExplainCmd(context.Background(), f.fn, "--test", tc.test)
		if code != 0 || errOut != "" {
			t.Errorf("%s: code=%d err=%q", tc.name, code, errOut)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: output omitted %q:\n%s", tc.name, w, out)
			}
		}
		if !f.got.Explain || f.got.Base != "" {
			t.Errorf("%s: options = %+v, want Explain set", tc.name, f.got)
		}
	}
}

func TestExplainFinalDecision(t *testing.T) {
	chdirRepo(t)
	d := explainDecision()
	d.FullSuite, d.Abstained, d.AbstainReason = false, false, ""
	d.Tests = []engine.Selected{{TestID: goA, Source: "certain", Reason: "direct"}}
	f := &fakeSelect{d: d}
	_, out, _ := runExplainCmd(context.Background(), f.fn, "--test", goA)
	if !strings.Contains(out, "decision: selected") {
		t.Errorf("selected test:\n%s", out)
	}
	_, out, _ = runExplainCmd(context.Background(), f.fn, "--test", goB)
	if !strings.Contains(out, "decision: not selected") {
		t.Errorf("unselected test:\n%s", out)
	}
}

func TestExplainWhenL1DemandedTheFullSuite(t *testing.T) {
	chdirRepo(t)
	d := explainDecision()
	d.L1 = &impact.Result{AllTests: d.AllTests, FullReason: "trigger:go.mod:go.mod"}
	d.AbstainReason = "trigger:go.mod:go.mod"
	f := &fakeSelect{d: d}
	code, out, _ := runExplainCmd(context.Background(), f.fn, "--test", goA)
	if code != 0 || !strings.Contains(out, "l1: full suite demanded (trigger:go.mod:go.mod)") || !strings.Contains(out, "decision: full suite: trigger:go.mod:go.mod") || !strings.Contains(out, "hops: unreachable") {
		t.Errorf("code=%d:\n%s", code, out)
	}
}

func TestExplainWithoutAUsableIndex(t *testing.T) {
	chdirRepo(t)
	f := &fakeSelect{d: engine.Decision{FullSuite: true, Abstained: true, AbstainReason: "no_index"}}
	code, out, errOut := runExplainCmd(context.Background(), f.fn, "--test", goA)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, w := range []string{"in inventory: unknown", "hops: unknown", "l1: not computed", "coverage state: n/a", "decision: full suite: no_index"} {
		if !strings.Contains(out, w) {
			t.Errorf("output omitted %q:\n%s", w, out)
		}
	}
}

func TestExplainUnknownTestIsAUsageError(t *testing.T) {
	chdirRepo(t)
	f := &fakeSelect{d: explainDecision()}
	code, out, errOut := runExplainCmd(context.Background(), f.fn, "--test", "go:example.com/m/zzz:TestNope")
	if code != 2 || out != "" || !strings.Contains(errOut, "go:example.com/m/zzz:TestNope") {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
	code, _, errOut = runExplainCmd(context.Background(), f.fn)
	if code != 2 || !strings.Contains(errOut, "--test") {
		t.Errorf("missing --test: code=%d err=%q", code, errOut)
	}
	code, _, _ = runExplainCmd(context.Background(), f.fn, "--test", goA, "stray")
	if code != 2 {
		t.Errorf("a positional argument: code=%d", code)
	}
}

func TestExplainPassesBaseAndMapsErrors(t *testing.T) {
	chdirRepo(t)
	f := &fakeSelect{d: explainDecision()}
	if code, _, _ := runExplainCmd(context.Background(), f.fn, "--test", goA, "--base", "main"); code != 0 || f.got.Base != "main" || !f.got.Explain {
		t.Errorf("code=%d options=%+v", code, f.got)
	}
	bad := &fakeSelect{err: engine.ErrInvalidRef}
	if code, _, _ := runExplainCmd(context.Background(), bad.fn, "--test", goA); code != 2 {
		t.Errorf("an invalid ref: code=%d, want 2", code)
	}
}

func TestExplainJSON(t *testing.T) {
	chdirRepo(t)
	f := &fakeSelect{d: explainDecision()}
	code, out, errOut := runExplainCmd(context.Background(), f.fn, "--test", goC, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	var doc struct {
		Test                 string             `json:"test"`
		InInventory          string             `json:"in_inventory"`
		Hops                 *int               `json:"hops"`
		Reachable            bool               `json:"reachable"`
		L1                   string             `json:"l1"`
		Rank                 *float64           `json:"rank"`
		Features             map[string]float64 `json:"features"`
		CoverageState        string             `json:"coverage_state"`
		PredictorProbability string             `json:"predictor_probability"`
		Shard                string             `json:"shard"`
		Lambda               string             `json:"lambda"`
		Decision             string             `json:"decision"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%q", err, out)
	}
	if doc.Test != goC || doc.InInventory != "yes" || doc.Hops == nil || *doc.Hops != 2 || !doc.Reachable || doc.Rank == nil || *doc.Rank != 0.125 ||
		doc.Features["hops"] != 2 || doc.Features["lines_added"] != 3 || doc.CoverageState != "not_requested" ||
		doc.PredictorProbability != "n/a (no predictor in M3)" || doc.Shard != "n/a" || doc.Lambda != "n/a (no lock)" || doc.Decision != "full suite: no_lock" || !strings.HasPrefix(doc.L1, "candidate") {
		t.Errorf("doc = %+v", doc)
	}
	f2 := &fakeSelect{d: explainDecision()}
	_, out, _ = runExplainCmd(context.Background(), f2.fn, "--test", pyK, "--json")
	if !strings.Contains(out, `"hops":null`) || !strings.Contains(out, `"reachable":false`) {
		t.Errorf("an unreachable test must report null hops: %s", out)
	}
}

func TestExplainEndToEnd(t *testing.T) {
	e2eRepo(t)
	ctx := context.Background()
	code, out, errOut := invoke(ctx, "explain", "--test", e2eLeft)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, w := range []string{"test: " + e2eLeft, "in inventory: yes", "hops: 1", "l1: certain (graph)", "coverage state: not_requested", "decision: full suite: no_lock"} {
		if !strings.Contains(out, w) {
			t.Errorf("output omitted %q:\n%s", w, out)
		}
	}
	code, out, _ = invoke(ctx, "explain", "--test", "go:example.com/m/solo:TestSolo")
	if code != 0 || !strings.Contains(out, "hops: unreachable") || !strings.Contains(out, "l1: candidate") {
		t.Errorf("an unrelated package is a ranked candidate: code=%d\n%s", code, out)
	}
	code, out, errOut = invoke(ctx, "explain", "--test", "go:example.com/m/nope:TestNope")
	if code != 2 || out != "" || !strings.Contains(errOut, "TestNope") {
		t.Errorf("unknown test: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = invoke(ctx, "explain", "--test", e2eCore, "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 || errOut != "" || doc["l1"] != "certain (graph)" {
		t.Errorf("JSON explain: code=%d out=%q err=%q (%v)", code, out, errOut, err)
	}
}
