package events

import (
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/engine"
	"github.com/omorsi45/hazard/internal/impact"
	"github.com/omorsi45/hazard/internal/predict"
)

func decisionWith(res *impact.Result, reason string) engine.Decision {
	return engine.Decision{L1: res, AbstainReason: reason, FullSuite: true, Abstained: true}
}

func TestExamplesLabelsEachResidualCandidate(t *testing.T) {
	featA := impact.Features{Hops: 1, Reachable: true, LinesAdded: 3}
	featB := impact.Features{Hops: 99}
	d := decisionWith(&impact.Result{
		Certain: []impact.Member{{TestID: "go:m/p:TestCertain", Reason: "graph"}},
		Candidates: []impact.Candidate{
			{TestID: "go:m/p:TestA", Rank: 0.5, Features: featA},
			{TestID: "go:m/p:TestB", Rank: 0.125, Features: featB},
		},
	}, "no_lock")
	ev := Event{Group: "mut:x.go", Failing: []string{"go:m/p:TestA", "go:m/p:TestCertain", "go:m/p:TestGone"}}

	rows, stats, skip := Examples(d, ev, 10)
	if skip != "" {
		t.Fatalf("Examples skipped the event: %q", skip)
	}
	want := []predict.Example{
		{Rank: 0.5, Features: featA.Vector(), Label: true, Weight: 10, Group: "mut:x.go"},
		{Rank: 0.125, Features: featB.Vector(), Label: false, Weight: 10, Group: "mut:x.go"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v\nwant   %+v", rows, want)
	}
	if wantStats := (ExampleStats{FailingCertain: 1, FailingUnlisted: 1}); stats != wantStats {
		t.Errorf("stats = %+v, want %+v (one failing test already CERTAIN, one neither CERTAIN nor a candidate)", stats, wantStats)
	}
}

func TestExamplesYieldNothingWhenL1DemandsTheFullSuite(t *testing.T) {
	cands := []impact.Candidate{{TestID: "t", Rank: 0.1}}
	cases := map[string]struct {
		d    engine.Decision
		want string
	}{
		"no L1, engine reason":       {decisionWith(nil, "index_stale"), "index_stale"},
		"no L1, no reason":           {decisionWith(nil, ""), "no_l1"},
		"trigger keeps its kind":     {decisionWith(&impact.Result{FullReason: "trigger:go.mod:go.mod", Candidates: cands}, "trigger:go.mod:go.mod"), "trigger"},
		"unmapped change kind":       {decisionWith(&impact.Result{FullReason: "unmapped_change:src/x.go", Candidates: cands}, ""), "unmapped_change"},
		"index incomplete":           {decisionWith(&impact.Result{FullReason: "index_incomplete", Candidates: cands}, ""), "index_incomplete"},
		"nothing left to score":      {decisionWith(&impact.Result{Certain: []impact.Member{{TestID: "t", Reason: "graph"}}}, "no_lock"), "no_candidates"},
		"candidates but a full flag": {decisionWith(&impact.Result{Candidates: cands, FullReason: "python_dynamic_import:app"}, ""), "python_dynamic_import"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rows, stats, skip := Examples(tc.d, Event{Group: "g", Failing: []string{"t"}}, 1)
			if len(rows) != 0 || skip != tc.want {
				t.Errorf("Examples = %d rows, skip %q, want no rows and skip %q", len(rows), skip, tc.want)
			}
			if stats != (ExampleStats{}) {
				t.Errorf("stats = %+v for an event with no rows, want zero", stats)
			}
		})
	}
}

func TestExamplesOfASurvivedMutantAreAllNegative(t *testing.T) {
	d := decisionWith(&impact.Result{Candidates: []impact.Candidate{{TestID: "a", Rank: 0.2}, {TestID: "b", Rank: 0.1}}}, "no_lock")
	rows, stats, skip := Examples(d, Event{Group: "g"}, 1)
	if skip != "" || len(rows) != 2 || rows[0].Label || rows[1].Label || rows[0].Weight != 1 {
		t.Errorf("rows = %+v, skip %q, want two negative rows of weight 1", rows, skip)
	}
	if stats != (ExampleStats{}) {
		t.Errorf("stats = %+v, want zero", stats)
	}
}
