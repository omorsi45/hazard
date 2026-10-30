package eval

import (
	"reflect"
	"testing"

	"github.com/omorsi45/hazard/internal/control"
)

// inventory is the T(c) of the fixture events: c1 plus t1..t10.
var inventory = []string{"c1", "t1", "t10", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9"}

// fixtureMS prices c1 at 10, t1 at 20 and every other test at 5: cost(T) = 75.
var fixtureMS = map[string]int{"c1": 10, "t1": 20, "t10": 5, "t2": 5, "t3": 5, "t4": 5, "t5": 5, "t6": 5, "t7": 5, "t8": 5, "t9": 5}

func fixtureCost() *CostModel { return NewCostModel(fixtureMS) }

// fixtureLayer is a layer whose CERTAIN set is c1 and whose controller input
// has no abstention rule that fires by itself.
func fixtureLayer(overheadMS float64) *Layer {
	return &Layer{
		Certain: []string{"c1"}, OverheadMS: overheadMS,
		Input: control.Input{Certain: []string{"c1"}, AllTests: inventory, TestMS: fixtureMS, MinSavingRatio: 0},
	}
}

// fixtureEvent is an event with failing tests and scored candidates for the
// predictor "static", scored on layer.
func fixtureEvent(id, partition string, failing []string, cands []control.Candidate, layer *Layer) *Event {
	e := NewEvent(id, "go", partition, failing, inventory)
	e.Graph, e.Coverage, e.PLayer = layer, layer, layer
	e.Scored = map[string]*Scored{"static": NewScored(e, cands, 0.5, 0)}
	return e
}

func cand(id string, p float64) control.Candidate { return control.Candidate{TestID: id, P: p} }

func TestFullIsTheFullSuiteWithZeroOverhead(t *testing.T) {
	e := fixtureEvent("m1", "test", []string{"t1", "t2"}, nil, fixtureLayer(3))
	got := Evaluator{Cost: fixtureCost()}.Full(e)
	want := Outcome{Kind: "go", Failing: 2, Caught: 2, Selected: 11, Total: 11, CostS: 75, CostT: 75}
	if got != want {
		t.Errorf("Full = %+v, want %+v", got, want)
	}
}

func TestBaselineSelectsTheCertainSetAndMissesTheRest(t *testing.T) {
	e := fixtureEvent("m1", "test", []string{"c1", "t1"}, nil, fixtureLayer(3))
	got := Evaluator{Cost: fixtureCost()}.Baseline(e, e.Graph)
	want := Outcome{Kind: "go", Failing: 2, Caught: 1, Selected: 1, Total: 11, CostS: 10, CostT: 75, Overhead: 3}
	if got != want {
		t.Errorf("Baseline = %+v, want %+v", got, want)
	}
}

// An L1 full-suite reason (trigger, unmapped change, incomplete index) is an
// abstention for the baselines: recall 1 for the event, cost = full.
func TestBaselineAbstainsWhenL1DemandsTheFullSuite(t *testing.T) {
	for name, mutate := range map[string]func(*Layer){
		"full reason": func(l *Layer) { l.FullReason = "unmapped_change:x.go" },
		"stopped":     func(l *Layer) { l.Stopped = "index_stale" },
		"trigger":     func(l *Layer) { l.Input.TriggersFull = []string{"go.mod"} },
	} {
		l := fixtureLayer(4)
		mutate(l)
		e := fixtureEvent("m1", "test", []string{"t1"}, nil, l)
		got := Evaluator{Cost: fixtureCost()}.Baseline(e, l)
		want := Outcome{Kind: "go", Failing: 1, Caught: 1, Selected: 11, Total: 11, CostS: 75, CostT: 75, Overhead: 4, Abstained: true}
		if got != want {
			t.Errorf("%s: Baseline = %+v, want %+v", name, got, want)
		}
	}
}

func TestOutcomesWithoutDurationsCarryNoCost(t *testing.T) {
	e := fixtureEvent("m1", "test", []string{"t1"}, nil, fixtureLayer(0))
	got := Evaluator{}.Baseline(e, e.Graph)
	if got.CostS != 0 || got.CostT != 0 || got.Selected != 1 || got.Total != 11 {
		t.Errorf("Baseline without a cost model = %+v, want counts only", got)
	}
}

func TestPredictedSelectsCandidatesAtOrAboveLambda(t *testing.T) {
	cands := []control.Candidate{cand("t1", 0.4), cand("t2", 0.05)}
	e := fixtureEvent("m1", "test", []string{"t1"}, cands, fixtureLayer(3))
	ev := Evaluator{Cost: fixtureCost()}
	got := ev.Predicted(e, "static", 0.3)
	// c1 and t1 run: the controller adds the candidate whose probability reaches 0.3.
	want := Outcome{
		Kind: "go", Failing: 1, Caught: 1, Selected: 2, Total: 11, CostS: 30, CostT: 75,
		Overhead: 3.5, BrierSum: 0.36 + 0.0025, BrierN: 2,
	}
	if !outcomeNear(got, want) {
		t.Errorf("Predicted(0.3) = %+v, want %+v", got, want)
	}
	missed := ev.Predicted(e, "static", 0.5)
	if missed.Caught != 0 || missed.Selected != 1 || missed.Abstained {
		t.Errorf("Predicted(0.5) = %+v, want t1 missed with only c1 selected", missed)
	}
}

func outcomeNear(a, b Outcome) bool {
	bs := a.BrierSum - b.BrierSum
	if bs < 0 {
		bs = -bs
	}
	a.BrierSum, b.BrierSum = 0, 0
	return bs < 1e-12 && a == b
}

// The abstention rules of production apply: here the candidate cap cut more
// than 10% of the residual, so the controller runs the full suite.
func TestPredictedAbstainsAsTheControllerDoes(t *testing.T) {
	l := fixtureLayer(2)
	l.Input.RawCandidates, l.Input.Truncated = 10, 5
	e := fixtureEvent("m1", "test", []string{"t1"}, []control.Candidate{cand("t1", 0.01)}, l)
	got := Evaluator{Cost: fixtureCost()}.Predicted(e, "static", 0.3)
	if !got.Abstained || got.Caught != 1 || got.Selected != 11 || got.CostS != 75 {
		t.Errorf("Predicted = %+v, want the full suite with every failing test caught", got)
	}
	if got.BrierN != 1 {
		t.Errorf("brier pairs = %d, want the candidate still scored (1)", got.BrierN)
	}
}

func TestPredictedAbstainsWhenThePredictorFailed(t *testing.T) {
	e := fixtureEvent("m1", "test", []string{"t1"}, nil, fixtureLayer(2))
	e.Scored["static"] = &Scored{Err: "model drift"}
	got := Evaluator{Cost: fixtureCost()}.Predicted(e, "static", 0.3)
	if !got.Abstained || got.Selected != 11 || got.BrierN != 0 {
		t.Errorf("Predicted = %+v, want the full suite and no pair", got)
	}
}

func TestPredictedAbstainsWhenL1DemandsTheFullSuite(t *testing.T) {
	l := fixtureLayer(2)
	l.FullReason = "trigger:go.mod"
	l.Input.FullReason = l.FullReason
	e := fixtureEvent("m1", "test", []string{"t1"}, nil, l)
	e.Scored = map[string]*Scored{} // nobody was asked
	got := Evaluator{Cost: fixtureCost()}.Predicted(e, "static", 0.3)
	if !got.Abstained || got.Selected != 11 {
		t.Errorf("Predicted = %+v, want the full suite", got)
	}
}

func TestPairsAreLabelledByTheFailingSet(t *testing.T) {
	e := fixtureEvent("m1", "test", []string{"t1"}, []control.Candidate{cand("t1", 0.4), cand("t2", 0.05)}, fixtureLayer(0))
	got := e.Pairs("static")
	want := []ProbLabel{{P: 0.4, Label: true}, {P: 0.05, Label: false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pairs = %+v, want %+v", got, want)
	}
	if e.Pairs("logistic") != nil {
		t.Error("an unscored predictor has no pairs")
	}
}

// tuneTestFixture has tune and test disagree: on tune t1 scores 0.4, on test 0.1.
func tuneTestFixture() (tune, test []*Event) {
	for _, id := range []string{"a", "b", "c"} {
		tune = append(tune, fixtureEvent("tune-"+id, "tune", []string{"t1"}, []control.Candidate{cand("t1", 0.4), cand("t2", 0.05)}, fixtureLayer(1)))
		test = append(test, fixtureEvent("test-"+id, "test", []string{"t1"}, []control.Candidate{cand("t1", 0.1), cand("t2", 0.05)}, fixtureLayer(1)))
	}
	return tune, test
}

func gridOf(t *testing.T, spec string) []float64 {
	t.Helper()
	g, err := control.ParseGrid(spec)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Guard 1: lambda is chosen on the tune events. Here tune reaches the recall up
// to lambda 0.4 and test only up to 0.1, so a lambda fitted on test would be
// 0.1 and the test recall would look perfect.
func TestRunPChoosesLambdaOnTuneOnly(t *testing.T) {
	tune, test := tuneTestFixture()
	res := Evaluator{Cost: fixtureCost()}.RunP(tune, test, "static", gridOf(t, "0.05:0.5:0.05"), 0.99)
	if !res.Fit.Feasible || res.Fit.Lambda != 0.4 {
		t.Fatalf("fit = %+v, want the largest tune-feasible lambda 0.4", res.Fit)
	}
	if res.Fit.TunePositive != 3 || res.Fit.TuneRecall != 1 {
		t.Errorf("tune evidence = %+v, want 3 positive events at recall 1", res.Fit)
	}
	got := Summarize(res.Outcomes, []string{MetricRecallCommit}, 10, 1)
	wantValue(t, got, MetricRecallCommit, 0) // at lambda 0.4 the test events miss t1: reported as measured
	if len(res.Outcomes) != 3 {
		t.Errorf("outcomes = %d, want the 3 test events", len(res.Outcomes))
	}
}

func TestRunPReportsInfeasibleAtTheSmallestLambda(t *testing.T) {
	// t1 scores 0 on tune: no lambda of the grid can select it.
	var tune, test []*Event
	for _, id := range []string{"a", "b"} {
		tune = append(tune, fixtureEvent("tune-"+id, "tune", []string{"t1"}, []control.Candidate{cand("t1", 0)}, fixtureLayer(1)))
		test = append(test, fixtureEvent("test-"+id, "test", []string{"t1"}, []control.Candidate{cand("t1", 0.9)}, fixtureLayer(1)))
	}
	res := Evaluator{Cost: fixtureCost()}.RunP(tune, test, "static", gridOf(t, "0.05:0.5:0.05"), 0.99)
	if res.Fit.Feasible || res.Fit.Lambda != 0.05 || res.Fit.TuneRecall != 0 {
		t.Errorf("fit = %+v, want infeasible, reported at the smallest lambda 0.05", res.Fit)
	}
	// The test events are still evaluated, at 0.05, where t1 (0.9) is selected.
	wantValue(t, Summarize(res.Outcomes, []string{MetricRecallCommit}, 10, 1), MetricRecallCommit, 1)
}

func TestRunPWithoutATunePositiveHasNoOperatingPoint(t *testing.T) {
	tune := []*Event{fixtureEvent("tune-a", "tune", nil, []control.Candidate{cand("t1", 0.4)}, fixtureLayer(1))}
	_, test := tuneTestFixture()
	res := Evaluator{Cost: fixtureCost()}.RunP(tune, test, "static", gridOf(t, "0.05:0.5:0.05"), 0.99)
	if res.Fit.Feasible || res.Fit.Reason == "" || res.Outcomes != nil {
		t.Errorf("RunP = %+v, want no fit, a reason and no test outcome (lambda is never read off test)", res)
	}
}
