package eval

import (
	"github.com/omorsi45/hazard/internal/control"
)

// Layer is the deterministic layer (L1) of one event in one mode, with what the
// configurations need of it.
type Layer struct {
	Stopped     string        // the engine stopped before L1 ran: the reason; the full suite
	FullReason  string        // L1 demands the full suite
	Certain     []string      // CERTAIN and always-run test IDs, the selection of B1 and B2
	TruncMissed int           // failing tests cut by the candidate cap
	OverheadMS  float64       // what the engine spent on the event in this mode
	Input       control.Input // the controller's input without candidates and lambda
}

// demandsFull reports whether the layer alone sends the event to the full
// suite: the engine stopped, L1 says so, or the change matches a trigger.
func (l *Layer) demandsFull() bool {
	return l.Stopped != "" || l.FullReason != "" || len(l.Input.TriggersFull) > 0
}

// Scored is what a predictor said about the residual candidates of an event.
type Scored struct {
	Cands    []control.Candidate
	ScoreMS  float64
	USD      float64
	Err      string // the predictor failed on this event: the controller abstains
	BrierSum float64
	BrierN   int
}

// Event is one replayed event with the outcome of every layer and predictor.
type Event struct {
	ID, Kind, Partition string
	Failing             []string // F(c)
	All                 []string // T(c)
	Graph, Coverage     *Layer   // L1 in each mode; nil when not run
	PLayer              *Layer   // the layer the predictors scored: coverage when the index has it
	Scored              map[string]*Scored

	failSet map[string]bool
	costFor *CostModel // the model costT was computed with
	costT   float64
}

// NewEvent returns an event with its failing set indexed.
func NewEvent(id, kind, partition string, failing, all []string) *Event {
	e := &Event{ID: id, Kind: kind, Partition: partition, Failing: failing, All: all, failSet: make(map[string]bool, len(failing))}
	for _, t := range failing {
		e.failSet[t] = true
	}
	return e
}

// probability is the probability the controller reads of a candidate: one the
// predictor could not score, or an invalid one, counts as 1 (unknown is never
// zero).
func probability(c control.Candidate) float64 {
	if c.Failed || !(c.P >= 0 && c.P <= 1) {
		return 1
	}
	return c.P
}

// NewScored records the answer of a predictor for e and its Brier terms.
func NewScored(e *Event, cands []control.Candidate, scoreMS, usd float64) *Scored {
	s := &Scored{Cands: cands, ScoreMS: scoreMS, USD: usd, BrierN: len(cands)}
	for _, c := range cands {
		p, y := probability(c), 0.0
		if e.failSet[c.TestID] {
			y = 1
		}
		s.BrierSum += (p - y) * (p - y)
	}
	return s
}

// Pairs are the (probability, failed) pairs of the event for a predictor; none
// when the predictor was not asked or failed.
func (e *Event) Pairs(kind string) []ProbLabel {
	s := e.Scored[kind]
	if s == nil || s.Err != "" {
		return nil
	}
	pairs := make([]ProbLabel, len(s.Cands))
	for i, c := range s.Cands {
		pairs[i] = ProbLabel{P: probability(c), Label: e.failSet[c.TestID]}
	}
	return pairs
}

// Evaluator turns events into outcomes. Cost is nil when no duration is known:
// the outcomes then carry no cost, and no time metric may be read from them.
type Evaluator struct{ Cost *CostModel }

// costOfAll is cost(T(c)), kept on the event.
func (ev Evaluator) costOfAll(e *Event) float64 {
	if ev.Cost == nil {
		return 0
	}
	if e.costFor != ev.Cost {
		e.costT, e.costFor = ev.Cost.Sum(e.All), ev.Cost
	}
	return e.costT
}

// full is the outcome of running everything. The labels come from a run of the
// full suite, so every failing test is caught.
func (ev Evaluator) full(e *Event, overhead float64) Outcome {
	o := Outcome{Kind: e.Kind, Failing: len(e.Failing), Caught: len(e.Failing), Selected: len(e.All), Total: len(e.All), Overhead: overhead}
	o.CostT = ev.costOfAll(e)
	o.CostS = o.CostT
	return o
}

// abstain is the outcome of a selection that falls back to the full suite.
func (ev Evaluator) abstain(e *Event, overhead float64) Outcome {
	o := ev.full(e, overhead)
	o.Abstained = true
	return o
}

