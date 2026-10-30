package eval

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/labels"
	"github.com/omorsi45/hazard/internal/store"
)

func realPlan(t *testing.T) Plan {
	t.Helper()
	plan := basePlan(t)
	plan.Predictors = []PredictorState{{Kind: "static", ID: RowP0}, {Kind: "logistic", ID: RowP1}}
	return plan
}

// Guard 3: with no strong unit nothing is measured, so every cell reads N/A
// and the row shows the counts. A zero recall or saving would be a claim.
func TestBuildRealForAnEmptyPopulationIsNotAvailableWithCounts(t *testing.T) {
	l := RealLabels{Available: true, BuildID: "b1", Cutoff: "2026-01-01", Freshness: labels.FreshnessCurrent, RawFailingCases: 14}
	real := BuildReal(l, realPlan(t))
	if real.Population != RealLabel {
		t.Errorf("population = %q, want %q", real.Population, RealLabel)
	}
	for _, want := range []string{"N/A", "0 strong units", "14 raw failing cases"} {
		if !strings.Contains(real.Status, want) {
			t.Errorf("status %q lacks %q", real.Status, want)
		}
	}
	if got, want := rowIDs(real.Rows), []string{"B0", "B1", "B2", "P0", "P1"}; !equalStrings(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for _, r := range real.Rows {
		if r.Population != RealLabel {
			t.Errorf("row %s population = %q", r.ID, r.Population)
		}
		if !strings.Contains(r.Status, "0 strong units") || !strings.Contains(r.Status, "14 raw failing cases") {
			t.Errorf("row %s status = %q, want the counts", r.ID, r.Status)
		}
		for _, name := range []string{MetricRecallCommit, MetricRecallFailure, MetricTimeRatio, MetricAbstention, MetricSaving, MetricBrier} {
			m, ok := r.Metrics[name]
			if !ok || m.Value != nil || m.Low != nil || m.High != nil || m.NA != NotAvailable {
				t.Errorf("row %s %s = %+v, want N/A and no number", r.ID, name, m)
			}
		}
	}
}

func TestBuildRealForUnavailableOrStaleLabelsIsNotAvailable(t *testing.T) {
	for name, l := range map[string]RealLabels{
		"no database": {Available: false, Reason: "there is no database at x"},
		"stale":       {Available: true, Freshness: labels.FreshnessStale, RawFailingCases: 3},
	} {
		real := BuildReal(l, realPlan(t))
		if !strings.HasPrefix(real.Status, NotAvailable) {
			t.Errorf("%s: status = %q, want N/A", name, real.Status)
		}
		for _, r := range real.Rows {
			if r.Metrics[MetricRecallCommit].Value != nil {
				t.Errorf("%s: row %s has a recall", name, r.ID)
			}
		}
	}
	if s := BuildReal(RealLabels{Available: false, Reason: "there is no database at x"}, realPlan(t)).Status; !strings.Contains(s, "there is no database at x") {
		t.Errorf("status %q hides why the labels are unavailable", s)
	}
}

// With strong units the replay of real units is not part of this report: the
// rows say so and show no number.
func TestBuildRealWithStrongUnitsDoesNotInventNumbers(t *testing.T) {
	l := RealLabels{Available: true, Freshness: labels.FreshnessCurrent, RawFailingCases: 9, StrongCandidates: 4, StrongUnits: 3}
	real := BuildReal(l, realPlan(t))
	if !strings.HasPrefix(real.Status, "not evaluated") || !strings.Contains(real.Status, "3 strong units") {
		t.Errorf("status = %q, want not evaluated with the 3 strong units", real.Status)
	}
	for _, r := range real.Rows {
		if len(r.Metrics) != 0 || !strings.HasPrefix(r.Status, "not evaluated") {
			t.Errorf("row %s = %+v, want no numbers", r.ID, r)
		}
	}
}

func unitsAt(times ...string) []labels.Unit {
	out := make([]labels.Unit, len(times))
	for i, s := range times {
		at, err := time.Parse("2006-01-02", s)
		if err != nil {
			panic(err)
		}
		out[i] = labels.Unit{ID: "ci:" + s, CommitTime: at}
	}
	return out
}

func TestSplitUnitsTemporalPutsTheSplitPointInTest(t *testing.T) {
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := SplitUnits(unitsAt("2026-05-30", "2026-05-31", "2026-06-01", "2026-06-02"), SplitSpec{Method: SplitTemporal, At: at})
	if got == nil || got.Tune != 2 || got.Test != 2 || got.At != "2026-06-01" || got.Method != SplitTemporal {
		t.Errorf("split = %+v, want 2 tune and 2 test at 2026-06-01", got)
	}
	if SplitUnits(unitsAt("2026-05-30"), SplitSpec{Method: SplitTemporal}) != nil {
		t.Error("a temporal split without --split-at has no split to count")
	}
	empty := SplitUnits(nil, SplitSpec{Method: SplitTemporal, At: at})
	if empty == nil || empty.Tune != 0 || empty.Test != 0 {
		t.Errorf("split of no unit = %+v, want 0 and 0 (counts, not metrics)", empty)
	}
}

func TestSplitUnitsRandomHalvesTheUnitsRoundingTuneDown(t *testing.T) {
	units := unitsAt("2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04", "2026-01-05")
	got := SplitUnits(units, SplitSpec{Method: SplitRandom})
	if got == nil || got.Method != SplitRandom || got.Tune != 2 || got.Test != 3 || got.At != "" {
		t.Errorf("random split = %+v, want 2 tune and 3 test", got)
	}
}

func openLabelsDB(t *testing.T) (string, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hazard.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, s
}

func TestReadRealWithoutADatabaseIsUnavailableNotEmpty(t *testing.T) {
	got, err := ReadReal(context.Background(), filepath.Join(t.TempDir(), "absent.db"), SplitSpec{Method: SplitTemporal})
	if err != nil {
		t.Fatal(err)
	}
	if got.Available || got.Reason == "" {
		t.Errorf("labels = %+v, want unavailable with a reason", got)
	}
}

func TestReadRealCountsTheCurrentBuild(t *testing.T) {
	path, s := openLabelsDB(t)
	if _, err := s.DB().Exec(`INSERT INTO mining_sources(singleton,provider,project,observed_at) VALUES(1,'github','o/r','2025-06-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := labels.Build(context.Background(), s.DB(), labels.Options{Before: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadReal(context.Background(), path, SplitSpec{Method: SplitTemporal, At: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.Freshness != labels.FreshnessCurrent || got.BuildID == "" || got.Cutoff == "" {
		t.Fatalf("labels = %+v, want the current build", got)
	}
	if got.StrongUnits != 0 || got.RawFailingCases != 0 || got.Split == nil || got.Split.Tune != 0 || got.Split.Test != 0 {
		t.Errorf("counts = %+v, want zero strong units and an empty split", got)
	}
}
