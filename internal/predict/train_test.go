package predict

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omorsi45/hazard/internal/impact"
)

var trainEpoch = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// trainEvents builds n events with perEvent examples each, half of them
// positive, drawn from two overlapping clusters. Event i happened i hours after
// the epoch and belongs to group "mut:f<n-1-i>", so the chronological order
// is the reverse of the order of the names.
func trainEvents(g *lcg, n, perEvent int) []TrainEvent {
	out := make([]TrainEvent, 0, n)
	for i := 0; i < n; i++ {
		group := fmt.Sprintf("mut:f%02d", n-1-i)
		ex := make([]Example, 0, perEvent)
		for j := 0; j < perEvent; j++ {
			pos := j%2 == 0
			c := [2]float64{-0.5, 0.5}
			if pos {
				c = [2]float64{0.5, -0.5}
			}
			x0 := c[0] + g.gauss()
			ex = append(ex, Example{
				Rank: 0.5 + 0.1*x0, Features: []float64{x0, c[1] + g.gauss()},
				Label: pos, Weight: 1, Group: group,
			})
		}
		out = append(out, TrainEvent{
			ID: fmt.Sprintf("m%02d", i), Origin: "mutation", Group: group,
			Time: trainEpoch.Add(time.Duration(i) * time.Hour), Examples: ex,
		})
	}
	return out
}