// selection is the outcome of running exactly the tests in sel. A failing test
// that is not in sel is a miss, whether or not the inventory knows it.
func (ev Evaluator) selection(e *Event, sel []string, overhead float64) Outcome {
	o := Outcome{Kind: e.Kind, Failing: len(e.Failing), Selected: len(sel), Total: len(e.All), Overhead: overhead}
	for _, id := range sel {
		if e.failSet[id] {
			o.Caught++
		}
	}
	if ev.Cost != nil {
		o.CostT = ev.costOfAll(e)
		o.CostS = ev.Cost.Sum(sel)
	}
	return o
}

// Full is B0: run everything. It is not an abstention and has no overhead.
func (ev Evaluator) Full(e *Event) Outcome { return ev.full(e, 0) }

// Baseline is B1 or B2: the CERTAIN set and the always-run tests of a layer.
// When the layer demands the full suite that is what runs, counted as an
// abstention.
func (ev Evaluator) Baseline(e *Event, l *Layer) Outcome {
	if l == nil || l.demandsFull() {
		overhead := 0.0
		if l != nil {
			overhead = l.OverheadMS
		}
		return ev.abstain(e, overhead)
	}
	return ev.selection(e, l.Certain, l.OverheadMS)
}

// Predicted is P0 or P1: the controller's decision at lambda over the scored
// candidates of kind, abstentions included. A predictor that failed, or an
// event nobody scored although L1 allowed it, runs the full suite.
func (ev Evaluator) Predicted(e *Event, kind string, lambda float64) Outcome {
	l, s := e.PLayer, e.Scored[kind]
	if l == nil {
		return ev.abstain(e, 0)
	}
	overhead := l.OverheadMS
	var usd, brierSum float64
	brierN := 0
	if s != nil {
		overhead += s.ScoreMS
		usd = s.USD
		if s.Err == "" {
			brierSum, brierN = s.BrierSum, s.BrierN
		}
	}

	var o Outcome
	switch {
	case l.demandsFull():
		o = ev.abstain(e, overhead)
	case s == nil || s.Err != "":
		o = ev.abstain(e, overhead)
	default:
		in := l.Input
		in.Lambda, in.Candidates = lambda, s.Cands
		d := control.Decide(in)
		if d.FullSuite {
			o = ev.abstain(e, overhead)
			break
		}
		ids := make([]string, len(d.Members))
		for i, m := range d.Members {
			ids[i] = m.TestID
		}
		o = ev.selection(e, ids, overhead)
		o.TruncMissed = l.TruncMissed
	}
	o.USD, o.BrierSum, o.BrierN = usd, brierSum, brierN
	return o
}

// Fit is the operating point chosen on the tune events.
type Fit struct {
	Lambda       float64 `json:"lambda"`
	Feasible     bool    `json:"feasible"`
	TunePositive int     `json:"tune_events_with_failures"`
	TuneRecall   float64 `json:"tune_recall_commit"`
	Reason       string  `json:"reason,omitempty"` // why there is no operating point
}

// FitLambda is the largest value of the grid at which the per-commit recall of
// the tune events reaches the target. If none does, the fit is infeasible and
// sits at the smallest value; the target is never lowered. Without a tune event
// that has a failing test there is no recall to read and no fit.
func (ev Evaluator) FitLambda(tune []*Event, kind string, grid []float64, recall float64) Fit {
	if len(grid) == 0 {
		return Fit{Reason: "the lambda grid is empty"}
	}
	positive := 0
	for _, e := range tune {
		if len(e.Failing) > 0 {
			positive++
		}
	}
	if positive == 0 {
		return Fit{Reason: "no tune event has a failing test"}
	}
	fit := Fit{TunePositive: positive}
	for i := len(grid) - 1; i >= 0; i-- {
		hits := 0
		for _, e := range tune {
			if o := ev.Predicted(e, kind, grid[i]); o.Failing > 0 && o.Caught >= o.Failing {
				hits++
			}
		}
		fit.Lambda, fit.TuneRecall = grid[i], float64(hits)/float64(positive)
		if fit.TuneRecall >= recall {
			fit.Feasible = true
			return fit
		}
	}
	return fit // infeasible: Lambda is grid[0] and TuneRecall its recall
}

// PResult is a predictor's operating point and its outcomes on the test events.
type PResult struct {
	Fit      Fit
	Outcomes []Outcome
}

// RunP chooses lambda on the tune events only and evaluates the test events
// at it. The test events are never read to choose lambda; with no operating
// point they are not evaluated at all.
func (ev Evaluator) RunP(tune, test []*Event, kind string, grid []float64, recall float64) PResult {
	fit := ev.FitLambda(tune, kind, grid, recall)
	if fit.Reason != "" {
		return PResult{Fit: fit}
	}
	out := make([]Outcome, len(test))
	for i, e := range test {
		out[i] = ev.Predicted(e, kind, fit.Lambda)
	}
	return PResult{Fit: fit, Outcomes: out}
}
