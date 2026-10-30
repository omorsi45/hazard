package eval

import (
	"sort"
	"testing"

	"github.com/omorsi45/hazard/internal/control"
)

func basePlan(t *testing.T) Plan {
	t.Helper()
	return Plan{
		Baselines:   []string{"all", "l1-only", "l1-coverage"},
		Predictors:  []PredictorState{{Kind: "static", ID: RowP0}, {Kind: "jev", ID: RowP2, Status: "needs OPENROUTER_API_KEY"}},
		Grid:        gridOf(t, "0.05:0.5:0.05"),
		Recall:      0.99,
		Reps:        40,
		Seed:        1,
		HasCoverage: true,
		PMode:       "coverage",
		Cost:        fixtureCost(),
	}
}

func rowByID(t *testing.T, rows []Row, id string) Row {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row %s in %v", id, rowIDs(rows))
	return Row{}
}

func rowIDs(rows []Row) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func TestBuildSyntheticRowsInSeedOrderLabelledSynthetic(t *testing.T) {
	tune, test := tuneTestFixture()
	built := BuildSynthetic(basePlan(t), tune, test)
	if got, want := rowIDs(built.Rows), []string{"B0", "B1", "B2", "P0", "P2"}; !equalStrings(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for _, r := range built.Rows {
		if r.Population != SyntheticLabel {
			t.Errorf("row %s population = %q, want %q", r.ID, r.Population, SyntheticLabel)
		}
	}
	b0 := rowByID(t, built.Rows, RowB0)
	wantValue(t, b0.Metrics, MetricSaving, 0)
	wantValue(t, b0.Metrics, MetricRecallCommit, 1)
	wantValue(t, b0.Metrics, MetricLatency, 0)
	wantNA(t, b0.Metrics, MetricAbstention) // run-everything never abstains: not applicable, not zero
	wantNA(t, b0.Metrics, MetricBrier)

	p0 := rowByID(t, built.Rows, RowP0)
	if p0.Status != StatusMeasured || p0.Fit == nil || p0.Fit.Lambda != 0.4 || !p0.Fit.Feasible {
		t.Fatalf("P0 = %+v fit=%+v, want measured at the tune lambda 0.4", p0, p0.Fit)
	}
	wantValue(t, p0.Metrics, MetricRecallCommit, 0)
	if p0.Flag != "below 0.99" {
		t.Errorf("P0 flag = %q, want %q (test recall 0 is under the target)", p0.Flag, "below 0.99")
	}
	if p0.Metrics[MetricRecallCommit].Low == nil {
		t.Error("P0 recall has no bootstrap interval")
	}
	wantValue(t, p0.Metrics, MetricUSD, 0)
	if p0.Mode != "coverage" {
		t.Errorf("P0 l1 mode = %q, want the plan's coverage", p0.Mode)
	}

	p2 := rowByID(t, built.Rows, RowP2)
	if p2.Status != "needs OPENROUTER_API_KEY" || len(p2.Metrics) != 0 || p2.Fit != nil {
		t.Errorf("P2 = %+v, want no numbers and the status text", p2)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildSyntheticInfeasibleRowCarriesTheExactLabel(t *testing.T) {
	var tune, test []*Event
	for _, id := range []string{"a", "b"} {
		tune = append(tune, fixtureEvent("tune-"+id, "tune", []string{"t1"}, []control.Candidate{cand("t1", 0)}, fixtureLayer(1)))
		test = append(test, fixtureEvent("test-"+id, "test", []string{"t1"}, []control.Candidate{cand("t1", 0.9)}, fixtureLayer(1)))
	}
	for recall, want := range map[float64]string{0.99: "infeasible at R_c >= 0.99", 0.95: "infeasible at R_c >= 0.95"} {
		plan := basePlan(t)
		plan.Recall = recall
		p0 := rowByID(t, BuildSynthetic(plan, tune, test).Rows, RowP0)
		if p0.Note != want || p0.Fit == nil || p0.Fit.Feasible || p0.Fit.Lambda != 0.05 {
			t.Errorf("recall %v: P0 note=%q fit=%+v, want %q at the smallest lambda 0.05", recall, p0.Note, p0.Fit, want)
		}
		// Test metrics are reported at that lambda, not withheld.
		wantValue(t, p0.Metrics, MetricRecallCommit, 1)
	}
}

func TestBuildSyntheticWithoutATuneFailureHasNoNumbersForTheRow(t *testing.T) {
	tune := []*Event{fixtureEvent("tune-a", "tune", nil, []control.Candidate{cand("t1", 0.4)}, fixtureLayer(1))}
	_, test := tuneTestFixture()
	p0 := rowByID(t, BuildSynthetic(basePlan(t), tune, test).Rows, RowP0)
	if p0.Status == StatusMeasured || len(p0.Metrics) != 0 || p0.Status == "" {
		t.Errorf("P0 = %+v, want the status to say lambda could not be fitted, and no numbers", p0)
	}
}

func TestBuildSyntheticBaselineBelowTargetIsFlaggedAndUnavailableCoverageSaysSo(t *testing.T) {
	_, test := tuneTestFixture() // t1 fails and is not CERTAIN, so L1 alone misses it
	plan := basePlan(t)
	plan.HasCoverage = false
	built := BuildSynthetic(plan, nil, test)
	b1 := rowByID(t, built.Rows, RowB1)
	wantValue(t, b1.Metrics, MetricRecallCommit, 0)
	if b1.Flag != "below 0.99" {
		t.Errorf("B1 flag = %q, want %q", b1.Flag, "below 0.99")
	}
	b2 := rowByID(t, built.Rows, RowB2)
	if b2.Status != "unavailable: the index has no coverage" || len(b2.Metrics) != 0 {
		t.Errorf("B2 = %+v, want unavailable without coverage and no numbers", b2)
	}
	if b0 := rowByID(t, built.Rows, RowB0); b0.Flag != "" {
		t.Errorf("B0 flag = %q, want none", b0.Flag)
	}
	wantNA(t, b1.Metrics, MetricTruncation) // no candidate list, no cap to miss through
	wantNA(t, b1.Metrics, MetricBrier)
}

func TestBuildSyntheticWithoutDurationsHasNoTimeNumbers(t *testing.T) {
	tune, test := tuneTestFixture()
	plan := basePlan(t)
	plan.Cost = nil
	built := BuildSynthetic(plan, tune, test)
	for _, id := range []string{RowB0, RowB1, RowP0} {
		r := rowByID(t, built.Rows, id)
		for _, name := range []string{MetricTimeRatio, MetricSaving} {
			m := r.Metrics[name]
			if m.Value != nil || m.NA == "" || m.NA[:3] != NotAvailable {
				t.Errorf("%s %s = %+v, want N/A: no durations", id, name, m)
			}
		}
		if r.Metrics[MetricRecallCommit].Value == nil {
			t.Errorf("%s lost its recall without durations", id)
		}
	}
}

func TestBuildSyntheticBreaksDownByChangeKind(t *testing.T) {
	var test []*Event
	for i, kind := range []string{"go", "go", "cfg", "cfg"} {
		failing := []string{"c1"} // CERTAIN: caught
		if kind == "cfg" {
			failing = []string{"t1"} // not CERTAIN: missed by L1 alone
		}
		e := fixtureEvent("test-"+string(rune('a'+i)), "test", failing, nil, fixtureLayer(1))
		e.Kind = kind
		test = append(test, e)
	}
	b1 := rowByID(t, BuildSynthetic(basePlan(t), nil, test).Rows, RowB1)
	kinds := make([]string, 0, 2)
	for k := range b1.ByKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	if !equalStrings(kinds, []string{"cfg", "go"}) {
		t.Fatalf("by kind = %v, want cfg and go", kinds)
	}
	wantValue(t, b1.ByKind["go"], MetricRecallCommit, 1)
	wantValue(t, b1.ByKind["cfg"], MetricRecallCommit, 0)
	wantValue(t, b1.Metrics, MetricRecallCommit, 0.5)
}

func TestBuildSyntheticPairsAreTheTestPartitionAndFeedTheBins(t *testing.T) {
	tune, test := tuneTestFixture()
	built := BuildSynthetic(basePlan(t), tune, test)
	if len(built.Pairs) != 6 { // 3 test events x 2 candidates, none from tune
		t.Fatalf("pairs = %d, want 6", len(built.Pairs))
	}
	for _, p := range built.Pairs {
		if p.Config != RowP0 || len(p.Event) < 5 || p.Event[:5] != "test-" {
			t.Errorf("pair %+v: want only P0 pairs of test events", p)
		}
	}
	if len(built.Reliability) != 1 {
		t.Fatalf("reliability tables = %d, want one (P0)", len(built.Reliability))
	}
	rel := built.Reliability[0]
	if rel.Config != RowP0 || rel.Population != SyntheticLabel || rel.Pairs != 6 || len(rel.Bins) != 10 {
		t.Fatalf("reliability = %+v", rel)
	}
	// t1 scored 0.1 (bin 1, failed) and t2 0.05 (bin 0, passed), three events each.
	if rel.Bins[1].Count != 3 || rel.Bins[0].Count != 3 || rel.Bins[1].Observed == nil || *rel.Bins[1].Observed != 1 || *rel.Bins[0].Observed != 0 {
		t.Errorf("bins 0 and 1 = %+v %+v", rel.Bins[0], rel.Bins[1])
	}
}