func trainInput(t *testing.T, events []TrainEvent) TrainInput {
	t.Helper()
	return TrainInput{
		Cutoff: "2026-04-01",
		Events: events,
		Collected: Collected{
			Events:  []EventCount{{Origin: "mutation", Partition: "train", Status: "killed", Count: len(events)}},
			Skipped: map[string]int{"mutation:build_error": 2},
		},
		OutDir: filepath.Join(t.TempDir(), "out"),
		Now:    func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) },
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestTrainFitsBothPredictorsAndRecordsTheSplit(t *testing.T) {
	g := lcg(5)
	events := trainEvents(&g, 10, 20)
	events[0].Origin, events[1].Origin = "ci_strong", "ci_weak"
	in := trainInput(t, events)

	m, err := Train(in)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if len(m.Refusals) != 0 {
		t.Fatalf("refusals = %+v, want none", m.Refusals)
	}
	if got := dirEntries(t, in.OutDir); !reflect.DeepEqual(got, []string{"logistic.json", "static.json", "training.json"}) {
		t.Fatalf("output directory holds %v, want the three files", got)
	}

	// Oldest 80% of the groups fit; the newest 20% scale. Event i is group f(9-i).
	wantFit := []string{"mut:f09", "mut:f08", "mut:f07", "mut:f06", "mut:f05", "mut:f04", "mut:f03", "mut:f02"}
	wantScale := []string{"mut:f01", "mut:f00"}
	if got := m.Splits["fit"].Groups; !reflect.DeepEqual(got, wantFit) {
		t.Errorf("fit groups = %v, want %v", got, wantFit)
	}
	if got := m.Splits["scale"].Groups; !reflect.DeepEqual(got, wantScale) {
		t.Errorf("scale groups = %v, want %v", got, wantScale)
	}
	if got, want := m.Splits["scale"].EventIDs, []string{"m08", "m09"}; !reflect.DeepEqual(got, want) {
		t.Errorf("scale event IDs = %v, want %v", got, want)
	}
	if fit, scale := m.Splits["fit"], m.Splits["scale"]; fit.Events != 8 || fit.Examples != 160 || fit.Positives != 80 || fit.Negatives != 80 ||
		scale.Events != 2 || scale.Examples != 40 || scale.Positives != 20 || scale.Negatives != 20 {
		t.Errorf("split totals = fit %+v scale %+v", fit, scale)
	}
	if want := (ExampleCounts{Events: 10, Examples: 200, Positives: 100, Negatives: 100}); m.Examples != want {
		t.Errorf("example totals = %+v, want %+v", m.Examples, want)
	}
	if m.Cutoff != "2026-04-01" || m.CreatedAt != "2026-10-29T12:00:00Z" || m.FeatureVersion != impact.FeatureVersion {
		t.Errorf("manifest header = %q %q %q", m.Cutoff, m.CreatedAt, m.FeatureVersion)
	}
	if !reflect.DeepEqual(m.Collected, in.Collected) {
		t.Errorf("collected = %+v, want the input %+v", m.Collected, in.Collected)
	}

	static, err := ReadStatic(filepath.Join(in.OutDir, "static.json"))
	if err != nil {
		t.Fatalf("ReadStatic: %v", err)
	}
	logistic, err := ReadLogistic(filepath.Join(in.OutDir, "logistic.json"))
	if err != nil {
		t.Fatalf("ReadLogistic: %v", err)
	}
	if got := m.Artifacts["static"]; got.Digest != static.Digest || got.Path != filepath.Join(in.OutDir, "static.json") {
		t.Errorf("static artifact report = %+v, want digest %s", got, static.Digest)
	}
	if got := m.Artifacts["logistic"]; got.Digest != logistic.Digest {
		t.Errorf("logistic artifact report = %+v, want digest %s", got, logistic.Digest)
	}
	if !logistic.Model.Converged {
		t.Errorf("logistic model did not converge after %d iterations", logistic.Model.Iterations)
	}
	// P0 sees only the fit set, P1 both sets.
	if ti := static.Training; ti.Cutoff != "2026-04-01" || !reflect.DeepEqual(ti.FitGroups, wantFit) || len(ti.ScaleGroups) != 0 ||
		ti.Positives != 80 || ti.Negatives != 80 || ti.CreatedAt != m.CreatedAt || !reflect.DeepEqual(ti.Sources, map[string]int{"ci_strong": 1, "ci_weak": 1, "mutation": 6}) {
		t.Errorf("static training info = %+v", ti)
	}
	if ti := logistic.Training; !reflect.DeepEqual(ti.FitGroups, wantFit) || !reflect.DeepEqual(ti.ScaleGroups, wantScale) ||
		ti.Positives != 100 || ti.Negatives != 100 || !reflect.DeepEqual(ti.Sources, map[string]int{"ci_strong": 1, "ci_weak": 1, "mutation": 8}) {
		t.Errorf("logistic training info = %+v", ti)
	}
	// The fitted models separate the clusters: a high feature 0 scores above a low one.
	hi, err := logistic.Model.Prob([]float64{1, -1})
	lo, err2 := logistic.Model.Prob([]float64{-1, 1})
	if err != nil || err2 != nil || !(hi > 0.5 && lo < 0.5) {
		t.Errorf("logistic scores high=%v low=%v (%v, %v), want above and below one half", hi, lo, err, err2)
	}
	if p, q := static.Model.Predict(0.9), static.Model.Predict(0.1); !(p > q) {
		t.Errorf("static model scores rank 0.9 as %v and rank 0.1 as %v, want the first larger", p, q)
	}

	// training.json carries the same counts and reads back as JSON.
	data, err := os.ReadFile(filepath.Join(in.OutDir, "training.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"cutoff": "2026-04-01"`, `"mutation:build_error": 2`, `"refusals": []`, static.Digest, logistic.Digest} {
		if !strings.Contains(string(data), want) {
			t.Errorf("training.json lacks %q:\n%s", want, data)
		}
	}
}

// TestTrainOrdersGroupsByTimeThenGroup: a group is placed by the time of its
// latest event, equal times are ordered by group name.
func TestTrainOrdersGroupsByTimeThenGroup(t *testing.T) {
	g := lcg(9)
	events := trainEvents(&g, 5, 10)
	// All five events at the same instant, then the first group also has a late event.
	for i := range events {
		events[i].Time = trainEpoch
	}
	late := events[0]
	late.ID, late.Time = "late", trainEpoch.Add(time.Hour)
	events = append(events, late)
	in := trainInput(t, events)

	m, err := Train(in)
	if err != nil {
		t.Fatal(err)
	}
	// Ties by group: f00 f01 f02 f03 f04 would be the order, but f04 (event 0) has its
	// latest event an hour later, so it moves to the end.
	if got, want := m.Splits["fit"].Groups, []string{"mut:f00", "mut:f01", "mut:f02", "mut:f03"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fit groups = %v, want %v", got, want)
	}
	if got, want := m.Splits["scale"].Groups, []string{"mut:f04"}; !reflect.DeepEqual(got, want) {
		t.Errorf("scale groups = %v, want %v", got, want)
	}
}

// TestTrainRefusesAFitWithoutBothClasses is the guard on class support: a
// training set with no positive example must not become an artifact.
func TestTrainRefusesAFitWithoutBothClasses(t *testing.T) {
	cases := map[string]func(ex *Example){
		"no positives": func(ex *Example) { ex.Label = false },
		"no negatives": func(ex *Example) { ex.Label = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := lcg(5)
			events := trainEvents(&g, 10, 20)
			for i := range events {
				for j := range events[i].Examples {
					mutate(&events[i].Examples[j])
				}
			}
			in := trainInput(t, events)
			if err := os.MkdirAll(in.OutDir, 0o755); err != nil {
				t.Fatal(err)
			}
			m, err := Train(in)
			if err != nil {
				t.Fatalf("Train: %v", err)
			}
			if got := dirEntries(t, in.OutDir); len(got) != 0 {
				t.Errorf("a refused training wrote %v, want nothing", got)
			}
			if len(m.Artifacts) != 0 {
				t.Errorf("artifacts = %+v, want none", m.Artifacts)
			}
			refused := map[string]string{}
			for _, r := range m.Refusals {
				refused[r.Predictor] = r.Reason
			}
			wantClass := strings.TrimPrefix(name, "no ")
			for _, p := range []string{"static", "logistic"} {
				if r, ok := refused[p]; !ok || !strings.Contains(r, "no "+wantClass[:len(wantClass)-1]) {
					t.Errorf("refusal of %s = %q (present %v), want a message about missing %s", p, r, ok, wantClass)
				}
			}
		})
	}
}

// TestTrainWritesTheStaticArtifactWhenOnlyTheLogisticFitIsRefused: the scale
// set holds no positive, so Platt scaling is impossible, but P0 is valid.
func TestTrainWritesTheStaticArtifactWhenOnlyTheLogisticFitIsRefused(t *testing.T) {
	g := lcg(5)
	events := trainEvents(&g, 10, 20)
	for i := 8; i < 10; i++ { // the two newest events land in the scale set
		for j := range events[i].Examples {
			events[i].Examples[j].Label = false
		}
	}
	in := trainInput(t, events)
	sentinel := filepath.Join(in.OutDir, "logistic.json")
	if err := os.MkdirAll(in.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("an earlier fit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := Train(in)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if len(m.Refusals) != 1 || m.Refusals[0].Predictor != "logistic" || !strings.Contains(m.Refusals[0].Reason, "scale set has no positive") {
		t.Fatalf("refusals = %+v, want only the logistic fit, for its scale set", m.Refusals)
	}
	if _, err := ReadStatic(filepath.Join(in.OutDir, "static.json")); err != nil {
		t.Errorf("static.json is not a valid artifact: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "an earlier fit\n" {
		t.Errorf("logistic.json = %q, %v, want the earlier file left untouched", got, err)
	}
	if _, ok := m.Artifacts["logistic"]; ok || m.Artifacts["static"].Digest == "" {
		t.Errorf("artifacts = %+v, want static only", m.Artifacts)
	}
	if _, err := os.Stat(filepath.Join(in.OutDir, "training.json")); err != nil {
		t.Errorf("training.json missing after a partial training: %v", err)
	}
}

// TestTrainRefusesAFitThatDoesNotConverge: unconverged weights are not emitted.
func TestTrainRefusesAFitThatDoesNotConverge(t *testing.T) {
	g := lcg(5)
	in := trainInput(t, trainEvents(&g, 10, 20))
	in.Options = DefaultLogisticOptions()
	in.Options.MaxIter = 2

	m, err := Train(in)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if len(m.Refusals) != 1 || m.Refusals[0].Predictor != "logistic" || !strings.Contains(m.Refusals[0].Reason, "did not converge") {
		t.Fatalf("refusals = %+v, want the logistic fit refused for not converging", m.Refusals)
	}
	if _, err := os.Stat(filepath.Join(in.OutDir, "logistic.json")); !os.IsNotExist(err) {
		t.Errorf("logistic.json written for a fit that did not converge (%v)", err)
	}
	if _, err := ReadStatic(filepath.Join(in.OutDir, "static.json")); err != nil {
		t.Errorf("static.json: %v", err)
	}
}

func TestTrainWithoutExamplesRefusesBothAndWritesNothing(t *testing.T) {
	in := trainInput(t, []TrainEvent{{ID: "e", Origin: "mutation", Group: "mut:x", Time: trainEpoch}})
	m, err := Train(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Refusals) != 2 || len(m.Artifacts) != 0 {
		t.Errorf("refusals = %+v, artifacts = %+v, want both fits refused", m.Refusals, m.Artifacts)
	}
	if got := dirEntries(t, in.OutDir); len(got) != 0 {
		t.Errorf("wrote %v, want nothing", got)
	}
	if m.Examples.Events != 0 || len(m.Splits["fit"].Groups) != 0 {
		t.Errorf("manifest = %+v, want no events with examples", m)
	}
}
